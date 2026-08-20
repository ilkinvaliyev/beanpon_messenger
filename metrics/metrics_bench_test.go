package metrics

import (
	"testing"
	"time"
)

// Bir mesaj gönderiminin TÜM ölçüm maliyeti (WS yolu):
// 1 toplam + 3 adım + 1 sorgu + sayaçlar.
func BenchmarkPerMessageMetrics(b *testing.B) {
	start := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ObserveSince(DMSendDuration, start, "ws", "ok")
		ObserveSince(DMSendStep, start, "ws", "perm")
		ObserveSince(DMSendStep, start, "ws", "persist")
		ObserveSince(DMSendStep, start, "ws", "fanout")
		ObserveSince(QueryDuration, start, "unread")
		ClusterSkippedSoloTotal.Inc()
	}
}

func BenchmarkSingleObserve(b *testing.B) {
	start := time.Now()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ObserveSince(QueryDuration, start, "history")
	}
}

// Paralel: 8 goroutine aynı anda — kilit çekişmesi var mı?
func BenchmarkPerMessageMetricsParallel(b *testing.B) {
	start := time.Now()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			ObserveSince(DMSendDuration, start, "ws", "ok")
			ObserveSince(DMSendStep, start, "ws", "perm")
			ObserveSince(DMSendStep, start, "ws", "persist")
			ObserveSince(DMSendStep, start, "ws", "fanout")
		}
	})
}
