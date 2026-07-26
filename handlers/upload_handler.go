package handlers

// upload_handler.go — Laravel MessageController::uploadVoice + uploadMedia portu.
// Mobil bu iki endpoint-i multipart/form-data ilə çağırır:
//   POST /messenger/upload-voice  (field: "voice", "duration") → {url, duration, filename, size, type, waveform}
//   POST /messenger/upload-media  (field: "media")             → {url, filename, size, type, original_name}
// Səs waveform-u ffmpeg (WAV çevir) + audiowaveform (JSON) ilə çıxarılır; alınmasa
// düz fallback qaytarılır (Laravel ilə birebir).

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"beanpon_messenger/services"
	"beanpon_messenger/utils"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ── Issue 54: yükləmə ölçü limitləri ────────────────────────────────────────
//
// Əvvəllər HEÇ BİR limit yox idi: `readMultipart` client-in bildirdiyi
// `fh.Size` qədər buffer ayırırdı (`make([]byte, fh.Size)`) və faylı bütünlüklə
// RAM-a oxuyurdu. Kimliyi doğrulanmış istənilən istifadəçi çox-yüz-MB-lıq
// (və ya paralel) upload ilə prosesi OOM edə bilirdi.
//
// İndi üç qat müdafiə var:
//  1. `http.MaxBytesReader` — request GÖVDƏSİ hard cap (multipart parse
//     mərhələsində kəsilir, disk/RAM-a heç nə yazılmır).
//  2. `fileHeader.Size` yoxlaması — buffer AYRILMADAN əvvəl rədd.
//  3. `readMultipart` daxilində `io.LimitReader` — Content-Length yalan
//     danışsa belə ayrılan yaddaş sərhədlidir.
//
// Rəqəmlər: Cloudflare edge onsuz da 100 MB-da kəsir → media üçün ondan
// yuxarı qalxmağın mənası yoxdur. Səs mesajları praktikada « 25 MB.
const (
	maxVoiceUploadBytes int64 = 25 << 20  // 25 MB
	maxMediaUploadBytes int64 = 100 << 20 // 100 MB
	// multipartOverheadBytes — form sərhədləri, `duration` kimi digər
	// field-lər üçün kiçik pay. Gövdə limiti = fayl limiti + bu.
	multipartOverheadBytes int64 = 1 << 20 // 1 MB
)

// limitRequestBody — gövdəni hard cap-lə sarır. MÜTLƏQ hər hansı form oxuma
// (`c.PostForm`, `c.FormFile`, `c.Request.ParseMultipartForm`) çağırışından
// ƏVVƏL çağırılmalıdır, çünki parse gövdəni bir dəfə axıdır.
func limitRequestBody(c *gin.Context, maxFileBytes int64) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxFileBytes+multipartOverheadBytes)
}

// isBodyTooLarge — MaxBytesReader-in qaytardığı xətanı tanıyır (Go 1.19+).
func isBodyTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

// tooLargeMB — istifadəçiyə göstərilən limit (MB).
func tooLargeMB(maxBytes int64) int64 { return maxBytes >> 20 }

// ── Issue 55: MƏZMUN TİPİ FAYL ADINDAN GÜVƏNİLMİR ──────────────────────────
//
// Əvvəl həm QƏBUL/RƏDD qərarı, həm S3-ə yazılan `Content-Type` YALNIZ fayl
// adının uzantısından gəlirdi. `voiceExtWhitelist` isə tərif olunub HEÇ
// istifadə edilmirdi (ölü kod) — yəni `upload-voice` istənilən baytı
// istənilən uzantı ilə qəbul edirdi (pulsuz fayl hostinqi).
//
// İndi ilk 512 bayt `http.DetectContentType` ilə "iyləndirilir" və uzantı ilə
// AİLƏ səviyyəsində (image/video/audio) uyğunluğu yoxlanır. Sniff nəticəsi
// qeyri-müəyyəndirsə (`application/octet-stream`) uzantıya güvənilir —
// bəzi konteynerlər (m4a/3gp/webm) sabit imza vermir, yoxsa legitim
// yükləmələri rədd edərdik.
// ── REQRESSİYA DÜZƏLİŞİ: iOS SƏS MESAJLARI RƏDD OLUNURDU ───────────────────
//
// `http.DetectContentType` WHATWG sniff cədvəlini tətbiq edir və ORADA
// ISO-BMFF üçün TƏK bir imza var: `ftyp` qutusunun markaları arasında "mp4"
// keçirsə → **"video/mp4"**. iOS-un `AVAudioRecorder`-ı isə `.m4a` faylını
// `ftyp M4A ` major markası + `mp42`/`isom` uyğun markaları ilə yazır.
//
// Nəticə: HƏR iOS səs mesajı "video" kimi iylənirdi, `UploadVoice` onu
// "Desteklenmeyen ses formatı" (422) ilə rədd edirdi və istifadəçi yalnız
// "medya gönderilemedi" görürdü. YALNIZ səs sınırdı — şəkil/video yolları
// uzantı ilə eyni ailəyə düşdüyü üçün heç nə hiss etdirmirdi.
//
// ISO-BMFF konteyneri səs və video üçün EYNİDİR; ona görə markaya baxmadan
// "video" demək YANLIŞDIR. Aşağıda əvvəlcə marka oxunur:
//   - səs markası (M4A/M4B/M4P/F4A/F4B) → qəti "audio"
//   - başqa hər hansı ISO-BMFF        → QƏTİ DEYİL → "" (uzantıya güvən)
//
// Bu, sniff-in əsl məqsədini (HTML/SVG/şəkil maskalanması) tam saxlayır:
// ISO-BMFF heç vaxt HTML və ya SVG deyil.
func sniffFamily(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	head := data
	if len(head) > 512 {
		head = head[:512]
	}
	// ISO-BMFF (mp4/m4a/mov/3gp) — `http.DetectContentType`-dan ƏVVƏL.
	if iso, ok := isoBMFFFamily(head); ok {
		return iso
	}
	// İŞARƏLƏMƏ YOXLAMASI `http.DetectContentType`-dan ƏVVƏL OLMALIDIR.
	//
	// Go-nun sniff cədvəlində `<SVG` YOXDUR: XML elanı olmayan bir SVG
	// "text/plain; charset=utf-8" kimi görünür, `<?xml`-li olan isə
	// "text/xml" — heç biri "image/svg" prefiksinə düşmür. Yəni aşağıdakı
	// "unsafe" dalı YALNIZ `<html>` ilə başlayanları tuturdu; `.jpg` adı ilə
	// göndərilən bir SVG (içində `<script>`) rahatca keçib S3-ə yazılır və
	// etibarlı domendən inline servis edildikdə SAXLANMIŞ XSS olurdu.
	if isMarkupPayload(head) {
		return "unsafe"
	}
	ct := http.DetectContentType(head)
	switch {
	case strings.HasPrefix(ct, "image/"):
		return "image"
	case strings.HasPrefix(ct, "video/"):
		return "video"
	case strings.HasPrefix(ct, "audio/"):
		return "audio"
	case ct == "application/ogg":
		return "audio"
	case strings.HasPrefix(ct, "text/html"), strings.HasPrefix(ct, "image/svg"),
		strings.HasPrefix(ct, "text/xml"), strings.HasPrefix(ct, "application/xml"):
		// Aşkar TƏHLÜKƏLİ: etibarlı S3 yolu altında inline servis edilsə
		// saxlanmış-XSS potensialı. Heç vaxt media/səs kimi qəbul etmə.
		return "unsafe"
	default:
		return "" // qeyri-müəyyən → uzantıya güvən
	}
}

// markupPrefixes — brauzerin İCRA EDƏ biləcəyi işarələmə başlanğıcları.
// Hamısı kiçik hərflə; müqayisə hərf hassasiyyətsizdir (SVG/svg/Svg).
var markupPrefixes = [][]byte{
	[]byte("<?xml"), []byte("<svg"), []byte("<!doctype"), []byte("<html"),
	[]byte("<head"), []byte("<script"), []byte("<iframe"), []byte("<!--"),
	[]byte("<body"), []byte("<a "), []byte("<math"), []byte("<plist"),
}

// isMarkupPayload — baytların əvvəlindəki boşluqlar atıldıqdan sonra icra
// edilə bilən bir işarələmə ilə başlayırmı. `http.DetectContentType` bunların
// bir hissəsini "text/plain" kimi qaytardığı üçün ayrıca yoxlanılır.
func isMarkupPayload(head []byte) bool {
	trimmed := bytes.TrimLeft(head, " \t\r\n\f\v\x00")
	if len(trimmed) == 0 || trimmed[0] != '<' {
		return false
	}
	lower := bytes.ToLower(trimmed)
	for _, p := range markupPrefixes {
		if bytes.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// isoBMFFAudioBrands — yalnız SƏS daşıyan ISO-BMFF markaları.
// (`M4A `/`M4B `/`M4P ` Apple; `F4A `/`F4B ` Adobe.) Siyahıdakı bütün açarlar
// 4 baytdır — ISO-BMFF markası tərifinə görə sabit uzunluqdadır.
var isoBMFFAudioBrands = map[string]bool{
	"M4A ": true, "M4B ": true, "M4P ": true,
	"F4A ": true, "F4B ": true,
}

// isoBMFFFamily — `ftyp` qutusunu oxuyur.
//
// Qaytarır:
//   - ("audio", true)  → markalar arasında qəti səs markası var
//   - ("", true)       → ISO-BMFF-dir, amma ailə QƏTİ DEYİL (uzantıya güvən)
//   - ("", false)      → ümumiyyətlə ISO-BMFF deyil (adi sniff-ə keç)
func isoBMFFFamily(head []byte) (string, bool) {
	if len(head) < 12 || !bytes.Equal(head[4:8], []byte("ftyp")) {
		return "", false
	}
	boxSize := int(binary.BigEndian.Uint32(head[:4]))
	// Qutu başlığı ən azı 16 bayt (size+ftyp+major+minor). Bozuq/nəhəng
	// dəyərləri əldəki bufferlə məhdudlaşdır — panika olmasın.
	if boxSize < 16 || boxSize > len(head) {
		boxSize = len(head)
	}
	// Major marka head[8:12]; uyğun markalar head[16:] (12:16 minor versiyadır).
	if isoBMFFAudioBrands[string(head[8:12])] {
		return "audio", true
	}
	for st := 16; st+4 <= boxSize; st += 4 {
		if isoBMFFAudioBrands[string(head[st:st+4])] {
			return "audio", true
		}
	}
	return "", true
}

// UploadHandler — voice/media S3 upload + waveform.
type UploadHandler struct {
	s3 *services.S3Uploader
}

func NewUploadHandler(s3 *services.S3Uploader) *UploadHandler {
	return &UploadHandler{s3: s3}
}

// voiceExtWhitelist — Laravel mimes list-inə uyğun uzantılar.
var voiceExtWhitelist = map[string]bool{
	"ogg": true, "oga": true, "opus": true, "mp3": true, "wav": true,
	"aac": true, "m4a": true, "mp4": true, "webm": true, "3gp": true,
	"3gpp": true, "amr": true, "caf": true,
}

// UploadVoice — POST /messenger/upload-voice.
func (h *UploadHandler) UploadVoice(c *gin.Context) {
	uid, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	userID := uid.(uint)

	// Issue 54: gövdə limiti — form parse-dan ƏVVƏL.
	limitRequestBody(c, maxVoiceUploadBytes)

	// DİQQƏT (sıralama): `c.FormFile` BİRİNCİ çağırılmalıdır. Əvvəl
	// `c.PostForm("duration")` vardı; o da multipart parse-ı tetikləyir, amma
	// Gin `initFormCache` parse XƏTASINI UDUR → limit aşımı 413 yerinə
	// mənasız "Geçersiz duration" (422) qaytarırdı.
	fileHeader, err := c.FormFile("voice")
	if err != nil {
		if isBodyTooLarge(err) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{
				"error":    fmt.Sprintf("Ses dosyası çok büyük (en fazla %d MB)", tooLargeMB(maxVoiceUploadBytes)),
				"code":     "FILE_TOO_LARGE",
				"max_size": maxVoiceUploadBytes,
			})
			return
		}
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Ses dosyası bulunamadı"})
		return
	}
	// Issue 54: buffer AYRILMADAN əvvəl ölçü yoxlaması.
	if fileHeader.Size > maxVoiceUploadBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error":    fmt.Sprintf("Ses dosyası çok büyük (en fazla %d MB)", tooLargeMB(maxVoiceUploadBytes)),
			"code":     "FILE_TOO_LARGE",
			"max_size": maxVoiceUploadBytes,
		})
		return
	}

	// duration — required|integer|min:1|max:10000.
	durationStr := c.PostForm("duration")
	duration, err := strconv.Atoi(durationStr)
	if err != nil || duration < 1 || duration > 10000 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Geçersiz duration"})
		return
	}

	// Uzantı — bəzi client-lər filename-siz blob göndərir, MIME-dən təxmin et.
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(fileHeader.Filename), "."))
	if ext == "" {
		ext = "bin"
	}
	// Issue 55: `voiceExtWhitelist` NƏHAYƏT tətbiq olunur (əvvəl ölü kod idi).
	if !voiceExtWhitelist[ext] {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Desteklenmeyen ses formatı"})
		return
	}
	filename := uuid.NewString() + "." + ext

	// Faylın bütün baytlarını oxu.
	data, err := readMultipart(c, "voice", maxVoiceUploadBytes)
	if err != nil || len(data) == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Ses dosyası okunamadı"})
		return
	}

	// Issue 55: baytları iylə — uzantı yalan danışa bilər.
	switch sniffFamily(data) {
	case "unsafe", "image", "video":
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Desteklenmeyen ses formatı"})
		return
	}

	// --- S3-ə orijinal səsi qoy (kritik addım; alınmasa 500).
	s3Key := fmt.Sprintf("voices/user_%d/%s", userID, filename)
	if err := h.s3.Put(s3Key, data, mimeForExt(ext)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Ses dosyası kaydedilemedi"})
		return
	}
	if !h.s3.Exists(s3Key) {
		// Issue 56: yarımçıq yazılmış obyekt qalmasın.
		_ = h.s3.Delete(s3Key)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Ses dosyası kaydedilemedi"})
		return
	}

	// Issue 56: obyekti izləməyə al (mesaj göndərilməzsə 24 saat sonra silinir).
	services.TrackMediaUpload(userID, s3Key)

	// --- pixels-per-second: qısa səslərdə daha çox bar (Laravel ilə eyni).
	pps := 10
	switch {
	case duration <= 3:
		pps = 120
	case duration <= 10:
		pps = 60
	}

	// --- waveform (best-effort). Alınmasa fallback.
	waveform := generateWaveform(data, ext, pps)
	if len(waveform) == 0 {
		bars := duration * 10
		if bars > 80 {
			bars = 80
		}
		if bars < 10 {
			bars = 10
		}
		waveform = make([]int, bars)
		for i := range waveform {
			waveform[i] = 5
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"url":      utils.FilePathS3(s3Key),
		"duration": duration,
		"filename": filename,
		"size":     fileHeader.Size,
		"type":     "voice",
		"waveform": waveform,
	})
}

// imageExts / videoExts — Laravel uploadMedia ayrımı.
var imageExts = map[string]bool{"jpg": true, "jpeg": true, "png": true, "gif": true, "webp": true}
var videoExts = map[string]bool{"mp4": true, "mov": true, "avi": true, "mkv": true, "webm": true}

// UploadMedia — POST /messenger/upload-media.
func (h *UploadHandler) UploadMedia(c *gin.Context) {
	uid, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	userID := uid.(uint)

	// Issue 54: gövdə limiti — form parse-dan ƏVVƏL.
	limitRequestBody(c, maxMediaUploadBytes)

	fileHeader, err := c.FormFile("media")
	if err != nil {
		if isBodyTooLarge(err) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{
				"error":    fmt.Sprintf("Medya dosyası çok büyük (en fazla %d MB)", tooLargeMB(maxMediaUploadBytes)),
				"code":     "FILE_TOO_LARGE",
				"max_size": maxMediaUploadBytes,
			})
			return
		}
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Medya dosyası bulunamadı"})
		return
	}
	// Issue 54: buffer AYRILMADAN əvvəl ölçü yoxlaması.
	if fileHeader.Size > maxMediaUploadBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error":    fmt.Sprintf("Medya dosyası çok büyük (en fazla %d MB)", tooLargeMB(maxMediaUploadBytes)),
			"code":     "FILE_TOO_LARGE",
			"max_size": maxMediaUploadBytes,
		})
		return
	}

	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(fileHeader.Filename), "."))
	isImage := imageExts[ext]
	isVideo := videoExts[ext]
	if !isImage && !isVideo {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Desteklenmeyen dosya formatı"})
		return
	}

	filename := uuid.NewString() + "." + ext

	data, err := readMultipart(c, "media", maxMediaUploadBytes)
	if err != nil || len(data) == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Medya dosyası okunamadı"})
		return
	}

	// Issue 55: iylənmiş ailə elan olunan uzantı ilə uyğun gəlməlidir.
	// (`.jpg` adı ilə göndərilən HTML/SVG burada tutulur.)
	switch fam := sniffFamily(data); fam {
	case "unsafe":
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Desteklenmeyen dosya formatı"})
		return
	case "image":
		if !isImage {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Dosya içeriği uzantıyla uyuşmuyor"})
			return
		}
	case "video":
		if !isVideo {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Dosya içeriği uzantıyla uyuşmuyor"})
			return
		}
	case "audio":
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Dosya içeriği uzantıyla uyuşmuyor"})
		return
	}

	folder := "videos"
	mediaType := "video"
	if isImage {
		folder = "images"
		mediaType = "image"
	}
	// Laravel local `public` disk yerinə S3-ə yazırıq (container-lər arası
	// paylaşılan storage üçün daha etibarlı; URL sxemi s3-storage).
	s3Key := fmt.Sprintf("%s/user_%d/%s", folder, userID, filename)
	if err := h.s3.Put(s3Key, data, mimeForExt(ext)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Medya dosyası kaydedilemedi"})
		return
	}
	// Issue 43: obyektin HƏQİQƏTƏN yazıldığını təsdiqlə (UploadVoice onsuz da
	// belə edir). Bu yoxlama olmadan istemçi mövcud olmayan bir obyekt üçün
	// düzgün görünən URL alırdı → "medya sonsuza qədər yüklənir", səssiz.
	if !h.s3.Exists(s3Key) {
		// Issue 56: yarımçıq yazılmış obyekt qalmasın.
		_ = h.s3.Delete(s3Key)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Medya dosyası kaydedilemedi"})
		return
	}

	// Issue 56: obyekti izləməyə al. Mesaj göndərilməzsə 24 saat sonra
	// sahibsiz sayılıb S3-dən silinəcək (bax services/media_gc.go).
	services.TrackMediaUpload(userID, s3Key)

	c.JSON(http.StatusOK, gin.H{
		"url":           utils.FilePathS3(s3Key),
		"filename":      filename,
		"size":          fileHeader.Size,
		"type":          mediaType,
		"original_name": fileHeader.Filename,
	})
}

// readMultipart — form field-dəki faylın baytlarını oxuyur, `maxBytes` ilə
// sərhədli.
//
// Issue 54: əvvəl `make([]byte, fh.Size)` ilə client-in bildirdiyi ölçü qədər
// yaddaş HEÇ BİR yoxlama olmadan ayrılırdı. İndi:
//   - `fh.Size` limitdən böyükdürsə heç nə ayrılmır, dərhal xəta;
//   - oxuma `io.LimitReader` ilə sərhədlidir (Content-Length yalan danışsa belə);
//   - ilkin buffer tutumu `fh.Size` ilə limitin KİÇİYİ qədərdir.
func readMultipart(c *gin.Context, field string, maxBytes int64) ([]byte, error) {
	fh, err := c.FormFile(field)
	if err != nil {
		return nil, err
	}
	if fh.Size > maxBytes {
		return nil, fmt.Errorf("upload too large: %d > %d", fh.Size, maxBytes)
	}
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()

	capHint := fh.Size
	if capHint < 0 || capHint > maxBytes {
		capHint = maxBytes
	}
	// +1 tutum: normal halda fayl DƏQİQ `capHint` baytdır; əlavə 1 bayt
	// olmasa `readAllInto` son oxumadan sonra EOF-u aşkar etmək üçün massivi
	// ~1.25x böyüdüb HAMISINI kopyalayır (zirvə yaddaş ~2.25N).
	buf := make([]byte, 0, capHint+1)
	// LimitReader maxBytes+1 → limitin AŞILDIĞINI aşkar edə bilirik.
	data, err := readAllInto(buf, io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("upload too large: exceeds %d bytes", maxBytes)
	}
	return data, nil
}

// readAllInto — verilmiş tutumlu buffer üzərində io.ReadAll ekvivalenti
// (əlavə realloc-ları azaldır).
func readAllInto(buf []byte, r io.Reader) ([]byte, error) {
	for {
		if len(buf) == cap(buf) {
			buf = append(buf, 0)[:len(buf)]
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err != nil {
			if errors.Is(err, io.EOF) {
				return buf, nil
			}
			return buf, err
		}
	}
}

// generateWaveform — Laravel uploadVoice waveform məntiqi:
//  1. səsi temp local fayla yaz
//  2. ffmpeg ilə mono/16kHz WAV-a çevir (AAC/MP3 uyumluluğu üçün)
//  3. audiowaveform ilə JSON çıxar
//  4. normalize (5-95) → 80 bar-a downsample
//
// Alınmasa boş slice qaytarır (çağıran fallback tətbiq edir).
func generateWaveform(data []byte, ext string, pps int) []int {
	tmpDir, err := os.MkdirTemp("", "voice-*")
	if err != nil {
		return nil
	}
	defer os.RemoveAll(tmpDir)

	srcPath := filepath.Join(tmpDir, "src."+ext)
	if err := os.WriteFile(srcPath, data, 0o600); err != nil {
		return nil
	}

	// 1) WAV-a çevir (mono, 16kHz).
	wavPath := filepath.Join(tmpDir, "src.wav")
	convert := exec.Command("ffmpeg", "-y", "-i", srcPath, "-ac", "1", "-ar", "16000", wavPath)
	waveSource := srcPath
	if err := convert.Run(); err == nil {
		if _, statErr := os.Stat(wavPath); statErr == nil {
			waveSource = wavPath
		}
	}

	// 2) audiowaveform → JSON.
	jsonPath := filepath.Join(tmpDir, "wave.json")
	aw := exec.Command("audiowaveform",
		"-i", waveSource,
		"-o", jsonPath,
		"--pixels-per-second", strconv.Itoa(pps),
		"--bits", "8",
		"--output-format", "json",
	)
	if err := aw.Run(); err != nil {
		return nil
	}

	raw := readWaveJSON(jsonPath)
	if len(raw) == 0 {
		return nil
	}

	// 3) Normalize (5-95) — Laravel məntiqi.
	normalized := normalizeWave(raw)

	// 4) Downsample → 80 bar.
	return downsample(normalized, 80)
}

// readWaveJSON — audiowaveform json çıxışından `data` massivini oxuyur.
func readWaveJSON(path string) []int {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return nil
	}
	var parsed struct {
		Data []int `json:"data"`
	}
	if json.Unmarshal(b, &parsed) != nil {
		return nil
	}
	return parsed.Data
}

// normalizeWave — Laravel normalize: range çox kiçikdirsə düz 5; əks halda
// mütləq dəyərləri 5-95 aralığına map et.
func normalizeWave(raw []int) []int {
	if len(raw) == 0 {
		return nil
	}
	minV, maxV := raw[0], raw[0]
	for _, v := range raw {
		if v < minV {
			minV = v
		}
		if v > maxV {
			maxV = v
		}
	}
	out := make([]int, len(raw))
	if maxV-minV <= 1 {
		for i := range out {
			out[i] = 5
		}
		return out
	}
	maxAbs := 0
	for _, v := range raw {
		a := v
		if a < 0 {
			a = -a
		}
		if a > maxAbs {
			maxAbs = a
		}
	}
	if maxAbs == 0 {
		for i := range out {
			out[i] = 5
		}
		return out
	}
	for i, v := range raw {
		a := v
		if a < 0 {
			a = -a
		}
		val := 5 + (float64(a)/float64(maxAbs))*90
		iv := int(val + 0.5)
		if iv < 5 {
			iv = 5
		}
		if iv > 95 {
			iv = 95
		}
		out[i] = iv
	}
	return out
}

// downsample — Laravel: targetBars-a görə chunk ortalaması.
func downsample(vals []int, targetBars int) []int {
	if len(vals) == 0 {
		return nil
	}
	chunkSize := len(vals) / targetBars
	if chunkSize < 1 {
		chunkSize = 1
	}
	var out []int
	for i := 0; i < len(vals); i += chunkSize {
		end := i + chunkSize
		if end > len(vals) {
			end = len(vals)
		}
		sum := 0
		for _, v := range vals[i:end] {
			sum += v
		}
		out = append(out, int(float64(sum)/float64(end-i)+0.5))
	}
	return out
}

// mimeForExt — S3 obyekti üçün minimal ContentType.
func mimeForExt(ext string) string {
	switch ext {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "mp4":
		return "video/mp4"
	case "mov":
		return "video/quicktime"
	case "mp3":
		return "audio/mpeg"
	case "m4a", "aac":
		return "audio/mp4"
	case "ogg", "oga", "opus":
		return "audio/ogg"
	case "wav":
		return "audio/wav"
	case "webm":
		return "video/webm"
	default:
		return "application/octet-stream"
	}
}
