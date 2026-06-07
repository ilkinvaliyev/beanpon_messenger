# Migration: live_room_messages → sound_url + sound_id (Piounds)

Live room chat'e ses (Piound) gönderme özelliği için iki kolon eklenir.
`gif_url` / `image_url` ile aynı kalıp.

```sql
ALTER TABLE live_room_messages
    ADD COLUMN IF NOT EXISTS sound_url VARCHAR(500),
    ADD COLUMN IF NOT EXISTS sound_id BIGINT;

CREATE INDEX IF NOT EXISTS idx_live_room_messages_sound_id
    ON live_room_messages (sound_id);
```

Geri alma:

```sql
DROP INDEX IF EXISTS idx_live_room_messages_sound_id;
ALTER TABLE live_room_messages
    DROP COLUMN IF EXISTS sound_url,
    DROP COLUMN IF EXISTS sound_id;
```
