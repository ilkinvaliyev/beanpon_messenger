package websocket

import (
	"sync"
	"time"

	"beanpon_messenger/models"
)

// ── Issue 16 — "yazır…" siqnallarında blok yoxlaması + sürət limiti ──────────
//
// PROBLEM
// `typing` / `typing_stop` / `recording` / `recording_stop` frame-ləri HEÇ BİR
// yoxlamadan birbaşa `SendToUser(msg.ReceiverID, ...)`-a ötürülürdü. İki ayrı
// zəiflik:
//
//  1. BLOK KEÇİLİR. `receiver_id` tamamilə istemçinin nəzarətindədir. Sizi
//     bloklamış (və ya sizin bloklamış olduğunuz) istifadəçi hər saniyə
//     "X yazır…" göndərə bilirdi. Mesaj göndərə bilməyən adam beləliklə
//     hədəfin ekranında görünməyə davam edirdi — blokun mənası itir.
//     Bütün digər yollarda (`SendMessage`, WS `send_message`) blok yoxlanılır;
//     yalnız burada yoxlanmırdı.
//
//  2. SÜRƏT LİMİTİ YOXDUR. Bir soket saniyədə minlərlə frame göndərib
//     İSTƏNİLƏN user id-yə yönləndirə bilirdi. Hər frame hədəfin `Send`
//     buferinə yazılır → bufer dolur → hədəf "yavaş istehlakçı" sayılıb
//     BAĞLANTIDAN ATILIR (bax `enqueueEvict`). Yəni bir istifadəçi digərini
//     WebSocket-dən qopara bilirdi.
//
// HƏLL — ASİMMETRİK QAPI
// Start və stop siqnalları FƏRQLİ qaydalarla süzülür, çünki onların istemçi
// semantikası fərqlidir:
//
//   • START (`typing` / `recording`) — istemçi bunu KEEPALIVE kimi təkrar
//     göndərir (qarşı tərəfin göstəricisi bir neçə saniyədən sonra özü sönür).
//     Ona görə təkrarlar UDULMUR, sadəcə minimal intervalla (800 ms)
//     seyrəldilir: istifadəçi hər tuş vuruşunda frame göndərsə də şəbəkəyə
//     saniyədə ~1 frame çıxır.
//
//   • STOP (`typing_stop` / `recording_stop`) — yalnız KƏNAR-TETİKLƏMƏ ilə
//     buraxılır: qarşı tərəfə əvvəlcədən "yazır" göndərilməyibsə stop
//     siqnalının ötürüləcək məlumatı YOXDUR və atılır. Yalnız start-a limit
//     qoymaq KİFAYƏT DEYİLDİ — hücumçu `typing_stop` seli qurub qurbanın
//     `Send` buferini doldurub onu BAĞLANTIDAN ATDIRA bilirdi.
//
// Stop siqnalı vəziyyət `true` olduqda HƏMİŞƏ ötürülür — hətta blok ARADA
// qoyulmuş olsa belə. Əks halda blok anında qurbanın ekranında əbədi
// "X yazır…" ilişib qalardı.
//
// Blok qərarı 30 saniyə keşlənir (hər frame-də DB sorğusu YOX). Keş qəsdən
// qısadır: yeni qoyulan blok ən çox 30 saniyə gec təsir edir.

const (
	typingBlockCacheTTL = 30 * time.Second
	typingMinInterval   = 800 * time.Millisecond
	// typingPeerIdleTTL — bu müddət ərzində toxunulmayan qeyd zibil sayılır.
	typingPeerIdleTTL = 10 * time.Minute
	// typingGateMaxPeers — bir client-in keşdə saxlaya biləcəyi maksimum
	// qarşı tərəf sayı.
	//
	// DİQQƏT: dolduqda keşi TAM TƏMİZLƏMƏK OLMAZ — hücumçu hər frame-də YENİ
	// `receiver_id` göndərib limiti sıfırlaya və beləliklə sürət limitini
	// tamamilə keçə bilər. Bunun əvəzinə əvvəlcə KÖHNƏLMİŞ qeydlər atılır;
	// yer yenə açılmasa YENİ qarşı tərəflərə siqnal BURAXILMIR (mövcud
	// söhbətlər təsirlənmir).
	typingGateMaxPeers = 256
)

type typingPeerState struct {
	blocked        bool
	blockCheckedAt time.Time
	lastStartSent  time.Time
	// sentTyping — bu qarşı tərəfə SON ÖTÜRÜLƏN vəziyyət.
	sentTyping bool
	// touchedAt — zibil toplama üçün son toxunma vaxtı.
	touchedAt time.Time
}

// typingGate — client başına "yazır…" siqnal qapısı.
type typingGate struct {
	mu    sync.Mutex
	peers map[uint]*typingPeerState
}

func newTypingGate() *typingGate {
	return &typingGate{peers: make(map[uint]*typingPeerState)}
}

// pruneLocked — köhnəlmiş qeydləri atır. Çağıran `mu`-nu tutmalıdır.
func (g *typingGate) pruneLocked(now time.Time) {
	for k, st := range g.peers {
		if now.Sub(st.touchedAt) > typingPeerIdleTTL {
			delete(g.peers, k)
		}
	}
}

// allow — bu `receiverID`-yə "yazır/səs yazır" siqnalı ötürülsünmü?
//
// `isStart` — true: "yazmağa başladı", false: "dayandırdı".
func (g *typingGate) allow(h *Hub, senderID, receiverID uint, isStart bool) bool {
	if receiverID == 0 || receiverID == senderID {
		return false
	}

	now := time.Now()

	g.mu.Lock()
	st, ok := g.peers[receiverID]
	if !ok {
		// Vəziyyəti OLMAYAN qarşı tərəfə DAYANDIRMA siqnalı mənasızdır
		// (heç vaxt "yazır" göndərməmişik) — qeyd də açmırıq. Bu, hücumçunun
		// `typing_stop` seli ilə keşi doldurmasının qarşısını alır.
		if !isStart {
			g.mu.Unlock()
			return false
		}
		if len(g.peers) >= typingGateMaxPeers {
			g.pruneLocked(now)
			if len(g.peers) >= typingGateMaxPeers {
				g.mu.Unlock()
				return false // yer yoxdur — siqnalı at (mövcud söhbətlər qorunur)
			}
		}
		st = &typingPeerState{}
		g.peers[receiverID] = st
	}
	st.touchedAt = now

	// STOP — kənar-tetikləmə: "yazır" göndərilməyibsə ötürüləcək bir şey yoxdur.
	// Bu, təkrarlanan `typing_stop` selini TAM udur (DoS vektoru bağlanır).
	// Vəziyyət `true` idisə isə blokdan ASILI OLMAYARAQ ötürülür ki, qarşı
	// tərəfdə göstərici ilişib qalmasın.
	if !isStart {
		if !st.sentTyping {
			g.mu.Unlock()
			return false
		}
		st.sentTyping = false
		g.mu.Unlock()
		return true
	}

	// START — təkrarlar keepalive-dır, udulmur; yalnız seyrəldilir.
	if !st.lastStartSent.IsZero() && now.Sub(st.lastStartSent) < typingMinInterval {
		g.mu.Unlock()
		return false
	}

	needBlockCheck := st.blockCheckedAt.IsZero() || now.Sub(st.blockCheckedAt) > typingBlockCacheTTL
	blocked := st.blocked
	g.mu.Unlock()

	if needBlockCheck {
		// DB sorğusu kilid TUTMADAN — kilid altında I/O etmirik.
		blocked = models.IsBlocked(h.db, senderID, receiverID)
		g.mu.Lock()
		if cur, ok := g.peers[receiverID]; ok {
			cur.blocked = blocked
			cur.blockCheckedAt = now
		}
		g.mu.Unlock()
	}

	if blocked {
		return false
	}

	g.mu.Lock()
	if cur, ok := g.peers[receiverID]; ok {
		cur.lastStartSent = now
		cur.sentTyping = true
	}
	g.mu.Unlock()
	return true
}

// allowGroupTyping — qrup "yazır…" siqnalları üçün qapı.
// Üzvlük yoxlaması `handleGroupTyping` içindədir; burada vəziyyət izlənməsi
// və tezlik məhdudlaşdırması var (bir qrup frame-i N üzvə yayıldığı üçün
// gücləndirmə əmsalı DM-dən qat-qat böyükdür).
func (g *typingGate) allowGroupTyping(conversationID uint, isStart bool) bool {
	if conversationID == 0 {
		return false
	}
	// Qrup açarları DM user id-ləri ilə toqquşmasın deyə ayrı ad fəzası.
	// (Toqquşma üçün user id ≈ 1.8e19 olmalıdır — mümkün deyil.)
	key := ^conversationID

	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()

	st, ok := g.peers[key]
	if !ok {
		if !isStart {
			return false
		}
		if len(g.peers) >= typingGateMaxPeers {
			g.pruneLocked(now)
			if len(g.peers) >= typingGateMaxPeers {
				return false
			}
		}
		st = &typingPeerState{}
		g.peers[key] = st
	}
	st.touchedAt = now

	// STOP — kənar-tetikləmə (DM ilə eyni məntiq).
	if !isStart {
		if !st.sentTyping {
			return false
		}
		st.sentTyping = false
		return true
	}
	// START — keepalive; seyrəldilir, udulmur.
	if !st.lastStartSent.IsZero() && now.Sub(st.lastStartSent) < typingMinInterval {
		return false
	}
	st.lastStartSent = now
	st.sentTyping = true
	return true
}
