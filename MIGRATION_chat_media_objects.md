# MIGRATION — `chat_media_objects` (Issue 56: sahibsiz S3 obyektləri)

## Problem

`POST /messenger/upload-media` və `/messenger/upload-voice` faylı S3-ə yazıb
URL qaytarır. Mesajın **göndərilməsi** isə AYRI bir sorğudur. Aralarında hər
şey baş verə bilər:

* istifadəçi şəkli composer-dən silir / fikrini dəyişir,
* tətbiq öldürülür, şəbəkə qopur,
* göndərmə icazə xətası (blok, spam-ban, qrup icazəsi) ilə rədd olunur.

Bu hallarda obyekt S3-də **əbədi** qalırdı: heç bir mesaj ona istinad etmir,
heç bir yerdə qeydi yoxdur, üstəlik `S3Uploader`-də **`Delete` metodu belə
yox idi**. Bucket ölçüsü yalnız artırdı.

## Niyə daha sadə həllər işləmir

**Bucket lifecycle qaydası ("N gündən köhnəni sil") — YOX.**
İstinad olunan media ilə sahibsiz eyni prefiksdədir (`images/user_7/…`).
Belə qayda istifadəçinin real şəkillərini də silərdi.

**"Mesaj silinəndə obyekti də sil" — YOX.**
İki səbəb:

1. Mesaj mətni `encrypted_text` sütununda **şifrəli** saxlanılır → server
   tərəfdə "bu açara neçə mesaj istinad edir?" sualını SQL ilə cavablamaq
   mümkün deyil.
2. `BroadcastMessage` **eyni** media URL-ini 20 alıcıya ayrı-ayrı mesaj sətri
   kimi yazır — birini silmək qalan 19-u sındırardı.

## Həll — istinad izləməsi

* Yükləmə anında `chat_media_objects`-ə sətir yazılır.
* Mesaj göndərilərkən mətn **hələ açıq** olduğu an (şifrələmədən ƏVVƏL)
  içindəki S3 açarları çıxarılır və `referenced_at` yazılır.
* Arxa-plan təmizləyicisi YALNIZ `referenced_at IS NULL` **və** yaşı 24
  saatdan böyük sətirləri silir (əvvəl S3-dən, sonra cədvəldən).

İstinad olunan media heç vaxt silinmir; broadcast təhlükəsizdir.

## Tətbiq

```sql
CREATE TABLE IF NOT EXISTS chat_media_objects (
    id            BIGSERIAL PRIMARY KEY,
    user_id       BIGINT      NOT NULL,
    s3_key        VARCHAR(512) NOT NULL,
    referenced_at TIMESTAMPTZ NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Təkrar yazılışın qarşısını alır (eyni açar iki dəfə qeydə düşməsin).
CREATE UNIQUE INDEX IF NOT EXISTS chat_media_objects_s3_key_uniq
    ON chat_media_objects (s3_key);

-- Təmizləyicinin isti sorğusu: WHERE referenced_at IS NULL AND created_at < ?
CREATE INDEX IF NOT EXISTS chat_media_objects_orphan_idx
    ON chat_media_objects (created_at)
    WHERE referenced_at IS NULL;

CREATE INDEX IF NOT EXISTS chat_media_objects_user_idx
    ON chat_media_objects (user_id);
```

`CREATE INDEX CONCURRENTLY` yalnız cədvəldə artıq data varsa lazımdır — yeni
cədvəl üçün adi forma kifayətdir.

## Deploy sırası SƏRBƏSTDİR

Kod **fail-open**-dur: cədvəl yoxdursa `MediaTracker` ilk xətada özünü
söndürür (`42P01 / does not exist`), bir dəfə log yazır və bir daha DB-yə
vurmur. Yükləmə və göndərmə axınları **heç bir şəkildə pozulmur** — sadəcə
təmizləmə işləmir. Ona görə miqrasiyanı deploy-dan əvvəl də, sonra da
çalışdıra bilərsiniz.

## Tənzimləmə

`services/media_gc.go` daxilində (`NewMediaTracker`):

| Sahə | Default | Mənası |
|---|---|---|
| `orphanTTL` | 24 saat | Bu müddətdən artıq istinadsız = sahibsiz |
| `sweepEvery` | 30 dəqiqə | Təmizləmə dövrü |
| `batchSize` | 500 | Bir dövrdə maksimum silinən obyekt |

`orphanTTL`-i 24 saatdan aşağı salmayın: istifadəçi şəkli seçib telefonu
kənara qoya, sonra göndərə bilər.

## Geriyə dönük təmizləmə (opsional, BİR DƏFƏLİK)

Miqrasiyadan ƏVVƏL yüklənmiş sahibsiz obyektlər cədvəldə yoxdur, ona görə
təmizləyici onlara toxunmur (təhlükəsiz default). Onları təmizləmək üçün
S3 tərəfdə obyekt siyahısını çıxarıb `messages` cədvəlindəki istinadlarla
tutuşdurmaq lazımdır — **amma mətn şifrəli olduğu üçün bu, yalnız tətbiq
səviyyəsində (deşifrə edərək) mümkündür**. Tövsiyə: bunu ayrıca bir dəfəlik
skript kimi, iş saatlarından kənarda və əvvəlcə `--dry-run` ilə çalışdırın.

## Yoxlama

```sql
-- İzləmə işləyirmi? (yükləmədən sonra sətir görünməlidir)
SELECT COUNT(*) FROM chat_media_objects;

-- Neçəsi istinadsızdır və nə qədər köhnədir?
SELECT COUNT(*) AS orphan_count, MIN(created_at) AS oldest
FROM chat_media_objects
WHERE referenced_at IS NULL;

-- Sağlam vəziyyət: göndərilən mesajların mediası tez işarələnir.
SELECT
  COUNT(*) FILTER (WHERE referenced_at IS NOT NULL) AS referenced,
  COUNT(*) FILTER (WHERE referenced_at IS NULL)     AS pending
FROM chat_media_objects
WHERE created_at > NOW() - INTERVAL '1 hour';
```
