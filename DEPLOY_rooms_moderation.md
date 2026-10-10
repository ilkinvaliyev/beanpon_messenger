# Rooms — moderation, privacy, timeline events (2026-10-10)

Deploy order:
1. Laravel: `php artisan migrate` (`2026_10_10_100000_add_room_moderation_features`) and deploy Laravel
   (new cloud route `POST /api/cloud/notification/new-room-message`).
2. Messenger (this service). The room SELECT/INSERT paths reference the new columns/tables.

What changed (all additive for old clients):
- Owner-block cascade: a user blocked with the room owner (either way) gets 404 on every room endpoint and
  never sees the owner's rooms in any list or push.
- Hide: `POST /rooms/:id/hide|unhide`, `GET /rooms/hidden` (hidden rooms show nowhere; joining unhides).
- Admin: `DELETE /rooms/:id/members/:user_id` (kick), `PUT /rooms/:id/members/:user_id/write-block`,
  `PUT /rooms/:id/settings` (`screenshot_disabled`, `messaging_locked` + `lock_hours` 8/24, 0 = until reopened).
  Admins may promote; only the owner demotes.
- Send refuses `ROOM_LOCKED` / `ROOM_WRITE_BLOCKED` (403) for non-admins — covers post shares too.
- `GET /rooms/:id/members`: outsiders → empty, members → owner + admins, admins → everyone (newest first,
  `q`, `limit`, `offset`, `write_blocked`), plus `total`.
- Join/leave lines (`room_events`): WS `room_member_event` to the members of that moment; REST
  `GET /rooms/:id/messages?include_events=1` returns only events at/after the viewer's `joined_at`.
- Messages: two-way block filter fixed, paging with `before_id` + `has_more`, member-only
  `GET /rooms/:id/messages/search`, reactors `GET /rooms/:id/messages/:mid/reactions`.
- Deleting a message stays a soft delete + `room_message_deletions` audit row.
- Room media + avatars are now marked referenced (the 24 h orphan-media sweep used to delete them).
- Push: `ScheduleRoomPushNotification` → Laravel `new-room-message` (route `room_chat_page`), skipping members
  who read past the message, muted, hidden or owner-blocked members.
- `GET /rooms?exclude_joined=1` (Rooms tab), `GET /rooms/share-targets` (share sheet ranking).

## Room send idempotency (2026-10-10, additive — no migration)

`POST /rooms/:id/messages` for the iOS room cache + durable send queue:
- `client_message_id` (optional UUID) becomes the message id in canonical lowercase (the DB returns uuids
  lowercase; the reply, WS echo, delete and reaction paths all use that spelling). Not a UUID → 400
  `INVALID_CLIENT_MESSAGE_ID`. Missing → server UUID as before.
- An id that is already stored is answered BEFORE the freeze / lock / write-block / membership gates and
  before the auto-join: same sender + same room → `{"duplicate": true, "data": <full payload>}` (soft-deleted
  → `"deleted": true` + only `data.id`); another sender / chat → 409 `CLIENT_MESSAGE_ID_CONFLICT`. Counters,
  WS fan-out and push are never repeated.
- `require_member: true` (new clients) → 403 `ROOM_NOT_MEMBER` instead of the one-tap auto-join, so a queued
  retry never re-joins a user who left / was removed. Old clients omit it → unchanged behaviour.
- Delete / reaction endpoints canonicalize the message id the same way.
- The send reply and WS `new_room_message` carry `created_at` with the stored microsecond precision (same
  value as `GET /rooms/:id/messages`; it used to be cut to whole seconds, which put live messages inside a
  history page's time span on clients).

## Entry bans + unique room names (2026-10-10)

Deploy order: Laravel `php artisan migrate` (`2026_10_10_200000_add_room_bans_and_unique_names`) BEFORE this
messenger build — every room endpoint reads `room_bans`.

- Kick (`DELETE /rooms/:id/members/:user_id`) = entry ban: a `room_bans` row is written first, then the
  membership goes (deleted by room + user, so a racing join can't survive; `ensureMember` re-checks the ban
  after its own insert). The banned user gets 404 `ROOM_NOT_FOUND` on every room endpoint (`loadVisibleRoom`),
  the room is filtered from discover, chats, hidden rooms and pushes, join is impossible, and an admin can't
  promote them (403 `ROOM_USER_BANNED`). The owner can never be banned. WS `room_member_removed` now carries
  `banned: true` (new clients close the room; old ones show the reader state and hit the 404). The delayed
  room push re-checks membership + bans when it fires (left / removed during the delay → no push), and WS
  fan-out, the chats list and the share sheet skip banned users even if a membership row were left behind.
- Admins only: `GET /rooms/:id/bans?limit=&offset=` → `{bans: [{user_id, name, username, is_verified,
  profile_image, banned_at, banned_by_username}], total, has_more}` (newest first) and
  `DELETE /rooms/:id/bans/:user_id` (lift; idempotent). A lifted user can find and join again — not re-added.
- Room names are unique among live rooms, compared `lower(btrim(name))`: create and rename trim the name, take
  a per-name advisory lock and check inside one transaction → 409 `ROOM_NAME_TAKEN`; empty / >255 chars →
  400 `ROOM_NAME_INVALID` (an empty name on `PUT /rooms/:id` still means "no change"). The migration adds the
  partial unique index `rooms_name_unique_ci` — if live duplicates already exist it creates the lookup index
  `rooms_name_lookup_ci` instead and logs them (rename them, then add the unique index in a follow-up
  migration); the messenger check works either way.
