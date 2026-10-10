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
