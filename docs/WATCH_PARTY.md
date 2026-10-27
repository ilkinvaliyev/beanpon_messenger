# Birlikdə izlə (watch party) — contract

Single source of truth for the watch-party feature. Backend logic lives in the
`watch` package of **beanpon_messenger** (REST `/api/v1/watch/*` + WebSocket
`/ws/watch`). Laravel (`beanpon`) only owns the schema (migration
`2026_10_21_000000_create_watch_party_tables`) and the Filament admin
(providers, title catalog, rooms monitor, settings). Clients: piokio_ios
(`Features/WatchParty/`) first, piokio_android 1:1. Design boards
(claude.ai design canvas "Piokio iOS — yeni dizayn", sections "Birlikdə izlə —
kəşf, gizli kod və otaq yaratma" + "Birlikdə izlə — otaqda: izləyici, aparıcı,
mini pəncərə, xülasə"): `MainWatchPreview`, `WatchHub`, `WatchCode`,
`WatchCodeInactive`, `WatchJoin`, `WatchCreateSource`, `WatchCreateSettings`,
`WatchCreated`, `WatchLobby`, `WatchRoom`, `WatchRoomTools`, `WatchLandscape`,
`WatchPiP`, `WatchRoomHost`, `WatchHostPanel`, `WatchHostSettings`,
`WatchEnded`.

The old `raves` prototype (Laravel `/v2/raves*`, messenger `/ws/rave`) is
superseded and unused by every client; it is left untouched.

Package layout (`beanpon_messenger/watch`): `module.go` (routes, loops),
`hub.go` (in-memory rooms, clock, sessions, tick), `ws.go` (socket + frames),
`playback.go` (controls, breaks, pause requests, reactions, media end),
`rooms.go` (create / join / leave / host lifecycle / media change),
`members.go` (members, roles, kicks, requests, invites, nudges), `chat.go`
(messages, pins, moments, polls, queue), `discovery.go` (config, home, hub,
lists, titles), `media.go` (media validation), `youtube.go` (Data API +
cache), `push.go` (7-language pushes),
`sweeper.go`, `summary.go`, `views.go` (JSON builders), `settings.go`,
`users.go`, `util.go`, `watch_test.go`.

## 1. Legal rules (do not break)

- **YouTube = `sync` provider.** Played with the official YouTube IFrame Player
  API inside a WebView (iOS `WKWebView.loadHTMLString(_, baseURL:)`, Android
  `WebView.loadDataWithBaseURL`). `baseURL` = `https://<app id>` (iOS bundle id,
  Android applicationId, lower-cased) so YouTube receives the required Referer
  (otherwise error 152/153). Required Minimum Functionality:
  - nothing is drawn **over** the player (no badges, emojis, chat) — the
    room chrome sits in a strip directly under the video. Only exception: the
    landscape full screen shows **playback controls** over the picture (play /
    pause, ±10 s, progress, exit / minimize / close, title) that fade in on a
    tap and fade out 2.5 s later while the film runs — YouTube's developer
    policies guide allows playback-control overlays that don't clash with the
    player's UI (the player runs with `controls=0`). In portrait the player
    itself still takes no touches; a tap on the picture is the app's own
    gesture: it pauses / plays the film **for that viewer only** (host
    included — "detached", then back with the room on play) and, while paused,
    a centred play glyph + "only for you" is shown over the picture (a playback
    control). Only the controls under the player change the room for everyone;
  - **subtitles** = YouTube's own captions module (`loadModule('captions')`,
    `getOption/setOption('captions', 'tracklist' | 'translationLanguages' |
    'track')`): the viewer picks a track or an auto-translate language in the
    app's subtitle sheet (CC button). Personal — never synced. A video's dubbed
    audio tracks are not reachable through the IFrame API;
  - opening a user's profile from the room puts the room into the mini window
    first (the film stays in sight — never covered by another screen);
  - **no background playback**: the film stops for that viewer when the app
    leaves the screen or the screen locks (the room and chat go on; on return
    it jumps to the room clock). The policies name "play even when your API
    service window is closed or minimized" as a violation, so there is no
    system PiP and no lock-screen audio for YouTube either (the in-app mini
    window is fine — the app is on screen);
  - the player viewport is at least 200×200 pt (the mini window uses a
    200 pt-tall frame);
  - one auto-playing player per screen; scripted playback only while the
    player is on screen;
  - videos with `status.embeddable = false`, live streams and age-restricted
    videos are never offered (server: `resolveMedia`, `ytResults`).
- **Netflix / Apple TV = `companion` providers.** Nothing is injected into, or
  scraped from, those services. Every user watches in the official app with
  their **own** account; Piokio only keeps a shared room clock, chat and
  the sync helpers (countdown, "my time", banners). `has_account` is a
  **self-declaration** ("Netflix hesabın var"), never a login or credential.
  Rate changes are refused for companion rooms (the apps can't follow them).
- Provider catalog entries (posters, descriptions) are curated by admins in
  Filament; nothing is fetched from Netflix/Apple.
- **No live voice chat** (product decision 2026-10-22): the room talks only
  through chat messages (text, voice notes, GIFs, photos, sounds, polls).
  `watch_rooms.voice_enabled` stays in the schema unused (always false).

## 2. Schema (Laravel migration, Postgres)

| Table | Purpose |
|---|---|
| `watch_settings` | single row `id = 1`: feature switch + limits (§9) |
| `watch_providers` | `key` PK (`youtube`, `netflix`, `apple_tv`), `name`, `mode` (`sync`/`companion`), `is_active`, `sort`, `requires_subscription`, `brand_color`, `open_url_template` (`{ref}`), `search_url_template` (`{q}`), `link_patterns` (json regex list, first capture group = ref; matched case-insensitively), `ios_app_store_url`, `android_package`, `android_store_url` |
| `watch_titles` | admin catalog: `provider`, `kind` (`movie`/`series`/`documentary`/`show`/`video`), `title`, `original_title`, `search_text` (folded title + original: every i variant → `i`, then lower case — Go `foldSearch` folds the query the same way), `year`, `duration_sec`, `seasons_count`, `age_rating`, `genres` (json), `poster_path`, `backdrop_path` (S3 keys), `provider_ref`, `provider_url`, `description`, `is_active`, `is_featured`, `sort` |
| `watch_rooms` | the room (see §3) |
| `watch_room_members` | `room_id`, `user_id`, `role` (`host`/`cohost`/`viewer`), `state` (`joined`/`left`/`kicked`), `chat_muted`, `joined_at`, `left_at`, `last_seen_at`, `last_position_ms`; UNIQUE (room, user) |
| `watch_room_requests` | approval requests: `status` (`pending`/`accepted`/`rejected`/`cancelled`/`expired`), `expires_at` (+`request_ttl_minutes`), `decided_at`, `decided_by`; UNIQUE (room, user) — a new request reuses the row |
| `watch_room_invites` | `inviter_id`, `invitee_id`; UNIQUE (room, invitee). An invite pre-approves (bypasses approval, code and followers-only, not lock/kick/full) |
| `watch_room_reminders` | "Başlayanda xəbər ver" for scheduled rooms; `notified_at` |
| `watch_room_sessions` | one row per media played in a room: media snapshot, `started_at`, `ended_at`, `watched_ms`, `peak_viewers`, `viewers_count`, `messages_count`, `reactions_count`, `breaks_count` |
| `watch_session_viewers` | PK (session, user), `first_seen_at` — everyone who was connected while the session ran |
| `watch_room_messages` | chat: `type` (`text`/`voice`/`gif`/`photo`/`sound`/`moment`/`poll`/`system`), `text`, `media_url`, `meta` (jsonb), `position_ms` (room clock when sent), `client_id` (uuid, idempotency), `deleted_at` |
| `watch_room_polls` / `watch_room_poll_votes` | polls (`kind` `next`/`custom`), `options` jsonb `[{id,label,title,provider,title_id,media_ref,poster_url,backdrop_url,subtitle,kind,duration_ms,queue_id}]`, `closes_at`, `winner_option_id`; one vote per user (changeable) |
| `watch_room_queue` / `watch_room_queue_votes` | "Növbə": suggested media, `status` (`queued`/`played`/`removed`), `votes_count` |
| `watch_room_reaction_stats` | PK (session, `bucket` = position_s / 30, emoji) → `count` (reaction map) |
| `watch_provider_accounts` | PK (user, provider) → `has_account` (self-declared) |

Never write these tables from Laravel code paths other than the Filament admin
actions listed in §10.

## 3. Room model

`watch_rooms`: `host_id`, `created_by`, `name` (1–40 chars; empty → media
title), `provider`, `title_id?`, media snapshot (`media_ref`, `media_title`,
`media_subtitle` (YouTube channel), `media_kind`, `media_poster_url`,
`media_backdrop_url` (full URLs), `media_duration_ms`, `season?`, `episode?`),
settings (`access` `public`/`approval`/`code`, `followers_only`, `locked`,
`chat_mode` `everyone`/`hosts`/`off`,
`pause_requests_enabled`, `flying_reactions_enabled`), `code` (6 chars),
`status` (`scheduled` → `lobby` → `live` → `ended`), `scheduled_at`,
`opened_at`, `started_at`, `ended_at`, `end_reason`
(`host`/`admin`/`empty`/`expired`), `open_reminder_sent_at`, playback
(`position_ms`, `is_playing`, `playback_rate`, `position_updated_ms`,
`state_version`), `break_until_ms`, `current_session_id`,
`pinned_message_id`, counters (`watching_count` = WebSocket-online now,
`member_count` = joined, `peak_count`), `last_active_at`.

- **Code**: 6 chars from `ABCDEFGHJKMNPQRSTUVWXYZ23456789` (no 0/O/1/I/L),
  shown `M8K · 4TQ` (clients may send it with spaces / dots / dashes, any
  case). Unique among rooms with `status <> 'ended'` (partial unique index).
  Host can rotate it. Every room has a code; only the host and cohosts see it.
- **Room clock** (server-authoritative): at server time `T` (ms)
  `expected = clamp(position_ms + (is_playing ? max(0, T − updated_at_ms) × rate : 0), 0, duration)`.
  `updated_at_ms` may be in the **future** (countdown start): the clock does
  not move until then. Every change bumps `version`.
- **Sessions**: `POST start` / media change opens a new `watch_room_sessions`
  row; media end / media change / room end closes it. Messages and reactions
  are attributed to `current_session_id`. The "Film bitdi" summary is per
  session. `watched_ms` counts wall time while the clock runs (not during a
  countdown, a pause or a break).
- **Host continuity**: the host leaving hands the room over (cohost first —
  online first —, then the earliest joined online member, then any member) or
  ends it when alone. A host who stays offline 5 minutes while others watch is
  replaced the same way (`host` event, reason `auto`).
- **In memory**: a room is loaded on first use (REST or socket) and evicted
  2 minutes after its last socket closed (10 s for ended rooms). Like the live
  hub this is per process — messenger runs one replica (with more replicas,
  every REST call and socket of a room must reach the same instance); the DB
  is the source of truth, so a reload rebuilds the same state. A room ended
  (or deleted) directly in the DB (Filament fallback) is closed in memory
  within 30 s. At boot `watching_count` is reset to 0 for open rooms (nobody
  is connected to a fresh process).
- **Locks**: one mutex per room; DB writes happen under it, pushes /
  YouTube calls never do. A room busy with a slow request is skipped by that
  second's tick, the external-end check and the discovery lists (cards then
  use the row's `watching_count`), never blocked on. Dedupes that must outlive
  an eviction (request announcements, join/leave lines) live in the module's
  rate limiter, not in the room.

## 4. Permissions

- Guests (`users.is_guest`) get 403 `GUEST_FORBIDDEN` on every write and on
  join; discovery reads are allowed (public, non-followers-only rooms only).
  Feature switch off → 403 `WATCH_DISABLED` on writes / hub / lists / socket.
- Hidden from everyone (404 `ROOM_NOT_FOUND`): blocked either way with the
  host, deleted/deactivated hosts, Hidden Mode (platform switch). Members
  always see their room. Kicked users still read the room (`me.state =
  kicked`) but get 403 `KICKED` on join. Only joined members skip the host
  visibility check — a former member whose host blocked them (or is gone)
  gets 404 too. A `code` room is not readable by id (`GET /rooms/:id` → 404)
  for anyone but the host, (former) members, invitees and requesters —
  everybody else needs the code (`GET /code/:code`).
- **Visible in lists** (`/watch/home`, `/watch/hub`, `/watch/rooms`): status
  `lobby`/`live`, `access <> 'code'`, not locked, host not hidden / blocked /
  gone, and if `followers_only` the viewer is followed by the host
  (`follows.status = 'accepted'`). The host's own rooms are always visible.
- **Join** (`POST /join`), checked in this order: room exists/visible →
  `ended` 410 `ROOM_ENDED` → `scheduled` 409 `ROOM_NOT_STARTED` → kicked 403
  `KICKED` → already joined (idempotent, `state: joined`) → locked 403
  `ROOM_LOCKED` → full (`joined ≥ max_members`) 403 `ROOM_FULL` → invited or
  an earlier member (`state = left`) ⇒ join → followers-only and not followed
  403 `FOLLOWERS_ONLY` → `code` access: wrong/missing code 403
  `CODE_REQUIRED` (5 misses, then one try per 2 min → 429 `RATE_LIMITED`) ⇒
  join → `approval`: rejected within `request_ttl_minutes` 403
  `REQUEST_REJECTED`, else create/refresh the request ⇒ `state: requested`
  (code not needed when found from a list; host + cohosts are told once per
  requester per TTL, however often they cancel and ask again) → `public` ⇒
  join. An invite pre-approves only while its sender could let the person in
  themselves: a host / cohost, or any member of a `public` room that is not
  followers-only.
- **Controllers** (play/pause/seek/rate/resync/break/media change/queue play/
  pin/poll pick + close): host + cohosts (max 5 cohosts). Host only: settings,
  code rotate, transfer, roles, kick, chat-mute, start, end, open, remind,
  help. Host + cohosts: accept/reject requests, invite in non-public rooms
  (in public rooms every member may invite people they follow).
- **Chat**: `chat_mode = off` → 403 `CHAT_OFF` for everyone except the host;
  `hosts` → only host/cohosts; chat-muted members (cohosts too — only the host
  can't be muted) → 403 `CHAT_MUTED`. Text max `chat_max_length` (500, 422
  `MESSAGE_TOO_LONG`); `utils.ContainsBadWord` → 422 `MESSAGE_BLOCKED` (also
  room names, moment notes, poll questions / labels and typed media titles).
  Spam-banned users (`models.IsMessagingBannedByActions`, like the rest of
  messenger) are shadow-banned: their line is stored with `meta.shadow = true`
  and echoed only to themselves, it can't be pinned, and their polls / queue
  suggestions (room-wide) are refused with 403 `ACTION_RESTRICTED`. Lines from
  users with a block either way are never delivered (history + socket; also
  poll lines and the pinned line).
- **Chat media** (`media_url`): `https` only, on the `APP_URL` host (uploads
  are `APP_URL/api/s3-storage/…`) or `beanpon.com`, `piokio.app`, `piokio.az`,
  `giphy.com`, `tenor.com` (and their subdomains); anything else → 422
  `INVALID_INPUT` (a foreign host could track who opens the room).
- Rate limits (in memory, per user): messages / moments / polls / queue adds
  2.5 / s per room (burst 5), reactions 8 / s (burst 15), controls 10 / s, any
  socket frame 20 / s (burst 40), code lookups 0.2 / s (burst 10), joins
  0.2 / s per room (burst 6), wrong room codes 5 then 1 / 2 min, room creates +
  media changes 0.2 / s (burst 5, shared bucket), queue adds 0.2 / s (burst 5),
  polls 1 / 20 s (burst 3), link resolves 0.5 / s (burst 10), YouTube trending
  lists 0.5 / s (burst 10), YouTube searches 1 / 30 s (burst 20) + a daily cap
  (`YOUTUBE_SEARCH_DAILY_MAX` for everyone, one user ≤ 1/6 of it, min 3; only
  uncached queries count), invites 0.5 / s (burst 5), pause
  requests 1 / min, remind-unready 1 / min, help 1 / min per target. Excess →
  429 `RATE_LIMITED` / socket `ack` with `code: RATE_LIMITED`. The quota-costing
  steps (YouTube lookups) always run after the permission checks.

## 5. REST (`/api/v1/watch`, JWT)

Success = `{"data": …}`; error = `{"error": "<az text>", "code": "UPPER_SNAKE"}`
— clients localize by `code`.

### Shared shapes

- `UserLite` `{id, username, name, avatar, is_verified}` (usernames are shown
  without "@").
- `Media` `{provider, title_id, media_ref, title, subtitle, kind, poster_url, backdrop_url, duration_ms, season, episode, year, age_rating, genres, open_url}`
- `Playback` `{position_ms, is_playing, rate, updated_at_ms, server_ms, break_until_ms, version}`
- `RoomCard` `{id, name, status, access, followers_only, locked, media: Media, playback: Playback, watching_count, member_count, host: UserLite, friends: {users: [UserLite ≤3], count}, scheduled_at, started_at, created_at}` — friends = people the viewer follows who are connected right now.
- `RoomDetail` = `RoomCard` + `{chat_mode, pause_requests_enabled, flying_reactions_enabled, code (host/cohost only, else null), pinned_message: Message|null (members only; null when the viewer and its author have a block), current_session_id, max_members, end_reason, me: {role, state, request_status, request_expires_at, invited, reminder, chat_muted, can_control, is_host, has_account (companion providers only, else null), host_follows_me}, provider_info: Provider}`
- `Provider` `{key, name, mode, requires_subscription, brand_color, open_url_template, search_url_template, link_patterns, ios_app_store_url, android_package, android_store_url}`
- `TitleResult` `{provider, title_id, media_ref, title, kind, year, duration_ms, seasons_count, age_rating, genres, poster_url, backdrop_url, channel, open_url, embeddable, social: {type: "resume"|"friends_watched"|"trending", users: [UserLite ≤3], remaining_ms}|null}` — `title` is "" for a pasted companion link that is not in the catalog (the client asks the user for a name).
- `Member` `{user: UserLite, role, chat_muted, online, ready: ready|loading|no_account|on_the_way|offline, sync: {state: in_sync|behind|ahead|unknown|detached, drift_ms}, joined_at}`
- `Message` `{id, client_id, type, user: UserLite|null, text, media_url, meta, position_ms, created_at, deleted}` — `system` lines carry `meta {event: joined|left, user_id}`, `poll` lines `meta {poll_id, kind}`, `moment` lines `meta {position_ms}`.
- `Poll` `{id, kind, question, options: [{id, label, provider, title_id, media_ref, poster_url, subtitle, kind, duration_ms, votes}], total_votes, my_option, closes_at_ms, closed, winner_option_id, created_by}`
- `QueueItem` `{id, media: Media, suggested_by: UserLite|null, votes, voted, created_at}`

### Endpoints

| Method + path | Notes |
|---|---|
| `GET /config` | `{enabled, home_preview_enabled, providers: [Provider], limits: {max_members, chat_max_length, break_minutes, max_schedule_days, request_ttl_minutes, next_poll_seconds, name_max_length, code_length}, accounts: {provider: bool}, youtube_search_available}` |
| `PUT /accounts/:provider` `{has_account}` | self-declaration (companion providers only); rooms the user is in get a `ready` update |
| `GET /home` | `{enabled, live_count, friends_watching, rooms: [RoomCard ≤8]}` (friends first) |
| `GET /hub?filter=all\|friends\|movies\|series\|<provider>&sort=popular\|new&cursor=` | `{live_count, watching_count, friends_count, friends_rooms: [RoomCard ≤5], trending: [{title: TitleResult, rooms_count, watching_count, rank}], rooms: [RoomCard], next_cursor}` (20 per page) |
| `GET /rooms?filter=&sort=popular\|new&title_id=&media_ref=&cursor=` | `{rooms, next_cursor, total}` |
| `GET /titles/featured?provider=&region=` | `{provider, items: [TitleResult]}` — catalog (featured first) or YouTube trending (cached 1 h per region; `region` ∈ AZ TR RU UZ KZ GE UA US GB DE ES, anything else = `AZ`; a region without a chart falls back to TR / US) |
| `GET /titles/search?provider=&q=&region=` | catalog (companion) or YouTube search (cached 6 h, signed-in users only — guests 403 `GUEST_FORBIDDEN`; 503 `SEARCH_UNAVAILABLE` without a key / when switched off / on an API error, 503 `SEARCH_LIMIT` once the day's cap is spent — the client offers "paste a link"); empty `q` = featured |
| `POST /titles/resolve` `{url}` | signed-in users: link → `{provider, result: TitleResult}`; 422 `LINK_UNSUPPORTED`; YouTube 422 `NOT_EMBEDDABLE` (unknown / private / not embeddable / live / age-restricted), 503 `YOUTUBE_UNAVAILABLE` when the API does not answer. Without an API key the title / channel / thumbnail come from YouTube's keyless oEmbed (400/401/403/404 there → `NOT_EMBEDDABLE`; a network error keeps the bare result) — create / media change / queue do the same |
| `POST /rooms` | `{name, provider, title_id?, media_ref?, media_title?, media_subtitle?, duration_ms?, season?, episode?, url?, access, followers_only, scheduled_at? (RFC 3339)}` → `RoomDetail` (status `lobby`, or `scheduled` when `scheduled_at` is > 2 min ahead; ≤ `max_schedule_days`). Errors: `PROVIDER_UNAVAILABLE`, `MEDIA_REQUIRED`, `NOT_EMBEDDABLE`, `LINK_UNSUPPORTED`, `YOUTUBE_UNAVAILABLE` (503), `MESSAGE_BLOCKED` (name / typed title), `SCHEDULE_INVALID`, `ROOM_LIMIT` (the host already has an open room), `DAILY_LIMIT`. With a YouTube key every video is checked; without one the client's title is used |
| `GET /rooms/:id` | `RoomDetail` (code rooms: see §4) |
| `GET /code/:code` | `{room: RoomDetail}`; ended / unknown → 404 `CODE_NOT_FOUND` (`scheduled` rooms are returned — the client shows "starts at" + reminder) |
| `POST /rooms/:id/join` `{code?}` | `{state: "joined"\|"requested", room: RoomDetail}` |
| `DELETE /rooms/:id/request` | cancel own pending request |
| `POST /rooms/:id/leave` | host leaving → host transfer or end when alone |
| `POST\|DELETE /rooms/:id/remind` | reminder for scheduled rooms → `{reminder}` |
| `POST /rooms/:id/open` | host: scheduled → lobby (pushes reminders + invitees) |
| `POST /rooms/:id/start` `{position_ms?, delay_seconds?}` | host: lobby → live, opens a session, 3 s countdown for everyone (`delay_seconds` 3–600 = the lobby's "start in N minutes": the same countdown, longer; the `countdown` frame carries `seconds`). Open / start against a row changed behind the server's back: ended → 410 `ROOM_ENDED` (+ `ended` frame), anything else → 409 `STATUS_CHANGED` (reload the room) |
| `POST /rooms/:id/end` | host: end for everyone |
| `PATCH /rooms/:id` | host: `name, access, followers_only, locked, chat_mode, pause_requests_enabled, flying_reactions_enabled` → `RoomDetail` |
| `POST /rooms/:id/code` | host: rotate → `{code}` |
| `POST /rooms/:id/media` | controller: change media (new session; live rooms restart with a countdown; the rate goes back to 1×) — same media fields as create |
| `POST /rooms/:id/transfer` `{user_id}` | host → another joined member (old host becomes cohost) |
| `GET /rooms/:id/members` | `{members: [Member], online_count}` (host first, then cohosts, online, joined_at) |
| `POST /rooms/:id/members/:uid/role` `{role: cohost\|viewer}` | host (409 `COHOST_LIMIT` above 5) |
| `POST /rooms/:id/members/:uid/chat-mute` `{muted}` | host |
| `DELETE /rooms/:id/members/:uid` | host: kick (permanent for this room; also works on a requester) |
| `GET /rooms/:id/requests` | host/cohost: `{requests: [{user, created_at, expires_at, context: {type: "mutual"\|"friend_in_room"\|"mutual_friends", user?, count?}\|null}]}` |
| `POST /rooms/:id/requests/:uid/accept\|reject`, `POST /rooms/:id/requests/accept-all` | host/cohost; 404 `REQUEST_NOT_FOUND` (also for a requester who is gone or now has a block with the host — that request is cancelled). accept-all takes ≤25 per call → `{accepted, remaining}` (call again while `remaining` > 0; 0 when the room is full; requests that can never be accepted are cancelled on the way, `request_resolved` `cancelled`) |
| `GET /rooms/:id/invite-candidates?q=` | members: people the viewer follows (mutuals first): `{users: [{user, invited, member}]}` |
| `POST /rooms/:id/invites` `{user_ids}` | ≤50 per call, only people the inviter follows → `{invited}`; push `watch_invite` (scheduled rooms: pushed when opened) |
| `POST /rooms/:id/remind-unready` | host: push `watch_remind` to joined members not ready/offline + invitees not joined → `{reminded}` |
| `POST /rooms/:id/help/:uid` | host: WS `help` (+ push `watch_help` when offline) to a member |
| `GET /rooms/:id/messages?before_id=&limit=` | members: `{messages, has_more}` (newest first, ≤50) |
| `POST /rooms/:id/messages` `{client_id (uuid), type: text\|voice\|gif\|photo\|sound, text?, media_url? (allowlisted, §4), meta? (object ≤4 KB), reply_to_id?}` | members; idempotent by the sender's own `client_id` → `Message`. `reply_to_id` = a live person's line of this room (text/voice/gif/photo/sound, not someone else's shadow line; 404 `MESSAGE_NOT_FOUND` / 422 otherwise): the server stores `meta.reply {id, user_id, username, type, text (≤140 runes)}` — a client-sent `meta.reply` is dropped. No migration (meta is jsonb); old apps ignore the key |
| `DELETE /rooms/:id/messages/:mid` | own or host/cohost |
| `DELETE /rooms/:id/messages` | host only: clears the whole chat for everyone (soft delete; the pin goes too) → WS `messages_cleared` |
| `POST /rooms/:id/pin` `{message_id\|null}` | controller (a shadow line → 404 `MESSAGE_NOT_FOUND`) |
| `POST /rooms/:id/moments` `{note?}` | members (live rooms): `moment` line at the current room position |
| `POST /rooms/:id/polls` `{question?, options: [{label, title_id? \| provider + media_ref? \| url?}] (2–4)}` | members (chat rules apply; spam-banned 403 `ACTION_RESTRICTED`); one open poll per room (409 `POLL_ACTIVE`); closes after 5 min; posts a `poll` line → `Poll` |
| `POST /polls/:pid/vote` `{option_id}` → `Poll` · `POST /polls/:pid/pick` `{option_id}` (controller: plays that option) · `POST /polls/:pid/close` (controller, or its creator while still a member; a `next` poll plays its winner) | 409 `POLL_CLOSED` |
| `GET /rooms/:id/queue` → `{items}` · `POST /rooms/:id/queue` (media fields; ≤20, 409 `QUEUE_FULL` / `ALREADY_QUEUED`; spam-banned 403 `ACTION_RESTRICTED`) · `POST /queue/:qid/vote` (toggle) · `DELETE /queue/:qid` (controller, or its suggester while still a member) · `POST /queue/:qid/play` (controller) |
| `GET /rooms/:id/summary?session_id=` | members (also after leaving): `{session: {id, media, started_at, ended_at, watched_ms}, room: RoomDetail, stats: {viewers, messages, reactions, breaks, together_ms}, participants: [UserLite ≤8, host first], participants_count, reaction_map: {bins: [{count, top_emoji}] (48), duration_ms, peak: {emoji, count, position_ms}\|null, markers: [{emoji, position_ms}] (≤5)}, next_poll: Poll\|null}` |

Internal (`/internal`, `X-Internal-Secret`): `POST /internal/watch/rooms/:id/end`
(Filament "Bitir") and `POST /internal/watch/settings/reload`.

## 6. WebSocket `/ws/watch?room_id=&token=`

Only joined members connect (HTTP 403 `NOT_MEMBER` / 410 `ROOM_ENDED`
before the upgrade). One socket per user per room (a new one replaces the
old: the old gets `replaced` and closes). Frames: `{"type", "room_id",
"data", "server_ms"}` (one JSON object per frame). Client frames: `{"type",
"id"?, "data"}`; any frame with an `id` is answered with
`{"type":"ack","data":{"id","ok","code"?}}`.

Server → client:

| type | data |
|---|---|
| `state` | full snapshot on connect: `{room: RoomDetail, playback, online_ids, ready: {uid: state}, pause_request, active_poll, countdown_start_ms, session_id, session_ended}` |
| `playback` | `Playback` + `{action: play\|pause\|seek\|rate\|start\|resync\|break\|break_end\|media\|ended, actor_id}` |
| `countdown` | `{start_at_ms, position_ms, seconds}` |
| `media` | `{media: Media, session_id}` |
| `status` | `{status}` |
| `settings` | changed settings (`code` only to host/cohosts) |
| `host` | `{host: UserLite, previous_host_id, reason: left\|transfer\|auto}` |
| `message` · `message_deleted` `{id}` · `pinned` `{message\|null}` | chat (`pinned` per recipient: `null` for one with a block with the author) |
| `messages_cleared` `{by}` | the host cleared the chat: drop every line already shown (keep only your own unsent ones) |
| `reaction` | `{emoji, user_id}` (only when flying reactions are on; not echoed to the sender; emoji only — whitelisted emoji ranges + ZWJ / VS16 / keycap / tag sequences, ≤12 code points, else ack `INVALID_EMOJI`) |
| `presence` | `{user_id, online}` |
| `member` | `{member: Member, change: joined\|updated\|left\|kicked\|role}` |
| `ready` | `{user_id, state}` |
| `pause_request` | `{user: UserLite, supporters: [UserLite], created_at_ms}` or `null` (expires after 2 min) |
| `poll` | `Poll` (per recipient — `my_option`) |
| `queue` | `{items: [QueueItem]}` (per recipient — `voted`) |
| `request` | (host/cohosts) `{user: UserLite, context, created_at}` / `request_resolved` `{user_id, status}` |
| `sync_report` | (host/cohosts, every 5 s while live) `{in_sync, total, members: [{user_id, state, drift_ms}]}` |
| `resync` | (target) `{playback, actor_id}` — hard re-sync to the room clock |
| `help` | (target) `{from: UserLite, provider}` |
| `session_ended` | `{session_id}` → clients show the "Film bitdi" summary |
| `kicked` · `ended` `{end_reason, session_id}` · `replaced` · `error` `{code}` | terminal |
| `pong` | `{t, server_ms}` |

Client → server: `ping {t}`, `control {action: play|pause|seek|rate|resync_all, position_ms?, rate?}`
(live rooms only; rates 0.5/0.75/1/1.25/1.5/2, sync providers only),
`report {position_ms?, is_playing?, drift_ms?, state?}` (every 5 s while live;
companion rooms send `{state: "in_sync"}` after "Sinxronam" and `{drift_ms}`
after "Mənim vaxtım"), `ready {state: ready|loading|no_account|on_the_way}`,
`reaction {emoji}`, `pause_request {}` (viewers; a second request supports the
first), `pause_support {}`, `pause_cancel {}` (requester or controller),
`break {minutes}` (1–15, default `break_minutes`, controller), `break_end {}`,
`resync_user {user_id}` (controller), `media_ended {}`
(YouTube player ended; counted from a controller, or any member while no
controller is online, and only when the clock is within 20 s of the end).
`play` after the film ended (`session_ended`) starts it again from the
beginning as a new session with the 3 s countdown (an open "what next" poll is
dropped — so is it when a controller changes the media). During a countdown
`seek` / `rate` keep the countdown (only the start point / speed change);
`play` starts at once. `break` after the film ended → ack `MEDIA_ENDED`. An
unknown frame type with an `id` gets `ack {ok: false, code: UNSUPPORTED}`
(newer clients can feature-detect).

## 7. Sync algorithm (clients)

- Clock offset from `ping`/`pong`: `offset = server_ms − (t + rtt/2)`, median of
  the last 5 samples; re-measured every 30 s. `serverNow = now + offset`.
- **YouTube**: every 1 s compare the player time with the room clock.
  `|drift| > 2.0 s` → `seekTo`; `0.35–2.0 s` → playback rate 1.25 (behind) /
  0.75 (ahead) until within 0.15 s, then back to the room rate. Buffering
  pauses the correction. A viewer who pauses locally becomes `detached` (pill
  "Senkrondan çıxdın · Qayıt"); any host action re-attaches.
- **Countdown**: everyone schedules playback at `start_at_ms` (3-2-1 overlay);
  companion rooms flash "Play" + haptic at 0.
- **Companion**: the screen shows the ticking room clock; host actions raise a
  banner + haptic ("Aparıcı dayandırdı · 42:13"); "Sinxronam" confirms; "Mənim
  vaxtım" computes `drift = myTime − expected` and tells the user how many
  seconds to skip. The server ends the session when the clock reaches the
  title's duration.
- Player readiness drives `ready` in the lobby (`loading` until the player is
  ready / the user confirms the companion app is open).

## 8. Push notifications

messenger renders the text in the receiver's language (`push.go`, 7 languages:
az en tr ru de es uz; language = newest active `fcm_tokens.lang`, fallback
`en` like Laravel `NotificationLocale`) and calls Laravel
`POST {BACKEND_URL}/notification/send` (`x-api-key: CLOUD_TOKEN`,
`save_history: false`). Skipped while `user_notification_pauses.paused_until >
now`. `data`: `{route: "watch_room", room_id, notification_type, type, ...}`.
Title = "Birlikdə izlə" (localized).

| type | receiver | text (az) |
|---|---|---|
| `watch_invite` | invitee | "{host} səni “{room}” otağına dəvət etdi" |
| `watch_request` | offline host + cohosts (`user_id` in data) | "{user} “{room}” otağına qoşulmaq istəyir" |
| `watch_request_accepted` | requester | "{host} istəyini qəbul etdi — otağa qoşul" |
| `watch_room_opened` | reminders + invitees | "“{room}” başladı — {host} səni gözləyir" |
| `watch_open_reminder` | host of a scheduled room (at `scheduled_at`) | "Otağını açmaq vaxtıdır: “{room}”" |
| `watch_remind` | not-ready members + invitees | "“{room}” başlamaq üzrədir — hazır ol" |
| `watch_help` | offline member | "{host}: {provider} tətbiqini öz hesabınla aç" |

Clients route `route == "watch_room"` to the room (join flow when not a member).

## 9. Settings (`watch_settings`, cached 60 s)

`enabled` (feature switch, default ON), `home_preview_enabled`,
`max_members` (50), `max_active_rooms_per_user` (1 — one open room per host: a lobby / scheduled / live room must end before the next one; the create re-checks it under a per-user advisory lock, class 7313), `max_rooms_per_day` (20),
`request_ttl_minutes` (10), `empty_room_minutes` (2; was 15 before migration `2026_10_22_000000`),
`scheduled_grace_minutes` (120), `max_schedule_days` (7),
`youtube_search_enabled`, `chat_max_length` (500), `break_minutes` (5),
`next_poll_seconds` (100). Zero / out-of-range values fall back to the
defaults. Before the table exists the feature is off.

## 10. Admin (Laravel Filament, group "Birlikdə izlə")

`WatchProviderResource` (toggle/sort/edit links), `WatchTitleResource`
(catalog CRUD, poster/backdrop upload to S3), `WatchRoomResource` (read-only
list + members; header "Ayarlar" edits `watch_settings`; "Bitir" → messenger
`/internal/watch/rooms/:id/end`, falling back to a direct DB update).

## 11. Sweeper (messenger, every 30 s) + tick (every 1 s)

Sweeper: expire pending requests (`request_resolved` to controllers); end
`lobby`/`live` rooms nobody is connected to whose `last_active_at` is older
than `empty_room_minutes`; push `watch_open_reminder` once at `scheduled_at`;
end scheduled rooms not opened within `scheduled_grace_minutes`; close polls
past `closes_at` (a `next` poll plays its winner; no votes → its first option).

Tick (loaded rooms): break end (3 s countdown), countdown bookkeeping, pause
request expiry, media end (companion rooms by the clock; sync rooms by the
players, or 15 s after the clock passed the duration when no player reported
it — a running break ends with the film), `sync_report` every 5 s, reaction
stats flush every 3 s, counters, `last_active_at` + session progress every
30 s, host auto-transfer, external-end check + eviction every 30 s.

## 12. Deploy order

1. Laravel migration `2026_10_21_000000` (+ Filament).
2. messenger (env: `YOUTUBE_API_KEY`, `YOUTUBE_SEARCH_DAILY_MAX` (default 90);
   without the YouTube key search/trending are
   empty but pasted links still work — titles then come from YouTube oEmbed,
   lengths from the players).
   YouTube Data API quota: search costs 100 units of the default 10 000 / day,
   a video lookup 1 — searches are cached 6 h, limited per user and capped per
   day (`YOUTUBE_SEARCH_DAILY_MAX` × 100 units, Pacific day) so room creation
   always keeps units; raise the cap after Google grants more quota.
3. piokio_gateway (`/watch*` → messenger; `/ws/watch` already routed by `/ws/`).
4. Apps.

Kino zalı (§13): Laravel migration `2026_10_26_000000` (`watch_rooms.cinema_*`
+ `watch_room_seats`) **before** the messenger deploy — every room create
writes the new columns, so a missing migration breaks every create. Then
messenger, then the apps (gateway: nothing new, `/watch/*` already routed).

## 13. Kino zalı (cinema mode, 3D hall)

An optional mode of a room: the members sit in a virtual hall (one place
each) and the new apps draw it in 3D (the film on the hall's screen, heads of
the people in front, neighbours at the sides). **Additive only**: an old app
joins a cinema room like any room and never notices — the server seats it,
never sends it offers, and it just watches the plain player.

### Layout

`seats` 6…60 (clamped). `cols = clamp(round(sqrt(seats × 1.6)), 3, 10)`;
when that doesn't divide the seats, `cols + 1` and then `cols − 1` are tried
and the first that fills every row wins. `rows = ceil(seats / cols)`; the last
row may be short (centred). `couch` (side sofa spots) = `seats / 3` clamped
to 2…6. 12 → 3×4, 18 → 3×6, 24 → 4×6, 40 → 5×8, 60 → 6×10. Codes = row
letter (A = front) + 1-based seat number in the row (`C4`). Best seat = the
lowest `score = |row − (rows − 1) / 2| + 0.7 × |col − rowCentre|` (middle
row, centre of the row). Swift `CinemaLayout.forSeats` = Go `layoutFor` (the
same numbers for every count — change both together).

### Create

`POST /rooms` adds `cinema` (absent / `enabled: false` = a plain room; old
servers ignore it):

```json
{"enabled": true, "seats": 40, "allow_seat_change": true, "seat": 19,
 "reserve": [{"user_id": 7, "seat": 18}, {"user_id": 9, "seat": 20}]}
```

`seat` = the host's seat (default: the best one). `reserve` ≤ 2 seats for
accounts the host follows (accepted follow, no block, not a guest); invalid
entries are skipped silently. Reserved friends get the normal room invite;
their seat is kept until they join, or 10 minutes into the film.
`GET /cinema/friends?q=` → `{users: [UserLite]}` — who can get a kept seat
(accepted follows, mutual first, ≤ 50).

Room detail (`GET /rooms/:id`, the cards) adds `cinema {enabled, seats, rows,
cols, couch, allow_seat_change}` for cinema rooms only (omitted otherwise).

### Hall state

`GET /rooms/:id/cinema` (members) and the socket `state` frame's `cinema` key
(`null` for plain rooms):

```json
{"enabled": true, "seats": 40, "rows": 5, "cols": 8, "couch": 6,
 "allow_seat_change": true, "rev": 1729950000123,
 "places": [{"user": {UserLite}, "user_id": 7, "kind": "seat", "index": 18,
             "code": "C3", "view": "hall", "reserved": false, "chosen": true,
             "notify": false}],
 "queue": [12, 15], "held": [20],
 "my_offer": {"index": 20, "code": "C5", "expires_at_ms": 1729950030000}}
```

- `kind` `seat` | `couch` | `none` (the hall and the sofas are full).
  `view` `hall` | `full` ("Tam ekran": the place is kept, the person watches
  the plain player; the others still see them sitting).
- A place with a block either way shows `user: null, user_id: 0` (an
  anonymous occupied place).
- `queue` = couch / no-place members with "Yer boşalanda xəbər ver" on,
  oldest first. `held` = seats held by an open queue offer.
- `my_offer` only in per-viewer answers (the socket `state`, `GET`, and the
  seat / notify / swap answers), never in the `cinema_places` broadcast.
- `rev` grows with every change sent to the clients (it starts at the room's
  load time in ms, so it keeps growing across a restart): a client drops an
  answer whose `rev` is lower than the state it shows.

### Actions (all `{data: …}`, guarded like other writes)

| Request | Body | Answer |
|---|---|---|
| `POST /rooms/:id/cinema/seat` | `{kind: seat\|couch, index}` | hall state |
| `POST /rooms/:id/cinema/view` | `{view: hall\|full}` | `{ok}` |
| `POST /rooms/:id/cinema/notify` | `{on}` | hall state |
| `POST /rooms/:id/cinema/offer/decline` | `{index}` | `{ok}` |
| `POST /rooms/:id/cinema/swap` | `{index}` | `{id, expires_at_ms}` |
| `POST /rooms/:id/cinema/swap/:sid/accept` | — | hall state |
| `POST /rooms/:id/cinema/swap/:sid/decline` | — | hall state |

- Seat: free, or held for me by my queue offer. "Yerini dəyişmək olar" off →
  a member who picked a seat keeps it (`SEAT_CHANGE_OFF`); the host may always
  move, and leaving a couch / no place for a seat is always fine. Getting a
  seat any way gives back my own open queue offer (it goes to the next
  person) and voids my open swaps.
- Swap: ask the person on that seat (must be in the room right now with a
  cinema app, no block) — they get `cinema_swap_offer`, 30 s. Accept = the two
  places change hands (only while the asked person is still on that seat). A
  new ask replaces my previous one.

### Socket (new apps connect with `&cinema=1`)

| Frame | To | Data |
|---|---|---|
| `cinema_places` | everyone (old apps ignore it) | hall state (no `my_offer`) |
| `cinema_seat_offer` | the first cinema app in the queue | `{index, code, expires_at_ms}` (seat held 30 s) |
| `cinema_seat_offer_expired` | that person | `{index}` (timed out, or they got a seat another way) |
| `cinema_swap_offer` | the asked person | `{id, from: UserLite, from_kind, from_index, from_code, to_index, to_code, expires_at_ms}` |
| `cinema_swap_result` | the asker (and the asked person when it lapses) | `{id, status: accepted\|declined\|expired, user_id}` |

Every way a swap ends sends a result: answer, 30 s timeout, a new ask from the
same person, either side leaving or changing place (`expired`).

### Seating rules (server)

- Every member gets a place on join / accept / socket connect: the reserved
  seat, else the best free seat, else a couch spot, else `none`. Leaving /
  kick frees the place; a freed seat is offered to the queue first.
- Offers go only to sockets with `cinema=1`. If nobody in the queue can take
  a freed seat, an old app without a place simply gets it.
- While connected people wait and the hall is full, the seat of a member
  offline for 10 minutes goes to the queue (checked every 30 s); they get a
  place again when they come back.
- Errors: `CINEMA_OFF` (409, not a cinema room), `SEAT_TAKEN` (409),
  `SEAT_CHANGE_OFF` (403), `SWAP_UNAVAILABLE` (409 — target gone / offline /
  old app / blocked / the seat just became free), `OFFER_NOT_FOUND` (404).

### Clients (YouTube rules stay in force)

The official player is never warped or covered: for seat views it is pinned
to the projected screen as a plain rectangle (≥ 200 × 200; a far seat's
shorter picture is letterboxed onto the black screen masking). Looking around
(press and hold + drag) pans the whole picture — hall and screen move
together, the screen never turns — and every control fades out while it does,
so nothing can overlap the player. Only the overview (camera above the rows)
floats it, clamped to a free area. All chrome (header, mode row, time pill,
neighbours, chat) is laid out around it; the scrims, the house-lights dim +
screen glow and the "PIOKIO" wordmark under the screen are drawn under it. The
viewer can hide every control (eye button / a tap on the hall). Flying
reactions rise over the chat in YouTube rooms (over our own poster in
companion rooms). The green "speaking" ring = the person wrote in the chat a
moment ago (there is no voice chat).

The iOS app no longer offers friends' kept seats (product decision
2026-10-27): it sends only `enabled / seats / allow_seat_change / seat` and
never calls `GET /cinema/friends`. The server side of `reserve` stays as it
is (older builds of the same app may still send it).
