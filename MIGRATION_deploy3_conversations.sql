-- ═══════════════════════════════════════════════════════════════════════════
-- DEPLOY 3 — SOHBET LİSTESİ INDEX MİGRASYONU
--
-- `GetConversations` sorgusu yeniden yazıldı (bak handlers/message_handler.go).
-- Yeni sorgu BU İKİ INDEX'e dayanıyor. Index'ler olmadan sorgu DOĞRU çalışır
-- ama HIZLANMAZ (ölçüldü: 154 ms → yalnızca 105 ms).
--
-- ÖNCE index'leri oluşturun, SONRA yeni sunucuyu deploy edin.
-- Ters sırada da bir şey kırılmaz, sadece o aralıkta kazanç olmaz.
--
-- `CONCURRENTLY` tabloyu KİLİTLEMEZ (canlıda güvenli) ama:
--   • transaction içinde çalıştırılamaz — psql'den TEK TEK çalıştırın
--   • uzun sürebilir (10M satırda ~10-30 dk)
--   • başarısız olursa INVALID index bırakır → DROP edip tekrar deneyin
-- ═══════════════════════════════════════════════════════════════════════════


-- ── 1. GİDEN YÖN: "bu kişiye attığım son mesaj" ────────────────────────────
--
-- Mevcut `idx_messages_pair_created (sender_id, receiver_id, created_at DESC)`
-- neredeyse doğru ama `conversation_id IS NULL` şartını taşımıyor. O şart
-- index'te olmadığı için PostgreSQL her satırda heap'e inip kontrol etmek
-- zorunda kalıyor ve planlayıcı sıralı index taramasını seçmiyor.
--
-- Bu index iki işi birden yapıyor:
--   • karşı taraf listesini "gevşek index taraması" ile çıkarmak
--     (out_peers — her adımda bir sonraki FARKLI receiver_id'ye atlama)
--   • her karşı taraf için son giden mesajı LIMIT 1 ile almak
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_dm_out_last
  ON messages (sender_id, receiver_id, created_at DESC)
  WHERE conversation_id IS NULL;


-- ── 2. GELEN YÖN: "bu kişiden gelen son mesaj" ─────────────────────────────
--
-- Aynısının ters yönü. Bugün böyle bir index YOK — gelen yön için
-- `idx_messages_receiver_dm (receiver_id, created_at DESC)` var ama içinde
-- sender_id olmadığı için "her gönderen için son mesaj" sorusuna cevap veremiyor.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_dm_in_last
  ON messages (receiver_id, sender_id, created_at DESC)
  WHERE conversation_id IS NULL;


-- Index-only scan'in gerçekten heap'e inmemesi için visibility map güncel olmalı.
-- (Uzun sürer, tabloyu kilitlemez.)
VACUUM ANALYZE messages;


-- ═══════════════════════════════════════════════════════════════════════════
-- ÖLÇÜM (PostgreSQL 16.13, 430.280 mesaj, 202.001 kullanıcı, 11.909 sohbet)
-- ═══════════════════════════════════════════════════════════════════════════
--
--  kullanıcı profili              ÖNCE      SONRA     blok (önce→sonra)
--  ─────────────────────────────  ────────  ────────  ──────────────────
--  130k mesaj / 200 sohbet        154 ms     18 ms    24.030 → 8.690
--                                                     + 34 MB temp → 0
--  50k mesaj / 1 sohbet            53 ms    2,3 ms     4.390 →    65
--                                                     + 13 MB temp → 0
--  491 mesaj / 16 sohbet          2,2 ms    3,0 ms       701 →   499
--
-- Küçük kullanıcıda 0,8 ms'lik fark yerel CPU gürültüsüdür; okunan blok
-- sayısı orada da DÜŞÜYOR — uzak veritabanında (I/O baskın) yeni sorgu
-- her profilde daha ucuzdur.
--
-- DOĞRULAMA: 120 senaryoda eski ve yeni sorgu satır satır BİREBİR aynı
-- sonucu verdi — 4 status filtresi × 3 archived filtresi × 10 kullanıcı,
-- artı şu kenar durumları:
--   • tüm mesajları silinmiş sohbet (listede ÇIKMAMALI)              ✓
--   • yalnız giden / yalnız gelen mesajı olan sohbet                 ✓
--   • kendine mesaj (self-chat), yarısı silinmiş                     ✓
--   • giden hepsi silinmiş ama gelen duruyor                         ✓
--   • iki yönde AYNI mikrosaniyeli mesaj (eşitlik)                   ✓
--
-- ═══════════════════════════════════════════════════════════════════════════
-- GERİ ALMA
-- ═══════════════════════════════════════════════════════════════════════════
-- Sorgu geri alınırsa index'ler zararsızdır (sadece disk yer kaplar).
-- Yine de silmek isterseniz:
--   DROP INDEX CONCURRENTLY IF EXISTS idx_messages_dm_out_last;
--   DROP INDEX CONCURRENTLY IF EXISTS idx_messages_dm_in_last;
