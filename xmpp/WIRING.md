# XMPP bridge — wiring guide

This document shows the **exact, minimal** edits to plug the bridge into the
existing service. The bridge files (`xmpp/*.go`) are self-contained and do not
touch the live `websocket/hub.go` or `cmd/main/main.go` yet — these are the
changes to make when you switch the subsystem on.

> Scope reminder: only `chat_page` (1:1) and `group_chat_page` (groups). Live /
> Rave / Mafia are untouched.

---

## 1. main.go — construct and start the bridge

```go
// after wsHub is created and before router setup:
xmppCfg := xmpp.LoadConfig()
xmppReg := xmpp.NewRegistry(2 * time.Minute)

// wsHub must implement xmpp.LegacyDelivery + xmpp.IngressSink (see §2).
xmppBridge := xmpp.NewBridge(xmppCfg, xmppReg, wsHub, wsHub)
wsHub.AttachXMPP(xmppBridge)   // give the Hub a handle (see §3)
xmppBridge.Start()             // no-op if XMPP_ENABLED=false
```

```go
// inside the existing /internal group:
internal.POST("/xmpp/auth", xmppBridge.AuthHandler(cfg.JWTSecret))
```

That is the entire main.go footprint.

---

## 2. Hub must satisfy two tiny interfaces

`xmpp.LegacyDelivery` and `xmpp.IngressSink` are deliberately thin and map onto
methods the Hub already has. Add an adapter file, e.g.
`websocket/hub_xmpp.go`:

```go
package websocket

import "beanpon_messenger/xmpp"

// SendToUserLegacy is just the existing SendToUser (legacy WS path).
func (h *Hub) SendToUserLegacy(userID uint, messageType string, data interface{}) {
    h.SendToUser(userID, messageType, data)
}

// IngestDM re-enters the SAME pipeline a `send_message` uses. Reuse the exact
// permission + spam + moderation + persistence + fan-out code path. The
// simplest correct approach is to factor the body of the hub.go
// `case "send_message":` block into a method and call it here.
func (h *Hub) IngestDM(senderID, receiverID uint, text, kind, replyToID, storyID string) {
    h.processIncomingDM(senderID, receiverID, text, kind, replyToID, storyID)
}

// IngestGroup re-enters the SAME pipeline the REST SendGroupMessage uses.
func (h *Hub) IngestGroup(senderID, conversationID uint, text, kind string) {
    h.processIncomingGroup(senderID, conversationID, text, kind)
}

// GroupMemberIDs exposes membership for legacy fan-out.
func (h *Hub) GroupMemberIDs(conversationID uint) []uint {
    return getGroupParticipantIDs(conversationID)
}

// AttachXMPP stores the bridge handle on the Hub.
func (h *Hub) AttachXMPP(b *xmpp.Bridge) { h.xmpp = b }
```

Add one field to the `Hub` struct in `hub.go`:

```go
xmpp *xmpp.Bridge // nil when XMPP disabled
```

---

## 3. The EGRESS seam (the heart of backward compatibility)

Today every outgoing chat message is delivered via `SendToUser` /
`SendToMultipleUsers`. Introduce a single choke point so the recipient's
capability decides the transport.

### 1:1 (chat_page)

In `HandleNewMessage`, where it currently sends the `new_message` event to the
receiver, replace the direct `SendToUser` with:

```go
if h.xmpp != nil && h.xmpp.Enabled() {
    if h.xmpp.RouteDM(xmpp.DM1to1{
        MessageID: messageID, SenderID: senderID, ReceiverID: receiverID,
        Text: content, Kind: msgType, ReplyToMessageID: derefStr(replyToMessageID),
    }) {
        // delivered over XMPP → do NOT also send over legacy WS
    } else {
        h.SendToUser(receiverID, "new_message", messageData) // OLD client
    }
} else {
    h.SendToUser(receiverID, "new_message", messageData)
}
```

The sender's own echo and the conversation-list update stay exactly as they are.

### Group (group_chat_page)

In the group fan-out (handlers/group_message_handler.go around
`SendToMultipleUsers(memberIDs, "new_group_message", wsPayload)`), split members
by capability:

```go
var legacyMembers []uint
for _, mid := range memberIDs {
    if h.xmpp != nil && h.xmpp.Registry().IsXMPP(mid) {
        continue // will receive via the MUC room
    }
    legacyMembers = append(legacyMembers, mid)
}
// NEW members: one MUC publish
if h.xmpp != nil && h.xmpp.Enabled() {
    h.xmpp.RouteGroup(xmpp.GroupMsg{
        MessageID: messageID, ConversationID: conversationID,
        SenderID: senderID, Text: content, Kind: msgType,
    }, wsPayload)
}
// OLD members: legacy fan-out, unchanged
h.SendToMultipleUsers(legacyMembers, "new_group_message", wsPayload)
```

---

## 4. The INGRESS seam

When a NEW client sends a message, ejabberd routes the stanza to the component;
`Bridge.OnMessage` calls `IngestDM` / `IngestGroup`. Those re-enter the **same**
Hub pipeline as a legacy send — so permission checks, spam shadow-ban, AI
moderation, persistence and fan-out all run identically. The fan-out then uses
the EGRESS seam above, which may route the message back out to an OLD recipient
over legacy WS. That closes the loop:

```
A (new, XMPP) --stanza--> ejabberd --> component --> Bridge.OnMessage
   --> Hub.IngestDM  (permission/spam/moderation/persist)
   --> Hub fan-out --> RouteDM(B)
        B new?  -> XMPP stanza
        B old?  -> legacy WS  "new_message"
```

---

## 5. Presence / registry

- Legacy connect: in `registerClient`, call `h.xmpp.Registry().MarkLegacy(userID)`
  when the bridge is present.
- XMPP connect: handled automatically — the auth callback marks the user XMPP,
  and `<presence>` keeps it fresh (2-minute TTL).
- This is what guarantees: *if both users are on the new (XMPP) version, the
  conversation runs fully over XMPP; if either is still on the old version, the
  bridge translates for that leg.*

---

## 6. Rollout switch

- `XMPP_ENABLED=false` → every seam above takes the legacy branch. The service
  is byte-for-byte its current self. Ship the code dark, enable per environment.
- Turn on for staging, validate 1:1 then groups, then production.
- Retire the legacy branches only once old app versions fall below your
  threshold.
```
