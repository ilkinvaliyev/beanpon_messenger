package websocket

import (
	"context"
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

// ── DEPLOY 3 / DM-S2: düzgün kapanış testləri ──────────────────────────────

// Bağlı client yoxdursa `Shutdown` DB-yə heç toxunmamalı və dərhal qayıtmalıdır.
// (`db` nil-dir: DB-yə gedən hər hansı yol panic verər — test onu tutar.)
func TestShutdown_NoClients(t *testing.T) {
	h := &Hub{clients: make(map[uint]*Client)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Shutdown(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown bağlantısız halda asıldı")
	}
}

// Hər client-in `done` kanalı bağlanmalıdır — `writePump` close frame-i
// məhz bu siqnalla göndərir. `ctx` müddəti dolsa belə bu addım tamamlanır,
// çünki frame göndərmə DB yazımından ƏVVƏL edilir.
func TestShutdown_ClosesEveryClient(t *testing.T) {
	h := &Hub{clients: make(map[uint]*Client)}
	for i := uint(1); i <= 5; i++ {
		h.clients[i] = &Client{UserID: i, Send: make(chan []byte, 1), done: make(chan struct{})}
	}
	// Süresi ÇOXDAN dolmuş ctx: DB addımı atlansa da close frame-lər getməlidir.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fin := make(chan struct{})
	go func() { defer close(fin); h.Shutdown(ctx) }()
	select {
	case <-fin:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown asıldı")
	}

	for id, c := range h.clients {
		select {
		case <-c.done:
		default:
			t.Fatalf("client %d üçün done bağlanmadı (close frame getməzdi)", id)
		}
	}
}

// `closeSend` idempotentdir: `Shutdown` ilə `unregisterClient` eyni anda
// çağırsa belə ikinci `close(done)` panic verməməlidir.
func TestCloseSend_Idempotent(t *testing.T) {
	c := &Client{UserID: 1, Send: make(chan []byte, 1), done: make(chan struct{})}
	for i := 0; i < 3; i++ {
		c.closeSend()
	}
	select {
	case <-c.done:
	default:
		t.Fatal("done bağlanmadı")
	}
}

// ── C3 / DM-C1: tək instansda yayım etməmə ─────────────────────────────────

func withClusterState(started time.Time, lastPeer time.Time, solo bool, fn func()) {
	oldStarted, oldPeer, oldSolo := clusterStartedAt, clusterLastPeerAt.Load(), clusterSoloSkip
	defer func() {
		clusterStartedAt = oldStarted
		clusterLastPeerAt.Store(oldPeer)
		clusterSoloSkip = oldSolo
	}()
	clusterStartedAt = started
	if lastPeer.IsZero() {
		clusterLastPeerAt.Store(0)
	} else {
		clusterLastPeerAt.Store(lastPeer.UnixNano())
	}
	clusterSoloSkip = solo
	fn()
}

// Açılış pəncərəsində (ilk 60 sn) HƏMİŞƏ yayım edilməlidir — rolling deploy
// zamanı yeni instans köhnəni hələ görməmiş ola bilər.
func TestClusterHasPeers_BootWindowAlwaysPublishes(t *testing.T) {
	withClusterState(time.Now(), time.Time{}, true, func() {
		if !clusterHasPeers() {
			t.Fatal("açılış pəncərəsində yayım dayandırıldı — frame itkisi riski")
		}
	})
}

// Açılış pəncərəsi bitib, heç bir peer görülməyibsə yayım dayanır.
func TestClusterHasPeers_SoloStopsPublishing(t *testing.T) {
	withClusterState(time.Now().Add(-10*time.Minute), time.Time{}, true, func() {
		if clusterHasPeers() {
			t.Fatal("tək instansda yayım hələ də edilir")
		}
	})
}

// Peer TƏZƏ görülübsə yayım açıq olmalıdır.
func TestClusterHasPeers_FreshPeerPublishes(t *testing.T) {
	withClusterState(time.Now().Add(-10*time.Minute), time.Now().Add(-5*time.Second), true, func() {
		if !clusterHasPeers() {
			t.Fatal("təzə peer görüldüyü halda yayım dayandırıldı — MESAJ İTKİSİ")
		}
	})
}

// Peer damgası köhnəlibsə (TTL keçib) yayım yenidən dayanır.
func TestClusterHasPeers_StalePeerStops(t *testing.T) {
	withClusterState(time.Now().Add(-10*time.Minute), time.Now().Add(-2*clusterPeerTTL), true, func() {
		if clusterHasPeers() {
			t.Fatal("köhnəlmiş peer damgası ilə yayım davam edir")
		}
	})
}

// Kill-switch: `WS_CLUSTER_SOLO_SKIP=false` → HƏMİŞƏ köhnə davranış.
func TestClusterHasPeers_KillSwitchRestoresOldBehaviour(t *testing.T) {
	withClusterState(time.Now().Add(-10*time.Minute), time.Time{}, false, func() {
		if !clusterHasPeers() {
			t.Fatal("kill-switch açıq olduğu halda yayım dayandırıldı")
		}
	})
}

// `notePeerFrame` damgayı yeniləyir → yayım açılır.
func TestNotePeerFrameEnablesPublishing(t *testing.T) {
	withClusterState(time.Now().Add(-10*time.Minute), time.Time{}, true, func() {
		if clusterHasPeers() {
			t.Fatal("başlanğıc vəziyyət səhv")
		}
		notePeerFrame()
		if !clusterHasPeers() {
			t.Fatal("peer frame-indən sonra yayım açılmadı")
		}
	})
}

// ── C3 / DM-C2: `conversation_update` v2-yə göndərilmir ────────────────────

func TestNeedsConversationUpdate(t *testing.T) {
	h := &Hub{clients: map[uint]*Client{
		1: {UserID: 1, ProtoVersion: protoV2},     // yeni iOS
		2: {UserID: 2, ProtoVersion: protoLegacy}, // Flutter / köhnə iOS
	}}

	old := convUpdateMode
	defer func() { convUpdateMode = old }()

	convUpdateMode = "v2skip"
	if h.needsConversationUpdate(1) {
		t.Fatal("v2 istemçiyə hələ də conversation_update gedir")
	}
	if !h.needsConversationUpdate(2) {
		t.Fatal("KÖHNƏ İSTEMÇİ SINDI: legacy client-ə conversation_update getmir")
	}
	if !h.needsConversationUpdate(999) {
		t.Fatal("bilinməyən/uzaq istifadəçiyə göndərilmir — fail-open pozulub")
	}

	// Kill-switch: hamıya göndər.
	convUpdateMode = "all"
	if !h.needsConversationUpdate(1) {
		t.Fatal("WS_CONV_UPDATE=all olduğu halda v2-yə göndərilmir")
	}
}

// ── W3 / DM-B1: yavaş istemcide bağlantı kopartılmaması ────────────────────

// Geçici frame listesi KİLİTLİ. Buraya kritik bir tip sızarsa mesaj sessizce
// atılır — gerçek veri kaybı olur.
func TestIsDroppableFrame(t *testing.T) {
	for _, ok := range []string{"user_typing", "group_typing", "user_status",
		"unread_count_update", "online_users"} {
		if !isDroppableFrame(ok) {
			t.Fatalf("%q geçici sayılmalıydı", ok)
		}
	}
	// Bunlar ASLA atılamaz: atılırsa kullanıcı mesajı/onayı kaybeder.
	for _, critical := range []string{
		"new_message", "message_ack", "message_error", "message_duplicate",
		"message_delivered", "message_read", "message_edited", "message_deleted",
		"new_group_message", "conversation_update", "reaction_updated",
	} {
		if isDroppableFrame(critical) {
			t.Fatalf("KRİTİK FRAME ATILABİLİR İŞARETLENMİŞ: %q — veri kaybı", critical)
		}
	}
}

// Kuyruk doluyken: geçici frame atılır, bağlantı YAŞAR.
func TestTrySend_TransientDoesNotEvict(t *testing.T) {
	h := &Hub{clients: make(map[uint]*Client), unregister: make(chan *Client, 4)}
	c := &Client{UserID: 1, Send: make(chan []byte, 1), done: make(chan struct{})}
	c.Send <- []byte("dolu") // kuyruk dolu

	if c.trySend(h, "user_typing", []byte("x")) {
		t.Fatal("dolu kuyruğa yazıldı?")
	}
	if c.evicting.Load() {
		t.Fatal("geçici frame yüzünden bağlantı kopartıldı (W3 düzeltmesi çalışmıyor)")
	}
	select {
	case <-h.unregister:
		t.Fatal("unregister'a düştü — bağlantı kopartılıyor")
	default:
	}
}

// Kuyruk doluyken: kritik frame'de bağlantı kopartılır (eski davranış).
// Sessizce atmak gerçek kayıp olurdu; kopan istemci delta-sync ile toparlar.
func TestTrySend_CriticalEvicts(t *testing.T) {
	h := &Hub{clients: make(map[uint]*Client), unregister: make(chan *Client, 4)}
	c := &Client{UserID: 1, Send: make(chan []byte, 1), done: make(chan struct{})}
	c.Send <- []byte("dolu")

	if c.trySend(h, "new_message", []byte("x")) {
		t.Fatal("dolu kuyruğa yazıldı?")
	}
	if !c.evicting.Load() {
		t.Fatal("kritik frame kaybedildiği halde bağlantı kopartılmadı")
	}
	select {
	case got := <-h.unregister:
		if got != c {
			t.Fatal("yanlış client unregister edildi")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unregister'a hiç düşmedi")
	}
}

// Kuyrukta yer varsa her iki tip de normal şekilde yazılır.
func TestTrySend_HappyPath(t *testing.T) {
	h := &Hub{clients: make(map[uint]*Client), unregister: make(chan *Client, 4)}
	c := &Client{UserID: 1, Send: make(chan []byte, 4), done: make(chan struct{})}
	for _, typ := range []string{"new_message", "user_typing"} {
		if !c.trySend(h, typ, []byte("x")) {
			t.Fatalf("%q yazılamadı", typ)
		}
	}
	if c.evicting.Load() {
		t.Fatal("boş kuyrukta bağlantı kopartıldı")
	}
	if len(c.Send) != 2 {
		t.Fatalf("kuyrukta %d frame var, 2 bekleniyordu", len(c.Send))
	}
}

// Kill-switch: `WS_DROP_TRANSIENT=false` → eski davranış (geçici frame de kopartır).
func TestTrySend_KillSwitchRestoresOldBehaviour(t *testing.T) {
	old := dropTransientFrames
	dropTransientFrames = false
	defer func() { dropTransientFrames = old }()

	h := &Hub{clients: make(map[uint]*Client), unregister: make(chan *Client, 4)}
	c := &Client{UserID: 1, Send: make(chan []byte, 1), done: make(chan struct{})}
	c.Send <- []byte("dolu")

	c.trySend(h, "user_typing", []byte("x"))
	if !c.evicting.Load() {
		t.Fatal("kill-switch açıkken eski davranış (kopartma) uygulanmadı")
	}
	<-h.unregister
}
