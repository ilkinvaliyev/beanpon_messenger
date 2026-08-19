-- ═══════════════════════════════════════════════════════════════════════════
-- DEPLOY 1 — INDEX MİGRASYONU
--
-- Mevcut index'ler incelendi. İYİ HABER: ihtiyaç duyduğumuz index'lerin
-- ÇOĞU ZATEN VAR. Sadece BİR yeni index gerekiyor.
--
-- `CONCURRENTLY` tabloyu KİLİTLEMEZ (canlıda güvenli) ama:
--   • transaction içinde çalıştırılamaz — tek tek, psql'den çalıştırın
--   • uzun sürebilir (10M satırda ~10-30 dk)
--   • başarısız olursa INVALID index bırakır → DROP edip tekrar deneyin
-- ═══════════════════════════════════════════════════════════════════════════

-- ── 1. OKUNMAMIŞ SAYACI (tek gerçek eksik) ─────────────────────────────────
-- Mevcut `idx_messages_unread_count (receiver_id) WHERE read=false AND
-- is_deleted_by_receiver=false` yetersiz: `conversation_id IS NULL` ve
-- `deleted_at IS NULL` şartları index'te olmadığı için sorgu Bitmap Heap
-- Scan'e düşüyor (2835 blok okuyor).
--
-- ÖLÇÜLDÜ (PostgreSQL 16, 430k satır, 21k okunmamış):
--   mevcut index + join'siz : 15.2 ms
--   mevcut index + join'lü   : 21.3 ms   ← Deploy 1'deki halim
--   YENİ index  + join'lü    :  6.3 ms   ← Index Only Scan, Heap Fetches=0
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_unread_dm
  ON messages (receiver_id, sender_id)
  WHERE read = false
    AND is_deleted_by_receiver = false
    AND conversation_id IS NULL
    AND deleted_at IS NULL;


-- ── 2. DELTA-SYNC (yeniden baglanma backfill'i) ────────────────────────────
-- `SyncMessages` sorgusu `(sender_id = ? OR receiver_id = ?)` + `ORDER BY
-- (updated_at, id)` yapıyor. Bu ikisi zıt yönlere çektiği için mevcut
-- index'lerin hiçbiri kullanılamıyor → Parallel Seq Scan + top-N sort.
--
-- ÖLÇÜLDÜ (PostgreSQL 16, 430k satır, ~130k mesajlı kullanıcı):
--   index yok  + OR sorgusu        : 43-50 ms   ← BUGÜN
--   index YOK  + UNION ALL sorgusu : (index olmadan da doğru, ama yavaş)
--   index VAR  + UNION ALL sorgusu :  0.26-0.38 ms
--
-- Bu endpoint HER yeniden bağlanmada çağrılıyor — şebeke dalgalanmasında
-- tüm filo aynı anda tam tablo taraması yaptırıyor.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_sync_snd
  ON messages (sender_id, updated_at, id) WHERE conversation_id IS NULL;

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_sync_rcv
  ON messages (receiver_id, updated_at, id) WHERE conversation_id IS NULL;

-- Index-only scan'in gerçekten heap'e inmemesi için visibility map güncel olmalı:
VACUUM ANALYZE messages;

-- ═══════════════════════════════════════════════════════════════════════════
-- İSTEĞE BAĞLI TEMİZLİK — önce doğrulayın
-- ═══════════════════════════════════════════════════════════════════════════

-- ── A. ŞÜPHELİ ÖLÜ INDEX ───────────────────────────────────────────────────
-- `idx_messages_unread_laravel` şu predikatı taşıyor:
--     WHERE read = false AND deleted_at IS NULL AND is_deleted_by_receiver IS NULL
-- Dikkat: `is_deleted_by_receiver IS NULL` (= false DEĞİL). Sütun NOT NULL ise
-- bu index SIFIR satır eşler → hiç kullanılmaz ama HER INSERT/UPDATE'te
-- bakım maliyeti öder.
--
-- ÖNCE ÇALIŞTIRIN:
--   SELECT count(*) FROM messages WHERE is_deleted_by_receiver IS NULL;
--   SELECT idx_scan FROM pg_stat_user_indexes WHERE indexrelname='idx_messages_unread_laravel';
-- Sonuç 0 ve 0 ise:
-- DROP INDEX CONCURRENTLY idx_messages_unread_laravel;

-- ── B. GEREKSİZ TEKRAR ─────────────────────────────────────────────────────
-- `messages_conversation_id_index (conversation_id)` ile
-- `idx_messages_conv_created (conversation_id, created_at DESC) WHERE deleted_at IS NULL`
-- büyük ölçüde örtüşüyor. İkincisi kısmi olduğu için birincisi tamamen
-- gereksiz değil — kullanım sayısına bakın:
--   SELECT indexrelname, idx_scan FROM pg_stat_user_indexes
--    WHERE relname='messages' ORDER BY idx_scan;
-- `messages_conversation_id_index` uzun süredir 0 ise düşürülebilir.

-- ═══════════════════════════════════════════════════════════════════════════
-- GEREKMEYEN INDEX'LER (kontrol edildi — eklemeyin)
-- ═══════════════════════════════════════════════════════════════════════════
-- • (receiver_id, sender_id, created_at DESC) — GEREKMİYOR.
--   Mevcut `idx_messages_pair_created (sender_id, receiver_id, created_at DESC)`
--   her iki yönü de karşılıyor: ilk iki sütunda eşitlik olduğu için
--   (A→B) ve (B→A) dallarının İKİSİ de aynı index'ten okunuyor.
--   Sorunun kaynağı index değil, sorgunun `OR` şekli (bkz. Go tarafı düzeltmesi).
--
-- • (receiver_id, sender_id, created_at DESC) — GEREKMİYOR (yukarıda açıklandı).
--
-- NOT — `sync_seq` / `messages_sync_seq_dm_idx` HAKKINDA:
--   Sütun `DEFAULT ((pg_current_xact_id())::text)::bigint` ile otomatik
--   doluyor (1.45M satırın 0 tanesi NULL) ve iOS istemcisi `since_seq`
--   protokolünü zaten konuşuyor. AMA `DEFAULT` yalnızca INSERT'te çalışır:
--   mesaj okundu/teslim edildi/düzenlendiğinde `updated_at` değişir,
--   `sync_seq` DEĞİŞMEZ. Delta-sync bu durum değişikliklerini de taşımak
--   zorunda olduğu için seq tabanlı imleç, bir UPDATE trigger'ı olmadan
--   okundu/teslim bilgisini KAYBEDERDİ.
--   Yukarıdaki iki index + UNION ALL sorgusu aynı hızı (0.3 ms) trigger'sız,
--   şema değişikliği olmadan ve mevcut "5 sn güvenlik penceresi" mantığını
--   bozmadan veriyor. `sync_seq` bu yüzden KULLANILMIYOR.
--   (İleride istenirse: BEFORE UPDATE trigger + since_seq — ama gerek yok.)
