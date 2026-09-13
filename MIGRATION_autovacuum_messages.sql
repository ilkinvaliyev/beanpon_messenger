-- ═══════════════════════════════════════════════════════════════════════════
-- AUTOVACUUM AYARI — `messages` ve `conversations`
--
-- NEDEN
--
-- Üretim ölçümü (18 dakikalık pencere):
--     sohbet listesi   ortalama 15,7 ms   p95 64 ms    ← 4 kat sapma
--
-- `pg_stat_user_tables` şunu gösterdi:
--     last_vacuum      2026-08-19 18:29   (elle, Deploy 1 migration'ı)
--     last_autovacuum  2026-08-19 12:15   (bir gün önce)
--
-- İki sonuç:
--
--  1. Deploy 3'te eklenen `idx_messages_dm_out_last` / `idx_messages_dm_in_last`
--     index'lerinin dayandığı GÖRÜNÜRLÜK HARİTASI (visibility map) bayat.
--     Index-only scan yalnız "tamamen görünür" işaretli sayfalarda heap'e
--     inmez; bayat haritada her satır için heap okuması yapılır.
--
--  2. Sohbet listesi sorgusu tam olarak EN YENİ mesajlara bakar (her karşı
--     taraf için son mesaj) — yani en çok değişen, en bayat sayfalara.
--     p95 sapmasının en olası kaynağı budur.
--
-- PostgreSQL'in varsayılanı `autovacuum_vacuum_scale_factor = 0.2`: tablonun
-- %20'si ölü satır olana kadar bekler. Milyonlarca satırlık bir `messages`
-- tablosunda bu, autovacuum'un pratikte HİÇ koşmaması demektir.
--
-- `messages` üstelik sadece INSERT almıyor: `read`, `delivered`,
-- `is_deleted_by_*`, `starred_by_*` sütunları sürekli UPDATE ediliyor. Her
-- UPDATE yeni bir satır sürümü yaratır (PostgreSQL MVCC) — yani ölü satır
-- üretimi yüksek, temizlik ise seyrek.
-- ═══════════════════════════════════════════════════════════════════════════


-- ── 1. TEK SEFERLİK: haritayı ŞİMDİ güncelle ───────────────────────────────
--
-- `VACUUM` (FULL DEĞİL) tabloyu KİLİTLEMEZ — okuma ve yazma sürer. Yalnız
-- disk I/O üretir, o yüzden yoğun olmayan bir saatte çalıştırın.
-- Büyük tabloda dakikalar sürebilir; ekranda takılı görünmesi normaldir.
VACUUM (ANALYZE, VERBOSE) messages;


-- ── 2. KALICI: autovacuum'u bu tabloya göre sıkılaştır ─────────────────────
--
-- scale_factor 0.2 → 0.02 : %20 yerine %2 ölü satırda tetiklenir.
-- analyze 0.1 → 0.01      : planlayıcı istatistikleri taze kalır (sorgu planı
--                           bayat istatistikle yanlış index seçebilir).
-- cost_limit 200 → 1000   : autovacuum kendini yavaşlatmasın; aksi halde
--                           tetiklense bile saatlerce sürüp geride kalır.
--
-- Bu ayarlar YALNIZ bu tabloya uygulanır, sunucu geneline dokunmaz.
ALTER TABLE messages SET (
  autovacuum_vacuum_scale_factor  = 0.02,
  autovacuum_analyze_scale_factor = 0.01,
  autovacuum_vacuum_cost_limit    = 1000
);

-- `conversations` her mesajda UPDATE alıyor (sayaçlar, last_message_at) —
-- satır sayısı küçük ama ölü satır üretimi mesaj hızıyla aynı. Sohbet listesi
-- sorgusu bu tabloya her satır için LATERAL ile gidiyor.
ALTER TABLE conversations SET (
  autovacuum_vacuum_scale_factor  = 0.05,
  autovacuum_analyze_scale_factor = 0.02,
  autovacuum_vacuum_cost_limit    = 1000
);


-- ═══════════════════════════════════════════════════════════════════════════
-- DOĞRULAMA
-- ═══════════════════════════════════════════════════════════════════════════

-- Ayarlar tuttu mu?
--   SELECT relname, reloptions FROM pg_class
--   WHERE relname IN ('messages','conversations');

-- Autovacuum artık düzenli koşuyor mu? (birkaç saat sonra bakın —
-- `last_autovacuum` GÜNLERCE eskimemeli)
--   SELECT relname, n_live_tup, n_dead_tup,
--          last_vacuum, last_autovacuum, last_analyze
--   FROM pg_stat_user_tables
--   WHERE relname IN ('messages','conversations');

-- Index-only scan gerçekten heap'e inmiyor mu? `Heap Fetches: 0` görmek
-- istiyoruz (sohbet listesi sorgusunu EXPLAIN ANALYZE ile çalıştırın).

-- ETKİSİNİ ÖLÇÜN — vacuum'dan ÖNCE ve SONRA:
--   ./scripts/chat-metrics.py http://10.10.0.5:5082/metrics --watch 300
-- `sohbet listesi` satırındaki ~p95 düşmeli.


-- ═══════════════════════════════════════════════════════════════════════════
-- GERİ ALMA
-- ═══════════════════════════════════════════════════════════════════════════
--   ALTER TABLE messages RESET (
--     autovacuum_vacuum_scale_factor, autovacuum_analyze_scale_factor,
--     autovacuum_vacuum_cost_limit);
--   ALTER TABLE conversations RESET (
--     autovacuum_vacuum_scale_factor, autovacuum_analyze_scale_factor,
--     autovacuum_vacuum_cost_limit);
