package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrik ADLARI kilitlenir. Bir ad değişirse Grafana panelleri ve alarmlar
// SESSİZCE boşalır — derleyici bunu yakalamaz, bu test yakalar.
func TestMetricNamesAreStable(t *testing.T) {
	// Gözlem yapılmadan histogram/counter serileri toplanmaz; hepsini bir kez
	// dokunarak görünür yap.
	DMSendDuration.WithLabelValues("ws", "ok").Observe(0.01)
	DMSendStep.WithLabelValues("ws", "perm").Observe(0.01)
	QueryDuration.WithLabelValues("conversations").Observe(0.01)
	PushDuration.WithLabelValues("sent").Observe(0.01)
	WSClients.Set(1)
	WSSendQueueMax.Set(0)
	WSEvictedTotal.Add(0)
	ClusterPublishedTotal.Add(0)
	ClusterSkippedSoloTotal.Add(0)
	ClusterDroppedTotal.Set(0)

	want := []string{
		"beanpon_messenger_dm_send_duration_seconds",
		"beanpon_messenger_dm_send_step_seconds",
		"beanpon_messenger_query_duration_seconds",
		"beanpon_messenger_push_duration_seconds",
		"beanpon_messenger_ws_clients",
		"beanpon_messenger_ws_send_queue_max",
		"beanpon_messenger_ws_evicted_total",
		"beanpon_messenger_cluster_published_total",
		"beanpon_messenger_cluster_skipped_solo_total",
		"beanpon_messenger_cluster_dropped_total",
	}

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	have := make(map[string]bool, len(families))
	for _, f := range families {
		have[f.GetName()] = true
	}
	for _, name := range want {
		if !have[name] {
			t.Fatalf("metrik kayıp: %s (ad değişti mi? panel/alarm sessizce boşalır)", name)
		}
	}
}

// Etiketler SABİT ve KÜÇÜK kümeler olmalı. Kullanıcı id'si gibi sınırsız bir
// değer etikete girerse Prometheus şişer — bu test kullanımı belgeler.
func TestLabelCardinalityIsBounded(t *testing.T) {
	transports := []string{"ws", "rest"}
	results := []string{"ok", "duplicate", "rejected", "error"}
	steps := []string{"perm", "persist", "fanout"}

	for _, tr := range transports {
		for _, r := range results {
			DMSendDuration.WithLabelValues(tr, r).Observe(0.001)
		}
		for _, st := range steps {
			DMSendStep.WithLabelValues(tr, st).Observe(0.001)
		}
	}
	// 2×4 + 2×3 = 14 seri. Bu sayı büyürse birileri etikete değişken koymuştur.
	families, _ := prometheus.DefaultGatherer.Gather()
	for _, f := range families {
		switch f.GetName() {
		case "beanpon_messenger_dm_send_duration_seconds":
			if n := len(f.GetMetric()); n > 8 {
				t.Fatalf("dm_send_duration seri sayısı %d — etikete değişken sızmış", n)
			}
		case "beanpon_messenger_dm_send_step_seconds":
			if n := len(f.GetMetric()); n > 6 {
				t.Fatalf("dm_send_step seri sayısı %d — etikete değişken sızmış", n)
			}
		}
	}
}

// `ObserveSince` panik etmemeli ve makul bir değer yazmalı.
func TestObserveSince(t *testing.T) {
	start := time.Now().Add(-50 * time.Millisecond)
	ObserveSince(QueryDuration, start, "history")

	families, _ := prometheus.DefaultGatherer.Gather()
	for _, f := range families {
		if f.GetName() != "beanpon_messenger_query_duration_seconds" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetValue() == "history" && m.GetHistogram().GetSampleCount() == 0 {
					t.Fatal("gözlem yazılmadı")
				}
			}
		}
	}
}

// Çift kayıt PANİK ETMEMELİ — ölçüm hiçbir zaman uygulamayı düşürmemeli.
func TestDoubleRegisterDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("çift kayıt panik etti: %v", r)
		}
	}()
	dup := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystem,
		Name: "ws_clients", Help: "duplicate",
	})
	register(dup) // aynı ad — panik etmemeli
}

// Kova aralığı: 1 ms'lik bir gözlem en küçük kovaya, 1 s'lik gözlem üst
// kovalara düşmeli. Aralık kayarsa grafikler okunmaz hale gelir.
func TestBucketRangeCoversMessagingLatencies(t *testing.T) {
	if msBuckets[0] > 0.001 {
		t.Fatalf("en küçük kova %v — 1 ms'lik gönderimler ayırt edilemez", msBuckets[0])
	}
	last := msBuckets[len(msBuckets)-1]
	if last < 1.0 {
		t.Fatalf("en büyük kova %v — 1 s'den yavaş gönderimler görünmez", last)
	}
	if len(msBuckets) > 20 {
		t.Fatalf("kova sayısı %d — kardinalite gereksiz büyük", len(msBuckets))
	}
}
