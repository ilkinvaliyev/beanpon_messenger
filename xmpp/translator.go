package xmpp

import "strconv"

// translator.go — converts between the legacy JSON message shape (the data
// the Hub already produces in HandleNewMessage / SendToMultipleUsers) and XMPP
// stanzas, in BOTH directions. This is what lets an OLD client and a NEW
// client talk to each other.
//
// Legacy outgoing payloads are plain map[string]interface{} (see
// websocket/hub.go HandleNewMessage). We translate those to stanzas for NEW
// recipients, and translate inbound stanzas back into the same map shape so
// the rest of the legacy pipeline is unchanged.

// DM1to1 describes a 1:1 chat message in neutral terms (transport-agnostic).
// Both the legacy path and the XMPP path can produce/consume this.
type DM1to1 struct {
	MessageID        string
	SenderID         uint
	ReceiverID       uint
	Text             string
	Kind             string // text/image/video/voice/call/...
	ReplyToMessageID string
	StoryID          string
}

// ToChatStanza builds an XMPP <message type="chat"> for a 1:1 message.
func (c Config) ToChatStanza(m DM1to1) Message {
	return Message{
		From:   c.UserJID(m.SenderID),
		To:     c.UserJID(m.ReceiverID),
		Type:   "chat",
		ID:     m.MessageID,
		Body:   m.Text,
		Active: &struct{}{},
		Meta: &Meta{
			Kind:             m.Kind,
			ReplyToMessageID: m.ReplyToMessageID,
			StoryID:          m.StoryID,
		},
	}
}

// FromChatStanza parses an inbound 1:1 stanza into a DM1to1. ok is false if the
// stanza is not a deliverable chat body (e.g. a pure typing/marker stanza).
func FromChatStanza(s Message) (m DM1to1, ok bool) {
	sender, sOK := UserIDFromJID(s.From)
	receiver, rOK := UserIDFromJID(s.To)
	if !sOK || !rOK {
		return DM1to1{}, false
	}
	if s.Body == "" {
		return DM1to1{}, false // typing/marker-only stanza, no body to persist
	}
	m = DM1to1{
		MessageID:  s.ID,
		SenderID:   sender,
		ReceiverID: receiver,
		Text:       s.Body,
		Kind:       "text",
	}
	if s.Meta != nil {
		if s.Meta.Kind != "" {
			m.Kind = s.Meta.Kind
		}
		m.ReplyToMessageID = s.Meta.ReplyToMessageID
		m.StoryID = s.Meta.StoryID
	}
	return m, true
}

// GroupMsg describes a group (group_chat_page) message in neutral terms.
type GroupMsg struct {
	MessageID      string
	ConversationID uint
	SenderID       uint
	Text           string
	Kind           string
}

// ToGroupStanza builds an XMPP <message type="groupchat"> targeted at the
// group's MUC room. The Go service publishes into the room on the sender's
// behalf after authorising the send.
func (c Config) ToGroupStanza(m GroupMsg) Message {
	return Message{
		From: c.UserJID(m.SenderID),
		To:   c.GroupRoomJID(m.ConversationID),
		Type: "groupchat",
		ID:   m.MessageID,
		Body: m.Text,
		Meta: &Meta{
			Kind:           m.Kind,
			ConversationID: strconv.FormatUint(uint64(m.ConversationID), 10),
		},
	}
}

// FromGroupStanza parses an inbound groupchat stanza into a GroupMsg.
func FromGroupStanza(s Message) (m GroupMsg, ok bool) {
	conv, cOK := GroupIDFromRoomJID(s.To)
	if !cOK && s.Meta != nil && s.Meta.ConversationID != "" {
		if n, err := strconv.ParseUint(s.Meta.ConversationID, 10, 64); err == nil {
			conv, cOK = uint(n), true
		}
	}
	sender, sOK := UserIDFromJID(s.From)
	if !cOK || !sOK || s.Body == "" {
		return GroupMsg{}, false
	}
	kind := "text"
	if s.Meta != nil && s.Meta.Kind != "" {
		kind = s.Meta.Kind
	}
	return GroupMsg{
		MessageID:      s.ID,
		ConversationID: conv,
		SenderID:       sender,
		Text:           s.Body,
		Kind:           kind,
	}, true
}

// LegacyDMPayload converts a DM1to1 into the exact map shape the legacy WS
// path emits (see websocket/hub.go HandleNewMessage), so an OLD recipient
// receives a byte-identical "new_message" to what they get today.
//
// NOTE: only the transport-relevant fields are filled here; the Hub enriches
// reply/story data exactly as it does for native WS messages.
func LegacyDMPayload(m DM1to1) map[string]interface{} {
	p := map[string]interface{}{
		"id":          m.MessageID,
		"sender_id":   m.SenderID,
		"receiver_id": m.ReceiverID,
		"text":        m.Text,
		"type":        m.Kind,
		"read":        false,
		"is_history":  false,
	}
	if m.ReplyToMessageID != "" {
		p["reply_to_message_id"] = m.ReplyToMessageID
	}
	return p
}
