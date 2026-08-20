# DEPLOY 5 — ÖLÇÜM

**Migration YOK. Env değişikliği YOK. İstemci değişikliği YOK.**
Davranış değişmiyor — sadece sayaç ekleniyor.

---

## Neden

Dört tur optimizasyon yaptık ve üretimde **tek bir milisaniye ölçmedik**.
Bütün rakamlar (154 ms → 18 ms gibi) benim test ortamımdan, sahte veriyle.

Sunucuda `/metrics` ucu zaten vardı ama sadece HTTP isteklerini sayıyordu.
Mesajlaşmanın kalbi olan **WebSocket yolu tamamen görünmezdi**.

Artık görünür. Yeni port, yeni servis, yeni ayar yok — aynı `/metrics`
adresinden okunuyor.

---

## Eklenen ölçümler

| metrik | ne söyler |
|---|---|
| `dm_send_duration_seconds{transport,result}` | **Ana gösterge.** Mesaj göndermenin sunucudaki toplam süresi. `transport`: ws \| rest. `result`: ok \| duplicate \| rejected \| error |
| `dm_send_step_seconds{transport,step}` | Süre hangi adımda geçti. `step`: perm (izin kontrolleri) \| persist (INSERT + conversation) \| fanout (yayın + push kapısı) |
| `query_duration_seconds{query}` | Ağır sorgular: conversations \| history \| sync \| unread |
| `push_duration_seconds{result}` | Bildirim gönderimi (sent \| failed) |
| `ws_clients` | Bu instansa bağlı WebSocket sayısı |
| `ws_send_queue_max` | En dolu istemcinin gönderim kuyruğu (kapasite 256) |
| `ws_evicted_total` | **Kuyruk dolduğu için KOPARTILAN bağlantı sayısı** |
| `cluster_published_total` | Diğer instanslara yayınlanan frame |
| `cluster_skipped_solo_total` | Tek instans olduğu için **yapılmayan** yayın (C3 kazancı) |
| `cluster_dropped_total` | Yayın kuyruğu dolduğu için **atılan** frame |

Hepsinin başında `beanpon_messenger_` var.

---

## Maliyet — ÖLÇÜLDÜ, tahmin değil

`metrics/metrics_bench_test.go` ile ölçüldü (Intel Xeon 2.1 GHz, 2 çekirdek):

```
BenchmarkPerMessageMetrics           447 ns/op    0 B/op   0 allocs/op
BenchmarkSingleObserve               105 ns/op
BenchmarkPerMessageMetricsParallel   319 ns/gözlem  (8 goroutine çekişmeli)
```

**Mesaj başına toplam ölçüm maliyeti: ~450 nanosaniye ve SIFIR bellek ayırma.**

Bunu ölçtüğü şeyle karşılaştır:

| iş | süre |
|---|---|
| tek veritabanı sorgusu | ~1.000.000 ns (1 ms) |
| mesaj göndermenin tamamı | ~4.000.000 ns (4 ms) |
| **bütün ölçümler** | **~450 ns** |

Yani ölçüm, ölçtüğü işin **on binde biri**. Saniyede 1.000 mesajda bir
çekirdeğin **%0,045'i**; saniyede 10.000 mesajda %0,45'i.

**Bellek / kardinalite:**

```
26 seri, 10 metrik ailesi, /metrics çıktısı ~28 KB
```

Prometheus 100.000+ seride zorlanmaya başlar. 26 seri hiçbir şey. Etiketlerde
sınırsız değer (kullanıcı id'si vb.) olmadığı için bu sayı **zamanla
büyümez** — testle kilitlendi.

**Örnekleyici goroutine:** 15 saniyede bir `RLock` alıp bağlı istemcileri
sayıyor. 10.000 bağlantıda bile 15 saniyede bir ~10.000 döngü adımı —
ölçülemeyecek kadar küçük.

**Kısacası: hayır, sunucuyu yormaz.** Ölçmediğin şeyi yönetemezsin; bu ölçümün
maliyeti pratikte sıfır.

**Kardinalite:** etiketlerde kullanıcı id'si, mesaj id'si gibi sınırsız değer
YOK — hepsi sabit, küçük kümeler. (Prometheus'u şişiren şey budur; testle
kilitlendi.)

---

## NASIL BAKACAKSIN

Üç yol var. Birincisi hiçbir kurulum istemiyor.

### Yol 1 — hazır script (kurulum yok, hemen çalışır)

`scripts/chat-metrics.py` — sunucuda çalıştır, tek komut:

```bash
./scripts/chat-metrics.py
./scripts/chat-metrics.py http://10.10.0.5:5082/metrics
```

Çıktı şöyle:

```
=== MESAJ GÖNDERME (sunucu tarafındaki toplam süre) ===
                                   adet   ortalama       ~p50       ~p95
  WebSocket (yeni iOS)             5000     7.0 ms     8.0 ms    16.0 ms
  REST (Flutter / eski iOS)         800    13.9 ms    16.0 ms    32.0 ms

=== SÜRE NEREDE GEÇİYOR (WebSocket) ===
  izin kontrolleri                 5000     1.8 ms     2.0 ms     4.0 ms
  veritabanına yazma               5000     3.5 ms     4.0 ms     8.0 ms
  yayın + push kapısı              5000     797 µs     1.0 ms     2.0 ms

  → En pahalı adım: VERITABANINA YAZMA  (bir sonraki işimiz burası)
...
  kopartılan bağlantı (toplam)                    2   <-- yavaş istemci sorunu (W3)
  yapılmayan yayın (tek instans)             24,500   <-- C3 kazancı
```

Sorunlu yerleri kendisi işaretliyor.

**Önemli:** düz çalıştırdığında rakamlar **sunucu açılışından beri toplamdır**.
Yani dünkü bir yavaşlık bugünkü ortalamayı kirletir. "Şu anda ne oluyor" için:

```bash
./scripts/chat-metrics.py --watch 60
```

İki ölçüm alıp **farkı** gösterir — son 60 saniyenin gerçek davranışı.

Gereksinim: `python3`. Harici paket yok.

### Yol 2 — ham hâli

```bash
curl -s http://10.10.0.5:5082/metrics | grep beanpon_messenger_dm_send
```

Histogramlarda `_sum` ve `_count` var; ortalama = `_sum / _count`.

### Yol 3 — Prometheus + Grafana (grafik ve geçmiş)

Repoda zaten bir `prometheus.yml` var (`app:5082` hedefliyor) ama
`docker-compose.yml`'de Prometheus servisi **yok**. Yani ya başka yerde
çalışıyor ya da hiç ayağa kalkmamış. Kontrol et:

```bash
docker ps | grep -i -E "prometheus|grafana"
```

Yoksa `docker-compose.yml`'e eklemek yeterli:

```yaml
  prometheus:
    image: prom/prometheus:latest
    restart: always
    volumes:
      - ./prometheus.yml:/etc/prometheus/prometheus.yml:ro
      - prometheus_data:/prometheus
    networks: [monitoring]

  grafana:
    image: grafana/grafana:latest
    restart: always
    ports:
      - "127.0.0.1:3000:3000"     # yalnız sunucudan; dışarı AÇMA
    volumes:
      - grafana_data:/var/lib/grafana
    networks: [monitoring]

volumes:
  prometheus_data:
  grafana_data:
```

Sonra `ssh -L 3000:127.0.0.1:3000 sunucu` ile tünelleyip tarayıcıdan
`localhost:3000` (ilk giriş admin/admin). Veri kaynağı olarak
`http://prometheus:9090`.

Grafana'da kullanacağın sorgular aşağıda.

---

## Deploy'dan sonra ilk bakılacaklar (Grafana / PromQL)

Birkaç saat veri biriktikten sonra bunları çalıştır.

**1. Mesaj göndermek gerçekte kaç ms sürüyor?**

```promql
histogram_quantile(0.50, sum(rate(beanpon_messenger_dm_send_duration_seconds_bucket[5m])) by (le, transport))
histogram_quantile(0.95, sum(rate(beanpon_messenger_dm_send_duration_seconds_bucket[5m])) by (le, transport))
histogram_quantile(0.99, sum(rate(beanpon_messenger_dm_send_duration_seconds_bucket[5m])) by (le, transport))
```

Ortanca (p50) ile p99 arasındaki fark önemli: p50 iyi ama p99 kötüyse
"çoğu zaman hızlı, bazen çok yavaş" demektir — kullanıcıyı rahatsız eden odur.

**2. Süre nerede geçiyor?**

```promql
histogram_quantile(0.95, sum(rate(beanpon_messenger_dm_send_step_seconds_bucket[5m])) by (le, step))
```

Üç çubuk göreceksin: `perm`, `persist`, `fanout`. En yükseği bir sonraki
işimiz olacak.

**3. Bağlantı kopartılıyor mu? (denetim maddesi W3)**

```promql
increase(beanpon_messenger_ws_evicted_total[1h])
max_over_time(beanpon_messenger_ws_send_queue_max[1h])
```

Birincisi sıfırdan büyükse kullanıcı "bağlantı kopuyor" yaşıyor demektir.
İkincisi 256'ya yaklaşıyorsa sorun yakında çıkacak demektir.

**4. C3 gerçekten çalıştı mı?**

```promql
rate(beanpon_messenger_cluster_skipped_solo_total[5m])
rate(beanpon_messenger_cluster_published_total[5m])
```

Tek instansta birincisi artmalı, ikincisi ~0 olmalı (10 saniyede bir
heartbeat hariç).

**5. Canlı yayın kaybediliyor mu?**

```promql
beanpon_messenger_cluster_dropped_total
```

Sıfırdan büyükse kuyruk doluyor ve frame'ler atılıyor.

**6. Sohbet listesi sorgusu (Deploy 3'ün etkisi)**

```promql
histogram_quantile(0.95, sum(rate(beanpon_messenger_query_duration_seconds_bucket{query="conversations"}[5m])) by (le))
```

**7. Push ne kadar sürüyor / başarısız mı?**

```promql
histogram_quantile(0.95, sum(rate(beanpon_messenger_push_duration_seconds_bucket[5m])) by (le, result))
sum(rate(beanpon_messenger_push_duration_seconds_count{result="failed"}[5m]))
```

---

> `/metrics` şu an kimlik doğrulaması olmadan açık ama konteyner portu iç IP'ye
> (`10.10.0.5`) bağlı, yani dışarıdan erişilmiyor. Dışarı açarsan
> `ginprom.Token(...)` ile kilitle.

---

## Testler

`metrics/metrics_test.go` — 5 test, veritabanı gerekmiyor:

- metrik **adları** kilitli (ad değişirse panel/alarm sessizce boşalır —
  derleyici bunu yakalamaz)
- etiket kardinalitesi sınırlı (etikete değişken sızarsa test patlar)
- `ObserveSince` doğru yazıyor
- çift kayıt **panik etmiyor** (ölçüm asla uygulamayı düşürmemeli)
- kova aralığı 1 ms – 2 s'yi kapsıyor

---

## Deploy

```bash
docker compose up -d --build
```

## Geri alma

`git revert`. Şema, env, istemci — hiçbirine dokunulmadı.

---

## Sonraki adım

Birkaç gün veri biriksin, sonra 2 numaralı sorguya bakalım:
**`perm`, `persist`, `fanout` — hangisi en yüksek?** Bir sonraki maddeyi artık
tahminle değil, o grafiğe bakarak seçeceğiz.
