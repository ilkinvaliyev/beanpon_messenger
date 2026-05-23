package services

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
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
	senderNotified := false
	if result.Category == models.CategoryOffPlatform {
		if q.notifyOffPlatformSender(job.SenderID) {
			senderNotified = true
			actions = append(actions, "notify_sender")
		}
	}

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
}

// notifyOffPlatformSender — off_platform kateqoriyasında MESAJI GÖNDƏRƏN
// adama xəbərdarlıq bildirişi göndərir (qarşı tərəfə YOX).
//
// İki kanaldan istifadə edir:
//  1. Real-time WebSocket eventi (istifadəçi onlayndırsa dərhal görür).
//  2. Laravel backend-in push notification endpoint-i — beanpon-un
//     mövcud /notification/new-message infrastrukturu ilə eyni pattern.
//     Burada göndərən = receiver_id (yəni bildiriş GÖNDƏRƏNƏ gedir).
//
// Qaytarır: ən azı bir kanal uğurlu olubsa true.
func (q *ModerationQueue) notifyOffPlatformSender(senderID uint) bool {
	const warnText = "Söhbətləri başqa platformalara köçürmək Beanpon icma qaydalarına ziddir. " +
		"Təhlükəsizliyiniz üçün yazışmalarınızı tətbiq daxilində saxlayın."

	delivered := false

	// 1) Real-time WebSocket xəbərdarlığı
	if q.notifier != nil {
		q.notifier.SendToUser(senderID, "moderation_warning", map[string]interface{}{
			"category": models.CategoryOffPlatform,
			"title":    "Diqqət",
			"message":  warnText,
			"sent_at":  time.Now().UTC(),
		})
		delivered = true
	}

	// 2) Laravel push notification — beanpon backend-ə.
	//    receiver_id = senderID → bildiriş mesajı YAZAN adama gedir.
	if q.cfg != nil && q.cfg.BackendUrl != "" && q.cfg.CloudToken != "" {
		if q.sendBackendWarning(senderID, warnText) {
			delivered = true
		}
	}

	return delivered
}

// sendBackendWarning — Laravel-in mövcud notification endpoint-inə
// xəbərdarlıq push-u göndərir.
func (q *ModerationQueue) sendBackendWarning(targetUserID uint, text string) bool {
	url := q.cfg.BackendUrl + "/notification/moderation-warning"

	payload := map[string]interface{}{
		"receiver_id": targetUserID, // bildiriş bu istifadəçiyə (göndərənə) gedir
		"type":        "moderation_warning",
		"category":    models.CategoryOffPlatform,
		"message":     text,
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

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
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
