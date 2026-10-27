# Chat streaks ("seriya") — messenger side (2026-10-27)

Every rule lives in **piokio_community** (contract: `piokio_community/docs/CHAT_STREAKS.md`).
The messenger only signals and relays — never add streak logic here.

## What this service does

- `websocket/chat_streak.go`
  - `HandleNewMessage` (every persisted DM: WS, REST, XMPP, media) calls
    `signalChatStreak(sender, receiver, type)` → async worker pool (2 workers, queue 2048) →
    community `POST {COMMUNITY_INTERNAL_URL}/internal/chat-streak/activity`
    `{sender_id, receiver_id}` with `X-Api-Key: COMMUNITY_INTERNAL_API_KEY`.
  - Never blocks the send path; skips `call` rows and self chats; at most once per
    direction per Baku day per instance (a failed call is retried on the next message —
    community is idempotent anyway).
  - The community reply's `notify` list (`[{user_id, payload}]`) is sent to those users as
    the WS event **`chat_streak`** (`SendToUser`, works across the WS cluster).
  - `POST /internal/chat-streak/notify` (`X-Internal-Secret`, ≤ 50 items) — community pushes
    events that start there (a restore) the same way (`RelayChatStreak`).
- Old apps ignore the unknown WS type.

## Env

| Variable | Value |
|---|---|
| `COMMUNITY_INTERNAL_URL` | base URL of piokio_community (internal network if possible), no trailing `/` |
| `COMMUNITY_INTERNAL_API_KEY` | = piokio_community `INTERNAL_API_KEY` |

`COMMUNITY_INTERNAL_URL` empty → nothing is signalled (streaks simply do not count).
community needs `MESSENGER_INTERNAL_URL` (this service) + `MESSENGER_INTERNAL_SECRET`
(= this service's `INTERNAL_SECRET`) for the relay.

## Deploy order

1. Laravel migration `2026_10_27_000000_create_chat_streak_tables`
2. piokio_community
3. **this service** (with the two envs above)
4. gateway (`/v3/chat-streaks*` → community)
5. apps
