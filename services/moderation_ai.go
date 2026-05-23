package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"beanpon_messenger/models"
)

// ─────────────────────────────────────────────────────────────────────────────
// AI Moderasiya Servisi
//
// Mesajları OpenAI gpt-4o modeli ilə analiz edir. Heç bir üçüncü tərəf
// SDK istifadə olunmur — standart net/http ilə Chat Completions API çağırılır
// (vendor qovluğuna yeni asılılıq əlavə etmədən).
//
// Model seçimi: gpt-4o
//   • Niyanslı təsnifat — "burda yaz" (flaq deyil) vs "Instagramda yaz"
//     (off_platform) kimi incə fərqləri dəqiq tutur.
//   • JSON mode (response_format) dəstəyi — strukturlu, etibarlı cavab.
//   • gpt-4o-mini-dən bahadır, amma moderasiya qərarlarında daha dəqiqdir.
//
// Bu servis YALNIZ queue worker tərəfindən çağırılır — istifadəçi sorğusunu
// HEÇ VAXT bloklamır.
// ─────────────────────────────────────────────────────────────────────────────

const (
	openAIChatURL = "https://api.openai.com/v1/chat/completions"
	// Model: gpt-4o — niyanslı moderasiya qərarlarını (məs. "burda yaz" vs
	// "Instagramda yaz" fərqi) gpt-4o-mini-dən daha dəqiq tutur.
	openAIModel = "gpt-4o"
)

// ModerationResult — AI analizinin nəticəsi.
//
// Əgər mesaj təmizdirsə Flagged=false olur və qalan sahələr boş qalır —
// bu halda heç bir log yazılmır, heç bir notification getmir.
type ModerationResult struct {
	Flagged    bool    `json:"flagged"`    // true → ən azı bir risk kateqoriyası tapıldı
	Category   string  `json:"category"`   // models.Category* sabitlərindən biri
	Confidence float64 `json:"confidence"` // 0.0 - 1.0
	Reason     string  `json:"reason"`     // qısa izah (niyə bu kateqoriya)

	// RawResponse — AI-dan gələn xam JSON (audit/debug üçün saxlanır).
	RawResponse string `json:"-"`
}

// ModerationAIService — OpenAI ilə danışan servis.
type ModerationAIService struct {
	apiKey     string
	model      string
	httpClient *http.Client
}

// NewModerationAIService — yeni servis. apiKey boşdursa servis "disabled"
// rejimində işləyir (Analyze həmişə Flagged=false qaytarır) — beləliklə
// key konfiqurasiya olunmayıbsa tətbiq sınmır.
func NewModerationAIService(apiKey string) *ModerationAIService {
	return &ModerationAIService{
		apiKey: strings.TrimSpace(apiKey),
		model:  openAIModel,
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
}

// Enabled — servis konfiqurasiya olunubmu?
func (s *ModerationAIService) Enabled() bool {
	return s.apiKey != ""
}

// ─────────────────────────────────────────────────────────────────────────────
// SİSTEM PROMPT-u
//
// Bu prompt moderasiya keyfiyyətinin ən kritik hissəsidir. Məqsəd:
//   - GƏRƏKSİZ xəbərdarlıq YOX — adi söhbət, zarafat, emosional ifadə,
//     söyüş, mübahisə təkbaşına flaq DEYİL.
//   - Yalnız AÇIQ, ciddi risk siqnalı olduqda flaq.
//   - Çoxdilli (Azərbaycan, türk, rus, ingilis) və jarqon/sleng anlayışı.
//   - Strukturlu JSON cavab.
//
// ─────────────────────────────────────────────────────────────────────────────
const moderationSystemPrompt = `Sən bir mesajlaşma tətbiqi üçün məzmun moderasiya mütəxəssisisən. İki istifadəçi arasındakı şəxsi mesajları analiz edirsən. Sənin işin yalnız CİDDİ və AÇIQ risk siqnallarını aşkar etməkdir.

ÇOX VACİB PRİNSİP: Gərəksiz xəbərdarlıq vermə. Aşağıdakılar TƏK BAŞINA risk DEYİL və flaq edilməməlidir:
- Adi söyüş, kobud danışıq, emosional ifadə
- Zarafat, kinayə, sarkazm, dostlar arası "sənə öldürərəm" tipli şişirtmə
- Mübahisə, narazılıq, tənqid
- Romantik və ya şəxsi söhbət
- Adi alqı-satqı (qanuni mallar: telefon, paltar, mebel və s.)
- Siyasi fikir bildirmə və ya hökuməti tənqid etmə (bu, dövlət əleyhinə fəaliyyət DEYİL)
- Səhhət, kədər, gündəlik problemlərdən danışma

YALNIZ aşağıdakı kateqoriyalardan biri AÇIQ şəkildə mövcuddursa flaq et:

1. "threat" — Göndərən konkret olaraq qarşı tərəfə (və ya başqasına) fiziki zərər, zorakılıq və ya cinayət hədəsi verir. Niyyət ciddi və real görünməlidir. Dostlar arası şişirtmə deyil.

2. "illegal_goods" — Qanunsuz malların alqı-satqısı və ya təklifi: narkotik maddələr, silah, partlayıcı, saxta sənəd/pul, oğurlanmış mal. Söhbət konkret ticarət/təklif/sifariş ətrafında olmalıdır.

3. "anti_state" — Dövlət əleyhinə zorakı fəaliyyət təşkili, terror aktına çağırış və ya təbliğat, zorakı çevriliş planlaması. Sadəcə hökuməti tənqid etmək VƏ YA siyasi narazılıq bu kateqoriya DEYİL.

4. "off_platform" — Göndərən söhbəti/əlaqəni bu tətbiqdən KƏNARA, BAŞQA bir platformaya və ya birbaşa əlaqə vasitəsinə yönəltməyə çalışır. Bu kateqoriya istənilən dildə (Azərbaycan, türk, rus, ingilis, alman, ispan, özbək və s.) işləyir — dildən asılı olmayaraq mənanı tut.

   Xarici platformalar (tam ad, qısaltma və jarqon — hamısını tanı):
     - Instagram → "ig", "insta", "inst", "instaqram", "инста"
     - TikTok → "tt", "tik tok", "tikok", "тикток"
     - Telegram → "tg", "tele", "телега", "телеграм"
     - WhatsApp → "wp", "wapp", "vatsap", "votsap", "whatsap", "ватсап"
     - Facebook → "fb", "фб"
     - Snapchat → "snap", "снэп"
     - Discord, Signal, Viber, Messenger və s.
   Əlaqə vasitələri: telefon nömrəsi, e-mail, başqa platformada istifadəçi adı (@username), profil linki.
   Səhv yazılış / əyləncəli formalar da (məs. "telegrama keçək", "tt-da", "ınsta") eyni qaydada tutulmalıdır.

   ⭐ ƏMİNLİK SƏVİYYƏSİ — bu kateqoriyada confidence dəyəri ÇOX VACİBDİR. İki halı fərqləndir:

   A) YÜKSƏK ƏMİNLİK (confidence 0.90 və ya yuxarı) — AÇIQ köçürmə TƏKLİFİ/ÇAĞIRIŞI:
      Göndərən birbaşa "gəl ora keçək / orada yazaq / oraya yaz / nömrəni ver" deyir.
      Nümunələr (hər dildə): "instada danışaq?", "Telegrama keçək", "WhatsApp-a yaz",
      "ig-dən yazışaq", "nömrəni ver", "numaranı ver", "напиши в телеграм",
      "let's talk on Instagram", "add me on Snapchat", "schreib mir auf WhatsApp".
      Bunlar AÇIQ cəhddir → confidence 0.90+.

   B) AŞAĞI ƏMİNLİK (confidence 0.55 - 0.75) — DOLAYI / ZƏİF siqnal:
      Göndərən birbaşa "keçək" demir, amma xarici platformaya / əlaqə məlumatına
      MARAQ göstərir, yoxlayır, sual verir. Köçürmə niyyəti var ola bilər, amma açıq deyil.
      Nümunələr (hər dildə): "instada varsan?", "instan var?", "instada adın nədir?",
      "Telegramın var?", "do you have Instagram?", "what's your insta?",
      "есть инстаграм?", "instagramın var mı?".
      Bunlar köçürmə MARAĞIDIR amma açıq təklif deyil → confidence 0.55-0.75.

   Hər iki halda da flagged=true və category="off_platform" qaytar — fərq YALNIZ
   confidence rəqəmindədir. Confidence-i düzgün təyin et: açıq təklif = yüksək,
   dolayı maraq = aşağı.

   ÇOX VACİB — bunlar flaq DEYİL (flagged=false):
   • Konkret xarici platforma adı OLMAYAN ümumi ifadələr: "burda yaz", "ora yaz",
     "başqa yerdə danışaq", "sonra yazaram" — heç bir xarici platforma göstərmir. FLAQ ETMƏ.
   • Xarici platforma adının sadəcə söhbət mövzusu kimi çəkilməsi: "TikTok-da gülməli
     video gördüm", "ig-də onun şəklini gördüm". FLAQ ETMƏ.

5. "harassment" — Davamlı təcavüz, sistematik təhqir, alçaltma, qorxutma, stalking davranışı. Tək bir kobud söz deyil — təcavüzkar nümunə.

6. "scam" — Dələduzluq: saxta qazanc/mükafat vədi, fişinq, saxta investisiya, pul köçürmə fırıldağı, hesab oğurlamaq cəhdi.

7. "csae" — Uşaqların cinsi istismarı və ya təhlükəsizliyi ilə bağlı hər hansı məzmun. Ən yüksək prioritet — şübhə varsa belə flaq et.

8. "self_harm" — Göndərən özünə zərər vurmaq və ya intihar niyyəti ifadə edir.

Mesajı analiz et və YALNIZ aşağıdakı JSON formatında cavab ver:
{
  "flagged": true və ya false,
  "category": yuxarıdakı 8 dəyərdən biri (flagged false isə boş string ""),
  "confidence": 0.0 ilə 1.0 arası ədəd (nə qədər əminsən),
  "reason": qısa izah, 1 cümlə (flagged false isə boş string "")
}

Əgər mesajda yuxarıdakı kateqoriyalardan HEÇ BİRİ açıq şəkildə yoxdursa, mütləq {"flagged": false, "category": "", "confidence": 0, "reason": ""} qaytar. Şübhə kiçikdirsə və ya mesaj qeyri-müəyyəndirsə, flaq ETMƏ. Yalnız açıq və əmin olduğun hallarda flaq et.`

// chatRequest — OpenAI Chat Completions request strukturu.
type chatRequest struct {
	Model          string              `json:"model"`
	Messages       []chatMessage       `json:"messages"`
	Temperature    float64             `json:"temperature"`
	MaxTokens      int                 `json:"max_tokens"`
	ResponseFormat *chatResponseFormat `json:"response_format,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponseFormat struct {
	Type string `json:"type"` // "json_object"
}

// chatResponse — OpenAI cavab strukturu (yalnız lazım olan sahələr).
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// aiVerdict — modelin qaytardığı JSON-un daxili strukturu.
type aiVerdict struct {
	Flagged    bool    `json:"flagged"`
	Category   string  `json:"category"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

// Analyze — bir mesaj mətnini analiz edir.
//
// Bu metod queue worker tərəfindən çağırılır. Səhv baş verərsə (şəbəkə,
// API limiti və s.) Flagged=false qaytarır — moderasiya "fail-open"-dur,
// yəni AI əlçatmaz olduqda mesajlaşma dayanmır.
func (s *ModerationAIService) Analyze(ctx context.Context, text string) (*ModerationResult, error) {
	// Servis konfiqurasiya olunmayıb — sakitcə keç.
	if !s.Enabled() {
		return &ModerationResult{Flagged: false}, nil
	}

	// Çox qısa mesajlar (məs. "ok", "salam", emoji) demək olar ki, heç vaxt
	// risk daşımır — API çağırışına dəyməz, pul və gecikmə qənaəti.
	trimmed := strings.TrimSpace(text)
	if len([]rune(trimmed)) < 3 {
		return &ModerationResult{Flagged: false}, nil
	}

	reqBody := chatRequest{
		Model: s.model,
		Messages: []chatMessage{
			{Role: "system", Content: moderationSystemPrompt},
			{Role: "user", Content: "Analiz ediləcək mesaj:\n\"\"\"\n" + trimmed + "\n\"\"\""},
		},
		Temperature:    0, // determinist — eyni mesaj eyni nəticə
		MaxTokens:      200,
		ResponseFormat: &chatResponseFormat{Type: "json_object"},
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return &ModerationResult{Flagged: false}, fmt.Errorf("request marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openAIChatURL, bytes.NewReader(payload))
	if err != nil {
		return &ModerationResult{Flagged: false}, fmt.Errorf("request build: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.apiKey)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return &ModerationResult{Flagged: false}, fmt.Errorf("openai request: %w", err)
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return &ModerationResult{Flagged: false}, fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return &ModerationResult{Flagged: false},
			fmt.Errorf("openai status %d: %s", resp.StatusCode, string(rawBody))
	}

	var parsed chatResponse
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		return &ModerationResult{Flagged: false}, fmt.Errorf("response unmarshal: %w", err)
	}
	if parsed.Error != nil {
		return &ModerationResult{Flagged: false},
			fmt.Errorf("openai error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return &ModerationResult{Flagged: false}, errors.New("openai: boş choices")
	}

	content := parsed.Choices[0].Message.Content

	var verdict aiVerdict
	if err := json.Unmarshal([]byte(content), &verdict); err != nil {
		// JSON parse oluna bilmədi — fail-open.
		return &ModerationResult{Flagged: false}, fmt.Errorf("verdict unmarshal: %w (content=%s)", err, content)
	}

	// Flaq deyilsə — təmiz mesaj, heç nə etmə.
	if !verdict.Flagged {
		return &ModerationResult{Flagged: false, RawResponse: content}, nil
	}

	// Flaqdır amma kateqoriya tanınmırsa — etibar etmə, təmiz say.
	if !models.IsValidCategory(verdict.Category) {
		return &ModerationResult{Flagged: false, RawResponse: content},
			fmt.Errorf("tanınmayan kateqoriya: %q", verdict.Category)
	}

	// confidence-i 0..1 aralığına sıxışdır.
	conf := verdict.Confidence
	if conf < 0 {
		conf = 0
	}
	if conf > 1 {
		conf = 1
	}

	return &ModerationResult{
		Flagged:     true,
		Category:    verdict.Category,
		Confidence:  conf,
		Reason:      verdict.Reason,
		RawResponse: content,
	}, nil
}
