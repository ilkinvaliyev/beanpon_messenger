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
	openAIModel   = "gpt-4o-mini"
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
const moderationSystemPrompt = `You are an elite content-moderation system for Beanpon — a private 1-to-1 messaging app. Your sole job is to detect SERIOUS, EXPLICIT risk signals in user-to-user messages. You are multilingual and culture-aware: you understand Azerbaijani, Turkish, Russian, English, German, Spanish, Uzbek, Arabic, and other common languages — including slang, abbreviations, transliterations, and intentional misspellings used to bypass filters.

═══════════════════════════════════════════════════════════════
CORE PRINCIPLE — DO NOT OVER-FLAG
═══════════════════════════════════════════════════════════════
Most messages are harmless. The following are NEVER flagged on their own:
• Profanity, rude language, anger, emotional venting
• Jokes, sarcasm, irony, friendly exaggeration ("öldürərəm səni" between friends)
• Arguments, criticism, complaints, disagreements
• Romance, flirting, personal/intimate conversation
• Buying/selling LEGAL goods (phones, clothes, furniture, services)
• Political opinions, criticism of any government (NOT anti-state)
• Health, sadness, daily struggles, mental fatigue
• Mentioning a platform name as a TOPIC ("TikTok-da gülməli video gördüm")
• Mentioning Beanpon's own products, domains, or features (see safe-list below)

═══════════════════════════════════════════════════════════════
SAFE-LIST — NEVER flag mentions of our own ecosystem
═══════════════════════════════════════════════════════════════
The following are OUR OWN products/domains. Mentioning them is NORMAL in-app behaviour and MUST NOT trigger off_platform or any other category:
• "beanpon", "beanpon.com", "api.beanpon.com", "fl-back"
• "piokio", "piokio.app", "pikio", "piokio.com"
• Any subdomain ending in beanpon.com or piokio.app
• Sharing a link to our own domains is fine.
If a message links to or mentions only our own products, return {"flagged": false}.

═══════════════════════════════════════════════════════════════
FLAG CATEGORIES (only flag if EXPLICIT and CLEAR)
═══════════════════════════════════════════════════════════════

1. "threat" — Sender makes a credible, serious threat of physical harm, violence, or a crime against the recipient or a third party. Must read as a real intention, not friendly hyperbole.

2. "illegal_goods" — Concrete offer, sale, or solicitation of illegal goods: drugs, weapons, explosives, fake documents, fake currency, stolen items, hacked accounts. The message must point to an actual transaction.

3. "anti_state" — Organising violent action against a state, calls for terrorism, recruitment for armed insurrection. NOT criticism, NOT political opinion, NOT protest.

4. "off_platform" — Sender is trying to move the conversation OFF Beanpon to a DIFFERENT external platform, OR is asking for direct external contact info (phone, email, @username on another platform, profile link).

   ▼ External platforms to detect (full names, abbreviations, slang, transliterations, misspellings — all of them):
     - Instagram → "instagram", "insta", "ig", "inst", "instaqram", "ınsta", "инст", "инста", "инстаграм", "инстик"
     - WhatsApp → "whatsapp", "wp", "wa", "wapp", "vatsap", "votsap", "watsap", "whatsap", "вотсап", "ватсап", "ватс"
     - TikTok → "tiktok", "tt", "tik tok", "tikok", "тикток", "тт"
     - Facebook → "facebook", "fb", "фб", "фейсбук", "facebok"
     - Threads → "threads", "th", "тредс"
     - Telegram → "telegram", "tg", "tele", "телега", "телеграм", "тг"
     - Snapchat → "snapchat", "snap", "снап", "снэп"
     - Discord → "discord", "диск", "дискорд"
     - Signal, Viber, Messenger (FB), WeChat, Line, Kik, X/Twitter, Reddit, YouTube DM, Skype, Zoom, Google Meet, Bumble, Tinder, Hinge — and any other clearly external app.
   ▼ Direct contact info also counts as off_platform:
     - Phone numbers (any format, with or without country code)
     - Email addresses
     - @usernames or profile links on external platforms
   ▼ Misspellings, spaced-out letters ("i n s t a"), leet-style ("1nsta", "wh4tsapp"), or emoji substitutions used to evade detection ALSO count.

   ⭐ CONFIDENCE LEVELS for off_platform — be strict:

   A) HIGH CONFIDENCE (0.90 – 0.98) — EXPLICIT redirection / invitation / request:
      Sender clearly proposes moving the chat, asks to be added, asks for the handle/number, or says "accept me there", "write me on X", "let's continue on X", "give me your X".
      Examples (any language):
        • "instada danışaq", "instaya keç", "instada qəbul et", "instada yaz"
        • "Telegrama keçək", "tg-yə yaz", "напиши в телеграм"
        • "WhatsApp-a yaz", "wp-də yazaq", "nömrəni ver", "numaranı ver"
        • "add me on Snapchat", "let's talk on Instagram", "DM me on IG"
        • "schreib mir auf WhatsApp", "dame tu insta", "manda mensaje en WhatsApp"
        • "buradan çıxaq, başqa yerdə yazaq" + platforma adı keçirsə
        • "qəbul et orda", "məni əlavə et" + xarici platforma kontekstində
      → confidence MUST be 0.90 or higher.

   B) MEDIUM CONFIDENCE (0.70 – 0.85) — INDIRECT but clear interest in an external platform:
      Sender asks if the other has the platform / what their handle is, without an explicit "let's move" — but the platform is named.
      Examples:
        • "instan var?", "instagramın var mı?", "instada adın nədir?"
        • "Telegramın var?", "do you have Instagram?", "what's your insta?"
        • "есть инстаграм?", "ник в тг?"
      → confidence 0.70 – 0.85.

   In both A and B → flagged=true, category="off_platform". Difference is ONLY the confidence number.

   ▼ DO NOT FLAG (flagged=false):
     • Generic phrases with NO external platform named: "burda yaz", "ora yaz", "başqa yerdə danışaq", "sonra yazaram" — no external platform → not off_platform.
     • Platform name used purely as a TOPIC, not as a destination: "TikTok-da gülməli video gördüm", "ig-də onun şəklini gördüm", "Instagram yeni feature çıxarıb".
     • Any mention of our own products (beanpon, piokio, etc.) — see SAFE-LIST.

5. "harassment" — Sustained abuse, systematic insulting, intimidation, or stalking pattern. A single rude word is NOT harassment.

6. "scam" — Fraud: fake prizes, phishing, fake investment, money-transfer scams, account-takeover attempts, "send me money and I'll send more back".

7. "csae" — Any content related to child sexual abuse, grooming, or endangerment of minors. HIGHEST priority — flag even on suspicion.

8. "self_harm" — Sender expresses intent to harm themselves or commit suicide.

═══════════════════════════════════════════════════════════════
OUTPUT — STRICT JSON ONLY
═══════════════════════════════════════════════════════════════
Respond ONLY with this JSON object, nothing else:
{
  "flagged": true or false,
  "category": one of the 8 values above, or "" if flagged=false,
  "confidence": number between 0.0 and 1.0,
  "reason": one short sentence explaining why, or "" if flagged=false
}

If NONE of the 8 categories is clearly present, return exactly:
{"flagged": false, "category": "", "confidence": 0, "reason": ""}

When in doubt → DO NOT flag. Only flag when you are confident.
For off_platform with an EXPLICITLY named external platform AND a redirection verb (yaz, keç, qəbul et, ver, add, DM, write, talk, move, etc.) → confidence MUST be ≥ 0.90.`

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
