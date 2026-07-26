# MIGRATION — `message_reads` üzərində UNIQUE indeks (Issue 15)

## Problem

`message_reads` üzərində `(message_id, user_id)` üçün UNIQUE məhdudiyyət YOX idi
və insert-lər `ON CONFLICT` işlətmirdi (`Clauses()` arqümansız çağırılırdı).

Eyni cüt ən azı üç paralel yoldan yazıla bilir:

1. göndərim anındakı avto-oxundu goroutine-i (`SendGroupMessage`),
2. eyni hesabın ikinci cihazında qrupun açılması (`GetGroupMessages`),
3. `POST /groups/:id/mark-read` (`MarkGroupConversationRead`).

Nəticə: `read_count` (`SELECT COUNT(*) FROM message_reads …`) GERÇƏK oxuyucu
sayından böyük çıxır ("5 nəfər gördü" — halbuki qrupda 3 nəfər var),
`GetMessageReads` eyni istifadəçini bir neçə dəfə sadalayır və cədvəl
lüzumsuz böyüyür.

## Deploy sırası SƏRBƏSTDİR

Kod **hədəf sütunSUZ** `ON CONFLICT DO NOTHING` göndərir. Bu forma:

* indeks **yoxdursa** — adi INSERT kimi davranır (köhnə davranış: dublikat
  yarana bilər, amma heç nə qırılmır),
* indeks **varsa** — dublikat sükutla atılır.

Ona görə migration-ı deploy-dan əvvəl də, sonra da çalışdıra bilərsiniz.

> Hədəfli forma (`ON CONFLICT (message_id, user_id)`) BİLƏRƏKDƏN işlədilməyib:
> uyğun unique indeks olmadan Postgres onu DƏRHAL rədd edir
> (`there is no unique or exclusion constraint matching the ON CONFLICT specification`),
> yəni kod migration-dan əvvəl deploy olunsa oxundu-işarələmə tamamilə qırılardı.

## Tətbiq

```sql
-- 1) Mövcud dublikatları təmizlə (ən köhnə sətri saxla).
DELETE FROM message_reads a
USING message_reads b
WHERE a.message_id = b.message_id
  AND a.user_id    = b.user_id
  AND a.id > b.id;

-- 2) UNIQUE indeks. CONCURRENTLY → cədvəl kilidlənmir (canlıda təhlükəsiz).
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS
  message_reads_message_user_uniq
  ON message_reads (message_id, user_id);
```

`CREATE INDEX CONCURRENTLY` transaction BLOKUNDA işləmir — ayrıca çalışdırın.
Uğursuz olarsa indeks `INVALID` qalır; `DROP INDEX` edib təkrarlayın.

## Yoxlama

```sql
-- Dublikat qalmamalı (0 sətir).
SELECT message_id, user_id, COUNT(*) FROM message_reads
GROUP BY 1,2 HAVING COUNT(*) > 1;
```
