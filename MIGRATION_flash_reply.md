# PiPoP (Flash) cevabı — messages snapshot kolonları (Laravel migration)

Kullanıcı bir PiPoP (flash) izlerken alttaki input ile flash sahibine DM
gönderebiliyor. Flash'lar messenger DB'sinde DEĞİL (rs_feed yazar / maingolang
okur) olduğu için mesaja JOIN yapılamaz — gönderim anında flash'ın **snapshot**'ı
mesaj satırına yazılır. Süresi (`flash_expires_at`) geçince client mesajda küçük
thumbnail yerine sadece "PiPoP" kartı gösterir.

Go tarafı artık bu 3 kolonu **okuyup yazıyor** (`models/message.go` →
`FlashID`/`FlashThumb`/`FlashExpiresAt`; `SendMessage` yazar, `GetMessages` /
`SearchMessages` / `SyncMessages` serialize eder, WS `new_message` içinde `flash`
objesi gider). Bu repo-da **AutoMigrate yoxdur** — `messages` tablosunu Laravel
yönettiği için kolonlar Laravel migration ile eklenmelidir. **Kolonlar artıq
DB-də varsa bu migration lazım deyil** (əvvəlcə `\d messages` ilə yoxla).

## 1) Migration oluştur

```bash
php artisan make:migration add_flash_snapshot_to_messages_table --table=messages
```

## 2) Migration içeriği

```php
public function up(): void
{
    Schema::table('messages', function (Blueprint $table) {
        $table->unsignedBigInteger('flash_id')->nullable()->after('story_id');
        $table->text('flash_thumb')->nullable()->after('flash_id');
        $table->timestamp('flash_expires_at')->nullable()->after('flash_thumb');
        $table->index('flash_id');
    });
}

public function down(): void
{
    Schema::table('messages', function (Blueprint $table) {
        $table->dropIndex(['flash_id']);
        $table->dropColumn(['flash_id', 'flash_thumb', 'flash_expires_at']);
    });
}
```

> `after(...)` MySQL içindir; Postgres'te yok sayılır, sorun değil.

## 3) Çalıştır

```bash
php artisan migrate
```

## Alternatif — düz SQL (Postgres)

```sql
ALTER TABLE messages ADD COLUMN IF NOT EXISTS flash_id BIGINT;
ALTER TABLE messages ADD COLUMN IF NOT EXISTS flash_thumb TEXT;
ALTER TABLE messages ADD COLUMN IF NOT EXISTS flash_expires_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_messages_flash_id ON messages (flash_id);
```

---

## Nasıl çalışıyor

- **Gönderim (`POST /messages`):** istek gövdesine `flash_id`, `flash_thumb`
  (küçük ön-izleme URL'i — flash'ın thumbnail'i, gönderim anında snapshot) ve
  `flash_expires_at` (ISO8601) eklenebilir. Değerler mesaj satırına yazılır ve
  cevaba `flash` objesi olarak konur.
- **`flash` objesi (additiv):** `{ id, available, thumbnail_url?, expires_at? }`.
  `available = flash_expires_at == null || now < flash_expires_at`. Süresi
  geçmişse `available:false` → client "PiPoP" kartı gösterir (tıklanamaz).
- **Serialization:** `GET /messages/:user_id` (fast + legacy sorgu, `m.*` ile
  kolonlar gelir), `SearchMessages`, `SyncMessages` cavablarına `flash` objesi
  eklendi. Real-time: WS `new_message` frame'inde de `flash` gider.
- **Geriye uyumluluk:** kolonlar nullable; flash olmayan mesajlarda `flash`
  objesi hiç konmaz. Köhnə client-lər tanımadığı `flash` sahəsini görmür —
  mövcud heç bir sahə dəyişməyib.
- **Tıklanamaz thumbnail:** client bu mesajı küçük, dokunulamaz bir kart olarak
  gösterir (flash'ı tekrar açmaz); yalnız gönderim anındaki snapshot'ı yansıtır.
