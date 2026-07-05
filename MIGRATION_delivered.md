# DM "delivered" (iki tick) — Laravel migration

Go tarafı artık `messages.delivered` kolonunu **okuyup yazıyor** (double tick /
çatdırıldı statusu: WS `mark_delivered` frame-i, canlı push-da server-side
işarələmə, read-implies-delivered). Model-də sahə çoxdan var idi
(`models/message.go` → `Delivered bool`), amma bu repo-da **AutoMigrate yoxdur** —
`messages` tablosunu Laravel yönettiği için kolon Laravel migration ile
eklenmelidir. **Kolon artıq DB-də varsa bu migration lazım deyil** (əvvəlcə
`\d messages` ilə yoxla).

## 1) Migration oluştur

```bash
php artisan make:migration add_delivered_to_messages_table --table=messages
```

## 2) Migration içeriği

```php
public function up(): void
{
    Schema::table('messages', function (Blueprint $table) {
        $table->boolean('delivered')->default(false)->after('read');
    });
}

public function down(): void
{
    Schema::table('messages', function (Blueprint $table) {
        $table->dropColumn('delivered');
    });
}
```

> `after('read')` MySQL içindir; Postgres'te `after` yok sayılır, sorun değil.

## 3) Çalıştır

```bash
php artisan migrate
```

## Alternatif — düz SQL

**Postgres:**
```sql
ALTER TABLE messages ADD COLUMN IF NOT EXISTS delivered BOOLEAN NOT NULL DEFAULT FALSE;
```

**MySQL:**
```sql
ALTER TABLE messages ADD COLUMN delivered TINYINT(1) NOT NULL DEFAULT 0 AFTER `read`;
```

### Opsional index (Postgres, böyük cədvəldə tövsiyə olunur)

`mark_delivered` sorğusu `WHERE sender_id = ? AND receiver_id = ? AND
delivered = false` işlədir. Mövcud `sender_id`/`receiver_id` indeksləri adətən
kifayətdir; çatdırılmamış mesaj sayı çox olan sistemdə partial index kömək edir:

```sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_undelivered
ON messages (receiver_id, sender_id) WHERE delivered = FALSE;
```

---

## Nasıl çalışıyor

- **Canlı çatdırılma:** hub `new_message`-i BAĞLI alıcının kanalına uğurla
  yazanda `delivered=true` + göndərənə `message_delivered` event-i (ani iki
  tick, WhatsApp davranışı).
- **Client ack:** push/offline gəlişlərində yeni client WS ilə
  `{"type":"mark_delivered","data":{"sender_id":<qarşı tərəf>}}` göndərir →
  toplu UPDATE + göndərənə `message_delivered` (`other_user_id`,
  `message_ids`, 500-dən çox olduqda əlavə `all_before`).
- **Read implies delivered:** bütün oxundu yolları (`mark_read` WS,
  `MarkConversationAsRead`, `GetMessages` auto-read, tək mesaj `MarkAsRead`)
  eyni sətirlərdə `delivered=true` də yazır (əlavə event yox — mövcud
  `message_read` onsuz da göndərəni xəbərdar edir).
- **Serialization (additiv):** `GET /messages/:user_id` cavabına `delivered`,
  `GET /conversations` cavabına `last_message_delivered` sahəsi əlavə olundu.
  Köhnə client-lər tanımadığı sahələri sadəcə görmür — heç bir mövcud sahə
  dəyişməyib.
