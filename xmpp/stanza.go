package xmpp

import "encoding/xml"

// stanza.go — minimal XMPP stanza structs used by the bridge. We only model
// what chat_page / group_chat_page need; this is intentionally NOT a full
// XMPP implementation.

// Message is an XMPP <message> stanza (RFC 6121).
//
// Type is "chat" for 1:1 (chat_page) and "groupchat" for MUC (group_chat_page).
type Message struct {
	XMLName xml.Name `xml:"message"`
	From    string   `xml:"from,attr,omitempty"`
	To      string   `xml:"to,attr,omitempty"`
	Type    string   `xml:"type,attr,omitempty"`
	ID      string   `xml:"id,attr,omitempty"`

	Body string `xml:"body,omitempty"`

	// Chat State Notifications (XEP-0085): one of composing/paused/active/
	// inactive/gone — replaces our typing/typing_stop/recording events.
	Composing *struct{} `xml:"http://jabber.org/protocol/chatstates composing,omitempty"`
	Paused    *struct{} `xml:"http://jabber.org/protocol/chatstates paused,omitempty"`
	Active    *struct{} `xml:"http://jabber.org/protocol/chatstates active,omitempty"`

	// Chat Markers (XEP-0333): received / displayed (read) markers — replace
	// our mark_read / read receipts.
	Received  *Marker `xml:"urn:xmpp:chat-markers:0 received,omitempty"`
	Displayed *Marker `xml:"urn:xmpp:chat-markers:0 displayed,omitempty"`

	// Beanpon extension element carrying our domain-specific metadata
	// (message type image/video/voice/call, reply_to, story_id, ...). This is
	// how rich features survive the trip through XMPP without losing data.
	Meta *Meta `xml:"urn:beanpon:msg:0 meta,omitempty"`
}

// Marker is the id reference inside a chat marker.
type Marker struct {
	ID string `xml:"id,attr"`
}

// Meta is the Beanpon-private payload attached to a stanza.
type Meta struct {
	// Kind mirrors the legacy "type" field: text/image/video/voice/call/...
	Kind             string `xml:"kind,attr,omitempty"`
	ReplyToMessageID string `xml:"reply_to,attr,omitempty"`
	StoryID          string `xml:"story_id,attr,omitempty"`
	// ConversationID is set for groupchat so the Go side can route/authorise.
	ConversationID string `xml:"conversation_id,attr,omitempty"`
}

// Presence is an XMPP <presence> stanza — used to observe who is online over
// XMPP and to populate the Registry.
type Presence struct {
	XMLName xml.Name `xml:"presence"`
	From    string   `xml:"from,attr,omitempty"`
	To      string   `xml:"to,attr,omitempty"`
	Type    string   `xml:"type,attr,omitempty"` // "", "unavailable", ...
}

// IQ is an XMPP <iq> stanza — used for component handshakes and queries. Kept
// minimal; extended as needed.
type IQ struct {
	XMLName xml.Name `xml:"iq"`
	From    string   `xml:"from,attr,omitempty"`
	To      string   `xml:"to,attr,omitempty"`
	Type    string   `xml:"type,attr,omitempty"`
	ID      string   `xml:"id,attr,omitempty"`
}
