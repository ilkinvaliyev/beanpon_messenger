package websocket

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"beanpon_messenger/cache"
	"beanpon_messenger/models"
)

// ── Issue 4 — instance-lar arası canlı çatdırma və paylaşılan presence ──────
//
// PROBLEM (audit-in P0 maddəsi)
// `Hub.clients` yalnız BU prosesin yaddaşındakı map-dir və `SendToUser` sırf
// ona baxır. `cache/redis.go`-da `Publish|Subscribe` ÜMUMİYYƏTLƏ yox idi.
// Tək replica ilə hər şey işləyir — İKİNCİ replica qalxdığı an:
//
//   • A instansındakı göndərən → B instansındakı alıcı: canlı mesaj HEÇ VAXT
//     çatmır. Mesaj DB-dədir, amma alıcı onu ancaq çatı YENİDƏN AÇANDA görür.
//     İstifadəçi üçün bu "mesajlar gəlmir" deməkdir — audit-dəki #1 şikayət.
//   • `IsUserOnline` YALAN danışır: B-dəki onlayn istifadəçi A-da "offline"
//     görünür → ona push göndərilir (çatda oturduğu halda), və ya əksinə
//     onlayn sayılıb push GÖNDƏRİLMİR.
//   • `IsUserInChatWith` / `IsUserInGroupChat` eyni şəkildə yalan danışır →
//     avto-oxundu və push susdurma qərarları səhv verilir.
//   • yazır…, oxundu, reaksiya, qrup üzvlük event-ləri — hamısı itir.
//
// Yəni sistem GİZLİ ŞƏKİLDƏ tək replica-ya pinlənmişdi: ikinci replica
// əlavə etmək xəta vermir, sadəcə mesajlaşmanı yarıdan bölür.
//
// HƏLL — iki hissə
//
//  1. YAYIM (fan-out). Hər instans `bp:msg:ws:fanout` kanalına abunə olur.
//     `SendToUser` / `SendToMultipleUsers` mesajı ƏVVƏLCƏ öz lokal
//     client-lərinə verir, SONRA kanala yayımlayır. Digər instanslar
//     mesajı alır və YALNIZ özündə olan alıcılara ötürür. Öz yayımını
//     `origin` sahəsindən tanıyıb ATIR (ikiqat çatdırma yoxdur).
//
//     Niyə tək ortaq kanal (per-user kanal deyil)? Per-user abunə hər
//     bağlantı/kopma zamanı SUBSCRIBE/UNSUBSCRIBE tələb edir — daha az
//     şəbəkə trafiki, amma daha çox hərəkət hissəsi və abunə sızması riski.
//     Ortaq kanalın xərci: hər mesaj (replica sayı − 1) dəfə əlavə ötürülür.
//     2–4 replica üçün bu tamamilə əhəmiyyətsizdir. Replica sayı çox artsa
//     per-user kanala keçid asandır — interfeys dəyişmir.
//
//  2. PAYLAŞILAN PRESENCE. Hər bağlı istifadəçi üçün `ws:presence:{id}`
//     açarı yazılır: hansı instansda olduğu + hazırda açıq DM/qrup.
//     TTL 90 s, heartbeat 30 s-də bir yeniləyir. İnstans qəfil ölsə qeyd
//     öz-özünə yox olur (zombi "onlayn" qalmır).
//     `IsUserOnline` / `IsUserInChatWith` / `IsUserInGroupChat` əvvəlcə
//     LOKAL map-ə baxır (sürətli yol, dəyişməyib), tapmasa Redis-ə düşür.
//
// FAIL-OPEN
// Redis söndürülübsə (`REDIS_ENABLED=false`) və ya əlçatmazsa BÜTÜN bu qat
// no-op olur və davranış BAYT-BAYT köhnə (tək instans) davranışa qayıdır.
// `WS_CLUSTER_ENABLED=false` ilə də açıq şəkildə söndürülə bilər.

// instanceID — bu prosesin unikal kimliyi. Öz yayımını tanımaq üçün.
var instanceID = func() string {
	if v := strings.TrimSpace(os.Getenv("INSTANCE_ID")); v != "" {
		return v
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "ws"
	}
	return host + "-" + uuid.NewString()[:8]
}()

// clusterEnabled — `WS_CLUSTER_ENABLED` (default: Redis aktivdirsə açıq).
var clusterEnabled = func() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WS_CLUSTER_ENABLED"))) {
	case "false", "0", "no":
		return false
	}
	return true
}()

// clusterReady — abunə həqiqətən qurulub? Yalnız o zaman yayım etmək
// mənalıdır (əks halda hər mesajda mənasız Redis gedişi olardı).
var clusterReady atomic.Bool

// clusterActive — yayım/presence qatı işləkdirmi?
func clusterActive() bool {
	if !clusterEnabled {
		return false
	}
	c := cache.GetClient()
	return c != nil && c.Enabled()
}

// clusterFrame — fanout kanalında gedən zərf.
type clusterFrame struct {
	Origin  string          `json:"o"`
	UserIDs []uint          `json:"u"`
	Type    string          `json:"t"`
	Data    json.RawMessage `json:"d"`
	// Broadcast — Issue 4 (presence): frame KONKRET alıcılara deyil, uzaq
	// instansdakı BÜTÜN bağlı client-lərə aiddir (`user_status`). Bu halda
	// `UserIDs` alıcı siyahısı YOX, İSTİSNA siyahısıdır — statusu dəyişən
	// istifadəçi öz status frame-ini almamalıdır.
	//
	// `omitempty` ilə köhnə (false) frame-lərin teli dəyişmir; yeni sahəni
	// tanımayan köhnə instans onu sadəcə görməzdən gəlir və frame-i adi
	// hədəfli frame kimi emal edər — istifadəçi siyahısı qarışıq olsa da
	// nəticə yalnız "presence çatmır" olur, mesaj yolu təsirlənmir.
	Broadcast bool `json:"b,omitempty"`
}

// presenceRecord — `ws:presence:{id}` dəyəri.
type presenceRecord struct {
	Instance string `json:"i"`
	DM       uint   `json:"dm,omitempty"`
	Group    uint   `json:"grp,omitempty"`
	At       int64  `json:"at"`
}

// ── Yayım tərəfi ────────────────────────────────────────────────────────────

// ── Yayım növbəsi ───────────────────────────────────────────────────────────
//
// Yayım BLOKLAMAMALIDIR: Redis yavaşlasa mesaj göndərmə yolu dayanmamalıdır.
// Amma hər mesaj üçün AYRI `go func` yaratmaq da yanlışdır — mesaj seli
// altında minlərlə goroutine Redis-in `writeTimeout`-unu gözləyərək yığılır
// (özü-özünə gücləndirən yaddaş/planlayıcı təzyiqi). Ona görə: sərhədli
// növbə + sabit sayda yazıcı. Növbə dolarsa frame ATILIR və sayılır —
// canlı yayım "best-effort"-dur, mesajın özü onsuz da DB-dədir və push var.
const (
	clusterPublishQueueSize = 4096
	clusterPublishWorkers   = 4
)

var (
	clusterPublishCh   = make(chan string, clusterPublishQueueSize)
	clusterPublishOnce sync.Once
	clusterDropped     atomic.Int64
)

func startClusterPublishers() {
	clusterPublishOnce.Do(func() {
		for i := 0; i < clusterPublishWorkers; i++ {
			go func() {
				for payload := range clusterPublishCh {
					c := cache.GetClient()
					if c == nil {
						continue
					}
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					_ = c.Publish(ctx, c.LocalKey(cache.WSFanout()), payload)
					cancel()
				}
			}()
		}
		// Atılan frame-ləri görünən et.
		go func() {
			ticker := time.NewTicker(5 * time.Minute)
			defer ticker.Stop()
			var last int64
			for range ticker.C {
				if d := clusterDropped.Load(); d > last {
					log.Printf("ws-cluster: yayım növbəsi dolu — %d frame atıldı (cəmi %d)", d-last, d)
					last = d
				}
			}
		}()
	})
}

// publishCluster — mesajı digər instanslara ötürür. Lokal çatdırma ÇAĞIRAN
// tərəfdə artıq edilib.
func (h *Hub) publishCluster(userIDs []uint, messageType string, data interface{}) {
	if !clusterReady.Load() || !clusterActive() || len(userIDs) == 0 {
		return
	}

	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	frame := clusterFrame{
		Origin:  instanceID,
		UserIDs: userIDs,
		Type:    messageType,
		Data:    raw,
	}
	payload, err := json.Marshal(frame)
	if err != nil {
		return
	}

	select {
	case clusterPublishCh <- string(payload):
	default:
		clusterDropped.Add(1)
	}
}

// publishClusterBroadcast — Issue 4: HƏR instansın BÜTÜN client-lərinə gedən
// frame (presence / `user_status`). `except` — frame-in ÇATDIRILMAYACAĞI
// istifadəçilər (statusu dəyişənin özü).
//
// `publishCluster`-dən yeganə fərqi `Broadcast: true` bayrağıdır; növbə,
// atılma sayğacı və `origin` süzgəci eyni qalır.
func (h *Hub) publishClusterBroadcast(except []uint, messageType string, data interface{}) {
	if !clusterReady.Load() || !clusterActive() {
		return
	}

	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	frame := clusterFrame{
		Origin:    instanceID,
		UserIDs:   except,
		Type:      messageType,
		Data:      raw,
		Broadcast: true,
	}
	payload, err := json.Marshal(frame)
	if err != nil {
		return
	}

	select {
	case clusterPublishCh <- string(payload):
	default:
		clusterDropped.Add(1)
	}
}

// StartClusterSubscriber — fanout kanalını dinləyir. `go` ilə çağırılır.
func (h *Hub) StartClusterSubscriber(ctx context.Context) {
	if !clusterActive() {
		log.Printf("ws-cluster: söndürülüb (Redis yoxdur və ya WS_CLUSTER_ENABLED=false) — tək instans rejimi")
		return
	}
	c := cache.GetClient()
	startClusterPublishers()
	clusterReady.Store(true)
	defer clusterReady.Store(false)

	log.Printf("ws-cluster: instans kimliyi %s", instanceID)
	c.SubscribeLoop(ctx, c.LocalKey(cache.WSFanout()), func(payload string) {
		var frame clusterFrame
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			return
		}
		// Öz yayımımız — lokal çatdırma artıq olub.
		if frame.Origin == instanceID {
			return
		}
		// Issue 4: presence frame-i hədəfsizdir — bütün lokal client-lərə.
		if frame.Broadcast {
			h.broadcastLocalRaw(frame.UserIDs, frame.Type, frame.Data)
			return
		}
		h.deliverLocalRaw(frame.UserIDs, frame.Type, frame.Data)
	})
}

// broadcastLocalRaw — uzaq instansdan gələn YAYIM frame-ini bu instansdakı
// BÜTÜN client-lərə ötürür (`except` siyahısındakılar xaric). `deliverLocalRaw`
// kimi YENİDƏN yayım ETMİR — əks halda instanslar arasında sonsuz döngə olardı.
func (h *Hub) broadcastLocalRaw(except []uint, messageType string, data json.RawMessage) {
	// Wire formatı `messageToBytes` ilə eyni olmalıdır: {"type":..,"data":..}
	payload, err := json.Marshal(struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}{Type: messageType, Data: data})
	if err != nil {
		return
	}

	skip := make(map[uint]struct{}, len(except))
	for _, uid := range except {
		skip[uid] = struct{}{}
	}

	// Issue 23 ilə eyni naxış: alıcılar QISA `RLock` altında snapshot edilir,
	// kanal yazıları kiliddən sonra.
	h.mutex.RLock()
	targets := make([]*Client, 0, len(h.clients))
	for userID, client := range h.clients {
		if _, excluded := skip[userID]; excluded {
			continue
		}
		targets = append(targets, client)
	}
	h.mutex.RUnlock()

	for _, client := range targets {
		select {
		case client.Send <- payload:
		default:
			client.enqueueEvict(h)
		}
	}
}

// deliverLocalRaw — uzaq instansdan gələn frame-i YALNIZ lokal client-lərə
// ötürür. Yenidən yayım ETMİR (yoxsa sonsuz döngə olardı).
func (h *Hub) deliverLocalRaw(userIDs []uint, messageType string, data json.RawMessage) {
	if len(userIDs) == 0 {
		return
	}
	// Wire formatı `messageToBytes` ilə eyni olmalıdır: {"type":..,"data":..}
	payload, err := json.Marshal(struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}{Type: messageType, Data: data})
	if err != nil {
		return
	}

	seen := make(map[uint]struct{}, len(userIDs))
	targets := make([]*Client, 0, len(userIDs))
	h.mutex.RLock()
	for _, userID := range userIDs {
		if _, dup := seen[userID]; dup {
			continue
		}
		seen[userID] = struct{}{}
		if client, ok := h.clients[userID]; ok {
			targets = append(targets, client)
		}
	}
	h.mutex.RUnlock()

	delivered := false
	for _, client := range targets {
		select {
		case client.Send <- payload:
			delivered = true
		default:
			client.enqueueEvict(h)
		}
	}

	// İki tick (delivered=true) yan effekti lokal `deliver` yolunda var;
	// uzaqdan gələn `new_message` üçün onu BURADA təkrarlayırıq. Əks halda
	// göndərən A instansında, alıcı B-də olduqda mesaj çatsa da ikinci tick
	// yalnız istemçinin `mark_delivered` ack-i ilə (gec) gəlirdi.
	if delivered && messageType == "new_message" {
		h.markRemoteDelivered(data)
	}
}

// markRemoteDelivered — uzaq instansdan gələn `new_message` frame-inin
// JSON gövdəsindən id/sender/receiver çıxarıb `delivered=true` yazır.
//
// Ayrıca funksiyadır, çünki `maybeMarkLivePushDelivered` Go tərəfdə qurulmuş
// `map[string]interface{}`-i gözləyir (dəyərlər `uint`); JSON turundan sonra
// eyni sahələr `float64` olur və tip iddiaları səssizcə uğursuz olardı.
func (h *Hub) markRemoteDelivered(data json.RawMessage) {
	var payload struct {
		ID         string `json:"id"`
		SenderID   uint   `json:"sender_id"`
		ReceiverID uint   `json:"receiver_id"`
		IsHistory  bool   `json:"is_history"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return
	}
	if payload.ID == "" || payload.SenderID == 0 || payload.ReceiverID == 0 || payload.IsHistory {
		return
	}
	// Yalnız ALICININ nüsxəsi delivered sayılır — göndərənin echo-su yox.
	// Bu instansda alıcı client-i varmı?
	h.mutex.RLock()
	_, hasReceiver := h.clients[payload.ReceiverID]
	h.mutex.RUnlock()
	if !hasReceiver {
		return
	}

	go func() {
		res := h.db.Model(&models.Message{}).
			Where("id = ? AND delivered = false", payload.ID).
			Update("delivered", true)
		if res.Error != nil || res.RowsAffected == 0 {
			return
		}
		h.SendToUser(payload.SenderID, "message_delivered", map[string]interface{}{
			"other_user_id": payload.ReceiverID,
			"message_ids":   []string{payload.ID},
		})
	}()
}

// ── Paylaşılan presence ─────────────────────────────────────────────────────

// writePresence — istifadəçinin presence qeydini yazır/yeniləyir.
func (h *Hub) writePresence(userID uint, dm, group uint) {
	if !clusterActive() {
		return
	}
	c := cache.GetClient()
	rec := presenceRecord{Instance: instanceID, DM: dm, Group: group, At: time.Now().Unix()}
	raw, err := json.Marshal(rec)
	if err != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.Set(ctx, c.LocalKey(cache.WSPresence(userID)), string(raw), cache.TTLWSPresence)
	}()
}

// clearPresence — bağlantı bağlananda qeydi silir.
//
// DİQQƏT: yalnız qeyd BİZƏ aiddirsə silinir. Reconnect zamanı istifadəçi
// artıq BAŞQA instansa keçmiş ola bilər; köhnə instansın gec gələn
// `unregister`-i onu offline göstərməməlidir.
func (h *Hub) clearPresence(userID uint) {
	if !clusterActive() {
		return
	}
	c := cache.GetClient()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		key := c.LocalKey(cache.WSPresence(userID))
		raw, found, err := c.Get(ctx, key)
		if err != nil || !found {
			return
		}
		var rec presenceRecord
		if json.Unmarshal([]byte(raw), &rec) != nil {
			return
		}
		if rec.Instance != instanceID {
			return // istifadəçi başqa instansdadır — toxunma
		}
		_ = c.Del(ctx, key)
	}()
}

// StartPresenceHeartbeat — lokal client-lərin presence qeydlərini TTL bitmədən
// yeniləyir. `go` ilə çağırılır.
func (h *Hub) StartPresenceHeartbeat(ctx context.Context) {
	if !clusterActive() {
		return
	}
	ticker := time.NewTicker(cache.WSPresenceRefresh)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.refreshAllPresence()
		}
	}
}

func (h *Hub) refreshAllPresence() {
	c := cache.GetClient()
	if c == nil || !c.Enabled() {
		return
	}

	type snap struct {
		userID uint
		dm     uint
		group  uint
	}
	h.mutex.RLock()
	snaps := make([]snap, 0, len(h.clients))
	for userID, client := range h.clients {
		s := snap{userID: userID}
		if client.ActiveChatWith != nil {
			s.dm = *client.ActiveChatWith
		}
		if client.ActiveGroupChat != nil {
			s.group = *client.ActiveGroupChat
		}
		snaps = append(snaps, s)
	}
	h.mutex.RUnlock()

	if len(snaps) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	now := time.Now().Unix()
	for _, s := range snaps {
		raw, err := json.Marshal(presenceRecord{
			Instance: instanceID, DM: s.dm, Group: s.group, At: now,
		})
		if err != nil {
			continue
		}
		_ = c.Set(ctx, c.LocalKey(cache.WSPresence(s.userID)), string(raw), cache.TTLWSPresence)
	}
}

// remotePresence — TƏK istifadəçi üçün uzaq presence qeydi (lokal deyilsə).
func remotePresence(userID uint) (presenceRecord, bool) {
	if !clusterActive() {
		return presenceRecord{}, false
	}
	c := cache.GetClient()
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	raw, found, err := c.Get(ctx, c.LocalKey(cache.WSPresence(userID)))
	if err != nil || !found {
		return presenceRecord{}, false
	}
	var rec presenceRecord
	if json.Unmarshal([]byte(raw), &rec) != nil {
		return presenceRecord{}, false
	}
	return rec, true
}

// remotePresenceMany — ÇOX istifadəçi üçün tək MGET gedişi.
// 5000 üzvlü qrupda açar-başına GET fəlakət olardı.
func remotePresenceMany(userIDs []uint) map[uint]presenceRecord {
	out := make(map[uint]presenceRecord)
	if !clusterActive() || len(userIDs) == 0 {
		return out
	}
	c := cache.GetClient()

	keys := make([]string, 0, len(userIDs))
	byKey := make(map[string]uint, len(userIDs))
	for _, uid := range userIDs {
		k := c.LocalKey(cache.WSPresence(uid))
		keys = append(keys, k)
		byKey[k] = uid
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for k, v := range c.MGet(ctx, keys...) {
		var rec presenceRecord
		if json.Unmarshal([]byte(v), &rec) != nil {
			continue
		}
		out[byKey[k]] = rec
	}
	return out
}
