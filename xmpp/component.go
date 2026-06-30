package xmpp

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

// component.go — a minimal XEP-0114 ("Jabber Component Protocol") client built
// on the standard library only (net + encoding/xml). No third-party XMPP
// dependency, consistent with the project's stdlib-only philosophy.
//
// The handshake (XEP-0114):
//  1. Open a TCP stream to ejabberd's component port (5347).
//  2. Send <stream:stream to="bridge.beanpon.com" ...>.
//  3. Server replies with a stream header containing a stream id.
//  4. Send <handshake>SHA1(streamID + secret)</handshake>.
//  5. Server replies with an empty <handshake/> on success.
//
// After that, the component may send/receive <message>/<presence>/<iq> for any
// JID on its domain. We send messages "from" user JIDs (the component is
// trusted by the server to do so).

// StanzaHandler is called for each inbound stanza routed to the component.
type StanzaHandler interface {
	OnMessage(Message)
	OnPresence(Presence)
}

// Component is a long-lived connection to ejabberd as an external component.
type Component struct {
	cfg     Config
	handler StanzaHandler

	mu      sync.Mutex
	conn    net.Conn
	enc     *xml.Encoder
	dec     *xml.Decoder
	closed  bool
	writeMu sync.Mutex
}

// NewComponent creates a component bound to cfg, delivering inbound stanzas to
// handler.
func NewComponent(cfg Config, handler StanzaHandler) *Component {
	return &Component{cfg: cfg, handler: handler}
}

// Run connects and then reads stanzas until the connection drops, reconnecting
// with a simple backoff. Call it in its own goroutine. It returns only if the
// component is disabled.
func (cp *Component) Run() {
	if !cp.cfg.Enabled {
		log.Printf("[xmpp] component disabled (XMPP_ENABLED=false) — no-op")
		return
	}
	backoff := time.Second
	for {
		if err := cp.connectAndServe(); err != nil {
			log.Printf("[xmpp] component connection ended: %v (retry in %s)", err, backoff)
		}
		cp.mu.Lock()
		closed := cp.closed
		cp.mu.Unlock()
		if closed {
			return
		}
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (cp *Component) connectAndServe() error {
	addr := fmt.Sprintf("%s:%d", cp.cfg.ComponentHost, cp.cfg.ComponentPort)
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	cp.mu.Lock()
	cp.conn = conn
	cp.enc = xml.NewEncoder(conn)
	cp.dec = xml.NewDecoder(conn)
	cp.mu.Unlock()

	streamID, err := cp.openStream()
	if err != nil {
		conn.Close()
		return fmt.Errorf("open stream: %w", err)
	}
	if err := cp.authenticate(streamID); err != nil {
		conn.Close()
		return fmt.Errorf("handshake: %w", err)
	}
	log.Printf("[xmpp] component %s connected to %s", cp.cfg.ComponentName, addr)

	// reset backoff implicitly handled by caller; now read loop.
	return cp.readLoop()
}

// openStream sends the component stream header and returns the server's stream
// id used to compute the handshake.
func (cp *Component) openStream() (string, error) {
	header := fmt.Sprintf(
		`<?xml version="1.0"?><stream:stream xmlns="jabber:component:accept" `+
			`xmlns:stream="http://etherx.jabber.org/streams" to="%s">`,
		cp.cfg.ComponentName)
	if _, err := cp.conn.Write([]byte(header)); err != nil {
		return "", err
	}
	// Read the opening <stream:stream ... id="..."> from the server.
	for {
		tok, err := cp.dec.Token()
		if err != nil {
			return "", err
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "stream" {
			for _, a := range se.Attr {
				if a.Name.Local == "id" {
					return a.Value, nil
				}
			}
			return "", fmt.Errorf("stream header without id")
		}
	}
}

// authenticate performs the SHA1(streamID+secret) handshake.
func (cp *Component) authenticate(streamID string) error {
	sum := sha1.Sum([]byte(streamID + cp.cfg.ComponentSecret))
	digest := hex.EncodeToString(sum[:])
	if _, err := cp.conn.Write([]byte("<handshake>" + digest + "</handshake>")); err != nil {
		return err
	}
	// Expect an empty <handshake/> back.
	for {
		tok, err := cp.dec.Token()
		if err != nil {
			return err
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "handshake" {
			return nil
		}
		if _, ok := tok.(xml.EndElement); ok {
			// server closed without handshake → auth failed
		}
	}
}

// readLoop decodes inbound stanzas and dispatches them to the handler.
func (cp *Component) readLoop() error {
	for {
		tok, err := cp.dec.Token()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "message":
			var m Message
			if err := cp.dec.DecodeElement(&m, &se); err == nil && cp.handler != nil {
				cp.handler.OnMessage(m)
			}
		case "presence":
			var p Presence
			if err := cp.dec.DecodeElement(&p, &se); err == nil && cp.handler != nil {
				cp.handler.OnPresence(p)
			}
		default:
			// iq / unknown — skip the element to stay in sync.
			_ = cp.dec.Skip()
		}
	}
}

// SendMessage writes a <message> stanza to the server. Safe for concurrent use.
func (cp *Component) SendMessage(m Message) error {
	cp.writeMu.Lock()
	defer cp.writeMu.Unlock()
	cp.mu.Lock()
	enc := cp.enc
	cp.mu.Unlock()
	if enc == nil {
		return fmt.Errorf("component not connected")
	}
	if err := enc.Encode(&m); err != nil {
		return err
	}
	return enc.Flush()
}

// Close shuts the component down and stops reconnection.
func (cp *Component) Close() {
	cp.mu.Lock()
	cp.closed = true
	if cp.conn != nil {
		cp.conn.Close()
	}
	cp.mu.Unlock()
}
