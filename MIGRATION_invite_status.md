# Migration: conversations.group_permissions

Qrup admin icazələri (mesaj/media/gif/səs/circle-video). NULL = hamısı açıq.

```sql
ALTER TABLE conversations
  ADD COLUMN IF NOT EXISTS group_permissions jsonb;
```

Geri almaq:

```sql
ALTER TABLE conversations DROP COLUMN IF EXISTS group_permissions;
```

---

# Migration: conversation_participants.invite_status

Qrup dəvəti onay axını üçün. Pending üzv qrupu siyahıda görür amma mesajları
görmür; "Qatıl" → active, "Rədd et" → sətir silinir.

Prod-da əl ilə işə salın (təsdiqlə):

```sql
ALTER TABLE conversation_participants
  ADD COLUMN IF NOT EXISTS invite_status varchar(10) DEFAULT 'active';

-- Köhnə qeydlər tam üzvdür:
UPDATE conversation_participants SET invite_status = 'active' WHERE invite_status IS NULL;

CREATE INDEX IF NOT EXISTS idx_cp_invite_status
  ON conversation_participants (conversation_id, invite_status);
```

Geri almaq:

```sql
DROP INDEX IF EXISTS idx_cp_invite_status;
ALTER TABLE conversation_participants DROP COLUMN IF EXISTS invite_status;
```
