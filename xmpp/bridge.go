package xmpp

import "log"

// bridge.go — the Router/orchestrator. This is the ONLY type the rest of the
// service talks to. The Hub holds a *Bridge and calls it at two seams:
//
//   EGRESS  (outgoing): after the Hub has already authorised + persisted a
//           message, it asks the Bridge to deliver. The Bridge picks XMPP or
//           legacy WS per recipient (RouteDM / RouteGroup).
//
//   INGRESS (incoming): the component receives a stanza from a NEW client; the
//           Bridge hands it back to the Hub through IngressSink so the normal
//           pipeline (permission, spam, moderation, persistence, fan-out)
//           runs unchanged.
//
// Crucially the Bridge contains NO business logic. It only routes by transport.

// LegacyDelivery is implemented by the Hub. It is how the Bridge delivers to an
// OLD (WebSocket) recipient — exactly the call the Hub already uses internally.
type LegacyDelivery interface {
	// SendToUserLegacy pushes a payload to a single user's legacy WS channel.
	SendToUserLegacy(userID uint, messageType string, data interface{})
}

// IngressSink is implemented by the Hub. It is how an inbound XMPP message
// re-enters the normal pipeline. These mirror the Hub's existing entry points
// so all business logic is reused.
type IngressSink interface {
	// IngestDM handles a 1:1 message that arrived over XMPP. The Hub must run
	// the SAME permission/spam/moderation/persistence path as a WS
	// send_message, then fan out (which may route back through the Bridge).
	IngestDM(senderID, receiverID uint, text, kind, replyToID, storyID string)

	// IngestGroup handles a group message that arrived over XMPP. The Hub must
	// run the SAME group-permission/persistence/fan-out path as the REST
	// SendGroupMessage handler.
	IngestGroup(senderID, conversationID uint, text, kind string)

	// GroupMemberIDs returns the member user_ids of a group conversation, used
	// to fan a group message out to legacy members.
	GroupMemberIDs(conversationID uint) []uint
}

// Bridge ties together the registry, component and translator.
type Bridge struct {
	cfg    Config
	reg    *Registry
	comp   *Component
	legacy LegacyDelivery
	sink   IngressSink
}

// NewBridge constructs the bridge. registry, legacy and sink must be non-nil
// when cfg.Enabled. The component is created here and started by Start.
func NewBridge(cfg Config, reg *Registry, legacy LegacyDelivery, sink IngressSink) *Bridge {
	b := &Bridge{cfg: cfg, reg: reg, legacy: legacy, sink: sink}
	b.comp = NewComponent(cfg, b) // Bridge is the StanzaHandler
	return b
}

// Start launches the component connection loop (no-op if disabled).
func (b *Bridge) Start() {
	if !b.cfg.Enabled {
		return
	}
	go b.comp.Run()
}

// Registry exposes the registry so the Hub/auth endpoint can mark users.
func (b *Bridge) Registry() *Registry { return b.reg }

// Enabled reports whether the subsystem is active.
func (b *Bridge) Enabled() bool { return b.cfg.Enabled }

// ── EGRESS ────────────────────────────────────────────────────────────────

// RouteDM delivers an already-authorised 1:1 message to a single recipient,
// choosing transport based on the recipient's current capability.
//
// Returns true if the message was handed to the XMPP transport; false means it
// was delivered (or should be delivered) over the legacy WS path. The Hub uses
// the return value to avoid double-delivery.
func (b *Bridge) RouteDM(m DM1to1) (viaXMPP bool) {
	if !b.cfg.Enabled {
		return false
	}
	if b.reg.IsXMPP(m.ReceiverID) {
		stanza := b.cfg.ToChatStanza(m)
		if err := b.comp.SendMessage(stanza); err != nil {
			log.Printf("[xmpp] RouteDM send failed for user %d: %v — falling back to legacy", m.ReceiverID, err)
			return false // fall back so the message is never lost
		}
		return true
	}
	return false // recipient is OLD → legacy path
}

// RouteGroup delivers an already-authorised group message. NEW members get it
// via the MUC room (one publish); OLD members get it via legacy fan-out. The
// Hub passes legacyMembers = members that are NOT XMPP-capable.
func (b *Bridge) RouteGroup(m GroupMsg, payloadForLegacy map[string]interface{}) {
	if !b.cfg.Enabled {
		// Caller should have fanned out via legacy already.
		return
	}
	// 1) Publish once into the MUC room → every NEW member receives it.
	stanza := b.cfg.ToGroupStanza(m)
	if err := b.comp.SendMessage(stanza); err != nil {
		log.Printf("[xmpp] RouteGroup MUC publish failed (conv %d): %v", m.ConversationID, err)
	}
	// 2) Legacy members are delivered by the Hub via SendToUserLegacy; see
	//    Hub.fanOutGroup which asks the registry per-member. We do not fan out
	//    legacy here to keep a single fan-out owner (the Hub).
	_ = payloadForLegacy
}

// ── INGRESS (StanzaHandler) ─────────────────────────────────────────────────

// OnMessage is invoked by the component for each inbound stanza.
func (b *Bridge) OnMessage(s Message) {
	switch s.Type {
	case "chat", "":
		if dm, ok := FromChatStanza(s); ok {
			b.sink.IngestDM(dm.SenderID, dm.ReceiverID, dm.Text, dm.Kind, dm.ReplyToMessageID, dm.StoryID)
		}
		// typing / markers (no body) could be forwarded here later.
	case "groupchat":
		if gm, ok := FromGroupStanza(s); ok {
			b.sink.IngestGroup(gm.SenderID, gm.ConversationID, gm.Text, gm.Kind)
		}
	}
}

// OnPresence updates the registry when an XMPP client appears/disappears.
func (b *Bridge) OnPresence(p Presence) {
	uid, ok := UserIDFromJID(p.From)
	if !ok {
		return
	}
	if p.Type == "unavailable" {
		// Do not blindly forget — the user may still have a legacy session.
		// The Hub owns legacy presence; here we only clear XMPP capability.
		if b.reg.Of(uid) == CapXMPP {
			b.reg.Forget(uid)
		}
		return
	}
	resource := resourceOf(p.From)
	b.reg.MarkXMPP(uid, resource)
}

func resourceOf(jid string) string {
	for i := 0; i < len(jid); i++ {
		if jid[i] == '/' {
			return jid[i+1:]
		}
	}
	return ""
}
