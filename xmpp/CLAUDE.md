# CLAUDE.md — XMPP subsystem (beanpon_messenger)

Instructions for Claude when working inside `xmpp/`. This subsystem migrates the
**messaging transport** of `chat_page` (1:1 DM) and `group_chat_page` (groups)
from the bespoke WebSocket+JSON protocol to standard **XMPP**, served by an
**ejabberd** backbone, while keeping old app versions working through a bridge.

> Read this together with the repo-root `CLAUDE.md`. Everything there still
> applies (English comments, surgical changes, simplicity-first, memory/perf,
> ask before adding dependencies). This file adds the XMPP-specific rules.

---

## 1. Scope — read this first

- **In scope:** ONLY `chat_page` (1:1 direct messages) and `group_chat_page`
  (group messages).
- **Out of scope, do NOT touch:** the Live (`/ws/live`), Rave (`/ws/rave`) and
  Mafia hubs. They keep their own WebSocket hubs. 1:1 **voice calls** also stay
  as they are (signaling over `/internal/calls/*` + LiveKit media) — XMPP does
  not replace calling.

## 2. The one principle that governs everything

**XMPP standardises TRANSPORT, not business logic.**

All of the following stay exactly where they are today — in `websocket/hub.go`
and the `handlers/` — and are reused unchanged:

- conversation permission model (`follow_based` / `request_based`,
  `pending`/`active`/`restricted`, `MaxPendingMessages`)
- spam shadow-ban (`IsMessagingBannedByActions`)
- AI moderation enqueue (`moderationEnqueue`)
- per-user settings (mute / archive / pin / nickname / wallpaper / screenshot)
- group permission matrix, roles, invites, pinned message
- AES message encryption in `services/encryption.go`
- PostgreSQL `messages` / `conversations` schema

The bridge must never re-implement or bypass any of these. If a change here
would duplicate a permission/spam/moderation rule, stop — route through the Hub
instead.

## 3. Architecture

```
            NEW app (XMPP)                         OLD app (legacy WS)
                 │                                        │
          c2s / wss│ (JWT auth via /internal/xmpp/auth)   │ /ws (JWT)
                 ▼                                        ▼
        ┌───────────────────┐   XEP-0114 component  ┌───────────────────┐
        │     ejabberd      │◄────────5347─────────►│   Go service      │
        │  (transport only) │                       │  Hub + handlers   │
        │  MUC, MAM, push   │                       │  (ALL business    │
        └───────────────────┘                       │   logic + DB)     │
                                                     └───────────────────┘
                                                        ▲          │
                                              xmpp.Bridge (router) │
                                                 reg / comp / xlate│
```

- **Identity:** JID == `<user_id>@beanpon.com`. No separate XMPP password; the
  existing **JWT** authenticates XMPP clients via ejabberd's HTTP auth backend
  calling `POST /internal/xmpp/auth` (`auth_handler.go`).
- **Bridge link:** the Go service joins ejabberd as an **external component**
  (XEP-0114, `component.go`) — stdlib only, no third-party XMPP dependency.
- **Groups:** a group `conversation_id` maps to MUC room
  `group_<conversation_id>@groups.beanpon.com`.

## 4. Files in this package

| File | Responsibility |
|---|---|
| `doc.go` | Package overview + backward-compat contract. |
| `config.go` | `XMPP_*` env config. `Enabled=false` ⇒ whole subsystem is a no-op. |
| `jid.go` | user_id ↔ JID and group_id ↔ MUC room mapping. |
| `registry.go` | Capability registry: is a user on a NEW (XMPP) or OLD (WS) client. The brain of routing. |
| `stanza.go` | Minimal `<message>/<presence>/<iq>` structs + Beanpon `<meta>` extension. |
| `translator.go` | JSON legacy shape ↔ XMPP stanza, both directions. |
| `component.go` | stdlib XEP-0114 component client (handshake + read/write loop, auto-reconnect). |
| `bridge.go` | Router/orchestrator. The ONLY type the Hub talks to. EGRESS (`RouteDM`/`RouteGroup`) + INGRESS (`OnMessage`/`OnPresence`). No business logic. |
| `auth_handler.go` | ejabberd → Go JWT validation endpoint. Marks user XMPP-capable on success. |
| `deploy/` | `ejabberd.yml`, `docker-compose.xmpp.yml`, `.env.xmpp.example`. |
| `WIRING.md` | Exact, minimal edits to plug the bridge into `main.go` and the Hub. |

## 5. Backward-compatibility contract (the behaviour we promised)

For each recipient **independently**, transport is chosen by their current
capability in the `Registry`:

- recipient on **NEW** client → deliver as an XMPP stanza
- recipient on **OLD** client → deliver over the legacy WS channel

Consequences:

- `A(old) ↔ B(new)` — bridge translates each leg; both chat normally.
- `A(new) ↔ B(new)` — both legs XMPP (the fully-migrated path).
- `A(old) ↔ B(old)` — pure legacy; XMPP not involved.

A user becomes NEW the instant they connect with an XMPP client (auth callback
+ presence). No flag flip, no data migration. **When both users are on the new
version, the conversation runs fully over XMPP; until then the bridge keeps
old↔new working.**

## 6. The two seams (where bridge meets Hub)

- **EGRESS** — after the Hub has authorised + persisted a message, it asks the
  Bridge to deliver. `RouteDM` returns whether it went out over XMPP so the Hub
  avoids double-delivery; group fan-out splits members into MUC (NEW) + legacy
  (OLD). See `WIRING.md` §3.
- **INGRESS** — a stanza from a NEW client reaches `Bridge.OnMessage`, which
  calls `IngestDM`/`IngestGroup` on the Hub. These MUST re-enter the SAME
  pipeline a legacy `send_message` / REST group send uses. See `WIRING.md` §4.

Never deliver a message from inside the bridge without it having passed the
Hub's authorisation pipeline.

## 7. Rules for changing this subsystem

1. **Keep transport and logic separate.** New permission/moderation behaviour
   goes in the Hub/handlers, not here. The bridge only routes and translates.
2. **No silent message loss.** If an XMPP send fails, fall back to legacy
   (`RouteDM` already does). Any new path must preserve "deliver or fall back".
3. **No new third-party XMPP dependency** without asking (root CLAUDE.md rule).
   The component is intentionally stdlib-only.
4. **Don't modify the Go `messages`/`conversations` schema** for XMPP. ejabberd
   owns its own tables (MAM/MUC/offline) in a separate `ejabberd` database.
5. **Feature parity via `<meta>`.** Rich fields (kind=image/video/voice/call,
   reply_to, story_id, conversation_id) travel in the `urn:beanpon:msg:0 meta`
   element so nothing is lost crossing XMPP. Extend `Meta` rather than stuffing
   data into the body.
6. **Respect the kill switch.** Every code path must be a no-op when
   `XMPP_ENABLED=false`; the service must behave byte-for-byte like today.
7. **Memory/perf (root §2.5):** the component read loop, reconnect goroutine and
   any timers must clean up; no goroutine leaks on disconnect; the registry is
   the only shared mutable state and is mutex-guarded.

## 8. Build & run

```bash
# Bring up ejabberd next to the Go service:
docker compose -f docker-compose.yml -f xmpp/deploy/docker-compose.xmpp.yml up -d

# Go service: add the vars from xmpp/deploy/.env.xmpp.example to .env.
# Start with XMPP_ENABLED=false in production until validated in staging.
```

Build check (the Go toolchain is required; this repo vendored, stdlib-only):

```bash
go build ./xmpp/...
```

## 9. Status

Skeleton / phase-1. Implemented: config, JID mapping, registry, stanza model,
translator, stdlib component client, router/bridge, JWT auth endpoint, deploy
config, wiring guide. **Not yet wired into the live Hub** — apply `WIRING.md`
when enabling. Typing/markers (XEP-0085/0333) and MAM history sync are stubbed
for a later phase.
