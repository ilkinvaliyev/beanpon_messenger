// Package xmpp is the Beanpon Messenger XMPP bridge.
//
// SCOPE — IMPORTANT
//
//	This package exists ONLY for chat_page (1:1 DM) and group_chat_page
//	(groups). Live / Rave / Mafia hubs are out of scope and keep their own
//	WebSocket hubs untouched.
//
// WHY IT EXISTS
//
//	The product is migrating its messaging transport from the bespoke
//	WebSocket+JSON protocol (websocket/hub.go) to standard XMPP, served by an
//	ejabberd backbone. This package is the "bridge": it lets the existing Go
//	service speak XMPP without throwing away any of its business logic.
//
// THE GOLDEN RULE
//
//	XMPP standardises TRANSPORT, not business logic. Every permission check,
//	spam shadow-ban, AI moderation enqueue, per-user setting (mute/archive/
//	pin/nickname/wallpaper/screenshot) and group permission stays exactly
//	where it is today — in the Hub and the handlers. The bridge only changes
//	(a) how an already-approved outgoing message reaches the recipient and
//	(b) how an incoming message arrives before it enters the normal pipeline.
//
// THE THREE PIECES
//
//	Component  — a stdlib-only XEP-0114 external-component client that
//	             connects to ejabberd (bridge.beanpon.com) and exchanges
//	             stanzas. No third-party XMPP dependency.
//
//	Registry   — the capability registry: which user_ids are currently on a
//	             NEW (XMPP-capable) client vs an OLD (legacy WebSocket) client.
//	             This is the single source of truth the Router consults.
//
//	Router     — decides, per recipient, whether a message goes out over XMPP
//	             or over the legacy WebSocket Send channel. This is the heart
//	             of backward compatibility.
//
//	Translator — converts between the legacy JSON message shape and XMPP
//	             stanzas, in both directions, so OLD and NEW clients can talk.
//
// BACKWARD-COMPATIBILITY CONTRACT (the behaviour the user asked for)
//
//	Let A and B be the two participants of a chat (or, for groups, any pair of
//	members). For each recipient independently:
//
//	  recipient on NEW client (XMPP)  -> deliver as an XMPP stanza
//	  recipient on OLD client (WS)    -> deliver over the legacy WS channel
//
//	Therefore:
//	  - A(old)  <-> B(new):  bridge translates each direction. Both can chat.
//	  - A(new)  <-> B(new):  both legs are XMPP — the "fully migrated" path.
//	  - A(old)  <-> B(old):  pure legacy, XMPP not involved at all.
//
//	A user is "NEW" the moment they connect with an XMPP-capable client; from
//	then on their traffic uses XMPP. No flag flip, no data migration — the
//	Registry observes the live connection and routes accordingly.
package xmpp
