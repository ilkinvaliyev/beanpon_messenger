# Live chat "soft clear" — Laravel migration

Go tarafı `live_rooms.chat_cleared_at` kolonunu **okuyup yazıyor** ama tabloyu
Laravel yönettiği için kolonu Laravel migration ile eklemen gerekiyor.

## 1) Migration oluştur

```bash
php artisan make:migration add_chat_cleared_at_to_live_rooms_table --table=live_rooms
```

## 2) Migration içeriği

```php
public function up(): void
{
    Schema::table('live_rooms', function (Blueprint $table) {
        $table->timestamp('chat_cleared_at')->nullable()->after('active_game');
    });
}

public function down(): void
{
    Schema::table('live_rooms', function (Blueprint $table) {
        $table->dropColumn('chat_cleared_at');
    });
}
```

> `after('active_game')` MySQL içindir; Postgres'te `after` yok sayılır, sorun değil.

## 3) Çalıştır

```bash
php artisan migrate
```

## Alternatif — düz SQL

**Postgres:**
```sql
ALTER TABLE live_rooms ADD COLUMN chat_cleared_at TIMESTAMP NULL;
```

**MySQL:**
```sql
ALTER TABLE live_rooms ADD COLUMN chat_cleared_at TIMESTAMP NULL AFTER active_game;
```

---

## Nasıl çalışıyor

- Host (WS `clear_chat`) veya admin (Filament `ClearChat`) → artık **DELETE yok**,
  sadece `UPDATE live_rooms SET chat_cleared_at = NOW()`.
- `chat_cleared` event'i eskisi gibi yayılıyor → herkesin UI'ı anında boşalıyor.
- Mesaj geçmişi endpoint'i yalnız `chat_cleared_at`'tan **sonraki** mesajları
  döndürüyor → pull-to-refresh'te de boş geliyor.
- **Mesajlar DB'de aynen kalıyor** (`live_room_messages` hiç silinmiyor).
