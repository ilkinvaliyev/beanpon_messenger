// Package metrics — mesajlaşma sıcak yolunun ÜRETİMDEKİ ölçümü.
//
// ── NEDEN ───────────────────────────────────────────────────────────────────
//
// `/metrics` ucu zaten açıktı (ginprom) ama YALNIZ HTTP isteklerini sayıyordu:
// yol, durum kodu, süre. Mesajlaşmanın kalbi olan WebSocket yolu tamamen
// görünmezdi. Yani "mesaj göndermek gerçekte kaç ms sürüyor", "süre hangi
// adımda geçiyor", "yavaş istemci yüzünden kaç bağlantı kopartıldı" gibi
// soruların üretimde bir cevabı yoktu — yapılan optimizasyonların etkisi de
// ölçülemiyordu.
//
// Bu paket o boşluğu dolduruyor. Metrikler `prometheus.DefaultRegisterer`
// üzerine yazılıyor, yani ginprom'un AYNI `/metrics` ucundan okunuyor — yeni
// port, yeni endpoint, yeni ayar YOK.
//
// ── MALİYET ─────────────────────────────────────────────────────────────────
//
// Bir histogram gözlemi ~30–60 ns (kilitsiz atomik). Mesaj başına ~8 gözlem =
// mikro-saniyenin altı. Ölçüm, ölçtüğü şeyi bozmaz.
//
// ── KARDİNALİTE ─────────────────────────────────────────────────────────────
//
// Etiketlerde kullanıcı id'si, mesaj id'si gibi SINIRSIZ değer YOK — hepsi
// sabit, küçük kümeler. (Prometheus'u öldüren şey budur.)
package metrics

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	namespace = "beanpon"
	subsystem = "messenger"
)

// msBuckets — 1 ms'den 2 s'ye kadar. Mesajlaşma yolunda ilgilendiğimiz aralık
// bu; daha ince kova eklemek kardinaliteyi büyütür, daha kaba olan da
// "1 ms mi 50 ms mi" sorusunu cevaplayamaz.
var msBuckets = prometheus.ExponentialBuckets(0.001, 2, 12) // 1ms … ~2s

var (
	// DMSendDuration — "gönder"e basılmasından yayının bitmesine kadar geçen
	// SUNUCU süresi. `transport`: ws | rest. `result`: ok | duplicate |
	// rejected | error.
	//
	// Bu, tüm çalışmanın ANA göstergesi.
	DMSendDuration = newHistogramVec("dm_send_duration_seconds",
		"DM gönderiminin sunucu tarafındaki toplam süresi",
		[]string{"transport", "result"})

	// DMSendStep — sürenin hangi adımda geçtiği. `step`:
	//   perm    — izin kontrolleri (blok, spam, conversation)
	//   persist — transaction (INSERT + conversation güncellemesi)
	//   fanout  — WebSocket yayını + push kapısı
	DMSendStep = newHistogramVec("dm_send_step_seconds",
		"DM gönderiminin adım süreleri",
		[]string{"transport", "step"})

	// QueryDuration — adı verilmiş ağır sorgular. `query`:
	//   conversations — sohbet listesi
	//   history       — sohbet geçmişi sayfası
	//   sync          — yeniden bağlanma delta-sync'i
	//   unread        — okunmamış sayacı
	QueryDuration = newHistogramVec("query_duration_seconds",
		"Adı verilmiş veritabanı sorgularının süresi",
		[]string{"query"})

	// PushDuration — bildirim gönderimi. `result`: sent | failed.
	PushDuration = newHistogramVec("push_duration_seconds",
		"Push bildirimi gönderim süresi",
		[]string{"result"})

	// WSClients — bu instansa bağlı WebSocket sayısı.
	WSClients = newGauge("ws_clients",
		"Bu instansa bağlı WebSocket istemci sayısı")

	// WSSendQueueMax — en dolu istemcinin gönderim kuyruğu (kapasite 256).
	// Bu sayı kapasiteye yaklaşıyorsa yavaş istemci sorunu var demektir.
	WSSendQueueMax = newGauge("ws_send_queue_max",
		"En dolu istemcinin gönderim kuyruğu derinliği")

	// WSEvictedTotal — gönderim kuyruğu dolduğu için KOPARTILAN bağlantı
	// sayısı. Sıfırdan büyükse kullanıcı "bağlantı kopuyor" yaşıyor demektir
	// (denetim maddesi W3).
	WSEvictedTotal = newCounter("ws_evicted_total",
		"Gönderim kuyruğu dolduğu için kopartılan bağlantı sayısı")

	// ClusterPublishedTotal — diğer instanslara yayınlanan frame sayısı.
	ClusterPublishedTotal = newCounter("cluster_published_total",
		"Diğer instanslara yayınlanan frame sayısı")

	// ClusterSkippedSoloTotal — "başka instans yok" diye YAPILMAYAN yayın
	// sayısı (C3 kazancı doğrudan burada görünür).
	ClusterSkippedSoloTotal = newCounter("cluster_skipped_solo_total",
		"Tek instans olduğu için yapılmayan yayın sayısı")

	// ClusterDroppedTotal — yayın kuyruğu dolduğu için ATILAN frame sayısı.
	// Sıfırdan büyükse canlı yayın gerçekten kaybediliyor demektir.
	ClusterDroppedTotal = newGauge("cluster_dropped_total",
		"Yayın kuyruğu dolduğu için atılan frame sayısı")
)

// ── Yardımcılar ────────────────────────────────────────────────────────────

// ObserveSince — `defer metrics.ObserveSince(vec, start, "ws", "perm")`.
func ObserveSince(vec *prometheus.HistogramVec, start time.Time, labels ...string) {
	vec.WithLabelValues(labels...).Observe(time.Since(start).Seconds())
}

func newHistogramVec(name, help string, labels []string) *prometheus.HistogramVec {
	v := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: subsystem,
		Name: name, Help: help, Buckets: msBuckets,
	}, labels)
	register(v)
	return v
}

func newCounter(name, help string) prometheus.Counter {
	c := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystem, Name: name, Help: help,
	})
	register(c)
	return c
}

func newGauge(name, help string) prometheus.Gauge {
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystem, Name: name, Help: help,
	})
	register(g)
	return g
}

// register — çift kayıt PANİK ETMEZ. `MustRegister` testlerde ya da paket iki
// kez yüklenirse prosesi çökertirdi; ölçüm hiçbir zaman uygulamayı düşürmemeli.
func register(c prometheus.Collector) {
	if err := prometheus.Register(c); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return
		}
		// Sessizce geç: metrik kaydı başarısız olsa da mesajlaşma çalışmalı.
		return
	}
}
