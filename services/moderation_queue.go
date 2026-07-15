package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"beanpon_messenger/config"
	"beanpon_messenger/models"

	"gorm.io/gorm"
)

// ─────────────────────────────────────────────────────────────────────────────
// Moderasiya Queue Sistemi
//
// Məqsəd: mesaj göndərmə HEÇ VAXT bloklanmamalıdır. Mesaj şifrələnib DB-yə
// yazılır və dərhal qarşı tərəfə çatdırılır. Analiz ARXA PLANDA baş verir.
//
// Necə işləyir:
//   1. SendMessage handler-i mesajı AÇIQ mətni ilə Enqueue() çağırır.
//   2. Enqueue() job-u buffered channel-a qoyur — bu əməliyyat
//      qeyri-bloklayıcıdır (channel doludursa job atılır, log yazılır).
//   3. Sabit sayda worker goroutine channel-dan job götürür, OpenAI ilə
//      analiz edir, risk varsa message_moderation_logs-a yazır və
//      kateqoriyaya görə əlavə aksiya görür (məs. göndərənə notification).
//
// Bu, xarici broker (Redis/RabbitMQ) tələb etmir — tək-prosesli Go
// servisi üçün in-memory channel + worker pool kifayətdir, ən sürətli
// və ən sadə həlldir. Əgər gələcəkdə horizontal scale lazım olsa,
// Enqueue/worker interfeysi saxlanılıb arxası Redis-ə dəyişdirilə bilər.
// ─────────────────────────────────────────────────────────────────────────────

// ModerationJob — analiz ediləcək bir mesaj.
type ModerationJob struct {
	MessageID  string
	SenderID   uint
	ReceiverID uint
	PlainText  string // ŞİFRƏLƏNMƏMİŞ mətn — yalnız RAM-da, heç yerə yazılmır
	CreatedAt  time.Time
}

// SenderNotifier — göndərənə real-time WebSocket xəbərdarlığı göndərmək üçün
// minimal interfeys. websocket.Hub bu interfeysi ödəyir (SendToUser metodu).
type SenderNotifier interface {
	SendToUser(userID uint, messageType string, data interface{})
}

// ModerationQueue — queue + worker pool.
type ModerationQueue struct {
	jobs     chan ModerationJob
	ai       *ModerationAIService
	db       *gorm.DB
	cfg      *config.Config
	notifier SenderNotifier
	http     *http.Client

	workerCount int
}

const (
	// moderationQueueSize — buffered channel tutumu. Trafik ani sıçrayışda
	// belə mesaj göndərmə bloklanmasın deyə geniş tutulub.
	moderationQueueSize = 2000

	// moderationWorkerCount — paralel worker sayı. OpenAI çağırışları
	// I/O-bound olduğu üçün bir neçə worker kifayətdir.
	moderationWorkerCount = 4
)

// NewModerationQueue — yeni queue. notifier nil ola bilər (o halda
// off_platform notification-ı atlanır).
func NewModerationQueue(
	ai *ModerationAIService,
	db *gorm.DB,
	cfg *config.Config,
	notifier SenderNotifier,
) *ModerationQueue {
	return &ModerationQueue{
		jobs:        make(chan ModerationJob, moderationQueueSize),
		ai:          ai,
		db:          db,
		cfg:         cfg,
		notifier:    notifier,
		http:        &http.Client{Timeout: 10 * time.Second},
		workerCount: moderationWorkerCount,
	}
}

// Start — worker goroutine-lərini işə salır. main.go-da bir dəfə çağırılır.
func (q *ModerationQueue) Start() {
	if !q.ai.Enabled() {
		log.Printf("⚠️ Moderasiya queue: OPENAI_API_KEY yoxdur — analiz deaktivdir")
		return
	}
	for i := 0; i < q.workerCount; i++ {
		go q.worker(i + 1)
	}
	log.Printf("✅ Moderasiya queue başladı (%d worker, buffer %d)", q.workerCount, moderationQueueSize)
}

// Enqueue — bir mesajı analiz növbəsinə qoyur.
//
// QEYRİ-BLOKLAYICIDIR: channel doludursa (analiz API-si yavaşdırsa və ya
// kütləvi trafik olarsa) job sakitcə atılır və xəbərdarlıq loglanır.
// Mesaj göndərmə axını HEÇ VAXT bu səbəbdən gözləmir.
func (q *ModerationQueue) Enqueue(job ModerationJob) {
	if q == nil || !q.ai.Enabled() {
		return
	}
	select {
	case q.jobs <- job:
		// uğurla növbəyə qoyuldu
	default:
		log.Printf("⚠️ Moderasiya queue dolu — mesaj %s analiz edilmədən atıldı", job.MessageID)
	}
}

// worker — channel-dan job götürüb emal edən goroutine.
func (q *ModerationQueue) worker(id int) {
	for job := range q.jobs {
		q.process(job)
	}
}

// process — bir job-u emal edir: AI analizi → log → aksiyalar.
func (q *ModerationQueue) process(job ModerationJob) {
	// panic worker-i öldürməsin
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ Moderasiya worker panic (msg %s): %v", job.MessageID, r)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	result, err := q.ai.Analyze(ctx, job.PlainText)
	if err != nil {
		// fail-open: analiz alınmadı, amma mesajlaşma davam edir
		log.Printf("⚠️ Moderasiya analizi xətası (msg %s): %v", job.MessageID, err)
		return
	}

	// Risk yoxdursa — heç nə etmə (gərəksiz log/notification yaratma).
	if result == nil || !result.Flagged {
		return
	}

	// Confidence eşiyi — AI flaq etsə belə, əminliyi kateqoriyanın minimum
	// həddindən aşağıdırsa nəticəni AT. Bu, "hər yazıda xəbərdarlıq" problemini
	// həll edir: yalnız AI kifayət qədər əmin olduqda log/notification yaranır.
	if !models.MeetsConfidenceThreshold(result.Category, result.Confidence) {
		log.Printf("ℹ️ Moderasiya: msg %s flaq edildi (%s) amma confidence aşağı (%.2f) — atıldı",
			job.MessageID, result.Category, result.Confidence)
		return
	}

	// ── Risk aşkar edildi → log yarat ──────────────────────────────────────
	severity := models.CategorySeverity[result.Category]
	if severity == "" {
		severity = models.SeverityMedium
	}
	// Yüksək əminlik + onsuz da high olan kateqoriya → critical-a qaldır.
	if result.Confidence >= 0.9 && severity == models.SeverityHigh {
		severity = models.SeverityCritical
	}

	actions := []string{"logged"}

	// off_platform → göndərənə xəbərdarlıq notification-ı gedir.
	//
	// Notification YALNIZ yüksək əminlikdə (≥0.90 — açıq köçürmə təklifi)
	// göndərilir. Aşağı əminlikli dolayı maraq ("instada varsan?", "nömrəni
	// ver" kimi) — bu sətirə qədər çatıb table-yə LOG kimi yazılır, amma
	// notification GETMİR. Fərqi models.ShouldNotify təyin edir.
	senderNotified := false
	// NOTIFICATION MÜVƏQQƏTİ SÖNDÜRÜLDÜ — off_platform aşkarlanması yenə də
	// message_moderation_logs table-yə LOG kimi yazılır, amma göndərənə
	// xəbərdarlıq notification-ı GETMİR.
	// if result.Category == models.CategoryOffPlatform {
	// 	if models.ShouldNotify(result.Category, result.Confidence) {
	// 		if q.notifyOffPlatformSender(job.SenderID) {
	// 			senderNotified = true
	// 			actions = append(actions, "notify_sender")
	// 		}
	// 	} else {
	// 		log.Printf("ℹ️ Moderasiya: msg %s off_platform amma əminlik aşağı (%.2f) — table-yə yazıldı, notification göndərilmədi",
	// 			job.MessageID, result.Confidence)
	// 	}
	// }

	// Kritik kateqoriyalar üçün gələcəkdə admin-alert genişləndirilə bilər.
	if severity == models.SeverityCritical {
		actions = append(actions, "admin_alert")
	}

	msgID := job.MessageID
	var receiverID *uint
	if job.ReceiverID != 0 {
		rid := job.ReceiverID
		receiverID = &rid
	}

	actionsJSON, _ := json.Marshal(actions)

	logRow := models.MessageModerationLog{
		MessageID:      &msgID,
		SenderID:       job.SenderID,
		ReceiverID:     receiverID,
		Category:       result.Category,
		Confidence:     result.Confidence,
		Severity:       severity,
		Reason:         result.Reason,
		Snippet:        truncateRunes(job.PlainText, 500),
		ActionsTaken:   string(actionsJSON),
		SenderNotified: senderNotified,
		AIRawResponse:  result.RawResponse,
	}

	if err := q.db.Create(&logRow).Error; err != nil {
		log.Printf("❌ Moderasiya logu yazıla bilmədi (msg %s): %v", job.MessageID, err)
		return
	}

	log.Printf("🚩 Moderasiya: msg %s → %s (severity=%s, confidence=%.2f, sender=%d)",
		job.MessageID, result.Category, severity, result.Confidence, job.SenderID)

	// off_platform aşkarlandıqda — mesajı, kimin göndərdiyini və izahı
	// Telegram qrupuna/çatına ötür. Bu, "platformadan çıxarma" cəhdlərini
	// real-time izləmək üçündür. Tam fail-safe: xəta mesajlaşmaya təsir etmir.
	if result.Category == models.CategoryOffPlatform {
		go q.sendTelegramAlert(job, result)
	}
}

// sendTelegramAlert — off_platform moderasiya hadisəsini Telegram-a göndərir.
//
// Mesajda yer alır: göndərənin adı/username/ID, qəbul edən ID, AI-ın
// confidence/reason dəyəri və mesaj mətninin parçası. Bot token / chat ID
// konfiqurasiya olunmayıbsa sakitcə keçir. Bütün xətalar yalnız loglanır —
// bu funksiya heç vaxt panik etmir və mesajlaşma axınını bloklamır.
func (q *ModerationQueue) sendTelegramAlert(job ModerationJob, result *ModerationResult) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ Telegram alert panic (msg %s): %v", job.MessageID, r)
		}
	}()

	if q.cfg == nil || q.cfg.TelegramBotToken == "" || q.cfg.TelegramChatID == "" {
		return
	}

	// Göndərənin oxunaqlı detallarını DB-dən çək (xəta olarsa ID ilə davam et).
	senderLabel := fmt.Sprintf("ID %d", job.SenderID)
	var sender models.User
	if q.db != nil {
		if err := q.db.Select("id", "name", "username").
			First(&sender, "id = ?", job.SenderID).Error; err == nil {
			senderLabel = fmt.Sprintf("%s (@%s, ID %d)", sender.Name, sender.Username, sender.ID)
		}
	}

	// Qəbul edənin (receiver) username-ini də DB-dən çək — göndərən kimi.
	// Xəta olsa / tapılmasa yalnız ID ilə davam edir.
	receiverLabel := fmt.Sprintf("%d", job.ReceiverID)
	var receiver models.User
	if q.db != nil {
		if err := q.db.Select("id", "name", "username").
			First(&receiver, "id = ?", job.ReceiverID).Error; err == nil {
			receiverLabel = fmt.Sprintf("%d (@%s)", receiver.ID, receiver.Username)
		}
	}

	text := fmt.Sprintf(
		"🚩 Platformadan çıxarma cəhdi\n\n"+
			"👤 Göndərən: %s\n"+
			"📩 Qəbul edən ID: %s\n"+
			"🎯 Əminlik: %.2f\n"+
			"📝 Səbəb: %s\n"+
			"🆔 Mesaj: %s\n\n"+
			"💬 Mesaj mətni:\n%s",
		senderLabel,
		receiverLabel,
		result.Confidence,
		emptyDash(result.Reason),
		job.MessageID,
		emptyDash(truncateRunes(job.PlainText, 1000)),
	)

	payload := map[string]interface{}{
		"chat_id": q.cfg.TelegramChatID,
		"text":    text,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("❌ Telegram alert payload marshal xətası: %v", err)
		return
	}

	url := "https://api.telegram.org/bot" + q.cfg.TelegramBotToken + "/sendMessage"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBuffer(body))
	if err != nil {
		log.Printf("❌ Telegram alert request xətası: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := q.http.Do(req)
	if err != nil {
		log.Printf("❌ Telegram alert göndərmə xətası: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		log.Printf("✅ Telegram alert göndərildi (off_platform, sender=%d, msg=%s)",
			job.SenderID, job.MessageID)
		return
	}
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	log.Printf("⚠️ Telegram alert statusu: %d — %s", resp.StatusCode, string(rb))
}

// emptyDash — boş mətni "—" ilə əvəz edir (Telegram mesajı boş sahə göstərməsin).
func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// Off-platform xəbərdarlıq mətni (push notification).
//
// FcmController-in `/notification/send` endpoint-i düz `title`/`body`
// gözləyir (key əsaslı çoxdilli render-i dəstəkləmir), ona görə mətn
// burada hazır verilir. data sahəsində `category` də göndərilir ki,
// app istəsə öz tərəfində lokallaşdıra bilsin.
const (
	offPlatformPushTitle = "Xəbərdarlıq"
	offPlatformPushBody  = "Başqa platformaya keçmək təklifi qadağandır."
)

// notifyOffPlatformSender — off_platform kateqoriyasında MESAJI GÖNDƏRƏN
// adama xəbərdarlıq bildirişi göndərir (qarşı tərəfə YOX).
//
// İki kanaldan istifadə edir:
//  1. Real-time WebSocket eventi (istifadəçi onlayndırsa dərhal görür).
//  2. Laravel backend-in push notification endpoint-i (FcmController).
//
// Qaytarır: ən azı bir kanal uğurlu olubsa true.
func (q *ModerationQueue) notifyOffPlatformSender(senderID uint) bool {
	delivered := false

	// 1) Real-time WebSocket xəbərdarlığı.
	//    Messenger tərəfində dil tərcümə infrastrukturu yoxdur, ona görə
	//    WS eventi sadə Azərbaycanca mətn daşıyır; client istəsə öz
	//    tərəfində lokallaşdıra bilər (category sahəsi ilə).
	if q.notifier != nil {
		q.notifier.SendToUser(senderID, "moderation_warning", map[string]interface{}{
			"category": models.CategoryOffPlatform,
			"title":    "Xəbərdarlıq",
			"message":  "Başqa platformaya keçmək təklifi qadağandır.",
			"sent_at":  time.Now().UTC(),
		})
		delivered = true
	}

	// 2) Laravel push notification (FcmController /notification/send).
	if q.cfg != nil && q.cfg.BackendUrl != "" && q.cfg.CloudToken != "" {
		if q.sendBackendWarning(senderID) {
			delivered = true
		}
	}

	return delivered
}

// sendBackendWarning — Laravel-in FcmController push endpoint-inə
// off-platform moderasiya xəbərdarlığı göndərir.
//
// Endpoint:  BackendUrl + "/notification/send"
// Header:    x-api-key: CloudToken   (token.verify middleware)
// Payload:   {user_id, title, body, data}  — FcmController::sendNotification
//
//	formatı (title/body məcburidir, düz mətn).
//
// QEYD endpoint yolu haqqında: Laravel-də həm `new-message`, həm `send`
// route-u `cloud/cloud.php` faylındadır və o fayl `prefix('api')` +
// `prefix('cloud')` + `prefix('notification')` ilə qoşulur. websocket/
// hub.go-dakı işləyən `sendPushNotification` `BackendUrl +
// "/notification/new-message"` yazır — yəni `BackendUrl` artıq `/api/cloud`
// hissəsini ehtiva edir. Burada da eyni baza ilə `/notification/send`.
//
// Bildiriş mesajı YAZAN adama getməlidir: user_id = targetUserID.
func (q *ModerationQueue) sendBackendWarning(targetUserID uint) bool {
	url := q.cfg.BackendUrl + "/notification/send"

	payload := map[string]interface{}{
		"user_id": targetUserID, // bildiriş off-platform cəhd edən şəxsə gedir
		"title":   offPlatformPushTitle,
		"body":    offPlatformPushBody,
		"data": map[string]interface{}{
			"type":     "moderation_warning",
			"category": models.CategoryOffPlatform,
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("❌ Moderasiya xəbərdarlıq payload marshal xətası: %v", err)
		return false
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBuffer(body))
	if err != nil {
		log.Printf("❌ Moderasiya xəbərdarlıq request xətası: %v", err)
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", q.cfg.CloudToken)

	resp, err := q.http.Do(req)
	if err != nil {
		log.Printf("❌ Moderasiya xəbərdarlıq göndərmə xətası: %v", err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		log.Printf("✅ Moderasiya xəbərdarlığı göndərənə çatdırıldı (user %d)", targetUserID)
		return true
	}
	log.Printf("⚠️ Moderasiya xəbərdarlıq backend statusu: %d", resp.StatusCode)
	return false
}

// truncateRunes — mətni n rune-a qədər kəsir (UTF-8 təhlükəsiz).
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
