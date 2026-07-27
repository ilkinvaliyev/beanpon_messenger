package services

import (
	"context"
	"log"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"gorm.io/gorm"
)

// ── Issue 56 — S3-də sahibsiz (orphaned) obyektlər ──────────────────────────
//
// PROBLEM
// `POST /messenger/upload-media` və `/upload-voice` faylı S3-ə yazıb URL
// qaytarır. Mesajın GÖNDƏRİLMƏSİ isə AYRI bir sorğudur. Aralarında hər şey
// baş verə bilər: istifadəçi fikrini dəyişir, şəkli composer-dən silir,
// tətbiq öldürülür, şəbəkə qopur, göndərmə icazə xətası ilə rədd olunur.
// Bu hallarda obyekt S3-də ƏBƏDİ qalırdı:
//   • heç bir mesaj ona istinad etmir → heç kim onu görmür;
//   • heç bir yerdə qeydi yoxdur → tapmaq da mümkün deyil;
//   • silmək üçün API belə yox idi (`S3Uploader`-də `Delete` metodu yox idi).
// Nəticə: bucket ölçüsü (və hesab) yalnız artır. Böyük 4K videolarda bu
// istifadəçi başına yüz MB-larla ölü data deməkdir.
//
// NİYƏ SADƏ "lifecycle rule" KİFAYƏT ETMİR
// Bucket səviyyəsində "N gündən köhnə obyektləri sil" qaydası İSTİNAD OLUNAN
// media ilə sahibsizi AYIRD EDƏ BİLMİR — hər ikisi eyni prefiksdədir
// (`images/user_7/...`). Belə bir qayda istifadəçinin illər əvvəlki real
// şəkillərini də silərdi.
//
// NİYƏ "mesaj silinəndə obyekti də sil" KİFAYƏT ETMİR
// Mesaj mətni ŞİFRƏLƏNMİŞ saxlanılır (`encrypted_text`), ona görə server
// tərəfdə "bu S3 açarına neçə mesaj istinad edir?" sualını SQL ilə cavablamaq
// mümkün deyil. Üstəlik `BroadcastMessage` EYNİ media URL-i 20 alıcıya ayrı
// mesaj sətri kimi yazır — birini silmək qalan 19-u sındırardı.
//
// HƏLL — İSTİNAD İZLƏMƏSİ
// Yüklənən hər obyekt üçün `chat_media_objects` cədvəlinə sətir yazılır.
// Mesaj göndərilərkən (mətn HƏLƏ AÇIQ olduğu an, şifrələmədən ƏVVƏL) mətndəki
// S3 açarları çıxarılır və həmin sətirlər `referenced_at` ilə işarələnir.
// Arxa-plan təmizləyicisi YALNIZ `referenced_at IS NULL` VƏ yaşı `orphanTTL`-dən
// böyük sətirləri silir. Yəni:
//   • istinad olunan media HEÇ VAXT silinmir (broadcast da təhlükəsizdir);
//   • sahibsiz obyekt 24 saat sonra həm S3-dən, həm cədvəldən gedir.
//
// FAIL-OPEN
// Cədvəl yoxdursa (miqrasiya işlədilməyib) izləmə özünü SÖNDÜRÜR və yükləmə
// axını heç bir şəkildə pozulmur. Bax MIGRATION_chat_media_objects.md.

// mediaKeyPattern — mesaj mətnindəki S3 media istinadları.
// Həm tam URL (`.../api/s3-storage/images/user_7/abc.jpg`), həm də çılpaq
// açar (`images/user_7/abc.jpg`) formasını tutur.
var mediaKeyPattern = regexp.MustCompile(`(?:images|videos|voices)/user_\d+/[A-Za-z0-9][A-Za-z0-9._\-]{0,120}`)

// ChatMediaObject — `chat_media_objects` cədvəli (messenger-ə məxsusdur).
type ChatMediaObject struct {
	ID           uint64     `gorm:"primaryKey"`
	UserID       uint       `gorm:"column:user_id;index"`
	S3Key        string     `gorm:"column:s3_key;uniqueIndex;size:512"`
	ReferencedAt *time.Time `gorm:"column:referenced_at;index"`
	CreatedAt    time.Time  `gorm:"column:created_at;index"`
}

func (ChatMediaObject) TableName() string { return "chat_media_objects" }

// MediaTracker — yüklənən obyektlərin istinad vəziyyətini izləyir və
// sahibsizləri təmizləyir.
type MediaTracker struct {
	db *gorm.DB
	s3 *S3Uploader

	// disabled — cədvəl yoxdursa (və ya davamlı xəta varsa) izləmə söndürülür.
	// Bir dəfə söndükdə prosesin ömrü boyunca sönülü qalır: hər sorğuda
	// mövcud olmayan cədvələ vurmaq mənasız yükdür.
	disabled atomic.Bool

	// orphanTTL — obyekt bu müddətdən artıq istinadsız qalırsa sahibsiz sayılır.
	orphanTTL time.Duration
	// sweepEvery — təmizləmə dövrü.
	sweepEvery time.Duration
	// batchSize — bir dövrdə silinən maksimum obyekt sayı.
	batchSize int
}

// mediaTracker — proses üzrə tək nüsxə. main.go-da qurulur; qurulmayıbsa
// bütün paket funksiyaları no-op-dur (test/CLI yolları üçün).
var mediaTracker atomic.Pointer[MediaTracker]

// NewMediaTracker — izləyicini qurur.
func NewMediaTracker(db *gorm.DB, s3 *S3Uploader) *MediaTracker {
	return &MediaTracker{
		db:         db,
		s3:         s3,
		orphanTTL:  24 * time.Hour,
		sweepEvery: 30 * time.Minute,
		batchSize:  500,
	}
}

// SetMediaTracker — qlobal nüsxəni təyin edir.
func SetMediaTracker(t *MediaTracker) { mediaTracker.Store(t) }

// TrackMediaUpload — yeni yüklənmiş obyekti qeydə alır. Qeyri-kritikdir:
// xəta olduqda yalnız loglanır, yükləmə uğurlu sayılır.
func TrackMediaUpload(userID uint, s3Key string) {
	if t := mediaTracker.Load(); t != nil {
		t.Track(userID, s3Key)
	}
}

// MarkMediaReferenced — mesaj mətnindəki (HƏLƏ ŞİFRƏLƏNMƏMİŞ) S3 açarlarını
// "istifadə olunub" kimi işarələyir. Göndərmə yollarının hamısından çağırılır.
func MarkMediaReferenced(plainText string) {
	if t := mediaTracker.Load(); t != nil {
		t.MarkReferenced(plainText)
	}
}

// Track — bax TrackMediaUpload.
func (t *MediaTracker) Track(userID uint, s3Key string) {
	if t == nil || t.disabled.Load() || s3Key == "" {
		return
	}
	row := ChatMediaObject{
		UserID:    userID,
		S3Key:     s3Key,
		CreatedAt: time.Now().UTC(),
	}
	// Eyni açar iki dəfə yazıla bilməz (uniqueIndex) — təkrar cəhd sükutla
	// atılır. `OnConflict` əvəzinə sadə xəta udma: açar UUID-dir, toqquşma
	// praktikada yoxdur.
	if err := t.db.Create(&row).Error; err != nil {
		t.noteFailure("chat_media_objects insert", err)
	}
}

// ExtractMediaKeys — mətndən S3 media açarlarını çıxarır (təkrarsız).
// İxrac olunub ki, test və digər paketlər eyni məntiqi işlədə bilsin.
func ExtractMediaKeys(text string) []string {
	if text == "" || !strings.Contains(text, "/user_") {
		return nil
	}
	// Tavan qəsdən yüksəkdir: 32-lik limit çoxşəkilli albomların QUYRUĞUNU
	// işarəsiz qoyurdu → həmin şəkillər 24 saat sonra sahibsiz sayılıb
	// silinərdi, halbuki mesaj onlara işarə edir.
	matches := mediaKeyPattern.FindAllString(text, 256)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	return out
}

// MarkReferenced — bax MarkMediaReferenced.
func (t *MediaTracker) MarkReferenced(plainText string) {
	if t == nil || t.disabled.Load() {
		return
	}
	keys := ExtractMediaKeys(plainText)
	if len(keys) == 0 {
		return
	}
	now := time.Now().UTC()
	if err := t.db.Model(&ChatMediaObject{}).
		Where("s3_key IN ? AND referenced_at IS NULL", keys).
		Update("referenced_at", now).Error; err != nil {
		t.noteFailure("chat_media_objects mark referenced", err)
	}
}

// noteFailure — cədvəl yoxdursa izləməni tamamilə söndür; digər xətaları logla.
func (t *MediaTracker) noteFailure(op string, err error) {
	if err == nil {
		return
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "undefined table") ||
		strings.Contains(msg, "no such table") ||
		strings.Contains(msg, "42p01") {
		if t.disabled.CompareAndSwap(false, true) {
			log.Printf("media-gc: `chat_media_objects` cədvəli yoxdur — media izləmə SÖNDÜRÜLDÜ "+
				"(miqrasiya üçün bax MIGRATION_chat_media_objects.md). Detal: %v", err)
		}
		return
	}
	log.Printf("media-gc: %s xətası: %v", op, err)
}

// StartReaper — sahibsiz obyektləri dövri olaraq təmizləyir.
// `go tracker.StartReaper(ctx)` şəklində çağırılır.
func (t *MediaTracker) StartReaper(ctx context.Context) {
	if t == nil {
		return
	}
	if t.s3 == nil || !t.s3.Enabled() {
		log.Printf("media-gc: S3 konfiqurasiya olunmayıb — təmizləyici başladılmadı")
		return
	}

	ticker := time.NewTicker(t.sweepEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if t.disabled.Load() {
				return
			}
			t.sweepOnce()
		}
	}
}

// sweepOnce — bir təmizləmə dövrü.
func (t *MediaTracker) sweepOnce() {
	cutoff := time.Now().UTC().Add(-t.orphanTTL)

	var rows []ChatMediaObject
	if err := t.db.
		Where("referenced_at IS NULL AND created_at < ?", cutoff).
		Order("id ASC").
		Limit(t.batchSize).
		Find(&rows).Error; err != nil {
		t.noteFailure("chat_media_objects sweep select", err)
		return
	}
	if len(rows) == 0 {
		return
	}

	deletedIDs := make([]uint64, 0, len(rows))
	var s3Failures int
	for _, r := range rows {
		if err := t.s3.Delete(r.S3Key); err != nil {
			// S3 silinməsi uğursuz oldu — sətri SAXLA ki, növbəti dövrdə
			// yenidən cəhd olunsun. (Sətri silsək obyekt əbədi sahibsiz qalar.)
			s3Failures++
			continue
		}
		deletedIDs = append(deletedIDs, r.ID)
	}

	if len(deletedIDs) > 0 {
		if err := t.db.Where("id IN ?", deletedIDs).Delete(&ChatMediaObject{}).Error; err != nil {
			t.noteFailure("chat_media_objects sweep delete", err)
			return
		}
	}

	log.Printf("media-gc: %d sahibsiz obyekt silindi (%d S3 xətası, cəmi namizəd %d)",
		len(deletedIDs), s3Failures, len(rows))
}
