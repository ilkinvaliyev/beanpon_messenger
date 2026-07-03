package handlers

// upload_handler.go — Laravel MessageController::uploadVoice + uploadMedia portu.
// Mobil bu iki endpoint-i multipart/form-data ilə çağırır:
//   POST /messenger/upload-voice  (field: "voice", "duration") → {url, duration, filename, size, type, waveform}
//   POST /messenger/upload-media  (field: "media")             → {url, filename, size, type, original_name}
// Səs waveform-u ffmpeg (WAV çevir) + audiowaveform (JSON) ilə çıxarılır; alınmasa
// düz fallback qaytarılır (Laravel ilə birebir).

import (
	"encoding/json"
	"fmt"
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

	// duration — required|integer|min:1|max:10000.
	durationStr := c.PostForm("duration")
	duration, err := strconv.Atoi(durationStr)
	if err != nil || duration < 1 || duration > 10000 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Geçersiz duration"})
		return
	}

	fileHeader, err := c.FormFile("voice")
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Ses dosyası bulunamadı"})
		return
	}

	// Uzantı — bəzi client-lər filename-siz blob göndərir, MIME-dən təxmin et.
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(fileHeader.Filename), "."))
	if ext == "" {
		ext = "bin"
	}
	filename := uuid.NewString() + "." + ext

	// Faylın bütün baytlarını oxu.
	data, err := readMultipart(c, "voice")
	if err != nil || len(data) == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Ses dosyası okunamadı"})
		return
	}

	// --- S3-ə orijinal səsi qoy (kritik addım; alınmasa 500).
	s3Key := fmt.Sprintf("voices/user_%d/%s", userID, filename)
	if err := h.s3.Put(s3Key, data, mimeForExt(ext)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Ses dosyası kaydedilemedi"})
		return
	}
	if !h.s3.Exists(s3Key) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Ses dosyası kaydedilemedi"})
		return
	}

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

	fileHeader, err := c.FormFile("media")
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Medya dosyası bulunamadı"})
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

	data, err := readMultipart(c, "media")
	if err != nil || len(data) == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Medya dosyası okunamadı"})
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

	c.JSON(http.StatusOK, gin.H{
		"url":           utils.FilePathS3(s3Key),
		"filename":      filename,
		"size":          fileHeader.Size,
		"type":          mediaType,
		"original_name": fileHeader.Filename,
	})
}

// readMultipart — form field-dəki faylın bütün baytlarını oxuyur.
func readMultipart(c *gin.Context, field string) ([]byte, error) {
	fh, err := c.FormFile(field)
	if err != nil {
		return nil, err
	}
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, fh.Size)
	total := 0
	for total < len(buf) {
		n, err := f.Read(buf[total:])
		total += n
		if err != nil {
			break
		}
	}
	return buf[:total], nil
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
