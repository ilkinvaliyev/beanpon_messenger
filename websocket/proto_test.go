package websocket

import (
	"testing"
	"time"
)

// Geriyə uyğunluq testi: `?cv=` parametrini GÖNDƏRMƏYƏN hər istemçi
// (Flutter, köhnə iOS, üçüncü tərəf) MÜTLƏQ protoLegacy almalıdır.
// Bu funksiyada bir səhv = bütün köhnə istemçilər "v2" sayılır =
// tarixçə frame-ləri kəsilir + gözlənilməyən `message_ack`.
func TestParseProtoVersion_LegacyDefaults(t *testing.T) {
	legacy := []string{"", " ", "abc", "0", "-1", "1", "v2", "2.0", "null", "undefined"}
	for _, in := range legacy {
		if got := parseProtoVersion(in); got != protoLegacy {
			t.Fatalf("parseProtoVersion(%q) = %d, istənilən %d (KÖHNƏ İSTEMÇİ SINDI)", in, got, protoLegacy)
		}
	}
}

func TestParseProtoVersion_V2(t *testing.T) {
	if got := parseProtoVersion("2"); got != protoV2 {
		t.Fatalf("parseProtoVersion(\"2\") = %d, istənilən %d", got, protoV2)
	}
	if got := parseProtoVersion(" 2 "); got != protoV2 {
		t.Fatalf("boşluqlu \"2\" = %d", got)
	}
	// Gələcək istemçi köhnə serverdə: protoMax-a sıxılmalıdır.
	if got := parseProtoVersion("99"); got != protoMax {
		t.Fatalf("parseProtoVersion(\"99\") = %d, istənilən %d", got, protoMax)
	}
}

// Default fan-out rejimi DƏYİŞMƏMƏLİDİR (env qoyulmayıbsa "all").
func TestStatusFanoutDefaultsToAll(t *testing.T) {
	if statusFanoutMode != statusFanoutAll {
		t.Fatalf("statusFanoutMode = %q, istənilən %q (WS_STATUS_FANOUT qoyulmayıb)", statusFanoutMode, statusFanoutAll)
	}
}

// v1 client-ə ack GÖNDƏRİLMƏMƏLİDİR.
func TestSendAckIfV2_LegacyGetsNothing(t *testing.T) {
	c := &Client{Send: make(chan []byte, 4), ProtoVersion: protoLegacy}
	c.sendAckIfV2("id", "cid", 7, timeZero(), false)
	if len(c.Send) != 0 {
		t.Fatalf("v1 client-ə %d frame yazıldı, istənilən 0", len(c.Send))
	}
}

func TestSendAckIfV2_V2GetsAck(t *testing.T) {
	c := &Client{Send: make(chan []byte, 4), ProtoVersion: protoV2}
	c.sendAckIfV2("mid", "cid", 7, timeZero(), false)
	if len(c.Send) != 1 {
		t.Fatalf("v2 client-ə %d frame yazıldı, istənilən 1", len(c.Send))
	}
	got := string(<-c.Send)
	for _, want := range []string{`"type":"message_ack"`, `"cid":"cid"`, `"id":"mid"`, `"duplicate":false`} {
		if !contains(got, want) {
			t.Fatalf("ack frame-də %s yoxdur: %s", want, got)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func timeZero() time.Time { return time.Unix(0, 0).UTC() }
