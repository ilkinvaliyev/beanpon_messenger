# MIGRATION — dəvət linklərinə müddət (Issue 18)

## Problem

`conversations.invite_token_expires_at` sütunu **mövcud idi** və qoşulma
yollarında (`PreviewByToken`, `JoinByToken`) yoxlanılırdı — amma **heç bir
yerdə TƏYİN EDİLMİRDİ**. Nə qrup yaradılanda, nə də `RefreshInviteToken`-də.
Yoxlama həmişə `NULL` görüb keçirdi, yəni **hər dəvət linki əbədi idi**:

* bir dəfə paylaşılan link (skrinşot, forward, arxivlənmiş söhbət, indekslənmiş
  veb səhifə) aylar sonra da qrupa giriş verirdi;
* `RefreshInviteToken` yalnız tokeni dəyişirdi → admin "bu link nə vaxta qədər
  etibarlıdır?" sualına cavab verə bilmirdi.

## Kod tərəfi (miqrasiya TƏLƏB ETMİR)

`handlers/group_handler.go`:

* `inviteTokenExpiry(now)` — `INVITE_TOKEN_TTL_HOURS` mühit dəyişənindən oxuyur,
  default **168 saat (7 gün)**. `0` → müddətsiz (açıq seçim).
* `CreateGroup` — yeni qrupun tokeninə TTL yazır.
* `RefreshInviteToken` — yenilənən tokenə TTL yazır və cavabda
  `invite_token_expires_at` qaytarır.

Bu dəyişiklik **yalnız yeni yaradılan/yenilənən** tokenlərə təsir edir.

## ⚠️ MÖVCUD QRUPLAR — BACKFILL (ayrıca qərar)

Kod dəyişikliyi köhnə qrupların linklərinə **toxunmur**: onların
`invite_token_expires_at` dəyəri `NULL` qalır və linkləri **əbədi işləməyə
davam edir**. Yəni əsl risk (indiyə qədər yayılmış bütün linklər) yalnız
aşağıdakı backfill ilə bağlanır.

Bu qəsdən ayrı addımdır: backfill bir anda **bütün** mövcud dəvət linklərini
etibarsız edir. Bu, məhsul qərarıdır — istifadəçilərə əvvəlcədən xəbər verilməli
və adminlərin "linki yenilə" düyməsindən xəbəri olmalıdır.

### Variant A — yumşaq keçid (TÖVSİYƏ OLUNAN)

Mövcud linklərə **30 günlük** möhlət verilir; bu müddətdə adminlər yeni link
yarada bilir.

```sql
UPDATE conversations
SET invite_token_expires_at = NOW() + INTERVAL '30 days'
WHERE chat_type = 'group'
  AND deleted_at IS NULL
  AND invite_token IS NOT NULL
  AND invite_token_expires_at IS NULL;
```

### Variant B — dərhal bağla (yüksək risk halında)

Yalnız sızma şübhəsi varsa. Bütün köhnə linklər **dərhal** ölür.

```sql
UPDATE conversations
SET invite_token_expires_at = NOW()
WHERE chat_type = 'group'
  AND deleted_at IS NULL
  AND invite_token IS NOT NULL
  AND invite_token_expires_at IS NULL;
```

### Variant C — yalnız köhnə/passiv qruplar

Son 90 gündə heç bir mesaj olmayan qrupların linkləri (ən çox sızmış, ən az
istifadə olunan) bağlanır; aktiv qruplar toxunulmur.

```sql
UPDATE conversations
SET invite_token_expires_at = NOW()
WHERE chat_type = 'group'
  AND deleted_at IS NULL
  AND invite_token IS NOT NULL
  AND invite_token_expires_at IS NULL
  AND COALESCE(last_message_at, created_at) < NOW() - INTERVAL '90 days';
```

## Geri qaytarma

```sql
-- Backfill-i ləğv et (linklər yenidən müddətsiz olur).
UPDATE conversations
SET invite_token_expires_at = NULL
WHERE chat_type = 'group' AND deleted_at IS NULL;
```

Kod tərəfini söndürmək üçün: `INVITE_TOKEN_TTL_HOURS=0`.

## Yoxlama

```sql
-- Müddətsiz link qalan qrup sayı (backfill-dən sonra 0 olmalıdır).
SELECT COUNT(*) FROM conversations
WHERE chat_type = 'group' AND deleted_at IS NULL
  AND invite_token IS NOT NULL AND invite_token_expires_at IS NULL;

-- Yaxın 7 gündə bitəcək linklər (adminlərə xatırlatma üçün).
SELECT id, group_name, invite_token_expires_at
FROM conversations
WHERE chat_type = 'group' AND deleted_at IS NULL
  AND invite_token_expires_at BETWEEN NOW() AND NOW() + INTERVAL '7 days'
ORDER BY invite_token_expires_at;
```
