package xmpp

import (
	"fmt"
	"strconv"
	"strings"
)

// jid.go — mapping between Beanpon numeric user_ids / group ids and XMPP JIDs.
//
// Identity model (decided): JID == "<user_id>@<domain>". There is NO separate
// XMPP account or password; the existing JWT remains the single source of
// truth. So mapping is a pure string operation, no DB lookup needed.

// UserJID returns the bare JID for a user, e.g. 42 -> "42@beanpon.com".
func (c Config) UserJID(userID uint) string {
	return fmt.Sprintf("%d@%s", userID, c.Domain)
}

// UserJIDResource returns a full JID with a resource (device), e.g.
// "42@beanpon.com/iphone". Resources enable multi-device (Carbons).
func (c Config) UserJIDResource(userID uint, resource string) string {
	if resource == "" {
		return c.UserJID(userID)
	}
	return fmt.Sprintf("%d@%s/%s", userID, c.Domain, resource)
}

// GroupRoomJID maps a group conversation_id to its MUC room JID, e.g.
// 7 -> "group_7@groups.beanpon.com".
func (c Config) GroupRoomJID(conversationID uint) string {
	return fmt.Sprintf("group_%d@%s", conversationID, c.MUCHost)
}

// UserIDFromJID extracts the numeric user_id from a JID's local part. Accepts
// bare or full JIDs ("42@beanpon.com", "42@beanpon.com/iphone"). Returns
// (0, false) if the local part is not a positive integer (e.g. a MUC room).
func UserIDFromJID(jid string) (uint, bool) {
	local := localPart(jid)
	n, err := strconv.ParseUint(local, 10, 64)
	if err != nil || n == 0 {
		return 0, false
	}
	return uint(n), true
}

// GroupIDFromRoomJID extracts conversation_id from a "group_<id>@host" room
// JID. Returns (0, false) if the JID is not a group room.
func GroupIDFromRoomJID(jid string) (uint, bool) {
	local := localPart(jid)
	if !strings.HasPrefix(local, "group_") {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(local, "group_"), 10, 64)
	if err != nil || n == 0 {
		return 0, false
	}
	return uint(n), true
}

// localPart returns the part of a JID before '@', stripping any resource.
func localPart(jid string) string {
	if i := strings.IndexByte(jid, '@'); i >= 0 {
		return jid[:i]
	}
	if i := strings.IndexByte(jid, '/'); i >= 0 {
		return jid[:i]
	}
	return jid
}
