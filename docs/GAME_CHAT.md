# Oyun masası söhbəti — sim müqaviləsi

Paket `gamechat/`. Domino, Pio və loto masalarının söhbəti. Canlı otaq söhbətinin
imkanları: mətn, GIF, şəkil, Piound, cavab (reply), `@ad`, söyüş süzgəci, kölgə ban
(`users.live_spam`), bloklar, söhbəti təmizləmə.

**Fərqlər:** mesajlar şifrələnmir və bazaya yazılmır (miqrasiya yoxdur) — otağın son
200 mesajı yaddaşda durur, otaq boşalandan 15 dəqiqə sonra silinir. Simdə hesab
nömrəsi yoxdur: göndərən `sid` (otağa xas açar) ilə tanınır.

> Otaqlar prosesin yaddaşındadır (canlı otaqlar — `LiveHub` — kimi). Bir neçə
> replica varsa eyni masanın üzvləri eyni replica-ya düşməlidir.

## Giriş

1. Müştəri oyun soketində (`piokio_game`) `{"t":"chat"}` göndərir → `{"t":"chat","rid","role","ticket"}` alır.
2. `GET /ws/game-chat?ticket=<bilet>` + adi istifadəçi JWT-si (digər soketlər kimi).
   Biletdəki istifadəçi JWT-dəki ilə eyni olmalıdır; əks halda `403 bad_ticket`.
3. Rol dəyişəndə (ev sahibliyi keçdi) müştəri yeni bilet alıb `ticket` kadrı ilə gətirir.

Bilet = `base64url(JSON) + "." + base64url(HMAC-SHA256(JWT sirri, "gchat1." + JSON-b64))`,
sahələr `{rid, g, c, u, r (host|player|watcher), n, a, e}`. Əkizi: piokio_game `roomchat/ticket.go`.

## Kadrlar (`{"type","data"}`)

Müştəri → server:

| type | data |
|---|---|
| `chat_message` | `cid`, `text` (≤ 500), `gif_url`, `image_url`, `sound_url`, `sound_id`, `sound_title`, `sound_dur`, `color`, `cover_type`, `cover_url`, `reply_to_id` |
| `delete_message` | `id` — öz mesajı; ev sahibi istənilən mesajı |
| `clear_chat` | — (ev sahibi) |
| `chat_settings` | `enabled`, `watchers` (ev sahibi) — söhbət açıq / tamaşaçılar yaza bilir |
| `ticket` | `ticket` — rolu yeniləyir |
| `ping` | — |

Server → müştəri:

| type | data |
|---|---|
| `joined` | `rid`, `sid` (mənim açarım), `role`, `name`, `avatar` (mənim ad / şəklim), `enabled`, `watchers`, `messages[]` (son 80) |
| `chat_message` | mesaj (aşağıda) |
| `message_deleted` | `id` |
| `chat_cleared` | `name` |
| `chat_settings` | `enabled`, `watchers`, `name` |
| `role` | `role` |
| `message_blocked` | `reason: bad_word`, `cid` |
| `error` | `code` (`chat_off`, `watchers_off`, `too_fast`, `too_long`, `bad_media`, `bad_ticket`), `cid` |
| `pong` | — |

Mesaj: `{id, cid, sid, name, avatar, role, text, gif_url, image_url, sound_url, sound_id,
sound_title, sound_dur, color, cover_type, cover_url, reply_to{id,name,text,kind}, mentions[], at}`.

Media ünvanı: nisbi yol öz yaddaşımıza çevrilir; tam ünvan yalnız `https` və icazəli
sahələrdən (`beanpon.com`, `piokio.app`, `giphy.com`, `APP_URL` sahəsi) qəbul olunur.

Hədlər: bir adam 600 ms-də bir mesaj; otaqda 400 bağlantı; kadr 8 KB.

## Səsli söhbət (voice, 2026-11-20)

Kod `gamechat/voice.go`. Masada BİR səsli söhbət ola bilər. Onu yalnız oyunda olan
(`host` / `player`) aça bilər; masadakı hər kəs (tamaşaçı da) açıq olduğunu görür və
YALNIZ özü istəyəndə qoşulur — server heç kimi özü qoşmur. Səs LiveKit-dən axır; bu
soket yalnız "kim içəridədir" siyahısını saxlayır və LiveKit token-i verir. LiveKit
`identity` = üzvün `sid`-i (hesab nömrəsi yenə simdə yoxdur). Token yalnız mikrofon
yayımlamağa icazə verir (video / ekran / data yox), 3 saat keçərlidir.

Env (piokio_live ilə eyni dəyərlər): `LIVEKIT_HOST` (wss://…), `LIVEKIT_API_KEY`,
`LIVEKIT_API_SECRET`. Biri boşdursa xüsusiyyət bağlıdır (`voice.available = false`).

Müştəri → server:

| type | data | kim |
|---|---|---|
| `voice_start` | `muted` | host / player — söhbəti açır və qoşulur (artıq açıqdırsa = qoşulma) |
| `voice_join` | `muted` | hər kəs — açıq söhbətə qoşulur |
| `voice_leave` | — | özü çıxır (sonuncu çıxanda söhbət bitir) |
| `voice_mute` | `muted` | öz mikrofonunun vəziyyəti (siyahıda göstərmək üçün) |
| `voice_end` | — | ev sahibi və ya söhbəti açan — hamı üçün bitirir |
| `voice_mute_user` | `sid` | ev sahibi — birinin mikrofonunu bağlatdırır |
| `voice_kick` | `sid` | ev sahibi — səsli söhbətdən çıxarır (bu söhbətə qayıda bilmir) |

Server → müştəri:

| type | data |
|---|---|
| `voice` | vəziyyət (aşağıda) — hər dəyişiklikdə masadakı HƏR KƏSƏ; `joined` kadrında da `voice` sahəsi kimi gəlir |
| `voice_token` | `url`, `room`, `token`, `muted` — yalnız qoşulana |
| `error` | `code`: `voice_unavailable`, `voice_off` (açıq söhbət yoxdur), `voice_players_only`, `voice_full` (30 nəfər), `voice_kicked` |

Vəziyyət: `{available, active, by, by_sid, at, count, members:[{sid, name, avatar, role,
muted, blocked?}], event?}`. `blocked` = bu üzvlə aramda blok var (müştəri onun səsini
kəsir). `event` (yalnız dəyişikliyin kadrında): `{kind: started|ended|muted|kicked, sid, name}`
— `muted` / `kicked`-də `sid` hədəfdir, `name` ev sahibi; `ended`-də `name` bitirən
(boşdursa hamı çıxıb).

Müştərinin qaydaları:

- **Siyahı həqiqətdir.** LiveKit-ə bağlıyam ⇔ `active` və `sid`-im `members`-dədir.
  Siyahıdan çıxmışamsa (kick, bitdi) LiveKit-dən ayrılıram. Siyahıda `muted = true`-yam,
  mikrofonum isə açıqdır (ev sahibi bağlatdı) → mikrofonu bağlayıram.
- Soket yenidən qoşulanda (`joined`): siyahıda varam, amma bu cihazda LiveKit bağlantım
  yoxdur → `voice_leave`; LiveKit-ə bağlıyam, siyahıda yoxam → `voice_join` (gələn token
  artıq bağlıyamsa atılır).
- Soket qopanda yer 25 saniyə saxlanır, sonra üzv çıxarılır.
- Başqa canlı yayımda / başqa masanın səsli söhbətində olan istifadəçidən əvvəlcə
  soruşulur ("o bağlanacaq"); 1:1 zəng vaxtı qoşulmaq olmur.

Kick və bitirmə LiveKit-də də icra olunur (`RoomService/RemoveParticipant`, `DeleteRoom`,
best-effort); mikrofon bağlatma yumşaqdır (müştəri icra edir). piokio_live-ın LiveKit
webhook-u bu otaqları tanımır və heç nə etmir (`live_rooms`-da yoxdurlar).

## Yayım sırası

messenger → gateway (`/ws/*` onsuz da messenger-ə gedir, dəyişiklik yoxdur) →
piokio_game (`{"t":"chat"}` / `{"t":"who"}`) → tətbiqlər.
