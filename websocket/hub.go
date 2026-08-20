package websocket

import (
	"beanpon_messenger/config"
	"beanpon_messenger/models"
	"beanpon_messenger/services"
	"beanpon_messenger/utils"
	"beanpon_messenger/xmpp"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
	// Issue 17: token artıq `Sec-WebSocket-Protocol: bearer, <JWT>` ilə də
	// gələ bilir (brauzer WS API-si ixtiyari başlıq göndərə bilmir, yeganə
	// kanal budur). Protokol danışığında server SEÇİLƏN alt-protokolu geri
	// əks etdirməlidir, yoxsa brauzer əl sıxmanı RƏDD edir. Burada həmişə
	// "bearer" seçirik — istemçi onu göndərməyibsə gorilla boş qaytarır və
	// davranış dəyişmir (köhnə `?token=` istemçiləri təsirlənmir).
	Subprotocols: []string{"bearer"},
}

// Client WebSocket bağlantısını temsil eder
type Client struct {
	UserID         uint
	Conn           *websocket.Conn
	Send           chan []byte
	Hub            *Hub
	ActiveChatWith *uint
	// Hazırda AÇIQ olan qrup çatı (conversation_id). DM ActiveChatWith-in
	// qrup ekvivalenti — istifadəçi qrup səhifəsindədirsə həmin qrupun
	// mesajları üçün FCM push GÖNDƏRİLMİR (onsuz da görür).
	ActiveGroupChat *uint

	// closeOnce — kapanış siqnalının (`done`) YALNIZ bir dəfə verilməsini təmin edir.
	// Əvvəllər həm registerClient (köhnə bağlantı atılarkən), həm də
	// unregisterClient eyni kanalı bağlaya bilirdi → ikiqat close panic →
	// goroutine ölür → həmin user-in soketi səssizcə qopurdu. İndi bütün
	// close-lar bu Once üzərindən keçir (idempotent).
	closeOnce sync.Once

	// done — client kapanış siqnalı. `Send` kanalı ARTIQ HEÇ VAXT bağlanmır;
	// bunun əvəzinə bu kanal bağlanır. Beləliklə paralel `Send <-` yazımları
	// (sendMessage/sendRecentMessages/broadcast — hamısı non-blocking select)
	// heç vaxt bağlı kanala yazmır → send-on-closed-channel panic (bütün
	// prosesi çökdürən) tamamilə aradan qalxır. writePump `done` ilə dayanır.
	done chan struct{}

	// typing — Issue 16: "yazır…" siqnalları üçün blok yoxlaması + sürət
	// limiti (bax websocket/typing_gate.go). Client ömürlüdür.
	typing *typingGate

	// evicting — Issue 22: `Send` buferi dolduqda client "yavaş istehlakçı"
	// sayılır və unregister-ə göndərilir. Bu, hər uğursuz yazımda AYRI bir
	// goroutine yaradırdı: 500 üzvlü qrupda tıxanmış bir client üçün yüzlərlə
	// goroutine eyni unregister kanalında növbəyə düşürdü. Atomik bayraq
	// çıxarılmanı yalnız BİR dəfə növbəyə qoyur.
	evicting atomic.Bool

	// ── ProtoVersion — İSTEMÇİ YETENEK PAZARLIĞI (geriyə uyumluluq) ──────────
	//
	// NİYƏ LAZIMDIR
	// Bu serverə eyni anda ÜÇ istemçi qoşulur: canlı Flutter tətbiqi
	// (`beanpon_app`), App Store-dakı KÖHNƏ `piokio_ios` buraxılışları və yeni
	// iOS buraxılışı. Serverdə bir davranışı dəyişmək köhnə istemçini SESSİZCƏ
	// sındıra bilər (frame gözləyir, gəlmir → mesaj görünmür). Ona görə heç bir
	// yeni davranış "avtomatik" deyil: istemçi ÖZÜNÜ TANITMALIDIR.
	//
	// NECƏ İŞLƏYİR
	// Yeni istemçi soketi `wss://…/ws?cv=2` ilə açır. Query string upgrade
	// sorğusunda gəlir, yəni `registerClient`-dən ƏVVƏL məlumdur (bu vacibdir:
	// `sendRecentMessages` qeydiyyat anında işləyir, istemçinin `hello` frame-i
	// göndərməsini gözləyə bilmərik). Cloudflare və Caddy query string-i
	// olduğu kimi ötürür.
	//
	//   ProtoVersion == 1  → parametr yoxdur = KÖHNƏ istemçi = BUGÜNKÜ davranış
	//                        bayt-bayt eyni. Flutter və köhnə iOS buradadır.
	//   ProtoVersion >= 2  → yeni istemçi: tarixçə selini almır, `message_ack`
	//                        alır (bax `handleIncomingMessage` / `send_message`).
	//
	// Sahə yalnız `HandleWebSocket`-də, client qurulanda BİR DƏFƏ yazılır və
	// sonra yalnız oxunur → kilid lazım deyil.
	ProtoVersion int
}

// Protokol versiyaları — sehrli rəqəmlər kodun içinə səpələnməsin.
const (
	// protoLegacy — `?cv=` göndərməyən istemçi. Flutter + köhnə iOS.
	protoLegacy = 1
	// protoV2 — WS ilə mesaj göndərən, `message_ack` anlayan, bağlantıda
	// tarixçə seli İSTƏMƏYƏN istemçi.
	protoV2 = 2
	// protoMax — serverin tanıdığı ən yüksək versiya. Daha böyük dəyər
	// göndərən istemçi buna sıxılır (gələcək istemçi köhnə serverdə çökməsin).
	protoMax = protoV2
)

// parseProtoVersion — `?cv=` parametrini təhlükəsiz oxuyur.
// Boş / pozuq / 1-dən kiçik dəyər → protoLegacy (yəni köhnə davranış).
func parseProtoVersion(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return protoLegacy
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < protoLegacy {
		return protoLegacy
	}
	if v > protoMax {
		return protoMax
	}
	return v
}

// enqueueEvict — Issue 22: yavaş client-i BİR dəfə unregister növbəsinə qoyur.
// Bloklamır (unregister kanalı Run döngüsündədir və Run bu anda çağıranı
// gözləyir ola bilər) — ona görə ayrıca goroutine-də yazılır.
func (c *Client) enqueueEvict(h *Hub) {
	if c.evicting.Swap(true) {
		return // artıq növbədədir
	}
	go func() {
		h.unregister <- c
	}()
}

// closeSend client-i təhlükəsiz (idempotent) bağlayır: `Send`-i DEYİL, `done`-u
// bağlayır. İstənilən qədər çağırıla bilər, yalnız ilki effekt edir. `Send`
// heç vaxt bağlanmadığı üçün ona paralel yazımlar panic verə bilməz.
func (c *Client) closeSend() {
	c.closeOnce.Do(func() {
		close(c.done)
	})
}

// errClientMessageIDTaken — Issue 9: göndərilən `client_message_id` BAŞQA
// istifadəçinin mesajına aiddir. Transaction-dan bu sentinel qaytarılır ki,
// çağıran tərəf onu ümumi "persist failed" xətasından ayıra bilsin.
var errClientMessageIDTaken = errors.New("client_message_id artıq istifadə olunub")

// Hub tüm client'ları yönetir
type Hub struct {
	clients    map[uint]*Client
	register   chan *Client
	unregister chan *Client
	// QEYD: burada bir `broadcast chan *Message` sahəsi vardı. Issue 22-dən
	// sonra ONA YAZAN QALMAMIŞDI (`SendToUser`/`SendToMultipleUsers` birbaşa
	// `deliver` çağırır) — kanal, `make`, `Run`-dakı select budağı və
	// `broadcastMessage` örtüyü ölü kod idi və "mesajlar bu kanaldan keçir"
	// təəssüratı yaradırdı. Hamısı silindi.
	mutex             sync.RWMutex
	db                *gorm.DB
	encryptionService interface {
		EncryptMessage(plainText string) (string, error)
		DecryptMessage(encryptedText string) (string, error)
	}
	httpClient *http.Client   // ← YENI
	config     *config.Config // ← YENI

	// moderationEnqueue — WS ilə göndərilən mesajları arxa-plan moderasiya
	// analizinə qoyur. Qeyri-bloklayıcıdır. nil olduqda atlanır.
	// Lokal funksiya tipi kimi saxlanır ki, services paketinə birbaşa
	// asılılıq (və potensial import cycle) olmasın.
	moderationEnqueue func(messageID string, senderID, receiverID uint, plainText string, createdAt time.Time)

	// xmpp — XMPP bridge (counterpart: none; server-only transport migration).
	// nil when the XMPP subsystem is disabled (XMPP_ENABLED=false). When set,
	// the Hub consults it on the egress path to route 1:1 / group messages to
	// NEW (XMPP) recipients while OLD recipients keep the legacy WS path. See
	// xmpp/WIRING.md.
	xmpp *xmpp.Bridge
}

// SetModerationEnqueue — WS axını üçün moderasiya enqueue callback-ini bağlayır.
// main.go-da queue qurulduqdan sonra çağırılır.
func (h *Hub) SetModerationEnqueue(fn func(messageID string, senderID, receiverID uint, plainText string, createdAt time.Time)) {
	h.moderationEnqueue = fn
}

// IncomingMessage client'tan gelen mesaj yapısı
type IncomingMessage struct {
	Type       string      `json:"type"`
	ReceiverID uint        `json:"receiver_id,omitempty"`
	Content    string      `json:"content,omitempty"`
	Data       interface{} `json:"data,omitempty"`
}

// OutgoingMessage client'a gönderilen mesaj yapısı
type OutgoingMessage struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

// Message WebSocket mesaj yapısı (broadcast için)
type Message struct {
	Type       string      `json:"type"`
	ReceiverID uint        `json:"receiver_id"`
	Data       interface{} `json:"data"`
}

// MessageData veritabanı mesaj yapısı
type MessageData struct {
	ID         string    `json:"id"`
	SenderID   uint      `json:"sender_id"`
	ReceiverID uint      `json:"receiver_id"`
	Text       string    `json:"text"`
	Read       bool      `json:"read"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// NewHub yeni hub oluştur
func NewHub(db *gorm.DB, encryptionService interface {
	EncryptMessage(plainText string) (string, error)
	DecryptMessage(encryptedText string) (string, error)
}, config *config.Config) *Hub { // ← config parametri əlavə
	return &Hub{
		clients:           make(map[uint]*Client),
		register:          make(chan *Client),
		unregister:        make(chan *Client),
		db:                db,
		encryptionService: encryptionService,
		httpClient:        &http.Client{Timeout: 10 * time.Second}, // ← YENI
		config:            config,                                  // ← YENI
	}
}

// Run hub'ı çalıştır
func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.registerClient(client)

		case client := <-h.unregister:
			h.unregisterClient(client)
		}
	}
}

// registerClient client'ı kaydet
func (h *Hub) registerClient(client *Client) {
	h.mutex.Lock()

	if existingClient, exists := h.clients[client.UserID]; exists {
		delete(h.clients, existingClient.UserID)
		// DİQQƏT: Köhnə client-in `Send` kanalını BURADA bağlamırıq.
		// Yalnız soketi bağlayırıq — bu, köhnə client-in readPump-ını qıracaq,
		// o da defer-də `unregister`-ə gedəcək və `Send` orada (closeSend ilə)
		// təhlükəsiz bağlanacaq. Beləliklə kanal yalnız BİR yerdən bağlanır.
		existingClient.Conn.Close()
		//log.Printf("Kullanıcı %d eski bağlantısı temizlendi", client.UserID)
	}

	h.clients[client.UserID] = client
	//log.Printf("Kullanıcı %d WebSocket'e bağlandı", client.UserID)

	//online oldugunu yazir
	h.setUserOnline(client.UserID)

	// Issue 4: paylaşılan presence — digər instanslar da bu istifadəçinin
	// onlayn olduğunu görsün (`IsUserOnline` yalan danışmasın).
	h.writePresence(client.UserID, 0, 0)

	// XMPP: this user is on a legacy WS client → mark them OLD in the registry
	// so the egress seam routes their incoming messages over legacy WS. No-op
	// when XMPP is disabled.
	h.markLegacyPresence(client.UserID)

	h.mutex.Unlock()

	// ── O(N) TARAMA ARTIQ YAZMA KİLİDİNİN ALTINDA DEYİL ─────────────────────
	// Əvvəl `statusTargetsLocked` MƏHZ BURADA, `h.mutex.Lock()` altında
	// çağırılırdı: hər bağlanmada bütün `h.clients` map-i gəzilib O(N) uzunluqda
	// bir dilim ayrılırdı və bu müddətdə `deliver()`-in `RLock`-u — yəni BÜTÜN
	// mesaj çatdırılması — dayanırdı. (Issue 23 kanal yazımlarını kiliddən
	// çıxarmışdı, amma TARAMANIN ÖZÜ içəridə qalmışdı.)
	//
	// İndi snapshot ayrıca, PAYLAŞILAN (RLock) kilid altında alınır → digər
	// oxuyucularla paralel işləyir və yazma kilidini heç bloklamır.
	//
	// Doğruluq: `statusTargets` istifadəçini id-yə görə onsuz da istisna edir,
	// ona görə snapshot-ın kiliddən sonra alınması davranışı dəyişmir.
	statusTargets := h.statusTargets(client.UserID)

	// Kullanıcı online durumunu diğer kullanıcılara bildir (kilidsiz)
	h.broadcastUserStatus(client.UserID, "online", statusTargets)

	//İlk bağlantıda okunmamış mesaj sayısını gönder.
	// `...Now`: bağlantı anı birləşdirmə pəncərəsini gözləməməlidir —
	// istemçi rozeti dərhal doğru göstərsin.
	go h.SendUnreadCountUpdateNow(client.UserID)

	// ── TARİXÇƏ SELİ ARTIQ YALNIZ KÖHNƏ İSTEMÇİYƏ GEDİR ─────────────────────
	//
	// `sendRecentMessages` hər bağlantıda:
	//   • 2 LEFT JOIN-li ağır bir sorğu işlədir (`sender_id = ? OR receiver_id = ?`
	//     + `ORDER BY created_at` — uyğun index yoxdur),
	//   • 30 mesajın şifrəsini açır (30 × AES),
	//   • 31 WebSocket frame yazır (256-lıq `Send` buferinin 12%-i).
	//
	// Üstəlik sorğu `ORDER BY m.created_at ASC` yazır — yəni "son 30" deyil,
	// istifadəçinin HƏYATDAKI İLK 30 MESAJI. Funksiya adı və şərhi ilə davranış
	// uyğun gəlmir.
	//
	// Yeni iOS istemçisi bu frame-ləri onsuz da EMAL ETMİR (`history_message`
	// üçün `switch`-də `case` yoxdur → `default: break`) və kaçırılan mesajları
	// `runDeltaSync` / `syncMissed` ilə daha dəqiq bərpa edir. Ona görə v2-də
	// tamamilə atlanır.
	//
	// KÖHNƏ istemçi (Flutter + köhnə iOS) üçün HEÇ NƏ DƏYİŞMİR — həmin 31
	// frame eyni sıra ilə göndərilir.
	if client.ProtoVersion < protoV2 {
		go h.sendRecentMessages(client)
	}
}

// unregisterClient client'ı çıkar
func (h *Hub) unregisterClient(client *Client) {
	h.mutex.Lock()

	// Identity yoxlaması: map-dəki client məhz BU client-dirmi?
	// Reconnect zamanı köhnə client soketi bağlanıb gec `unregister`-ə gələ
	// bilər, amma `clients[UserID]` artıq YENİ client-ə işarə edir. Köhnə
	// client yeni client-i map-dən silməməli və yeni user-i offline
	// göstərməməlidir. Yalnız öz `Send` kanalını bağlamalıdır.
	current, exists := h.clients[client.UserID]

	// Issue 23: fan-out kilid altında EDİLMİR — göndərmə aşağıda (kilidsiz)
	// baş verir. Alıcı siyahısı da artıq kiliddən SONRA alınır (bax
	// `registerClient`-dəki şərh).
	wentOffline := false

	if exists && current == client {
		// Bu, hazırkı aktiv client-dir — tam təmizlik.
		delete(h.clients, client.UserID)

		h.setUserOffline(client.UserID)

		// Issue 4: paylaşılan presence qeydini götür (yalnız BİZƏ aiddirsə).
		h.clearPresence(client.UserID)

		client.closeSend()

		_ = client.Conn.Close()
		//log.Printf("Kullanıcı %d WebSocket'ten ayrıldı", client.UserID)

		wentOffline = true
	} else {
		// Köhnə/əvəz olunmuş client — yeni bağlantıya toxunma, yalnız
		// öz kanalını və soketini təmizlə.
		client.closeSend()
		_ = client.Conn.Close()
	}

	h.mutex.Unlock()

	// Kullanıcı offline durumunu diğer kullanıcılara bildir (kilidsiz).
	// Snapshot map-dən SİLİNDİKDƏN sonra alınır — çıxan client özünə "offline"
	// frame-i almasın (`statusTargets` onu id-yə görə də istisna edir).
	if wentOffline {
		h.broadcastUserStatus(client.UserID, "offline", h.statusTargets(client.UserID))
	}
}

// ── `user_status` FAN-OUT ƏHATƏSİ ───────────────────────────────────────────
//
// PROBLEM (dəyişdirilmədi, yalnız açarı əlavə edildi)
// Bir istifadəçi bağlanan/kopan HƏR dəfə `user_status` frame-i O AN BAĞLI OLAN
// HƏR KƏSƏ göndərilir. Dost siyahısı süzgəci yoxdur. 10.000 bağlantı və
// dəqiqədə %10 dövriyyə = dəqiqədə 10 milyon frame; xərc istifadəçi sayının
// KVADRATI ilə artır və yatay ölçəklənməni kilidləyir.
//
// NİYƏ DEFAULT-DA DƏYİŞMİRİK
// Bu frame-i yeni iOS istemçisi ONSUZ DA EMAL ETMİR (`switch`-də `case` yoxdur),
// AMMA canlı Flutter tətbiqi onu söhbət siyahısındakı "onlayn" nöqtəsi üçün
// istifadə edir ola bilər. Süzgəci birbaşa açmaq həmin nöqtələri söndürərdi.
// Ona görə açar ƏLAVƏ olunur, default KÖHNƏ davranışdır; ölçüb sonra açacağıq.
//
//	WS_STATUS_FANOUT=all   (default) → bugünkü davranış, bayt-bayt eyni
//	WS_STATUS_FANOUT=chat            → yalnız həmin şəxsin söhbəti AÇIQ olanlara
const (
	statusFanoutAll  = "all"
	statusFanoutChat = "chat"
)

var statusFanoutMode = func() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WS_STATUS_FANOUT"))) {
	case statusFanoutChat:
		return statusFanoutChat
	default:
		return statusFanoutAll
	}
}()

// statusTargets — `user_status` fan-out-unun alıcı siyahısını kopyalayır.
//
// Kilidi ÖZÜ alır (paylaşılan `RLock`) — çağıran heç bir kilid tutmamalıdır.
// Əvvəlki `statusTargetsLocked` yazma kilidinin (`Lock`) altından çağırılırdı və
// bu müddətdə bütün mesaj çatdırılması dayanırdı; bax `registerClient` şərhi.
//
// Yalnız map gəzilir və göstəricilər kopyalanır — heç bir kanal yazısı yoxdur.
func (h *Hub) statusTargets(exceptUserID uint) []*Client {
	chatOnly := statusFanoutMode == statusFanoutChat

	h.mutex.RLock()
	defer h.mutex.RUnlock()

	targets := make([]*Client, 0, len(h.clients))
	for _, client := range h.clients {
		if client.UserID == exceptUserID { // Kendisi hariç
			continue
		}
		if chatOnly {
			// Yalnız `exceptUserID` ilə söhbəti AÇIQ olanlar. `ActiveChatWith`
			// `SetActiveChat` ilə yazılır və eyni `h.mutex` altında qorunur.
			if client.ActiveChatWith == nil || *client.ActiveChatWith != exceptUserID {
				continue
			}
		}
		targets = append(targets, client)
	}
	return targets
}

// broadcastUserStatus kullanıcı durumunu yayınla.
//
// Issue 23: bu fan-out ÇAĞIRANIN YAZMA KİLİDİ altında işləyirdi
// (`registerClient` / `unregisterClient` hər ikisi `h.mutex.Lock()` +
// `defer Unlock` idi). Yəni HƏR bağlanma və HƏR kopma bütün hub-ı — o
// cümlədən `deliver`-in `RLock`-unu — bağlı client sayı qədər (O(N))
// addımlıq müddətə dondururdu; buferi dolu bir client üçün əlavə olaraq
// `enqueueEvict` da həmin kilid altında çağırılırdı. İndi alıcılar
// `statusTargets` ilə PAYLAŞILAN kilid altında SNAPSHOT edilir (Issue 23-də
// yalnız kanal yazıları çıxarılmışdı, taramanın özü hələ də yazma kilidinin
// altındaydı), kanal yazıları isə kiliddən sonra baş verir — `deliver` və
// `SendToMultipleUsers` ilə tam eyni naxış.
//
// `Send` kanalı HEÇ VAXT bağlanmır (bağlanma siqnalı `done` kanalıdır,
// bax `closeSend`), ona görə snapshot alındıqdan sonra client kopsa belə
// yazı use-after-close verə bilməz — frame sadəcə buferdə qalır.
func (h *Hub) broadcastUserStatus(userID uint, status string, targets []*Client) {
	data := map[string]interface{}{
		"user_id": userID,
		"status":  status,
	}
	statusMessage := &Message{
		Type: "user_status",
		Data: data,
	}

	// Tüm bağlı kullanıcılara gönder.
	// Issue 22: payload BİR dəfə marshal olunur (əvvəl hər alıcı üçün ayrıca
	// JSON marshal edilirdi — N bağlı client = N marshal).
	payload := h.messageToBytes(statusMessage)
	if payload == nil {
		return
	}
	for _, client := range targets {
		select {
		case client.Send <- payload:
		default:
			client.enqueueEvict(h)
		}
	}

	// Issue 4: presence-i də instanslar ARASI et. Bura qədər yalnız BU
	// prosesin client-ləri xəbər tuturdu — B instansındakı istifadəçi A-da
	// bağlanan dostunu heç vaxt "online" görmürdü (mesaj yolu artıq
	// `publishCluster` ilə yayımlanır, status yolu isə qalmışdı).
	// Frame `origin` sahəsi ilə özünü tanıyır → yayan instans onu təkrar
	// emal etmir (bax StartClusterSubscriber).
	//
	// `WS_STATUS_FANOUT=chat` rejimində uzaq instans da eyni süzgəci tətbiq
	// etməlidir — `subject` sahəsi ona kimin statusu olduğunu bildirir.
	// Sahə `omitempty`-dir: köhnə instans onu görməzdən gəlir və frame-i
	// hamıya yayır, yəni bugünkü davranış → rolling deploy təhlükəsizdir.
	subject := uint(0)
	if statusFanoutMode == statusFanoutChat {
		subject = userID
	}
	h.publishClusterBroadcastScoped([]uint{userID}, "user_status", data, subject)
}

// deliver — mesajı alıcının `Send` buferinə non-blocking yazır.
//
// Issue 22: əvvəllər HƏR göndərmə BUFERSİZ `h.broadcast` kanalından keçib TƏK
// `Run` goroutine-i tərəfindən növbə ilə emal olunurdu. Nəticələr:
//   - head-of-line blocking: `SendToMultipleUsers` 500 üzvlü qrupda 500 dəfə
//     `Run` ilə rendezvous edirdi; bu müddətdə `register`/`unregister` də
//     dayanırdı → yeni bağlantılar və təmizləmə gecikirdi;
//   - bir yavaş/tıxanmış client bütün prosesin throughput tavanını təyin edirdi;
//   - hər uğursuz yazım AYRI goroutine yaradıb `unregister`-də növbəyə düşürdü.
//
// İndi çatdırma çağıranın öz goroutine-ində, yalnız qısa `RLock` altında baş
// verir. Per-client sıra POZULMUR — hər client-in öz `Send` kanalı və tək
// `writePump`-ı var, sıralamanı o təmin edir.
func (h *Hub) deliver(message *Message) {
	h.mutex.RLock()
	client, exists := h.clients[message.ReceiverID]
	h.mutex.RUnlock()
	if !exists {
		return
	}

	payload := h.messageToBytes(message)
	if payload == nil {
		return
	}

	select {
	case client.Send <- payload:
		// Canlı new_message push-u BAĞLI alıcıya çatdısa (kanala yazıla
		// bildi) → server tərəfdə dərhal delivered=true (WhatsApp davranışı,
		// ani iki tick). Uğursuz göndərmə (default branch) bura düşmür.
		h.maybeMarkLivePushDelivered(message)
	default:
		client.enqueueEvict(h)
	}
}

// deliverWithCluster — lokal çatdırma + Issue 4 üçün digər instanslara yayım.
// Alıcı bu instansda TAPILSA DA yayım edilir: istifadəçi reconnect anında
// başqa instansa keçmiş ola bilər və uzaq instansda ona çatmalıdır. Uzaq
// tərəf onu tapmazsa frame sadəcə atılır (ucuz), tapsa istemçi id-yə görə
// dublikatı özü süzür.
func (h *Hub) deliverWithCluster(message *Message) {
	h.deliver(message)
	h.publishCluster([]uint{message.ReceiverID}, message.Type, message.Data)
}

// maybeMarkLivePushDelivered — canlı `new_message` push-u bağlı alıcının Send
// kanalına uğurla yazıldıqda mesajı delivered=true işarələyir və göndərənə
// `message_delivered` event-i göndərir. Client ack-i (`mark_delivered` frame-i,
// bax handleMarkDelivered) push/offline gəlişlərini örtür; bu yol yalnız ani
// iki tick üçündür. delivered=false şərti ikiqat emit-in qarşısını alır.
func (h *Hub) maybeMarkLivePushDelivered(message *Message) {
	if message.Type != "new_message" {
		return
	}

	dataMap, ok := message.Data.(map[string]interface{})
	if !ok {
		return
	}

	// Tarixi mesajlar ayrı tiplə (history_message) gedir — yenə də qoruyucu.
	if isHistory, _ := dataMap["is_history"].(bool); isHistory {
		return
	}

	msgID, ok := dataMap["id"].(string)
	if !ok || msgID == "" {
		return
	}
	senderID, ok := dataMap["sender_id"].(uint)
	if !ok {
		return
	}
	receiverID, ok := dataMap["receiver_id"].(uint)
	if !ok {
		return
	}

	// new_message həm göndərənin echo-suna, həm alıcıya gedir — yalnız
	// ALICININ nüsxəsində işarələ (göndərən echo-su delivered demək deyil).
	if message.ReceiverID != receiverID {
		return
	}

	// DB işi hub Run döngüsünü bloklamasın deyə goroutine-də. h app-ömürlü
	// singleton-dur — retain/leak problemi yoxdur.
	go func() {
		res := h.db.Model(&models.Message{}).
			Where("id = ? AND delivered = false", msgID).
			Update("delivered", true)
		if res.Error != nil {
			log.Printf("live-push delivered işarələmə xətası: %v", res.Error)
			return
		}
		if res.RowsAffected == 0 {
			// Issue 60: burada əvvəl 1 saniyəlik `Sleep` + təkrar cəhd vardı.
			// Səbəbi WS yolunun mesajı ASYNC yazması idi (Issue 1) — o səbəb
			// ARTIQ YOXDUR: sətir yayımdan ƏVVƏL komit olunur. RowsAffected==0
			// indi yalnız "artıq delivered/read" və ya "sətir yoxdur" deməkdir;
			// gözləməyin faydası yox, sadəcə goroutine-i saxlayırdı.
			return
		}

		h.SendToUser(senderID, "message_delivered", map[string]interface{}{
			"other_user_id": receiverID,
			"message_ids":   []string{msgID},
		})
	}()
}

// SendToUser belirli kullanıcıya mesaj gönder
func (h *Hub) SendToUser(userID uint, messageType string, data interface{}) {
	h.deliverWithCluster(&Message{
		Type:       messageType,
		ReceiverID: userID,
		Data:       data,
	})
}

// SendToMultipleUsers birden fazla kullanıcıya mesaj gönder.
//
// Issue 22: qrup fan-out-u üçün isti yol. Əvvəl hər alıcı üçün ayrıca
// marshal + ayrıca kilid + `Run` döngüsü ilə ayrıca rendezvous vardı
// (N alıcı = 3N sinxronizasiya nöqtəsi). İndi: tək marshal, tək RLock
// snapshot-u, sonra kilidsiz non-blocking yazımlar.
func (h *Hub) SendToMultipleUsers(userIDs []uint, messageType string, data interface{}) {
	if len(userIDs) == 0 {
		return
	}

	// `new_message` (DM) çatdırma-işarələmə yan effekti daşıyır — onu
	// `deliver` yolunda saxlayırıq ki, davranış dəyişməsin.
	if messageType == "new_message" {
		unique := make([]uint, 0, len(userIDs))
		seen := make(map[uint]struct{}, len(userIDs))
		for _, userID := range userIDs {
			if _, dup := seen[userID]; dup {
				continue
			}
			seen[userID] = struct{}{}
			unique = append(unique, userID)
			h.deliver(&Message{Type: messageType, ReceiverID: userID, Data: data})
		}
		// Issue 4: bütün alıcılar üçün TƏK yayım (alıcı başına ayrıca deyil).
		h.publishCluster(unique, messageType, data)
		return
	}

	payload := h.messageToBytes(&Message{Type: messageType, Data: data})
	if payload == nil {
		return
	}

	// Alıcı snapshot-u TƏK RLock altında. Təkrarlanan id-lər süzülür —
	// əks halda eyni client eyni frame-i iki dəfə alırdı.
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

	for _, client := range targets {
		select {
		case client.Send <- payload:
		default:
			client.enqueueEvict(h)
		}
	}

	// Issue 4: lokal olmayan alıcılar üçün digər instanslara TƏK yayım.
	unique := make([]uint, 0, len(seen))
	for userID := range seen {
		unique = append(unique, userID)
	}
	h.publishCluster(unique, messageType, data)
}

// FilterUsersInGroupChat — Issue 23: verilmiş id siyahısından hazırda BU qrup
// səhifəsi AÇIQ olanları qaytarır. Əvvəl çağıran tərəf hər üzv üçün ayrıca
// `IsUserInGroupChat` çağırırdı → N dəfə RLock alıb-buraxma. İndi tək kilid.
func (h *Hub) FilterUsersInGroupChat(userIDs []uint, conversationID uint) []uint {
	if len(userIDs) == 0 {
		return nil
	}
	out := make([]uint, 0, len(userIDs))
	missing := make([]uint, 0, len(userIDs))

	h.mutex.RLock()
	for _, userID := range userIDs {
		client, ok := h.clients[userID]
		if !ok {
			missing = append(missing, userID)
			continue
		}
		if client.ActiveGroupChat != nil && *client.ActiveGroupChat == conversationID {
			out = append(out, userID)
		}
	}
	h.mutex.RUnlock()

	// Issue 4: lokal olmayan üzvlər üçün paylaşılan presence — TƏK MGET
	// gedişi (üzv başına GET 5000-lik qrupda fəlakət olardı).
	if len(missing) > 0 && conversationID != 0 {
		for userID, rec := range remotePresenceMany(missing) {
			if rec.Group == conversationID {
				out = append(out, userID)
			}
		}
	}
	return out
}

// HandleNewMessage yeni mesajı handle et ve WebSocket üzerinden yayınla
func (h *Hub) HandleNewMessage(senderID, receiverID uint, messageID, content, msgType string, createdAt time.Time, replyToMessageID *string, storyID *uint, conversationStatus string, silent bool) {
	messageData := map[string]interface{}{
		"id":                  messageID,
		"sender_id":           senderID,
		"receiver_id":         receiverID,
		"story_id":            storyID,
		"reply_to_message_id": replyToMessageID,
		"text":                content,
		"type":                msgType,
		"read":                false,
		"created_at":          createdAt.UTC().Format(time.RFC3339),
		"is_history":          false,
	}

	// Reply mesajı kontrolü
	if replyToMessageID != nil {
		var replyMessage models.Message
		if err := h.db.Where("id = ?", *replyToMessageID).First(&replyMessage).Error; err == nil {
			replyDecryptedText, err := h.encryptionService.DecryptMessage(replyMessage.EncryptedText)
			if err != nil {
				replyDecryptedText = "Mesaj çözülemedi"
			}

			messageData["reply_to_message"] = map[string]interface{}{
				"id":         replyMessage.ID,
				"sender_id":  replyMessage.SenderID,
				"text":       replyDecryptedText,
				"created_at": replyMessage.CreatedAt,
			}
		}
	}

	// Story bilgisi kontrolü
	if storyID != nil {
		var story models.Story
		if err := h.db.Where("id = ?", *storyID).First(&story).Error; err == nil {
			storyResponse := map[string]interface{}{
				"id":         story.ID,
				"type":       story.Type,
				"media_url":  utils.PrependS3URL(&story.MediaURL),
				"content":    story.Content,
				"user_id":    story.UserID,
				"created_at": story.CreatedAt,
				"available":  true,
			}

			if story.Type == "video" && story.MediaMetadata != nil {
				var metadata map[string]interface{}
				if err := json.Unmarshal([]byte(*story.MediaMetadata), &metadata); err == nil {
					if thumbnailURL, exists := metadata["thumbnail_url"].(string); exists && thumbnailURL != "" {
						storyResponse["thumbnail_url"] = utils.PrependS3URL(&thumbnailURL)
					}
				}
			}

			messageData["story"] = storyResponse
		} else {
			messageData["story"] = map[string]interface{}{
				"id":        *storyID,
				"available": false,
				"message":   "Bu story artık mevcut değil",
			}
		}
	}

	// Hem gönderen hem de alıcıya gönder.
	//
	// EGRESS SEAM (XMPP) — DUAL DELIVERY for zero message loss.
	//
	// The message is ALWAYS delivered over the legacy WS channel (to both the
	// sender's echo and the receiver). ADDITIONALLY, if the receiver is on a
	// NEW (XMPP) client, the same message is published as an XMPP stanza.
	//
	// Why dual-deliver instead of "XMPP OR WS"? A NEW client connects to BOTH
	// transports (the iOS facade keeps the legacy WS open while adding XMPP),
	// and it DEDUPLICATES inbound messages by id. So:
	//   - receiver on legacy WS only  → gets it via WS.
	//   - receiver on XMPP + WS        → gets both, dedup shows one.
	//   - receiver transiently offline on XMPP (presence TTL) → still gets it
	//     via WS; nothing is lost. (With auth_method: jwt, ejabberd has no
	//     mod_offline, so an XMPP-only delivery to an offline JID would be
	//     dropped — dual delivery removes that risk.)
	// The message is persisted in the DB regardless, so history/push cover the
	// fully-offline case exactly as today.
	//
	// When the XMPP subsystem is disabled this is byte-for-byte the old
	// behaviour (the XMPP block is skipped). See xmpp/WIRING.md.
	userIDs := []uint{senderID, receiverID}
	h.SendToMultipleUsers(userIDs, "new_message", messageData) // legacy WS (always)

	if h.xmpp != nil && h.xmpp.Enabled() && h.xmpp.Registry().IsXMPP(receiverID) {
		// Best-effort XMPP copy for the NEW client. iOS dedups by id, so this
		// never double-shows alongside the WS copy.
		h.xmpp.RouteDM(xmpp.DM1to1{
			MessageID:        messageID,
			SenderID:         senderID,
			ReceiverID:       receiverID,
			Text:             content,
			Kind:             msgType,
			ReplyToMessageID: derefStr(replyToMessageID),
		})
	}

	h.sendConversationUpdate(senderID, receiverID, messageData)

	go h.SendUnreadCountUpdate(receiverID)

	// 🔕 SƏSSİZ GÖNDƏRMƏ: göndərən "səssiz göndər" seçibsə, mesaj normal çatır
	// (DB + WS + unread), AMMA qarşı tərəfə push notification GETMİR.
	if silent {
		log.Printf("🔕 Səssiz mesaj: sender=%d → receiver=%d, push göndərilmir",
			senderID, receiverID)
		return
	}

	// ── Issue 10: push qapısı ────────────────────────────────────────────────
	//
	// Əvvəl yalnız `"active"` buraxılırdı, LAKİN REST yolu bura HƏMİŞƏ sabit
	// `"active"` ötürürdü — yəni praktikada REST hər statusda push göndərirdi,
	// WS isə yalnız `active`-də. Eyni mesaj nəqliyyatdan asılı olaraq bildiriş
	// doğurur və ya doğurmurdu.
	//
	// İndi hər iki yol GERÇƏK statusu ötürür və qapı BURADA, bir yerdə qərar
	// verir:
	//   • "active"   → normal söhbət, push var (dəyişmədi);
	//   • "pending"  → MESAJ İSTƏYİ. Push VAR — istifadəçi tanımadığı adamdan
	//                  gələn ilk mesajı görməlidir (REST-də onsuz da gedirdi,
	//                  yalnız WS-də yox idi). Spam qapısı `CanSendMessage`-in
	//                  `max_pending_messages` limiti + shadow-ban ilə qorunur;
	//   • "restricted" → tək tərəfli mesaj limiti aşılıb, söhbət kilidli →
	//                  push YOXDUR (əvvəl REST üzərindən GEDİRDİ — spam deşiyi);
	//   • ""         → söhbət ümumiyyətlə yaradılmayıb (mesaj banı) → push yox.
	//
	// ── "TEŞHİS (geçici)" LOG SƏTRİ SİLİNDİ ─────────────────────────────────
	// Burada hər mesajda işləyən bir `log.Printf("📨 PUSH-GATE: …")` vardı.
	// Yorumu "geçici" deyirdi, amma qalıcı olmuşdu və sistemin ƏN İSTİ
	// yolundaydı. İki ayrı zərəri vardı:
	//
	//  1. Go `log.Printf` arqumentlərini HƏMİŞƏ hesablayır (səviyyə qapısı
	//     yoxdur). Sətir `IsUserOnline(receiverID)` və
	//     `IsUserInChatWith(receiverID, senderID)` çağırırdı — AŞAĞIDAKI
	//     şərtlərdə TƏKRAR çağırılan eyni funksiyalar. Yəni mesaj başına
	//     2 yerinə 4 `h.mutex.RLock()` alınırdı (bu kilid `deliver()` ilə
	//     birbaşa yarışır) və keş soyuq olduqda 2 əlavə Redis GET edilirdi.
	//  2. Standart `log` qlobal mutex tutub stderr-ə sinxron yazır.
	//
	// İndi hər iki dəyər BİR DƏFƏ hesablanıb dəyişənə alınır.
	online := h.IsUserOnline(receiverID)
	inChat := false
	if online {
		// Yalnız onlayn olduqda mənalıdır — offline istifadəçi üçün bu
		// çağırış lazımsız bir kilid/Redis gedişi olardı.
		inChat = h.IsUserInChatWith(receiverID, senderID)
	}

	switch conversationStatus {
	case "active", "pending":
		if !online || !inChat {
			go h.sendPushNotification(senderID, receiverID, content, msgType)
		}
	default:
		// active/pending DIŞINDA → push GÖNDERİLMİYOR. (Əvvəlki `log.Printf`
		// buradan da silindi: `restricted`/boş status normal bir haldır, hər
		// dəfəsində sətir yazmağın diaqnostik dəyəri yoxdur.)
	}
}

// Bu yeni fonksiyonu ekle
func (h *Hub) sendConversationUpdate(senderID, receiverID uint, messageData map[string]interface{}) {
	// Gönderen ve alıcının conversation listelerini güncelle
	conversationData := map[string]interface{}{
		"type":              "conversation_update",
		"message_data":      messageData,
		"other_user_id":     receiverID, // Gönderende receiver görünür
		"last_message":      messageData["text"],
		"last_message_time": messageData["created_at"],
		"is_from_me":        true,
	}

	// Gönderene
	h.SendToUser(senderID, "conversation_update", conversationData)

	// Alıcıya (onun için other_user_id sender olacak)
	conversationDataForReceiver := map[string]interface{}{
		"type":              "conversation_update",
		"message_data":      messageData,
		"other_user_id":     senderID, // Alıcıda sender görünür
		"last_message":      messageData["text"],
		"last_message_time": messageData["created_at"],
		"is_from_me":        false,
	}

	h.SendToUser(receiverID, "conversation_update", conversationDataForReceiver)
}

// HandleMessageRead mesaj okundu durumunu handle et
func (h *Hub) HandleMessageRead(messageID string, senderID, readerID uint) {
	readData := map[string]interface{}{
		"message_id": messageID,
		"reader_id":  readerID,
		"read_at":    time.Now().UTC(),
	}

	// Sadece gönderene bildir (alıcı zaten okudu)
	h.SendToUser(senderID, "message_read", readData)

	go h.SendUnreadCountUpdate(readerID)

	log.Printf("Mesaj okundu WebSocket üzerinden yayınlandı: %s", messageID)
}

// IsUserOnline kullanıcının online olup olmadığını kontrol et.
//
// Issue 4: əvvəl YALNIZ bu prosesin map-inə baxırdı. İki replica ilə
// B instansındakı onlayn istifadəçi A-da "offline" görünürdü → çatda
// oturduğu halda ona push gedirdi. İndi lokal map (sürətli yol) tapmasa
// paylaşılan presence yoxlanılır.
func (h *Hub) IsUserOnline(userID uint) bool {
	h.mutex.RLock()
	_, exists := h.clients[userID]
	h.mutex.RUnlock()
	if exists {
		return true
	}
	_, remote := remotePresence(userID)
	return remote
}

// ── DEPLOY 3 / DM-S2: DÜZGÜN KAPANIŞ (graceful shutdown) ───────────────────
//
// ÖNCE: deploy sırasında proses öldürülüyordu. Bağlı her istemci için TCP
// aniden kopuyor, close frame HİÇ gitmiyordu. İstemci tarafında bu "beklenmedik
// hata" olarak görünür ve yeniden bağlanma merdiveni (backoff) devreye girer —
// yani kullanıcı her deploy'da 1-5 saniye "bağlanıyor" durumunda kalır.
// Ayrıca `user_presences` satırları `is_online = true` kalıyor, oturum süresi
// muhasebesi kayboluyordu (bak `setUsersOfflineBulk`).
//
// SONRA: SIGTERM alındığında her istemciye normal WebSocket close frame'i
// gönderilir. İstemci bunu "sunucu kapandı" olarak görür ve BEKLEMEDEN yeniden
// bağlanır (backoff'a düşmez) — deploy kesintisi saniyeler yerine milisaniyeler
// olur. Presence de tek SQL ile doğru şekilde kapatılır.
//
// `ctx` süresi dolarsa yarıda kesilir; kapanış hiçbir zaman asılı kalmaz.
func (h *Hub) Shutdown(ctx context.Context) {
	h.mutex.RLock()
	clients := make([]*Client, 0, len(h.clients))
	userIDs := make([]uint, 0, len(h.clients))
	for userID, c := range h.clients {
		clients = append(clients, c)
		userIDs = append(userIDs, userID)
	}
	h.mutex.RUnlock()

	if len(clients) == 0 {
		log.Printf("🛑 Kapanış: bağlı WebSocket yok")
		return
	}
	log.Printf("🛑 Kapanış: %d WebSocket bağlantısına close frame gönderiliyor", len(clients))

	// 1) Close frame. `closeSend` yalnız `done` kanalını kapatır; frame'i
	//    `writePump` yazar (bak writePump'ın `case <-c.done` dalı). İdempotent.
	for _, c := range clients {
		c.closeSend()
	}

	// 2) Paylaşılan (Redis) presence kayıtları — yalnız BİZE ait olanlar silinir
	//    (`clearPresence` instance kimliğini kontrol eder).
	for _, userID := range userIDs {
		h.clearPresence(userID)
	}

	// 3) SQL presence — tek sorgu, süre muhasebesi doğru.
	done := make(chan struct{})
	go func() {
		// `ctx` dolarsa `Shutdown` qayıdır, amma BU goroutine işləməyə davam
		// edir. Burada bir panic prosesi məhz kapanış anında çökdürərdi
		// (`readPump`/`writePump` ilə eyni müdafiə xətti).
		defer func() {
			if r := recover(); r != nil {
				log.Printf("⚠️ Kapanış presence yazımında panic: %v", r)
			}
		}()
		defer close(done)
		h.setUsersOfflineBulk(userIDs)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		log.Printf("⚠️ Kapanış süresi doldu — presence yazımı tamamlanmadı")
	}
}

// GetConnectedUsersCount bağlı kullanıcı sayısı
func (h *Hub) GetConnectedUsersCount() int {
	h.mutex.RLock()
	defer h.mutex.RUnlock()
	return len(h.clients)
}

// GetConnectedUsers bağlı kullanıcı listesi
func (h *Hub) GetConnectedUsers() []uint {
	h.mutex.RLock()
	defer h.mutex.RUnlock()

	users := make([]uint, 0, len(h.clients))
	for userID := range h.clients {
		users = append(users, userID)
	}
	return users
}

func (h *Hub) messageToBytes(message *Message) []byte {
	outgoing := &OutgoingMessage{
		Type: message.Type,
		Data: message.Data,
	}

	data, err := json.Marshal(outgoing)
	if err != nil {
		log.Printf("JSON marshal hatası: %v", err)
		return []byte(`{"type":"error","data":"Message format error"}`)
	}
	return data
}

// sendRecentMessages kullanıcıya son 30 mesajı gönder
// sendRecentMessages kullanıcıya son 30 mesajı gönder
func (h *Hub) sendRecentMessages(client *Client) {
	var messages []struct {
		ID               string  `json:"id"`
		SenderID         uint    `json:"sender_id"`
		ReceiverID       uint    `json:"receiver_id"`
		StoryID          *uint   `json:"story_id"` // YENİ ALAN
		ReplyToMessageID *string `json:"reply_to_message_id"`
		Text             string  `json:"text"`
		Read             bool    `json:"read"`
		SenderReaction   *string `json:"sender_reaction"`
		ReceiverReaction *string `json:"receiver_reaction"`
		CreatedAt        string  `json:"created_at"`
		// Reply mesajı bilgileri
		ReplyToMessageText   *string `json:"reply_to_message_text"`
		ReplyToMessageSender *uint   `json:"reply_to_message_sender"`
		ReplyToMessageType   *string `json:"reply_to_message_type"`
		ReplyToCreatedAt     *string `json:"reply_to_created_at"`
		// Story bilgileri
		StoryType      *string `json:"story_type"`
		StoryMediaURL  *string `json:"story_media_url"`
		StoryContent   *string `json:"story_content"`
		StoryUserID    *uint   `json:"story_user_id"`
		StoryCreatedAt *string `json:"story_created_at"`
	}

	query := `
    SELECT 
        m.id, 
        m.sender_id, 
        m.receiver_id,
        m.story_id,
        m.reply_to_message_id,
        m.encrypted_text as text,
        m.read,
        m.sender_reaction,
        m.receiver_reaction,
        m.created_at,
        reply.encrypted_text as reply_to_message_text,
        reply.sender_id as reply_to_message_sender,
        reply.created_at as reply_to_created_at,
        s."type" as story_type,
        s.media_url as story_media_url,
        s.content as story_content,
        s.user_id as story_user_id,
        s.created_at as story_created_at
    FROM messages m
    LEFT JOIN messages reply ON m.reply_to_message_id = reply.id
    LEFT JOIN stories s ON m.story_id = s.id
    WHERE (m.sender_id = ? OR m.receiver_id = ?)
    AND (
        CASE 
            WHEN m.sender_id = ? THEN m.is_deleted_by_sender = false
            ELSE m.is_deleted_by_receiver = false
        END
    )
    ORDER BY m.created_at ASC 
    LIMIT 30
`

	if err := h.db.Raw(query, client.UserID, client.UserID, client.UserID).Scan(&messages).Error; err != nil {
		log.Printf("Son mesajlar alınamadı: %v", err)
		return
	}

	for i := 0; i < len(messages); i++ {
		msg := messages[i]

		decryptedText, err := h.encryptionService.DecryptMessage(msg.Text)
		if err != nil {
			decryptedText = "Mesaj çözülemedi"
		}

		messageData := map[string]interface{}{
			"id":                  msg.ID,
			"sender_id":           msg.SenderID,
			"receiver_id":         msg.ReceiverID,
			"story_id":            msg.StoryID, // YENİ ALAN
			"reply_to_message_id": msg.ReplyToMessageID,
			"text":                decryptedText,
			"read":                msg.Read,
			"sender_reaction":     msg.SenderReaction,
			"receiver_reaction":   msg.ReceiverReaction,
			"created_at":          msg.CreatedAt,
			"is_history":          true,
		}

		// Story bilgisi varsa ekle
		if msg.StoryID != nil {
			if msg.StoryType != nil {
				// Story hala mevcut
				messageData["story"] = map[string]interface{}{
					"id":         *msg.StoryID,
					"type":       *msg.StoryType,
					"media_url":  msg.StoryMediaURL,
					"content":    msg.StoryContent,
					"user_id":    *msg.StoryUserID,
					"created_at": msg.StoryCreatedAt,
					"available":  true,
				}
			} else {
				// Story silinmiş veya erişilemiyor
				messageData["story"] = map[string]interface{}{
					"id":        *msg.StoryID,
					"available": false,
					"message":   "Bu story artık mevcut değil",
				}
			}
		}

		// Reply mesajı varsa ekle (mevcut kod aynı...)
		if msg.ReplyToMessageID != nil && msg.ReplyToMessageText != nil {
			replyDecryptedText, err := h.encryptionService.DecryptMessage(*msg.ReplyToMessageText)
			if err != nil {
				replyDecryptedText = "Mesaj çözülemedi"
			}

			messageData["reply_to_message"] = map[string]interface{}{
				"id":         *msg.ReplyToMessageID,
				"sender_id":  msg.ReplyToMessageSender,
				"text":       replyDecryptedText,
				"type":       msg.ReplyToMessageType,
				"created_at": msg.ReplyToCreatedAt,
			}
		}

		outgoingMessage := &OutgoingMessage{
			Type: "history_message",
			Data: messageData,
		}

		select {
		case client.Send <- h.messageToBytes(&Message{Type: outgoingMessage.Type, Data: outgoingMessage.Data}):
		default:
			log.Printf("Kullanıcı %d için mesaj geçmişi gönderilemedi", client.UserID)
			return
		}
	}

	completedMessage := &OutgoingMessage{
		Type: "history_loaded",
		Data: map[string]interface{}{
			"message": "Son 30 mesaj yüklendi",
			"count":   len(messages),
		},
	}

	select {
	case client.Send <- h.messageToBytes(&Message{Type: completedMessage.Type, Data: completedMessage.Data}):
	default:
		log.Printf("Kullanıcı %d için tamamlanma bildirimi gönderilemedi", client.UserID)
	}
}

// HandleWebSocket WebSocket bağlantısını handle et
func (h *Hub) HandleWebSocket(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		log.Printf("WebSocket: user_id context'te bulunamadı")
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	log.Printf("WebSocket: Context'ten alınan userID: %v (tip: %T)", userID, userID)

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("WebSocket upgrade hatası: %v", err)
		return
	}

	// ── OKUMA BOYUT LİMİTİ (əvvəl HEÇ YOX İDİ) ──────────────────────────────
	// `ReadBufferSize: 1024` (yuxarıda, upgrader) bir BUFER ölçüsüdür, LİMİT
	// deyil. Limit olmadan `ReadMessage` istənilən ölçüdə bir frame-i bütövlükdə
	// yaddaşa alır — pozuq və ya bədniyyətli bir istemçi prosesi şişirdə bilər.
	// Ən böyük real frame (uzun mətn + reply + payload) onlarla KB-dır; 256 KB
	// geniş marjdır. Limit aşıldıqda gorilla bağlantını 1009 ilə bağlayır.
	conn.SetReadLimit(256 << 10)

	client := &Client{
		UserID: userID.(uint),
		Conn:   conn,
		Send:   make(chan []byte, 256),
		Hub:    h,
		done:   make(chan struct{}),
		typing: newTypingGate(), // Issue 16
		// Yetenek pazarlığı — bax `Client.ProtoVersion` şərhi.
		// `?cv=` YOXDURSA protoLegacy (1) → köhnə istemçi davranışı.
		ProtoVersion: parseProtoVersion(c.Query("cv")),
	}

	h.register <- client

	go client.writePump()
	go client.readPump()
}

// readPump client'tan mesaj oku ve işle
func (c *Client) readPump() {
	defer func() {
		// Müdafiə xətti: readPump içindən (handleIncomingMessage və s.)
		// gözlənilməz panic bütün prosesi çökdürməsin — yalnız bu client düşsün.
		if r := recover(); r != nil {
			log.Printf("readPump panic (user %d): %v", c.UserID, r)
		}
		c.Hub.unregister <- c
	}()

	// Ping/Pong setup
	err := c.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	if err != nil {
		return
	}
	c.Conn.SetPongHandler(func(string) error {
		err := c.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		if err != nil {
			return err
		}
		return nil
	})

	for {
		_, messageBytes, err := c.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket hatası: %v", err)
			}
			break
		}

		var incomingMsg IncomingMessage
		if err := json.Unmarshal(messageBytes, &incomingMsg); err != nil {
			log.Printf("Mesaj parse hatası: %v", err)
			continue
		}

		// Gelen mesajları işle
		c.handleIncomingMessage(&incomingMsg)
	}
}

// handleIncomingMessage gelen mesajları işle
func (c *Client) handleIncomingMessage(msg *IncomingMessage) {
	switch msg.Type {
	case "ping":
		// Ping mesajına pong ile cevap ver
		response := &OutgoingMessage{
			Type: "pong",
			Data: map[string]interface{}{
				"timestamp": time.Now().Unix(),
			},
		}
		c.sendMessage(response)

	// Issue 16: bütün "yazır…" siqnalları eyni qapıdan keçir — blok yoxlaması
	// (30 sn keşli) + başlanğıc siqnalları üçün sürət limiti. Bax
	// websocket/typing_gate.go.
	case "typing":
		// Yazıyor durumunu karşı tarafa bildir (action: text yazma)
		c.emitTypingSignal(msg.ReceiverID, "typing", true)

	case "typing_stop":
		// Yazmayı bıraktı durumunu karşı tarafa bildir
		c.emitTypingSignal(msg.ReceiverID, "typing", false)

	case "recording":
		// Səs mesajı yazır (mic basılı tutub) — qarşı tərəfə bildir.
		// Eyni user_typing event-i, amma action: recording.
		c.emitTypingSignal(msg.ReceiverID, "recording", true)

	case "recording_stop":
		// Səs yazmağı dayandırdı (buraxdı/ləğv etdi).
		c.emitTypingSignal(msg.ReceiverID, "recording", false)

	case "group_typing":
		// Qrupda yazma — həmin qrupun digər üzvlərinə bildir.
		c.Hub.handleGroupTyping(c.UserID, msg.Data, true, c.typing)

	case "group_typing_stop":
		// Qrupda yazmağı dayandırdı.
		c.Hub.handleGroupTyping(c.UserID, msg.Data, false, c.typing)

	case "get_online_users":
		// Online kullanıcı listesini gönder
		onlineUsers := c.Hub.GetConnectedUsers()
		response := &OutgoingMessage{
			Type: "online_users",
			Data: map[string]interface{}{
				"users": onlineUsers,
				"count": len(onlineUsers),
			},
		}
		c.sendMessage(response)
	case "add_reaction":
		dataMap, ok := msg.Data.(map[string]interface{})
		if !ok {
			log.Printf("Reaction data parse edilemedi")
			return
		}

		messageID, ok1 := dataMap["message_id"].(string)
		emoji, ok2 := dataMap["emoji"].(string)
		if !ok1 || !ok2 {
			log.Printf("MessageID veya emoji eksik")
			return
		}

		c.Hub.handleAddReaction(c.UserID, messageID, emoji)

	case "remove_reaction":
		dataMap, ok := msg.Data.(map[string]interface{})
		if !ok {
			return
		}

		messageID, ok1 := dataMap["message_id"].(string)
		if !ok1 {
			return
		}

		c.Hub.handleRemoveReaction(c.UserID, messageID)
	case "send_message":
		dataMap, ok := msg.Data.(map[string]interface{})
		if !ok {
			log.Printf("Mesaj data parse edilemedi")
			return
		}
		receiverIDFloat, ok1 := dataMap["receiver_id"].(float64)
		content, ok2 := dataMap["text"].(string)
		if !ok1 || !ok2 {
			log.Printf("Geçersiz mesaj verisi")
			return
		}
		receiverID := uint(receiverIDFloat)
		var replyToMessageID *string
		var storyID *uint
		var msgType string

		if replyID, exists := dataMap["reply_to_message_id"].(string); exists && replyID != "" {
			replyToMessageID = &replyID
		}

		if storyIDFloat, exists := dataMap["story_id"].(float64); exists && storyIDFloat > 0 {
			storyIDUint := uint(storyIDFloat)
			storyID = &storyIDUint
		}

		if typeStr, exists := dataMap["type"].(string); exists {
			msgType = typeStr
		} else {
			msgType = "text"
		}

		if receiverID == 0 || content == "" {
			return
		}

		// ── İSTEMÇİNİN GÖNDƏRDİYİ `client_message_id` (varsa) ────────────────
		// Aşağıda ID kimi istifadə olunmazdan ƏVVƏL burada tutulur, çünki
		// `message_error` / `message_ack` frame-lərində istemçiyə GERİ
		// qaytarılmalıdır: v2 istemçi ekrandakı hansı optimistik baloncuğun
		// nəticələndiyini yalnız bununla bilir.
		clientMsgID, _ := dataMap["client_message_id"].(string)
		clientMsgID = strings.TrimSpace(clientMsgID)

		// 🚫 SPAM SHADOW-BAN — GLOBAL (yeni VƏ mövcud conversation üçün).
		//
		// Yalnız `actions` sütununa baxılır:
		//   • actions = NULL                  → mesaj BLOKLANIR
		//   • actions-da "message" var        → mesaj BLOKLANIR
		//   • actions = ["post","story"] və s.→ mesaj GEDƏ BİLƏR (mesaja təsir yox)
		// REST handler ilə eyni davranış.
		if models.IsMessagingBannedByActions(c.Hub.db, c.UserID) {
			log.Printf("🚫 SPAM SHADOW-BAN (WS): sender_id=%d → receiver_id=%d mesajı bloklandı (DB yazılmadı, WS yayılmadı)",
				c.UserID, receiverID)
			return
		}

		// 🎯 TEK SEFERDE: Conversation'ı getir + izin kontrolü yap
		//conversationHandler := handlers.NewConversationHandler(c.Hub, c.Hub.encryptionService)
		//conversation, canSend, errorMsg, err := conversationHandler.GetOrCreateConversationWithPermission(c.UserID, receiverID)

		conversation, canSend, errorMsg, err := c.Hub.getOrCreateConversationWithPermission(c.UserID, receiverID)

		if err != nil || !canSend {
			// 🚫 SPAM: spam'lı kullanıcıya hata bile gösterme — sessizce yut.
			// Mesaj DB'ye yazılmaz, karşı tarafa gitmez, gönderene message_error
			// dönülmez (shadow-ban davranışı).
			if errorMsg == spamSilentReason {
				log.Printf("Spam'lı kullanıcının mesajı sessizce engellendi: %d -> %d", c.UserID, receiverID)
				return
			}

			log.Printf("Mesaj gönderilemedi: %d -> %d, error: %v, msg: %s", c.UserID, receiverID, err, errorMsg)
			// `cid`: v2 istemçi hansı optimistik baloncuğun rədd edildiyini
			// bilməlidir. Köhnə istemçi bu ƏLAVƏ sahəni sadəcə görməzdən gəlir.
			c.sendMessage(&OutgoingMessage{
				Type: "message_error",
				Data: map[string]interface{}{
					"error": errorMsg,
					"code":  "SEND_NOT_ALLOWED",
					"cid":   clientMsgID,
				},
			})
			return
		}

		// Issue 9: idempotentlik — istemçi mesajın UUID-ni özü verə bilir
		// (`client_message_id`). WS bağlantısı qopub yenidən qurulanda istemçi
		// çatmamış mesajı TƏKRAR göndərir; server UUID-i ilə bu HƏR DƏFƏ yeni
		// sətir yaradırdı → söhbətdə dublikat. İndi eyni açar → eyni sətir.
		messageID := ""
		if clientMsgID != "" {
			if parsed, perr := uuid.Parse(clientMsgID); perr == nil {
				messageID = parsed.String()
			} else {
				c.sendMessage(&OutgoingMessage{
					Type: "message_error",
					Data: map[string]interface{}{
						"error": "client_message_id UUID formatında olmalıdır",
						"code":  "INVALID_CLIENT_MESSAGE_ID",
						"cid":   clientMsgID,
					},
				})
				return
			}
		}
		if messageID == "" {
			messageID = uuid.New().String()
		}
		// Issue 30: REST yolu ilə eyni — UTC saxla ki, `ORDER BY created_at`
		// iki yol arasında düzgün sıralansın (server TZ ≠ UTC olsa belə).
		createdAt := time.Now().UTC()

		// ── Issue 1: ÖNCE PERSİST, SONRA YAYINLA ────────────────────────────
		// Əvvəllər mesaj DB-yə yazılmadan HandleNewMessage ilə yayılırdı; async
		// yazma səssizcə uğursuz olsa mesaj hər iki ekranda görünüb yenidən
		// açanda YOX olurdu. İndi REST yolu ilə eyni: şifrələ → yaz → (uğurlusa)
		// conversation-u güncəllə → yay. Yazma xətası → göndərənə message_error,
		// yayım yoxdur. Wire dəyişmir (köhnə istemçilər eyni davranır).

		encryptedText, encErr := c.Hub.encryptionService.EncryptMessage(content)
		if encErr != nil {
			log.Printf("Mesaj şifreleme hatası (WS): %v", encErr)
			c.sendMessage(&OutgoingMessage{
				Type: "message_error",
				Data: map[string]interface{}{"error": "message_encrypt_failed", "code": "SEND_FAILED"},
			})
			return
		}

		message := models.Message{
			ID:               messageID,
			SenderID:         c.UserID,
			ReceiverID:       &receiverID,
			StoryID:          storyID,
			ReplyToMessageID: replyToMessageID,
			EncryptedText:    encryptedText,
			Read:             false,
			CreatedAt:        createdAt,
			UpdatedAt:        createdAt,
		}

		// ── Issue 8 + Issue 40: mesaj insert-i və conversation indeks
		// yeniləməsi (sayğac/status/last_message_at) TEK TRANSACTION.
		// Issue 8 WS yoluna yeniləməni gətirmişdi, amma REST-dəki eyni
		// atomiklik boşluğu ilə: xəta udulurdu → mesaj var, siyahı köhnə.
		//
		// Issue 9: `ON CONFLICT (id) DO NOTHING` — təkrar göndərilən eyni
		// `client_message_id` yeni sətir yaratmır və sayğaclar İKİNCİ DƏFƏ
		// artmır. `duplicate` olduqda yayım/moderasiya da təkrarlanmır;
		// göndərənə `message_duplicate` gedir ki, öz outbox-unu təmizləsin.
		duplicate := false
		if err := c.Hub.db.Transaction(func(tx *gorm.DB) error {
			res := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "id"}},
				DoNothing: true,
			}).Create(&message)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				var existing models.Message
				if err := tx.Unscoped().Where("id = ?", message.ID).First(&existing).Error; err != nil {
					return err
				}
				// Yalnız `sender_id` yoxlamaq KİFAYƏT DEYİL — eyni istifadəçi
				// eyni açarı BAŞQA alıcıya (və ya qrupa) işlətsə mesaj səssizcə
				// yaradılmazdı və istemçi uğur sayardı. Alıcı və söhbət növü də
				// uyğun gəlməlidir.
				sameReceiver := existing.ReceiverID != nil && *existing.ReceiverID == receiverID
				if existing.SenderID != c.UserID || !sameReceiver || existing.ConversationID != nil {
					return errClientMessageIDTaken
				}
				duplicate = true
				return nil
			}
			if conversation != nil {
				if err := applyConversationMessageUpdateDB(tx, conversation, c.UserID); err != nil {
					return err
				}
			}
			// Issue 56: `content` hələ AÇIQ mətndir — S3 media açarlarını
			// "istifadə olunub" işarələ (şifrələmədən sonra mümkün deyil).
			// TRANSACTION-IN İÇİNDƏ və insert-dən SONRA: əvvəl transaction
			// başlamazdan qabaq, qlobal handle üzərində edilirdi — yazma və ya
			// conversation yeniləməsi geri qayıtdıqda mesaj yox olurdu, media
			// isə əbədi "istinad olunub" qalıb GC-dən çıxırdı (S3 sızıntısı).
			services.MarkMediaReferenced(tx, content)
			return nil
		}); err != nil {
			if errors.Is(err, errClientMessageIDTaken) {
				c.sendMessage(&OutgoingMessage{
					Type: "message_error",
					Data: map[string]interface{}{
						"error": "client_message_id artıq istifadə olunub",
						"code":  "CLIENT_MESSAGE_ID_TAKEN",
						"cid":   clientMsgID,
					},
				})
				return
			}
			log.Printf("Mesaj DB'ye yazılamadı (WS): %v", err)
			c.sendMessage(&OutgoingMessage{
				Type: "message_error",
				Data: map[string]interface{}{
					"error": "message_persist_failed",
					"code":  "SEND_FAILED",
					"cid":   clientMsgID,
				},
			})
			return
		}

		if duplicate {
			// v2 üçün ƏLAVƏ `message_ack` (duplicate=true). `message_duplicate`
			// AYNEN qalır — köhnə istemçilər onu gözləyir.
			c.sendAckIfV2(messageID, clientMsgID, receiverID, createdAt, true)
			c.sendMessage(&OutgoingMessage{
				Type: "message_duplicate",
				Data: map[string]interface{}{
					"id":          messageID,
					"receiver_id": receiverID,
				},
			})
			return
		}

		// ── `message_ack` — v2 istemçi üçün "sunucu aldı" onayı ──────────────
		//
		// ÖNCE: göndərənin yeganə onayı `HandleNewMessage`-in ona geri
		// göndərdiyi TAM `new_message` echo-su idi (reply obyekti, story
		// obyekti, hər şey daxil). İstemçi ekrandakı optimistik baloncuğu bu
		// echo ilə METN QARŞILAŞDIRARAQ eşleştirməyə çalışırdı
		// (`ChatViewModel.removeFirstTemp` — üç ayrı strategiya: mətn, payload
		// url, voiceUrl) — kövrək və bahalı.
		//
		// İNDİ: v2 istemçi ~80 baytlıq bir onay alır və `cid` ilə DƏQİQ
		// eşleştirir. Echo hələ də göndərilir (köhnə istemçilər üçün lazımdır
		// və v2 istemçi onu id-yə görə onsuz da təkrar sayır) — echo-nun
		// dayandırılması Deploy 3-dədir.
		//
		// Ack YAYIMDAN ÖVVƏL göndərilir: göndərənin "tək tik"i fan-out, push
		// qapısı və `SendUnreadCountUpdate` işini gözləməsin.
		c.sendAckIfV2(messageID, clientMsgID, receiverID, createdAt, false)

		// Yenilənmiş status-u götür (pending→active keçmiş ola bilər) —
		// HandleNewMessage push qapısı bunu istifadə edir.
		conversationStatus := "new"
		if conversation != nil {
			conversationStatus = conversation.Status
		}

		// Artıq commit olunub — indi yay (silent yalnız REST-də var → false).
		c.Hub.HandleNewMessage(c.UserID, receiverID, messageID, content, msgType, createdAt, replyToMessageID, storyID, conversationStatus, false)

		// 🔍 MODERASIYA — qeyri-bloklayıcı, arxa planda qalır.
		if c.Hub.moderationEnqueue != nil && (msgType == "" || msgType == "text") {
			c.Hub.moderationEnqueue(messageID, c.UserID, receiverID, content, createdAt)
		}

	case "mark_read":
		// Mesajları okundu olarak işaretle
		dataMap, ok := msg.Data.(map[string]interface{})
		if !ok {
			log.Printf("mark_read data parse edilemedi")
			return
		}

		otherUserIDFloat, ok := dataMap["other_user_id"].(float64)
		if !ok {
			log.Printf("other_user_id eksik veya geçersiz")
			return
		}

		otherUserID := uint(otherUserIDFloat)
		c.Hub.handleMarkRead(c.UserID, otherUserID)

	case "mark_delivered":
		// Mesajları ÇATDIRILDI (delivered) olaraq işaretle — iki tick.
		// data.sender_id = mesajları GÖNDƏRƏN qarşı tərəf. Köhnə client-lər
		// bu frame-i göndərmir (tam additiv).
		dataMap, ok := msg.Data.(map[string]interface{})
		if !ok {
			log.Printf("mark_delivered data parse edilemedi")
			return
		}

		senderIDFloat, ok := dataMap["sender_id"].(float64)
		if !ok {
			log.Printf("sender_id eksik veya geçersiz")
			return
		}

		c.Hub.handleMarkDelivered(c.UserID, uint(senderIDFloat))

	case "get_unread_count":
		// ✅ YENİ: Client'ın talep ettiği durumda okunmamış sayıyı gönder
		count := c.Hub.GetUnreadCount(c.UserID)
		response := &OutgoingMessage{
			Type: "unread_count",
			Data: map[string]interface{}{
				"count": count,
			},
		}
		c.sendMessage(response)

	case "chat_opened":
		dataMap, ok := msg.Data.(map[string]interface{})
		if !ok {
			return
		}

		otherUserIDFloat, ok := dataMap["other_user_id"].(float64)
		if !ok {
			return
		}

		otherUserID := uint(otherUserIDFloat)
		c.Hub.SetActiveChat(c.UserID, &otherUserID)

	case "chat_closed":
		c.Hub.SetActiveChat(c.UserID, nil)

	// QRUP — DM chat_opened/chat_closed-un qrup ekvivalenti. Flutter
	// GroupChatPage açılanda group_chat_opened, bağlananda group_chat_closed
	// göndərir (websocket_chat_service.dart). Aktiv qrupdaykən həmin qrupun
	// push-u GETMİR.
	case "group_chat_opened":
		dataMap, ok := msg.Data.(map[string]interface{})
		if !ok {
			return
		}
		convIDFloat, ok := dataMap["conversation_id"].(float64)
		if !ok {
			return
		}
		convID := uint(convIDFloat)
		c.Hub.SetActiveGroupChat(c.UserID, &convID)

	case "group_chat_closed":
		c.Hub.SetActiveGroupChat(c.UserID, nil)

	case "screenshot_protection_changed":
		// Screenshot protection değişikliği için hiçbir şey yapmaya gerek yok
		// Bu sadece client'tan gelebilecek bir bildirim olabilir ama
		// normalde bu backend'den gelir, client'a gider
		log.Printf("Screenshot protection değişikliği alındı (bu normalde olmamalı)")

	default:
		log.Printf("Bilinmeyen mesaj tipi: %s", msg.Type)
	}
}

func (h *Hub) SetActiveChat(userID uint, chatWithUserID *uint) {
	var dm, group uint
	found := false

	h.mutex.Lock()
	if client, exists := h.clients[userID]; exists {
		client.ActiveChatWith = chatWithUserID
		found = true
		if chatWithUserID != nil {
			dm = *chatWithUserID
		}
		if client.ActiveGroupChat != nil {
			group = *client.ActiveGroupChat
		}

		if chatWithUserID != nil {
			log.Printf("Kullanıcı %d aktif chat: %d", userID, *chatWithUserID)
		} else {
			log.Printf("Kullanıcı %d chat'ten çıktı", userID)
		}
	}
	h.mutex.Unlock()

	// Issue 4: açıq çat konteksti paylaşılan presence-ə yazılır — başqa
	// instansdakı göndərən `IsUserInChatWith` ilə doğru cavab alsın
	// (əks halda çatda oturan istifadəçiyə push göndərilirdi).
	if found {
		h.writePresence(userID, dm, group)
	}
}

// IsUserInChatWith kontrol fonksiyonu.
// Issue 4: lokal map tapmasa paylaşılan presence-ə düşür.
func (h *Hub) IsUserInChatWith(userID, otherUserID uint) bool {
	h.mutex.RLock()
	client, exists := h.clients[userID]
	var local bool
	if exists {
		local = client.ActiveChatWith != nil && *client.ActiveChatWith == otherUserID
	}
	h.mutex.RUnlock()
	if exists {
		return local
	}
	rec, ok := remotePresence(userID)
	return ok && rec.DM == otherUserID && otherUserID != 0
}

// SetActiveGroupChat — istifadəçinin hazırda açıq olan qrup çatını qeyd edir
// (nil = qrup səhifəsindən çıxdı). SetActiveChat-in qrup ekvivalenti.
func (h *Hub) SetActiveGroupChat(userID uint, conversationID *uint) {
	var dm, group uint
	found := false

	h.mutex.Lock()
	if client, exists := h.clients[userID]; exists {
		client.ActiveGroupChat = conversationID
		found = true
		if conversationID != nil {
			group = *conversationID
		}
		if client.ActiveChatWith != nil {
			dm = *client.ActiveChatWith
		}

		if conversationID != nil {
			log.Printf("Kullanıcı %d aktif grup chat: %d", userID, *conversationID)
		} else {
			log.Printf("Kullanıcı %d grup chat'ten çıktı", userID)
		}
	}
	h.mutex.Unlock()

	// Issue 4: bax SetActiveChat.
	if found {
		h.writePresence(userID, dm, group)
	}
}

// IsUserInGroupChat — istifadəçi hazırda BU qrupun səhifəsindədirmi?
// True isə qrup mesajı push-u GÖNDƏRİLMİR (DM IsUserInChatWith məntiqi).
// Issue 4: lokal map tapmasa paylaşılan presence-ə düşür.
func (h *Hub) IsUserInGroupChat(userID, conversationID uint) bool {
	h.mutex.RLock()
	client, exists := h.clients[userID]
	var local bool
	if exists {
		local = client.ActiveGroupChat != nil && *client.ActiveGroupChat == conversationID
	}
	h.mutex.RUnlock()
	if exists {
		return local
	}
	rec, ok := remotePresence(userID)
	return ok && rec.Group == conversationID && conversationID != 0
}

// emitTypingSignal — Issue 16: "yazır…"/"səs yazır" siqnalını qarşı tərəfə
// ötürür, ancaq qapıdan keçirsə (blok yoxdur + sürət limiti pozulmayıb).
func (c *Client) emitTypingSignal(receiverID uint, action string, isStart bool) {
	if receiverID == 0 {
		return
	}
	if c.typing == nil || !c.typing.allow(c.Hub, c.UserID, receiverID, isStart) {
		return
	}
	c.Hub.SendToUser(receiverID, "user_typing", map[string]interface{}{
		"user_id": c.UserID,
		"typing":  isStart,
		"action":  action,
	})
}

// sendAckIfV2 — v2 istemçiyə minik "sunucu aldı" onayı göndərir.
//
// KÖHNƏ İSTEMÇİ (ProtoVersion < 2) ÜÇÜN NO-OP — heç bir frame yazılmır, yəni
// Flutter və köhnə iOS üçün tel bayt-bayt dəyişmir.
//
// Frame formatı:
//
//	{"type":"message_ack","data":{
//	   "cid":"<istemçinin client_message_id-si; boş ola bilər>",
//	   "id":"<serverdəki mesaj id>",
//	   "receiver_id":123,
//	   "created_at":"2026-08-19T12:34:56Z",
//	   "duplicate":false
//	}}
//
// `cid` ilə `id` bu server-də ADƏTƏN eynidir (istemçinin verdiyi UUID mesajın
// nihai id-si olur — Issue 9). Yenə də hər ikisi göndərilir: istemçi
// `client_message_id` vermədikdə `cid` boş olur və `id` serverin yaratdığı
// UUID-dir; həmçinin bu, gələcəkdə id sxemi dəyişsə (məs. UUIDv7) teli
// qırmadan keçidə imkan verir.
func (c *Client) sendAckIfV2(messageID, clientMsgID string, receiverID uint, createdAt time.Time, duplicate bool) {
	if c.ProtoVersion < protoV2 {
		return
	}
	c.sendMessage(&OutgoingMessage{
		Type: "message_ack",
		Data: map[string]interface{}{
			"cid":         clientMsgID,
			"id":          messageID,
			"receiver_id": receiverID,
			"created_at":  createdAt.UTC().Format(time.RFC3339),
			"duplicate":   duplicate,
		},
	})
}

// sendMessage client'a mesaj gönder
func (c *Client) sendMessage(msg *OutgoingMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("JSON marshal hatası: %v", err)
		return
	}

	select {
	case c.Send <- data:
	default:
		log.Printf("Client %d için mesaj gönderilemedi", c.UserID)
	}
}

// writePump client'a mesaj yaz
func (c *Client) writePump() {
	ticker := time.NewTicker(54 * time.Second)
	defer func() {
		// Issue 3: müdafiə xətti — `readPump` (:969) ilə eyni. Bu goroutine-də
		// gözlənilməz bir panic (nil Conn, gorilla-nın daxili vəziyyəti,
		// bozuq frame) recover edilmədən BÜTÜN prosesi çökdürürdü: bir
		// client-in problemi minlərlə bağlantını qoparırdı. İndi yalnız bu
		// client düşür — soket aşağıda bağlanır, `readPump` bunu görüb
		// `unregister`-ə gedir.
		if r := recover(); r != nil {
			log.Printf("writePump panic (user %d): %v", c.UserID, r)
		}
		ticker.Stop()
		err := c.Conn.Close()
		if err != nil {
			return
		}
	}()

	for {
		select {
		case <-c.done:
			// Client bağlanır: close frame göndər və çıx. `Send` artıq
			// bağlanmadığı üçün dayanma siqnalı buradan gəlir.
			_ = c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			_ = c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
			return

		case message, ok := <-c.Send:
			err := c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err != nil {
				return
			}
			if !ok {
				// `Send` artıq heç vaxt bağlanmır; bu dal müdafiə üçün qalır.
				err := c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				if err != nil {
					return
				}
				return
			}

			if err := c.Conn.WriteMessage(websocket.TextMessage, message); err != nil {
				log.Printf("Mesaj yazma hatası: %v", err)
				return
			}

		case <-ticker.C:
			err := c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err != nil {
				return
			}
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// ScheduleGroupPushNotification — GECİKMƏLİ qrup push-u (anti-spam):
// dərhal göndərmək əvəzinə `delay` gözləyir, göndərmədən ƏVVƏL hər alıcı
// üçün yoxlayır:
//  1. Mesajı artıq OXUYUB? (message_reads)        → push GETMİR
//  2. Qrup səhifəsi hazırda AÇIQDIR? (ActiveGroupChat) → push GETMİR
//
// Qalan alıcılara normal push gedir. Telegram/Slack yanaşması — istifadəçi
// mesajı onsuz da görübsə telefon heç titrəmir.
func (h *Hub) ScheduleGroupPushNotification(
	conversationID, senderID uint,
	groupName, message, messageID string,
	memberIDs []uint,
	delay time.Duration,
) {
	time.AfterFunc(delay, func() {
		// Issue 21: üzv başına message_reads COUNT ƏVƏZİNƏ tək sorğu — bu mesajı
		// artıq oxuyan üzvləri bir dəfəyə çək, yaddaşda diff et. 5000-lik qrupda
		// ~5000 point-query → 1 sorğu (DB connection pool tükənməsi/gecikmə önlənir).
		readers := make(map[uint]bool, len(memberIDs))
		if len(memberIDs) > 0 {
			var readerIDs []uint
			h.db.Table("message_reads").
				Where("message_id = ? AND user_id IN ?", messageID, memberIDs).
				Pluck("user_id", &readerIDs)
			for _, id := range readerIDs {
				readers[id] = true
			}
		}

		// Issue 23: üzv başına `IsUserInGroupChat` (hər biri ayrıca RWMutex
		// al-burax) → 5000-lik qrupda 5000 kilid əməliyyatı. İndi tək kilid.
		inChat := make(map[uint]struct{})
		for _, uid := range h.FilterUsersInGroupChat(memberIDs, conversationID) {
			inChat[uid] = struct{}{}
		}

		remaining := make([]uint, 0, len(memberIDs))
		for _, uid := range memberIDs {
			if uid == senderID {
				continue
			}
			// Gecikmə pəncərəsində OXUDU — push artıq lazımsız.
			if readers[uid] {
				continue
			}
			// Hazırda qrup səhifəsindədir — mesajı canlı görür.
			if _, open := inChat[uid]; open {
				continue
			}
			remaining = append(remaining, uid)
		}

		if len(remaining) == 0 {
			return // hamı oxudu/baxır — push YOX
		}
		h.SendGroupPushNotification(conversationID, senderID, groupName, message, remaining)
	})
}

// SendDismissThreadPush — istifadəçi söhbəti OXUYANDA cihazındakı həmin
// thread bildirişlərini tepsidən silmək üçün Laravel-ə silent dismiss push
// tapşırığı göndərir (`/notification/dismiss-thread`). Qeyri-bloklayıcı.
func (h *Hub) SendDismissThreadPush(userID uint, threadID string) {
	go func() {
		if h.config.CloudToken == "" || h.config.BackendUrl == "" {
			return
		}
		payload := map[string]interface{}{
			"user_id":   userID,
			"thread_id": threadID,
		}
		jsonData, err := json.Marshal(payload)
		if err != nil {
			return
		}
		req, err := http.NewRequest("POST",
			h.config.BackendUrl+"/notification/dismiss-thread",
			bytes.NewBuffer(jsonData))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", h.config.CloudToken)
		resp, err := h.httpClient.Do(req)
		if err != nil {
			return
		}
		resp.Body.Close()
	}()
}

// SendGroupPushNotification qrup mesajı üçün push notification göndərir (async).
// memberIDs — mute olmayan üzvlər (göndərən onsuz da Go tərəfdə çıxarılıb, amma
// Laravel də sender_id-ni təhlükəsizlik üçün siyahıdan çıxarır). Laravel
// `/notification/new-group-message` endpoint-i hamısına FCM göndərir.
func (h *Hub) SendGroupPushNotification(conversationID, senderID uint, groupName, message string, memberIDs []uint) {
	go func() {
		if len(memberIDs) == 0 {
			return
		}
		if h.config.CloudToken == "" {
			log.Printf("❌ CloudToken boş! (qrup push)")
			return
		}
		if h.config.BackendUrl == "" {
			log.Printf("❌ BackendUrl boş! (qrup push)")
			return
		}

		url := h.config.BackendUrl + "/notification/new-group-message"
		payload := map[string]interface{}{
			"receiver_ids":    memberIDs,
			"sender_id":       senderID,
			"conversation_id": conversationID,
			"group_name":      groupName,
			"message":         message,
		}

		jsonData, err := json.Marshal(payload)
		if err != nil {
			log.Printf("❌ Qrup push payload marshal hatası: %v", err)
			return
		}

		req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
		if err != nil {
			log.Printf("❌ Qrup push request oluşturma hatası: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", h.config.CloudToken)

		resp, err := h.httpClient.Do(req)
		if err != nil {
			log.Printf("❌ Qrup push göndərmə hatası: %v", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode == 200 {
			log.Printf("✅ Qrup push göndərildi: conv=%d sender=%d → %d üzv", conversationID, senderID, len(memberIDs))
		} else {
			log.Printf("❌ Qrup push uğursuz, status: %d", resp.StatusCode)
		}
	}()
}

// sendPushNotification push notification göndər (async)
func (h *Hub) sendPushNotification(senderID, receiverID uint, message, msgType string) {
	go func() {
		// Önce conversation'ı bulup mute kontrolü yap
		var conversation models.Conversation
		err := h.db.Where("(user1_id = ? AND user2_id = ?) OR (user1_id = ? AND user2_id = ?)",
			senderID, receiverID, receiverID, senderID).First(&conversation).Error

		if err != nil {
			log.Printf("❌ Conversation bulunamadı, notification gönderilmiyor: %v", err)
			return
		}

		// Receiver söhbəti arxivləyibsə push GÖNDƏRMƏ (arxiv = səssiz,
		// Telegram davranışı). Per-user: yalnız receiver-in öz arxiv bayrağı.
		var isArchivedByReceiver bool
		if conversation.User1ID == receiverID {
			isArchivedByReceiver = conversation.User1Archived
		} else {
			isArchivedByReceiver = conversation.User2Archived
		}
		if isArchivedByReceiver {
			log.Printf("🗄️ Kullanıcı %d konuşmayı arşivlemiş, notification gönderilmiyor", receiverID)
			return
		}

		// Receiver'ın mute durumunu kontrol et
		var isMuted bool
		var mutedUntil *time.Time

		if conversation.User1ID == receiverID {
			isMuted = conversation.User1Muted
			mutedUntil = conversation.User1MutedUntil
		} else {
			isMuted = conversation.User2Muted
			mutedUntil = conversation.User2MutedUntil
		}

		// Mute kontrolü
		if isMuted {
			// Eğer sürekli mute ise (MutedUntil == nil) notification gönderme
			if mutedUntil == nil {
				log.Printf("🔕 Kullanıcı %d sürekli mute, notification gönderilmiyor", receiverID)
				return
			}

			// Eğer mute süresi henüz bitmemişse notification gönderme
			if time.Now().Before(*mutedUntil) {
				log.Printf("🔕 Kullanıcı %d mute (bitiş: %s), notification gönderilmiyor",
					receiverID, mutedUntil.Format("15:04:05"))
				return
			}

			// Mute süresi bitmiş, mute'u kaldır
			if conversation.User1ID == receiverID {
				conversation.User1Muted = false
				conversation.User1MutedUntil = nil
			} else {
				conversation.User2Muted = false
				conversation.User2MutedUntil = nil
			}

			h.db.Save(&conversation)
			log.Printf("🔔 Kullanıcı %d mute süresi bittiği için mute kaldırıldı", receiverID)
		}

		// Mute değilse normal notification gönderme işlemi
		url := h.config.BackendUrl + "/notification/new-message"

		var notificationMessage string
		switch msgType {
		case "image":
			notificationMessage = "Image"
		case "video":
			notificationMessage = "Video"
		case "voice":
			notificationMessage = "Voice"
		default:
			notificationMessage = message
		}

		if h.config.CloudToken == "" {
			log.Printf("❌ CloudToken boş!")
			return
		}
		if h.config.BackendUrl == "" {
			log.Printf("❌ BackendUrl boş!")
			return
		}

		payload := map[string]interface{}{
			"receiver_id": receiverID,
			"sender_id":   senderID,
			"message":     notificationMessage,
		}

		jsonData, err := json.Marshal(payload)
		if err != nil {
			log.Printf("❌ Notification payload marshal hatası: %v", err)
			return
		}

		req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
		if err != nil {
			log.Printf("❌ Notification request oluşturma hatası: %v", err)
			return
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", h.config.CloudToken)

		resp, err := h.httpClient.Do(req)
		if err != nil {
			log.Printf("❌ Push notification gönderme hatası: %v", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode == 200 {
			// UĞUR sətri artıq default-da YAZILMIR (`PUSH_LOG=true` ilə açılır).
			// Push göndərilən hər mesaj üçün bir stderr yazımı idi; xəta yolu
			// (aşağıda) olduğu kimi qalır — problem görünməz olmasın.
			if h.config != nil && h.config.Ops.PushLog {
				log.Printf("✅ Push notification gönderildi: %d -> %d", senderID, receiverID)
			}
		} else {
			log.Printf("❌ Push notification başarısız, status: %d", resp.StatusCode)
		}
	}()
}

// GetUnreadCount kullanıcının okunmamış mesaj sayısını getir
//
// ── İKİ FƏRQLİ SAYĞAC BİRLƏŞDİRİLDİ ─────────────────────────────────────────
//
// ÖNCE: bu sorğuda `users` JOIN-i YOX idi, REST əkizində (`MessageHandler.
// GetUnreadCount`, message_handler.go) VAR idi. Yəni SİLİNMİŞ hesabdan gələn
// oxunmamış mesajlar WS yolunda sayılır, REST yolunda sayılmırdı. İstifadəçi
// eyni anda iki fərqli rozet dəyəri görürdü: WS `unread_count_update` frame-i
// bir rəqəm yazır, növbəti REST yeniləməsi onu DƏYİŞDİRİRDİ — "rozet zıplıyor".
//
// SONRA: hər iki yol EYNİ şərtlərdən keçir (`users.deleted_at IS NULL` daxil).
// Nəticə bəzi istifadəçilərdə AZALA bilər — bu düzgün istiqamətdir, çünki
// silinmiş hesabın mesajı söhbət siyahısında onsuz da görünmür.
//
// Sorğu `Raw` deyil, `Model(...)` ilə yazılıb ki, REST əkizi ilə hərfi-hərfinə
// eyni predikatlar işlənsin (kopyala-yapışdır sürüşməsi olmasın).
func (h *Hub) GetUnreadCount(userID uint) int {
	var count int64

	err := h.db.Model(&models.Message{}).
		Joins("JOIN users ON users.id = messages.sender_id").
		// Issue 59: `conversation_id IS NULL` — yalnız DM. Qrup mesajları ayrı
		// axındır; söhbət siyahısındakı `unread_counts` CTE-si də bu şərti
		// tətbiq edir, ona görə rozet ilə sətirlərin cəmi uyğun gəlsin.
		Where("messages.receiver_id = ? AND messages.read = false AND messages.is_deleted_by_receiver = false AND messages.conversation_id IS NULL AND users.deleted_at IS NULL", userID).
		Count(&count).Error
	if err != nil {
		log.Printf("Okunmamış mesaj sayısı alınamadı: %v", err)
		return 0
	}

	return int(count)
}

// ── OXUNMAMIŞ SAYĞACININ BİRLƏŞDİRİLMƏSİ (coalescing) ───────────────────────
//
// PROBLEM (ölçülmüş)
// `SendUnreadCountUpdate` HƏR mesajda çağırılır (`HandleNewMessage` →
// `go h.SendUnreadCountUpdate(receiverID)`), hər oxunmada bir daha. Hər çağırış
// `messages` üzərində tam bir `COUNT(*)` işlədir. Real indekslərlə (PostgreSQL
// 16, 430k sətir) ölçüldü: 21 min oxunmamışı olan istifadəçi üçün **15 ms**
// (Deploy 1-dən əvvəlki forma) — yəni 10 mesajlıq bir seriya 150 ms DB işi və
// 10 ayrı WS frame-i deməkdir.
//
// Halbuki `unread_count_update` MÜTLƏQ dəyər daşıyır (artım deyil): ardıcıl
// 10 frame-dən yalnız SONUNCUSU mənalıdır, əvvəlkilər onsuz da üzərinə yazılır.
//
// HƏLL — trailing-edge debounce, istifadəçi başına.
// Pəncərə ərzində gələn bütün istəklər BİR sorğuya birləşir; sayğac timer
// işlədiyi anda oxunur, yəni HƏMİŞƏ TƏZƏDİR. İstifadəçi üçün fərq görünmür
// (mesajın özü `new_message` ilə onsuz da dərhal gəlib; bu yalnız rozet
// rəqəmidir), server tərəfdə isə sorğu sayı seriyada 10-dan 1-ə düşür.
//
// Bağlantı anındakı ilk göndərmə İSTİSNADIR — bax `SendUnreadCountUpdateNow`.
const unreadCoalesceWindow = 300 * time.Millisecond

var unreadPending = struct {
	mu sync.Mutex
	m  map[uint]bool
}{m: make(map[uint]bool)}

// SendUnreadCountUpdate — birləşdirilmiş (debounce edilmiş) göndərmə.
// Pəncərə ərzində eyni istifadəçi üçün ikinci çağırış NO-OP-dur; artıq
// planlanmış timer işə düşəndə sayğacı TƏZƏ oxuyub göndərəcək.
func (h *Hub) SendUnreadCountUpdate(userID uint) {
	if userID == 0 {
		return
	}
	unreadPending.mu.Lock()
	if unreadPending.m[userID] {
		unreadPending.mu.Unlock()
		return // artıq planlanıb — o, təzə dəyəri göndərəcək
	}
	unreadPending.m[userID] = true
	unreadPending.mu.Unlock()

	time.AfterFunc(unreadCoalesceWindow, func() {
		unreadPending.mu.Lock()
		delete(unreadPending.m, userID)
		unreadPending.mu.Unlock()
		h.SendUnreadCountUpdateNow(userID)
	})
}

// SendUnreadCountUpdateNow — birləşdirmədən, DƏRHAL göndərir.
// Bağlantı qurulduqda (`registerClient`) istifadə olunur: istemçi ilk rozet
// dəyərini 300 ms gözləməməlidir.
func (h *Hub) SendUnreadCountUpdateNow(userID uint) {
	count := h.GetUnreadCount(userID)

	h.SendToUser(userID, "unread_count_update", map[string]interface{}{
		"count": count,
	})
	// LOG SİLİNDİ: bu funksiya HƏR mesajda çağırılır (`HandleNewMessage` →
	// `go h.SendUnreadCountUpdate(receiverID)`) və hər oxunmada bir daha.
	// Sətrin diaqnostik dəyəri yox idi, qlobal `log` mutex-i + sinxron stderr
	// yazımı isə mesaj başına ödənilirdi.
}

// handleGroupTyping qrupda yazma/dayandırma siqnalını həmin qrupun digər
// aktiv üzvlərinə yayır. data içindən conversation_id çıxarılır (JSON number
// → float64). DM "typing" ilə simmetrikdir, amma tək receiver yerine qrup
// üzvlərinə (göndərən istisna) göndərilir.
func (h *Hub) handleGroupTyping(userID uint, data interface{}, isTyping bool, gate *typingGate) {
	dataMap, ok := data.(map[string]interface{})
	if !ok {
		return
	}
	convFloat, ok := dataMap["conversation_id"].(float64)
	if !ok || convFloat <= 0 {
		return
	}
	conversationID := uint(convFloat)

	// ── Issue 16: SÜRƏT LİMİTİ (yalnız başlanğıc siqnalları) ────────────────
	// Qrup yolunda gücləndirmə əmsalı DM-dən böyükdür: bir frame N üzvə
	// yayılır, üstəlik AŞAĞIDA 2 DB sorğusu var. Limitsiz halda bir soket
	// saniyədə minlərlə frame göndərib bütün qrupu boğa bilərdi.
	if gate != nil && !gate.allowGroupTyping(conversationID, isTyping) {
		return
	}

	// ── Issue 16: GÖNDƏRƏNİN ÜZVLÜYÜ ─────────────────────────────────────────
	// Əvvəl HEÇ BİR yoxlama yox idi: istənilən istifadəçi ixtiyari
	// `conversation_id` göndərib öz adı və avatarı ilə HƏR QRUPDA
	// "X yazır…" göstərə bilirdi — üzv olmadığı, hətta mövcud olmayan
	// qrupda belə. Həm kimlik spoofinq-i, həm də üzv siyahısının sızması
	// (fan-out cavabından qrupun mövcudluğu bilinirdi).
	// Dəvəti qəbul etməmiş (pending) üzv də yaza bilmədiyi üçün siqnal
	// göndərə bilməməlidir — `invite_status='active'` şərti də var.
	var isMember int64
	if err := h.db.Model(&models.ConversationParticipant{}).
		Where("conversation_id = ? AND user_id = ? AND left_at IS NULL AND deleted_at IS NULL AND COALESCE(invite_status,'active') = 'active'",
			conversationID, userID).
		Count(&isMember).Error; err != nil || isMember == 0 {
		return
	}

	// Action: "typing" (default) və ya "recording" (səs yazır). Flutter
	// data-da göndərə bilər; göndərməsə typing sayılır.
	action := "typing"
	if a, ok := dataMap["action"].(string); ok && a == "recording" {
		action = "recording"
	}

	// Yazanın adı/avatarı — çatda inline göstərmək üçün (avatar + "X yazır").
	var sender struct {
		Name         string  `gorm:"column:name"`
		Username     string  `gorm:"column:username"`
		ProfileImage *string `gorm:"column:profile_image"`
	}
	h.db.Raw(`
		SELECT u.name, u.username, p.profile_image
		FROM users u
		LEFT JOIN profiles p ON p.user_id = u.id
		WHERE u.id = ?
	`, userID).Scan(&sender)

	// Qrupun aktiv üzvlərini çək.
	// Issue 7: `invite_status='active'` şərti — dəvəti qəbul etməmiş üzv
	// qrupun canlı fəaliyyətini (kim yazır, adı/avatarı) görməməlidir.
	var memberIDs []uint
	h.db.Model(&models.ConversationParticipant{}).
		Where("conversation_id = ? AND left_at IS NULL AND deleted_at IS NULL AND COALESCE(invite_status,'active') = 'active'", conversationID).
		Pluck("user_id", &memberIDs)

	// Issue 22: göndərəni çıxarıb TƏK toplu fan-out (əvvəl üzv başına ayrıca
	// marshal + kilid + Run döngüsü ilə rendezvous vardı).
	targets := make([]uint, 0, len(memberIDs))
	for _, mid := range memberIDs {
		if mid == userID {
			continue // göndərənə qaytarma
		}
		targets = append(targets, mid)
	}
	if len(targets) == 0 {
		return
	}

	h.SendToMultipleUsers(targets, "group_typing", map[string]interface{}{
		"conversation_id": conversationID,
		"user_id":         userID,
		"typing":          isTyping,
		"action":          action,
		"sender_name":     sender.Name,
		"sender_username": sender.Username,
		"sender_avatar":   utils.PrependBaseURL(sender.ProfileImage),
	})
}

// handleAddReaction mesaja reaction ekle
func (h *Hub) handleAddReaction(userID uint, messageID, emoji string) {
	var message models.Message
	if err := h.db.Where("id = ?", messageID).First(&message).Error; err != nil {
		log.Printf("Mesaj bulunamadı: %v", err)
		return
	}

	// Kullanıcının bu mesaja reaction verebilir mi kontrol et
	if userID != message.SenderID && (message.ReceiverID == nil || userID != *message.ReceiverID) {
		log.Printf("Kullanıcı %d bu mesaja reaction veremez", userID)
		return
	}

	// Issue 16: DM-də bloklanan tərəflər arasında reaksiya (və reaksiya push-u)
	// getməsin (REST block davranışı ilə uyğun). Qrup mesajları (ReceiverID==nil)
	// üzvlük ilə idarə olunur — burada yoxlanmır.
	if message.ReceiverID != nil {
		other := message.SenderID
		if userID == message.SenderID {
			other = *message.ReceiverID
		}
		if models.IsBlocked(h.db, userID, other) {
			return
		}
	}

	// ✅ İdempotensiya: eyni istifadəçi eyni emoji-ni təkrar atırsa, push göndərmə
	isDuplicate := false
	if userID == message.SenderID {
		if message.SenderReaction != nil && *message.SenderReaction == emoji {
			isDuplicate = true
		}
	} else {
		if message.ReceiverReaction != nil && *message.ReceiverReaction == emoji {
			isDuplicate = true
		}
	}

	// Reaction güncelle
	if userID == message.SenderID {
		message.SenderReaction = &emoji
	} else {
		message.ReceiverReaction = &emoji
	}

	now := time.Now().UTC()
	message.UpdatedAt = now

	if err := h.db.Save(&message).Error; err != nil {
		log.Printf("Reaction kaydedilemedi: %v", err)
		return
	}

	// WebSocket ile bildir
	reactionData := map[string]interface{}{
		"message_id": messageID,
		"user_id":    userID,
		"emoji":      emoji,
		"action":     "added",
	}

	h.SendToUser(message.SenderID, "reaction_updated", reactionData)
	if message.ReceiverID != nil {
		h.SendToUser(*message.ReceiverID, "reaction_updated", reactionData)
	}

	// ✅ YENİ: Conversation last_reaction sütunlarını yenilə və conversations siyahısına yay
	if message.ReceiverID != nil {
		h.updateConversationLastReaction(message.SenderID, *message.ReceiverID, userID, emoji, now, "added")

		// Reaksiyanı qarşı tərəfə push notification kimi göndər (mute olmayanlara,
		// yalnız reaksiya əslində dəyişibsə — duplikat-da push atma)
		if !isDuplicate {
			otherUserID := message.SenderID
			if userID == message.SenderID {
				otherUserID = *message.ReceiverID
			}
			if otherUserID != userID {
				if !h.IsUserOnline(otherUserID) || !h.IsUserInChatWith(otherUserID, userID) {
					go h.sendReactionPushNotification(userID, otherUserID, emoji)
				}
			}
		}
	}

	log.Printf("Reaction eklendi: User %d, Message %s, Emoji %s", userID, messageID, emoji)
}

// handleRemoveReaction mesajdan reaction kaldır
func (h *Hub) handleRemoveReaction(userID uint, messageID string) {
	var message models.Message
	if err := h.db.Where("id = ?", messageID).First(&message).Error; err != nil {
		log.Printf("Mesaj bulunamadı: %v", err)
		return
	}

	if userID != message.SenderID && (message.ReceiverID == nil || userID != *message.ReceiverID) {
		return
	}

	// Reaction kaldır
	if userID == message.SenderID {
		message.SenderReaction = nil
	} else {
		message.ReceiverReaction = nil
	}

	now := time.Now().UTC()
	message.UpdatedAt = now

	if err := h.db.Save(&message).Error; err != nil {
		log.Printf("Reaction kaldırılamadı: %v", err)
		return
	}

	// WebSocket ile bildir
	reactionData := map[string]interface{}{
		"message_id": messageID,
		"user_id":    userID,
		"action":     "removed",
	}

	h.SendToUser(message.SenderID, "reaction_updated", reactionData)
	if message.ReceiverID != nil {
		h.SendToUser(*message.ReceiverID, "reaction_updated", reactionData)
	}

	// ✅ YENİ: Reaksiya silindikdə conversations siyahısında son mesaja geri dön
	if message.ReceiverID != nil {
		h.updateConversationLastReaction(message.SenderID, *message.ReceiverID, userID, "", now, "removed")
	}

	log.Printf("Reaction kaldırıldı: User %d, Message %s", userID, messageID)
}

// updateConversationLastReaction conversations cədvəlində son reaksiya sütunlarını yenil
// və hər iki istifadəçinin conversations siyahısına `conversation_update` event göndər.
//
// action: "added" və ya "removed"
func (h *Hub) updateConversationLastReaction(senderID, receiverID, reactorID uint, emoji string, at time.Time, action string) {
	// Conversation-ı tap
	var conversation models.Conversation
	err := h.db.Where(
		"(user1_id = ? AND user2_id = ?) OR (user1_id = ? AND user2_id = ?)",
		senderID, receiverID, receiverID, senderID,
	).First(&conversation).Error
	if err != nil {
		log.Printf("⚠️ Reaction üçün conversation tapılmadı: %v", err)
		return
	}

	updates := map[string]interface{}{}
	if action == "added" {
		updates["last_reaction_emoji"] = emoji
		updates["last_reaction_at"] = at
		updates["last_reaction_by_user_id"] = reactorID
	} else {
		// removed → reaksiya sütunlarını sıfırla
		updates["last_reaction_emoji"] = nil
		updates["last_reaction_at"] = nil
		updates["last_reaction_by_user_id"] = nil
	}

	if err := h.db.Model(&conversation).Updates(updates).Error; err != nil {
		log.Printf("⚠️ conversation last_reaction yenilənə bilmədi: %v", err)
		// Yenilənmə uğursuz olsa da broadcast etməyə davam edək
	}

	// İki tərəf üçün də conversation_update yay
	atStr := ""
	if action == "added" {
		atStr = at.Format(time.RFC3339)
	}

	buildPayload := func(otherUserID uint, isFromMe bool) map[string]interface{} {
		payload := map[string]interface{}{
			"type":                     "conversation_update",
			"event":                    "reaction_" + action, // reaction_added / reaction_removed
			"other_user_id":            otherUserID,
			"last_reaction_emoji":      nilIfEmpty(emoji, action),
			"last_reaction_at":         nilIfEmptyStr(atStr),
			"last_reaction_by_user_id": reactorID,
			"is_reaction_from_me":      isFromMe,
		}
		return payload
	}

	// senderID üçün: qarşı tərəf receiverID
	h.SendToUser(senderID, "conversation_update", buildPayload(receiverID, reactorID == senderID))
	// receiverID üçün: qarşı tərəf senderID
	h.SendToUser(receiverID, "conversation_update", buildPayload(senderID, reactorID == receiverID))
}

// nilIfEmpty action=removed olduqda emoji boşdur, JSON-da null göndərək
func nilIfEmpty(emoji, action string) interface{} {
	if action == "removed" || emoji == "" {
		return nil
	}
	return emoji
}

func nilIfEmptyStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// sendReactionPushNotification reaksiya üçün push notification göndər (async)
func (h *Hub) sendReactionPushNotification(reactorID, receiverID uint, emoji string) {
	// Mute statusunu yoxla — eyni məntiq sendPushNotification-da olduğu kimi
	var conversation models.Conversation
	err := h.db.Where("(user1_id = ? AND user2_id = ?) OR (user1_id = ? AND user2_id = ?)",
		reactorID, receiverID, receiverID, reactorID).First(&conversation).Error
	if err != nil {
		log.Printf("❌ Reaction push: conversation tapılmadı: %v", err)
		return
	}

	var isMuted bool
	var mutedUntil *time.Time
	if conversation.User1ID == receiverID {
		isMuted = conversation.User1Muted
		mutedUntil = conversation.User1MutedUntil
	} else {
		isMuted = conversation.User2Muted
		mutedUntil = conversation.User2MutedUntil
	}

	if isMuted {
		if mutedUntil == nil {
			log.Printf("🔕 Reaction push: User %d sürəkli mute", receiverID)
			return
		}
		if time.Now().Before(*mutedUntil) {
			log.Printf("🔕 Reaction push: User %d mute (bitiş: %s)", receiverID, mutedUntil.Format("15:04:05"))
			return
		}
	}

	if h.config.CloudToken == "" || h.config.BackendUrl == "" {
		log.Printf("❌ Reaction push: CloudToken və ya BackendUrl boşdur")
		return
	}

	// Notification mətni Azərbaycanca
	// Backend FCM servisi adətən "Ali: <message>" formasında title/body düzəldir.
	notificationMessage := "Mesajınıza " + emoji + " ilə reaksiya verdi"

	url := h.config.BackendUrl + "/notification/new-message"
	payload := map[string]interface{}{
		"receiver_id": receiverID,
		"sender_id":   reactorID,
		"message":     notificationMessage,
		"type":        "reaction",
		"emoji":       emoji,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		log.Printf("❌ Reaction notification payload marshal xətası: %v", err)
		return
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		log.Printf("❌ Reaction notification request xətası: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", h.config.CloudToken)

	resp, err := h.httpClient.Do(req)
	if err != nil {
		log.Printf("❌ Reaction push göndərmə xətası: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		log.Printf("✅ Reaction push göndərildi: %d -> %d (%s)", reactorID, receiverID, emoji)
	} else {
		log.Printf("❌ Reaction push uğursuz, status: %d", resp.StatusCode)
	}
}

// handleMarkRead kullanıcının mesajlarını okundu olarak işaretle
func (h *Hub) handleMarkRead(readerID, otherUserID uint) {
	// Bu conversation'daki okunmamış mesajları okundu olarak işaretle.
	// Oxundu = çatdırıldı da (read implies delivered) — ayrıca message_delivered
	// event-i lazım deyil, mövcud message_read göndərəni onsuz da xəbərdar edir.
	result := h.db.Model(&models.Message{}).
		Where("sender_id = ? AND receiver_id = ? AND read = false", otherUserID, readerID).
		Updates(map[string]interface{}{"read": true, "delivered": true})

	if result.Error != nil {
		log.Printf("Mesajları okundu olarak işaretleme hatası: %v", result.Error)
		return
	}

	// Kaç mesaj okundu olarak işaretlendi
	updatedCount := result.RowsAffected

	if updatedCount > 0 {
		// Mesaj gönderende unread count güncelle
		go h.SendUnreadCountUpdate(otherUserID)

		// Mesaj gönderen kişiye bildir (message_read event)
		readData := map[string]interface{}{
			"reader_id":     readerID,
			"other_user_id": otherUserID,
			"read_count":    updatedCount,
		}

		h.SendToUser(otherUserID, "message_read", readData)

		log.Printf("Mesajlar okundu olarak işaretlendi: %d mesaj, reader: %d, sender: %d",
			updatedCount, readerID, otherUserID)
	}
}

// handleMarkDelivered — qarşı tərəfin (senderID) recipientID-yə göndərdiyi, hələ
// çatdırılmamış mesajları delivered=true işarələyir və göndərənə
// `message_delivered` event-i ilə bildirir (iki tick). Oxundu (read) ayrıca
// axındır — bax handleMarkRead. Client `mark_delivered` frame-ini push/offline
// gəlişlərində göndərir; canlı WS çatdırılması üçün bax maybeMarkLivePushDelivered.
func (h *Hub) handleMarkDelivered(recipientID, senderID uint) {
	// Əvvəlcə təsirlənəcək mesaj id-ləri (ən yenidən köhnəyə, maks 500) —
	// event-də göndərmək üçün. 500-dən çox varsa HAMISI update olunur, amma
	// event yalnız son 500 id + "all_before" daşıyır (client bulk tətbiq edir).
	type deliveredRow struct {
		ID        string    `gorm:"column:id"`
		CreatedAt time.Time `gorm:"column:created_at"`
	}
	var rows []deliveredRow
	if err := h.db.Model(&models.Message{}).
		Select("id, created_at").
		Where("sender_id = ? AND receiver_id = ? AND delivered = false", senderID, recipientID).
		Order("created_at DESC").
		Limit(500).
		Scan(&rows).Error; err != nil {
		log.Printf("mark_delivered id seçimi xətası: %v", err)
		return
	}

	if len(rows) == 0 {
		return
	}

	result := h.db.Model(&models.Message{}).
		Where("sender_id = ? AND receiver_id = ? AND delivered = false", senderID, recipientID).
		Update("delivered", true)

	if result.Error != nil {
		log.Printf("Mesajları delivered olarak işaretleme hatası: %v", result.Error)
		return
	}

	if result.RowsAffected == 0 {
		return
	}

	messageIDs := make([]string, 0, len(rows))
	for _, r := range rows {
		messageIDs = append(messageIDs, r.ID)
	}

	deliveredData := map[string]interface{}{
		"other_user_id": recipientID,
		"message_ids":   messageIDs,
	}

	// 500-dən çox təsirləndi → id siyahısı kəsilib. Ən yeni təsirlənən mesajın
	// vaxtı verilir ki, client ondan ƏVVƏLKİ bütün mesajları delivered saysın.
	if result.RowsAffected > int64(len(rows)) {
		deliveredData["all_before"] = rows[0].CreatedAt.UTC().Format(time.RFC3339)
	}

	// Göndərən onlayn deyilsə SendToUser sadəcə heç nə etmir (clients map-də yoxdur).
	h.SendToUser(senderID, "message_delivered", deliveredData)
}

// BroadcastScreenshotProtectionChange screenshot koruma değişikliğini her iki kullanıcıya bildir
func (h *Hub) BroadcastScreenshotProtectionChange(user1ID, user2ID uint, isDisabled bool, changedByUserID uint) {
	screenshotData := map[string]interface{}{
		"is_screenshot_disabled": isDisabled,
		"changed_by":             changedByUserID,
		"changed_at":             time.Now().UTC(),
	}

	// Her iki kullanıcıya da bildir
	h.SendToUser(user1ID, "screenshot_protection_changed", screenshotData)
	h.SendToUser(user2ID, "screenshot_protection_changed", screenshotData)

	log.Printf("Screenshot protection değişikliği yayınlandı: User1: %d, User2: %d, Disabled: %t, ChangedBy: %d",
		user1ID, user2ID, isDisabled, changedByUserID)
}

// spamSilentReason — spam'lı kullanıcının mesaj denemesinde dönülen sentinel
// errorMsg değeri. Çağıran kod bunu görünce kullanıcıya hata göstermeden
// sessizce çıkar (shadow-ban).
const spamSilentReason = "__SPAM_SILENT__"

func (h *Hub) getOrCreateConversationWithPermission(senderID, receiverID uint) (*models.Conversation, bool, string, error) {
	// Issue 16: WS gönderim yolu da REST kimi user block-u tətbiq etsin.
	// Əvvəllər yalnız REST (message_handler) yoxlayırdı → bloklanan istifadəçi
	// WS `send_message` frame ilə mesaj çatdıra bilirdi (block bypass/harassment).
	// REST ilə eyni mesaj qaytarılır (send_message case bunu message_error edir).
	if models.IsBlocked(h.db, senderID, receiverID) {
		return nil, false, "Bu kullanıcıya mesaj gönderemezsiniz", nil
	}

	// Gizli Mod: gizli kullanıcıya (close-friend olmayan) DM engellenir; gizli
	// kullanıcı da yalnız close-friends'e yazabilir. WS de REST kimi tətbiq edir
	// (əks halda WS ilə gizli istifadəçiyə mesaj bypass olurdu). Nonexistent/
	// deactivated kimi davranırıq — hidden durumu sızmasın.
	if models.DMHiddenBlocked(h.db, senderID, receiverID) {
		return nil, false, "İstifadəçi tapılmadı", nil
	}

	var conversation models.Conversation
	err := h.db.Where(
		"(user1_id = ? AND user2_id = ?) OR (user1_id = ? AND user2_id = ?)",
		senderID, receiverID, receiverID, senderID,
	).First(&conversation).Error

	if err != nil {
		// 🚫 SPAM KONTROLÜ: spam_bans'ta kaydı olan (deleted_at IS NULL)
		// kullanıcı YENİ conversation başlatamaz. Sessizce başarısız ol —
		// sentinel reason döndür, conversation oluşturma.
		if models.IsMessagingBanned(h.db, senderID) {
			return nil, false, spamSilentReason, nil
		}

		// 🚫 ACTIONS KONTROLÜ: spam_bans aktiv qeydinin `actions` sütunu
		// mesaj göndərməni qadağan edirsə (NULL, ya da массivində "message"
		// varsa) — YENİ conversation başlada bilməz. Eyni səssiz shadow-ban.
		if models.IsMessagingBannedByActions(h.db, senderID) {
			return nil, false, spamSilentReason, nil
		}

		// Issue 13: user1/user2 həmişə normallaşdırılmış (kiçik id = user1)
		// saxlanmalıdır — REST GetOrCreateConversation ilə eyni. Əks halda
		// WS-lə yaradılan `(sender,receiver)` sətri REST-in sıralanmış
		// axtarışına görünməz və eyni cüt üçün İKİNCİ conversation yaranır.
		u1, u2 := senderID, receiverID
		if u1 > u2 {
			u1, u2 = u2, u1
		}
		newConv := models.Conversation{
			User1ID: u1,
			User2ID: u2,
			Status:  "pending",
		}
		// Issue 13: konflikt sükutla udulsun və qazanan sətir oxunsun
		// (paralel "ilk mesaj" yarışı → iki conversation sətri).
		if err := h.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&newConv).Error; err != nil {
			return nil, false, "conversation oluşturulamadı", err
		}
		if newConv.ID == 0 {
			if err := h.db.Where("user1_id = ? AND user2_id = ?", u1, u2).
				First(&newConv).Error; err != nil {
				return nil, false, "conversation oluşturulamadı", err
			}
		}
		return &newConv, true, "", nil
	}

	if conversation.Status == "blocked" {
		return nil, false, "Bu kullanıcıya mesaj gönderemezsiniz", nil
	}

	// 🚫 ADMIN BLOK — söhbət admin (Filament) tərəfindən bloklanıbsa, BU
	// söhbətdə heç kim yeni mesaj göndərə bilməz (nə user1, nə user2).
	// spamSilentReason qaytarılır → WS handler mesajı SƏSSİZCƏ udar
	// (göndərənə xəta getmir, qarşı tərəfə çatmır). Köhnə mesajlar qalır.
	if conversation.Blocked {
		return nil, false, spamSilentReason, nil
	}

	return &conversation, true, "", nil
}

// applyConversationMessageUpdateDB — handlers.applyConversationMessageUpdate
// ilə eyni məntiq (paketlər arası ixrac etməmək üçün təkrarlanır).
func applyConversationMessageUpdateDB(db *gorm.DB, conversation *models.Conversation, senderID uint) error {
	now := time.Now().UTC()

	// C2 / DM-Q1: UPDATE + SELECT → tək `UPDATE ... RETURNING` (REST əkizi ilə
	// eyni — bax handlers.applyConversationMessageUpdate). Bir DB turu az.
	senderCol := "user2_message_count"
	if senderID == conversation.User1ID {
		senderCol = "user1_message_count"
	}
	updateSQL := fmt.Sprintf(`
        UPDATE conversations
        SET last_message_at = ?,
            total_messages_count = total_messages_count + 1,
            first_message_at = COALESCE(first_message_at, ?),
            %s = %s + 1
        WHERE id = ? AND deleted_at IS NULL
        RETURNING status, user1_message_count, user2_message_count,
                  max_pending_messages, has_previous_conversation
    `, senderCol, senderCol)

	var fresh models.Conversation
	if err := db.Raw(updateSQL, now, now, conversation.ID).Scan(&fresh).Error; err != nil {
		return err
	}
	if fresh.Status == "" {
		// Sətir yoxdur / yenilənmədi — KÖHNƏ DAVRANIŞ: status keçidini növbəti
		// mesaj tətbiq edər.
		return nil
	}
	conversation.User1MessageCount = fresh.User1MessageCount
	conversation.User2MessageCount = fresh.User2MessageCount
	conversation.Status = fresh.Status

	// Issue 8: REST əkizi (handlers.applyConversationMessageUpdate) bu bayrağı
	// hər iki sayğac >0 olan HƏR mesajda qaldırır; burada isə yalnız aşağıdakı
	// `Status != "active"` budağının içində yazılırdı. Nəticə: `active`-ə
	// BAŞQA yolla (updateConversationStatus / REST qəbul axını) keçmiş söhbətdə
	// WS ilə göndərilən mesaj bayrağı HEÇ VAXT qaldırmırdı — eyni söhbət REST
	// və WS-dən fərqli görünürdü. İndi məntiq REST ilə birebir eynidir.
	if fresh.User1MessageCount > 0 && fresh.User2MessageCount > 0 && !conversation.HasPreviousConversation {
		if err := db.Model(&models.Conversation{}).
			Where("id = ? AND has_previous_conversation = ?", conversation.ID, false).
			Update("has_previous_conversation", true).Error; err != nil {
			return err
		}
		conversation.HasPreviousConversation = true
	}

	switch {
	case fresh.User1MessageCount > 0 && fresh.User2MessageCount > 0 && fresh.Status != "active":
		if err := db.Model(&models.Conversation{}).
			Where("id = ? AND status <> ?", conversation.ID, "active").
			Updates(map[string]interface{}{
				"status":                    "active",
				"has_previous_conversation": true,
				"status_changed_at":         now,
			}).Error; err != nil {
			return err
		}
		conversation.Status = "active"

	case fresh.Status == "pending":
		maxCount := fresh.User1MessageCount
		if fresh.User2MessageCount > maxCount {
			maxCount = fresh.User2MessageCount
		}
		if (fresh.User1MessageCount == 0 || fresh.User2MessageCount == 0) &&
			maxCount > fresh.MaxPendingMessages {
			if err := db.Model(&models.Conversation{}).
				Where("id = ? AND status = ?", conversation.ID, "pending").
				Updates(map[string]interface{}{
					"status":             "restricted",
					"status_changed_at":  now,
					"restriction_reason": "Tek taraflı mesaj limiti aşıldı",
				}).Error; err != nil {
				return err
			}
			conversation.Status = "restricted"
		}
	}
	return nil
}
