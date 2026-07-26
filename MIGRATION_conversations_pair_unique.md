# MIGRATION — `conversations` cütü üzərində UNIQUE indeks (Issue 13)

## Problem

DM conversation-ları `GetOrCreateConversation` ilə **SELECT-sonra-INSERT** kimi
yaradılırdı: sətir kilidi yox, birləşik unique indeks yox. İki paralel "ilk
mesaj" hər ikisi də mövcud sətri tapmır və hər ikisi INSERT edirdi.

Nəticə: eyni istifadəçi cütü üçün **iki** conversation sətri → bölünmüş
tarixçə, bölünmüş `user1/user2_message_count` (pending limiti işləmir),
uyuşmayan mute / pin / nickname / wallpaper, səhv oxunmamış sayı.

Kod tərəfi HƏMİŞƏ normallaşdırılmış cüt yazır (kiçik id = user1) — həm REST,
həm WS. Qalan tək boşluq indeksdir.

## DİQQƏT — indeks PARTIAL olmalıdır (İKİ şərtlə)

1. **Qrup sətirləri.** `conversations` ikili məqsədlidir: qrup söhbətləri də
   burada saxlanılır (`chat_type='group'`) və o sətirlərdə `user1_id`/`user2_id`
   mənasızdır (eyni dəyərləri paylaşa bilərlər). **Tam** unique indeks qrupları
   qırar.

2. **Yumşaq silinmiş sətirlər (`deleted_at`).** `models.Conversation` GORM
   `DeletedAt` daşıyır, ona görə BÜTÜN oxumalar avtomatik `deleted_at IS NULL`
   əlavə edir. İndeks bunu istisna etməsə:
   * yumşaq silinmiş dublikat varsa **indeks yaradıla bilmir**
     (`Key (user1_id, user2_id)=(...) is duplicated`);
   * daha pisi, çalışma zamanı həmin cüt ÜÇÜN mesajlaşma TAMAMİLƏ KİLİDLƏNİR:
     ilk `SELECT` sətri görmür (soft-deleted) → `INSERT` indekslə TOQQUŞUR
     (indeks onu görür) → `DO NOTHING` → `ID=0` → yenidən `SELECT` yenə tapmır
     → xəta qaytarılır → **bütün göndərmə transaction-ı geri alınır** (HTTP 500
     / `message_error`), və bu hər dəfə təkrarlanır.

   Ona görə indeksdə `AND deleted_at IS NULL` MÜTLƏQDİR.

## Tətbiq

```sql
-- 1) Mövcud DM dublikatlarını AŞKAR ET.
--    Avtomatik silmə YOX — hansı sətrin tarixçəsi/ayarları saxlanacağı
--    məhsul qərarıdır.
SELECT user1_id, user2_id, COUNT(*), array_agg(id ORDER BY id) AS ids
FROM conversations
WHERE chat_type IS DISTINCT FROM 'group' AND deleted_at IS NULL
GROUP BY 1,2 HAVING COUNT(*) > 1;

-- 2) Dublikatlar birləşdirildikdən SONRA partial unique indeks.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS
  conversations_dm_pair_uniq
  ON conversations (user1_id, user2_id)
  WHERE chat_type IS DISTINCT FROM 'group'
    AND deleted_at IS NULL;
```

Dublikat birləşdirmə üçün təklif olunan sıra (ən köhnə sətri saxla):
`messages.conversation_id`, `conversation_participants`, mute/pin/wallpaper
sütunlarını köhnə sətrə köçür, sonra yeni sətri sil.

`CREATE INDEX CONCURRENTLY` transaction BLOKUNDA işləmir — ayrıca çalışdırın.
Uğursuz olarsa indeks `INVALID` qalır; `DROP INDEX` edib təkrarlayın.

## Deploy sırası SƏRBƏSTDİR

Kod `ON CONFLICT DO NOTHING` (hədəf sütunSUZ) işlədir. Bu forma:

* indeks **yoxdursa** — adi INSERT kimi davranır (köhnə davranış; yarış qalır,
  amma heç nə qırılmır),
* indeks **varsa** — dublikat sükutla atılır və qazanan sətir yenidən oxunur.

Ona görə migration-ı deploy-dan əvvəl də, sonra da çalışdıra bilərsiniz.
(`ON CONFLICT (user1_id, user2_id)` — yəni hədəfli forma — indeks olmadan
Postgres-də DƏRHAL xəta verərdi; bilərəkdən istifadə olunmayıb.)

## Yoxlama

```sql
-- 0 sətir qaytarmalıdır.
SELECT user1_id, user2_id, COUNT(*) FROM conversations
WHERE chat_type IS DISTINCT FROM 'group'
GROUP BY 1,2 HAVING COUNT(*) > 1;
```
