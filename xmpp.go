package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	xnetws "golang.org/x/net/websocket"

	"mellium.im/sasl"
	"mellium.im/xmlstream"
	"mellium.im/xmpp"
	"mellium.im/xmpp/disco"
	"mellium.im/xmpp/history"
	"mellium.im/xmpp/jid"
	"mellium.im/xmpp/mux"
	"mellium.im/xmpp/stanza"
	"mellium.im/xmpp/upload"
	"mellium.im/xmpp/websocket"
)

const receiptsNS = "urn:xmpp:receipts"
const markersNS = "urn:xmpp:chat-markers:0"
const chatStatesNS = "http://jabber.org/protocol/chatstates"

// activeState is the XEP-0085 "active" chat state, included alongside a
// content-bearing message so the recipient's client clears any lingering
// composing indicator instead of leaving it stuck until it times out on its
// own (which Conversations doesn't reliably do without an explicit state).
type activeState struct {
	XMLName xml.Name `xml:"http://jabber.org/protocol/chatstates active"`
}

const reactionsNS = "urn:xmpp:reactions:0"
const mamNS = "urn:xmpp:mam:2"

// newID generates a random stanza id, used to correlate delivery receipts
// (XEP-0184) with the message they acknowledge.
func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// proxyDial connects to target via an HTTP CONNECT proxy if HTTPS_PROXY (or
// HTTP_PROXY) is set, otherwise dials directly.
func proxyDial(ctx context.Context, target string) (net.Conn, error) {
	proxyURL := os.Getenv("HTTPS_PROXY")
	if proxyURL == "" {
		proxyURL = os.Getenv("HTTP_PROXY")
	}
	if proxyURL == "" {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", target)
	}

	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("parsing proxy URL %q: %w", proxyURL, err)
	}

	var d net.Dialer
	proxyAddr := u.Host
	if u.Port() == "" {
		proxyAddr = u.Hostname() + ":80"
	}

	conn, err := d.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("connecting to proxy %s: %w", proxyAddr, err)
	}

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Host: target},
		Host:   target,
		Header: make(http.Header),
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("writing CONNECT request: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading CONNECT response: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("proxy CONNECT returned %s", resp.Status)
	}

	// The bufio.Reader may have buffered bytes that arrived immediately after
	// the 200 response (e.g. the XMPP server's opening stream). Wrap the conn
	// so those bytes are replayed before reads go to the raw connection.
	return &bufConn{Conn: conn, r: br}, nil
}

// bufConn wraps a net.Conn with a bufio.Reader so that any bytes already
// buffered after an HTTP CONNECT exchange are not lost.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// connect dials and negotiates an XMPP client session for the given account.
func connect(ctx context.Context, cfg *Config) (*xmpp.Session, jid.JID, error) {
	j, err := jid.Parse(cfg.JID)
	if err != nil {
		return nil, jid.JID{}, fmt.Errorf("invalid XMPP_JID %q: %w", cfg.JID, err)
	}

	features := []xmpp.StreamFeature{
		xmpp.StartTLS(&tls.Config{ServerName: j.Domain().String()}),
		xmpp.SASL("", cfg.Password, sasl.ScramSha256Plus, sasl.ScramSha256, sasl.ScramSha1Plus, sasl.ScramSha1, sasl.Plain),
		xmpp.BindResource(),
	}

	// WebSocket path: use wss:// URL when XMPP_WEBSOCKET_URL is set.
	// We establish TCP→proxy→TLS manually so we can force HTTP/1.1 via TLS
	// ALPN. Many proxies negotiate HTTP/2 by default, and WebSocket upgrade
	// (101 Switching Protocols) is not supported over HTTP/2.
	if cfg.WebSocket != "" {
		wsURL, err := url.Parse(cfg.WebSocket)
		if err != nil {
			return nil, jid.JID{}, fmt.Errorf("parsing XMPP_WEBSOCKET_URL: %w", err)
		}
		host := wsURL.Hostname()
		port := wsURL.Port()
		if port == "" {
			port = "443"
		}

		// 1. TCP (possibly through an HTTP CONNECT proxy).
		rawConn, err := proxyDial(ctx, host+":"+port)
		if err != nil {
			return nil, jid.JID{}, fmt.Errorf("tcp dial for websocket: %w", err)
		}

		// 2. TLS with http/1.1 forced via ALPN.
		tlsConn := tls.Client(rawConn, &tls.Config{
			ServerName: host,
			NextProtos: []string{"http/1.1"},
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			rawConn.Close()
			return nil, jid.JID{}, fmt.Errorf("tls handshake for websocket: %w", err)
		}

		// 3. WebSocket upgrade + XMPP session on the TLS conn.
		// TLS is already provided by wss://, so omit StartTLS from features.
		wsFeatures := []xmpp.StreamFeature{
			xmpp.SASL("", cfg.Password, sasl.ScramSha256Plus, sasl.ScramSha256, sasl.ScramSha1Plus, sasl.ScramSha1, sasl.Plain),
			xmpp.BindResource(),
		}

		// Perform the WebSocket handshake on our pre-established TLS connection.
		origin := "https://" + host
		wsCfg, err := xnetws.NewConfig(cfg.WebSocket, origin)
		if err != nil {
			tlsConn.Close()
			return nil, jid.JID{}, fmt.Errorf("websocket config: %w", err)
		}
		wsCfg.Protocol = []string{websocket.WSProtocol}
		wsConn, err := xnetws.NewClient(wsCfg, tlsConn)
		if err != nil {
			tlsConn.Close()
			return nil, jid.JID{}, fmt.Errorf("websocket handshake: %w", err)
		}

		// Call xmpp.NewSession directly with xmpp.Secure explicitly set.
		// websocket.NewSession auto-detects secure by checking
		// LocalAddr().Scheme == "wss", but LocalAddr() returns the Origin
		// URL ("https://..."), so the check fails. We set the bit ourselves.
		n := websocket.Negotiator(func(*xmpp.Session, *xmpp.StreamConfig) xmpp.StreamConfig {
			return xmpp.StreamConfig{Features: wsFeatures}
		})
		session, err := xmpp.NewSession(ctx, j.Domain(), j, wsConn, xmpp.Secure, n)
		if err != nil {
			wsConn.Close()
			return nil, jid.JID{}, fmt.Errorf("websocket/xmpp session: %w", err)
		}
		return session, j, nil
	}

	target := cfg.Server
	if target == "" {
		target = j.Domain().String() + ":5222"
	}

	conn, err := proxyDial(ctx, target)
	if err != nil {
		return nil, jid.JID{}, fmt.Errorf("dialing %s: %w", target, err)
	}
	session, err := xmpp.NewClientSession(ctx, j, conn, features...)
	if err != nil {
		return nil, jid.JID{}, err
	}
	return session, j, nil
}

// sendMessage connects, sends a single chat message, and disconnects.
func sendMessage(ctx context.Context, cfg *Config, to, body string) error {
	session, _, err := connect(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer session.Close()

	go func() {
		_ = session.Serve(xmpp.HandlerFunc(func(t xmlstream.TokenReadEncoder, start *xml.StartElement) error {
			_, err := xmlstream.Copy(xmlstream.Discard(), t)
			return err
		}))
	}()

	return sendOnSession(ctx, session, to, body)
}

// sendOnSession sends a one-to-one chat message over an already-connected session.
func sendOnSession(ctx context.Context, session *xmpp.Session, to, body string) error {
	_, err := encodeMessage(ctx, session, to, body, stanza.ChatMessage)
	return err
}

// sendTyping connects, sends a single "composing" chat state (XEP-0085) to
// to, and disconnects.
func sendTyping(ctx context.Context, cfg *Config, to string) error {
	session, _, err := connect(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer session.Close()

	go func() {
		_ = session.Serve(xmpp.HandlerFunc(func(t xmlstream.TokenReadEncoder, start *xml.StartElement) error {
			_, err := xmlstream.Copy(xmlstream.Discard(), t)
			return err
		}))
	}()

	return sendChatState(ctx, session, to, "composing", stanza.ChatMessage)
}

// sendRoomTyping joins cfg.Room (if not already joined) and sends a
// "composing" chat state (XEP-0085) to it, then disconnects.
func sendRoomTyping(ctx context.Context, cfg *Config) error {
	if cfg.Room == "" {
		return fmt.Errorf("XMPP_ROOM is not set in .env")
	}
	session, _, err := connect(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer session.Close()

	go func() {
		_ = session.Serve(xmpp.HandlerFunc(func(t xmlstream.TokenReadEncoder, start *xml.StartElement) error {
			_, err := xmlstream.Copy(xmlstream.Discard(), t)
			return err
		}))
	}()

	if err := joinRoom(ctx, session, cfg.Room, cfg.Nick); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)

	return sendChatState(ctx, session, cfg.Room, "composing", stanza.GroupChatMessage)
}

// sendRoomMessage joins cfg.Room (if not already joined) and sends body as a
// groupchat message to it, then disconnects.
func sendRoomMessage(ctx context.Context, cfg *Config, body string) error {
	if cfg.Room == "" {
		return fmt.Errorf("XMPP_ROOM is not set in .env")
	}
	session, _, err := connect(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer session.Close()

	go func() {
		_ = session.Serve(xmpp.HandlerFunc(func(t xmlstream.TokenReadEncoder, start *xml.StartElement) error {
			_, err := xmlstream.Copy(xmlstream.Discard(), t)
			return err
		}))
	}()

	if err := joinRoom(ctx, session, cfg.Room, cfg.Nick); err != nil {
		return err
	}
	// Give the server a moment to process the join before sending, since some
	// MUC implementations reject messages sent before the join is acked.
	time.Sleep(500 * time.Millisecond)

	_, err = encodeMessage(ctx, session, cfg.Room, body, stanza.GroupChatMessage)
	return err
}

// sendPresence announces availability with a show state (e.g. "" for
// available, "xa" for extended-away) and a human-readable status, so a
// contact's roster shows more than just "online" — e.g. distinguishing the
// daemon actually running from some other client being briefly connected,
// or whether an agent is actively polling it right now.
func sendPresence(ctx context.Context, session *xmpp.Session, show, status string) error {
	p := struct {
		XMLName xml.Name `xml:"presence"`
		Show    string   `xml:"show,omitempty"`
		Status  string   `xml:"status"`
	}{Show: show, Status: status}
	return session.Encode(ctx, p)
}

// approveSubscription auto-accepts a presence subscription request so a
// contact who adds this account sees accurate online/offline status without
// manual approval.
func approveSubscription(ctx context.Context, session *xmpp.Session, from string) error {
	fromJID, err := jid.Parse(from)
	if err != nil {
		return fmt.Errorf("invalid subscriber JID %q: %w", from, err)
	}
	p := stanza.Presence{To: fromJID, Type: stanza.SubscribedPresence}
	subCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return session.Encode(subCtx, p)
}

// joinRoom sends MUC presence to join room under the given nickname.
func joinRoom(ctx context.Context, session *xmpp.Session, room, nick string) error {
	roomJID, err := jid.Parse(room)
	if err != nil {
		return fmt.Errorf("invalid XMPP_ROOM %q: %w", room, err)
	}
	occupant, err := roomJID.WithResource(nick)
	if err != nil {
		return fmt.Errorf("invalid nick %q: %w", nick, err)
	}

	join := struct {
		XMLName xml.Name `xml:"presence"`
		To      string   `xml:"to,attr"`
		X       struct {
			XMLName xml.Name `xml:"http://jabber.org/protocol/muc x"`
		} `xml:"x"`
	}{To: occupant.String()}

	joinCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return session.Encode(joinCtx, join)
}

// receiptRequest is the XEP-0184 <request/> element, added to outgoing
// direct chat messages so the recipient's client can confirm delivery.
// It's deliberately omitted for groupchat, where XEP-0184 says receipts
// don't apply (no single, unambiguous recipient).
type receiptRequest struct {
	XMLName xml.Name `xml:"urn:xmpp:receipts request"`
}

// encodeMessage sends a single message stanza of the given type, returning
// its stanza id (used to correlate an eventual delivery receipt).
func encodeMessage(ctx context.Context, session *xmpp.Session, to, body string, typ stanza.MessageType) (string, error) {
	toJID, err := jid.Parse(to)
	if err != nil {
		return "", fmt.Errorf("invalid recipient %q: %w", to, err)
	}

	var req *receiptRequest
	var active *activeState
	if typ == stanza.ChatMessage {
		req = &receiptRequest{}
		active = &activeState{}
	}

	id := newID()
	msg := struct {
		stanza.Message
		Body    string          `xml:"body"`
		Active  *activeState    `xml:"active,omitempty"`
		Request *receiptRequest `xml:"request,omitempty"`
	}{
		Message: stanza.Message{
			ID:   id,
			To:   toJID,
			Type: typ,
		},
		Body:    body,
		Active:  active,
		Request: req,
	}

	sendCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return id, session.Encode(sendCtx, msg)
}

// sendChatState sends a XEP-0085 chat state notification (e.g. "composing")
// as its own message stanza, with no body, to to. typ selects direct chat
// or groupchat framing, matching encodeMessage.
func sendChatState(ctx context.Context, session *xmpp.Session, to, state string, typ stanza.MessageType) error {
	toJID, err := jid.Parse(to)
	if err != nil {
		return fmt.Errorf("invalid recipient %q: %w", to, err)
	}

	msg := struct {
		stanza.Message
		State struct {
			XMLName xml.Name
		}
	}{
		Message: stanza.Message{
			To:   toJID,
			Type: typ,
		},
	}
	msg.State.XMLName = xml.Name{Space: chatStatesNS, Local: state}

	sendCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return session.Encode(sendCtx, msg)
}

// sendReceipt replies to a delivery receipt request (XEP-0184) for the
// message identified by id.
func sendReceipt(ctx context.Context, session *xmpp.Session, to, id string) error {
	toJID, err := jid.Parse(to)
	if err != nil {
		return fmt.Errorf("invalid recipient %q: %w", to, err)
	}

	receipt := struct {
		XMLName  xml.Name `xml:"message"`
		To       string   `xml:"to,attr"`
		Received struct {
			XMLName xml.Name `xml:"urn:xmpp:receipts received"`
			ID      string   `xml:"id,attr"`
		} `xml:"received"`
	}{To: toJID.String()}
	receipt.Received.ID = id

	rCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return session.Encode(rCtx, receipt)
}

// sendDisplayedMarker sends a XEP-0333 chat marker telling to that the
// message identified by id has been displayed (in our case: surfaced to the
// agent via `msg check`).
func sendDisplayedMarker(ctx context.Context, session *xmpp.Session, to, id string) error {
	toJID, err := jid.Parse(to)
	if err != nil {
		return fmt.Errorf("invalid recipient %q: %w", to, err)
	}

	marker := struct {
		XMLName   xml.Name `xml:"message"`
		To        string   `xml:"to,attr"`
		Displayed struct {
			XMLName xml.Name `xml:"urn:xmpp:chat-markers:0 displayed"`
			ID      string   `xml:"id,attr"`
		} `xml:"displayed"`
	}{To: toJID.String()}
	marker.Displayed.ID = id

	mCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return session.Encode(mCtx, marker)
}

// findElement returns the first child element among toks matching space and
// local name, if any.
func findElement(toks []xml.Token, space, local string) (xml.StartElement, bool) {
	for _, tok := range toks {
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == local && se.Name.Space == space {
			return se, true
		}
	}
	return xml.StartElement{}, false
}

// incoming is one received chat message, serialized to the inbox log.
type incoming struct {
	Time string `json:"time"`
	From string `json:"from"`
	Body string `json:"body"`
	// ID, Type, and Markable are populated from the message stanza so
	// `msg check` can send a XEP-0333 "displayed" chat marker back to the
	// sender once the message has actually been surfaced to the agent.
	// Type is the stanza type attribute ("chat" or "groupchat"); markers are
	// only sent for direct "chat" messages that requested one via <markable/>.
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Markable bool   `json:"markable,omitempty"`
	// AttachmentURL is the XEP-0066 out-of-band URL, if the message carried
	// one (e.g. a file uploaded via XEP-0363). AttachmentPath is filled in
	// later, by whoever calls listen, once/if the file is downloaded locally.
	AttachmentURL  string `json:"attachment_url,omitempty"`
	AttachmentPath string `json:"attachment_path,omitempty"`
	// ReactionToID and Reactions are set instead of Body when the stanza is a
	// XEP-0444 reaction: the emoji in Reactions are the sender's complete,
	// current reaction set to the message identified by ReactionToID (a new
	// reaction message always replaces, not adds to, the sender's prior one).
	ReactionToID string   `json:"reaction_to_id,omitempty"`
	Reactions    []string `json:"reactions,omitempty"`
}

// presenceRefresh is a small interval, well under the ~60-90s cadence an
// agent's poll loop runs at, so a stale-vs-active transition is reflected in
// presence promptly instead of only at the next reconnect.
const presenceRefresh = 20 * time.Second

// listen connects and invokes onMsg for every chat message with a non-empty
// body until the context is canceled or the connection drops. If onConnected
// is non-nil, it is called once the session is up (after presence is sent).
// If presenceFn is non-nil, it's called immediately and then every
// presenceRefresh to decide the current (show, status) to announce — this is
// how presence tracks whether an agent is actively polling `msg check`
// rather than just whether the daemon process is up.
func listen(ctx context.Context, cfg *Config, onMsg func(incoming), onConnected func(*xmpp.Session), presenceFn func() (show, status string)) error {
	session, _, err := connect(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer session.Close()

	// connCtx bounds the presence-refresh goroutine to this connection
	// attempt, distinct from the "done" channel below (which the outer
	// select also reads from — a second reader there would race to consume
	// its one buffered value).
	connCtx, cancelConn := context.WithCancel(ctx)
	defer cancelConn()

	show, status := "", "listening for messages"
	if presenceFn != nil {
		show, status = presenceFn()
	}
	// Announce availability (with a status so it's clear in a client's roster
	// that this is the daemon, not just a bare online blip) so the server
	// delivers messages to this resource.
	if err := sendPresence(ctx, session, show, status); err != nil {
		return fmt.Errorf("sending presence: %w", err)
	}
	if presenceFn != nil {
		go func() {
			ticker := time.NewTicker(presenceRefresh)
			defer ticker.Stop()
			for {
				select {
				case <-connCtx.Done():
					return
				case <-ticker.C:
					show, status := presenceFn()
					if err := sendPresence(ctx, session, show, status); err != nil {
						fmt.Fprintf(os.Stderr, "failed to refresh presence: %v\n", err)
						return
					}
				}
			}
		}()
	}

	if cfg.Room != "" {
		if err := joinRoom(ctx, session, cfg.Room, cfg.Nick); err != nil {
			return fmt.Errorf("joining room %s: %w", cfg.Room, err)
		}
	}

	if onConnected != nil {
		onConnected(session)
	}

	done := make(chan error, 1)
	go func() {
		done <- session.Serve(xmpp.HandlerFunc(func(t xmlstream.TokenReadEncoder, start *xml.StartElement) error {
			if start.Name.Local == "presence" {
				_, err := xmlstream.Copy(xmlstream.Discard(), t)
				if err != nil {
					return err
				}
				if attrVal(start.Attr, "type") == string(stanza.SubscribePresence) {
					return approveSubscription(ctx, session, attrVal(start.Attr, "from"))
				}
				return nil
			}
			if start.Name.Local != "message" {
				_, err := xmlstream.Copy(xmlstream.Discard(), t)
				return err
			}
			toks, err := xmlstream.ReadAll(t)
			if err != nil {
				return err
			}

			from := attrVal(start.Attr, "from")
			if se, ok := findElement(toks, receiptsNS, "received"); ok {
				fmt.Fprintf(os.Stderr, "delivery receipt from %s for message %s\n", from, attrVal(se.Attr, "id"))
				return nil
			}
			if _, ok := findElement(toks, receiptsNS, "request"); ok {
				if id := attrVal(start.Attr, "id"); id != "" && from != "" {
					if err := sendReceipt(ctx, session, from, id); err != nil {
						fmt.Fprintf(os.Stderr, "failed to send delivery receipt: %v\n", err)
					}
				}
			}

			if se, ok := findElement(toks, reactionsNS, "reactions"); ok {
				onMsg(incoming{
					Time:         time.Now().UTC().Format(time.RFC3339),
					From:         from,
					ID:           attrVal(start.Attr, "id"),
					Type:         attrVal(start.Attr, "type"),
					ReactionToID: attrVal(se.Attr, "id"),
					Reactions:    extractReactions(toks),
				})
				return nil
			}

			body := extractBody(toks)
			attachmentURL := extractOOBURL(toks)
			if body != "" || attachmentURL != "" {
				_, markable := findElement(toks, markersNS, "markable")
				onMsg(incoming{
					Time:          time.Now().UTC().Format(time.RFC3339),
					From:          from,
					Body:          body,
					ID:            attrVal(start.Attr, "id"),
					Type:          attrVal(start.Attr, "type"),
					Markable:      markable,
					AttachmentURL: attachmentURL,
				})
			}
			return nil
		}))
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}

// discoItem is one result from a disco#items query.
type discoItem struct {
	JID  string
	Name string
}

// discoItems queries target for its child services/items (XEP-0030), useful
// for finding a server's MUC component JID.
func discoItems(ctx context.Context, cfg *Config, target string) ([]discoItem, error) {
	session, _, err := connect(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer session.Close()

	go func() {
		_ = session.Serve(xmpp.HandlerFunc(func(t xmlstream.TokenReadEncoder, start *xml.StartElement) error {
			_, err := xmlstream.Copy(xmlstream.Discard(), t)
			return err
		}))
	}()

	return discoItemsOnSession(ctx, session, target)
}

// discoItemsOnSession is like discoItems but reuses an already-connected
// session (whose read loop must already be running via session.Serve).
func discoItemsOnSession(ctx context.Context, session *xmpp.Session, target string) ([]discoItem, error) {
	targetJID, err := jid.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("invalid JID %q: %w", target, err)
	}

	iq := stanza.IQ{ID: "disco1", To: targetJID, Type: stanza.GetIQ}
	payload := xmlstream.Wrap(nil, xml.StartElement{Name: xml.Name{Space: "http://jabber.org/protocol/disco#items", Local: "query"}})

	iqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := session.SendIQ(iqCtx, iq.Wrap(payload))
	if err != nil {
		return nil, err
	}
	defer resp.Close()

	toks, err := xmlstream.ReadAll(resp)
	if err != nil {
		return nil, err
	}

	var items []discoItem
	for _, tok := range toks {
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "item" {
			continue
		}
		items = append(items, discoItem{JID: attrVal(se.Attr, "jid"), Name: attrVal(se.Attr, "name")})
	}
	return items, nil
}

// resolveUploadService finds a XEP-0363 HTTP File Upload component on the
// account's server, preferring cfg.UploadService if set to skip discovery.
func resolveUploadService(ctx context.Context, cfg *Config, session *xmpp.Session) (jid.JID, error) {
	if cfg.UploadService != "" {
		return jid.Parse(cfg.UploadService)
	}

	accountJID, err := jid.Parse(cfg.JID)
	if err != nil {
		return jid.JID{}, err
	}
	domain := accountJID.Domain().String()

	items, err := discoItemsOnSession(ctx, session, domain)
	if err != nil {
		return jid.JID{}, fmt.Errorf("discovering services on %s: %w", domain, err)
	}
	for _, it := range items {
		itemJID, err := jid.Parse(it.JID)
		if err != nil {
			continue
		}
		info, err := disco.GetInfo(ctx, "", itemJID, session)
		if err != nil {
			continue
		}
		for _, feat := range info.Features {
			if feat.Var == upload.NS {
				return itemJID, nil
			}
		}
	}
	return jid.JID{}, fmt.Errorf("no XEP-0363 HTTP upload service found on %s (set XMPP_UPLOAD_SERVICE in .env to override)", domain)
}

// uploadAndGetURL requests an upload slot for the file at path, PUTs its
// contents, and returns the URL it will be retrievable from.
func uploadAndGetURL(ctx context.Context, cfg *Config, session *xmpp.Session, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return "", err
	}

	contentType := mime.TypeByExtension(filepath.Ext(path))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	uploadJID, err := resolveUploadService(ctx, cfg, session)
	if err != nil {
		return "", err
	}

	slot, err := upload.GetSlot(ctx, upload.File{
		Name: filepath.Base(path),
		Size: int(stat.Size()),
		Type: contentType,
	}, uploadJID, session)
	if err != nil {
		return "", fmt.Errorf("requesting upload slot: %w", err)
	}
	if slot.GetURL == nil || slot.PutURL == nil {
		return "", fmt.Errorf("upload slot missing put/get URL")
	}

	putReq, err := slot.Put(ctx, f)
	if err != nil {
		return "", err
	}
	// http.NewRequest can't infer a Content-Length from *os.File, so without
	// this the request goes out chunked, which some upload servers reject.
	putReq.ContentLength = stat.Size()
	resp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		return "", fmt.Errorf("uploading file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("uploading file: server returned %s", resp.Status)
	}

	return slot.GetURL.String(), nil
}

// oobX is the XEP-0066 Out of Band Data element, attached to a message
// pointing at a URL (e.g. an uploaded file) so clients render it inline.
type oobX struct {
	XMLName xml.Name `xml:"jabber:x:oob x"`
	URL     string   `xml:"url"`
}

// encodeFileMessage sends a message whose body and XEP-0066 <x/> both point
// at fileURL, so recipients see it as a file/image attachment rather than
// a plain link.
func encodeFileMessage(ctx context.Context, session *xmpp.Session, to, fileURL string, typ stanza.MessageType) error {
	toJID, err := jid.Parse(to)
	if err != nil {
		return fmt.Errorf("invalid recipient %q: %w", to, err)
	}

	var req *receiptRequest
	var active *activeState
	if typ == stanza.ChatMessage {
		req = &receiptRequest{}
		active = &activeState{}
	}

	msg := struct {
		stanza.Message
		Body    string          `xml:"body"`
		OOB     oobX            `xml:"x"`
		Active  *activeState    `xml:"active,omitempty"`
		Request *receiptRequest `xml:"request,omitempty"`
	}{
		Message: stanza.Message{
			ID:   newID(),
			To:   toJID,
			Type: typ,
		},
		Body:    fileURL,
		OOB:     oobX{URL: fileURL},
		Active:  active,
		Request: req,
	}

	sendCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return session.Encode(sendCtx, msg)
}

// reactionsX is the XEP-0444 Message Reactions element. Sending it replaces
// the sender's entire prior reaction set on the target message with Emoji.
type reactionsX struct {
	XMLName xml.Name `xml:"urn:xmpp:reactions:0 reactions"`
	ID      string   `xml:"id,attr"`
	Emoji   []string `xml:"reaction"`
}

// encodeReaction sends a XEP-0444 reaction to the message identified by
// targetID.
func encodeReaction(ctx context.Context, session *xmpp.Session, to, targetID, emoji string, typ stanza.MessageType) error {
	toJID, err := jid.Parse(to)
	if err != nil {
		return fmt.Errorf("invalid recipient %q: %w", to, err)
	}

	msg := struct {
		stanza.Message
		Reactions reactionsX `xml:"reactions"`
	}{
		Message: stanza.Message{
			ID:   newID(),
			To:   toJID,
			Type: typ,
		},
		Reactions: reactionsX{ID: targetID, Emoji: []string{emoji}},
	}

	sendCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return session.Encode(sendCtx, msg)
}

// sendReaction connects, reacts to targetID with emoji, and disconnects.
func sendReaction(ctx context.Context, cfg *Config, to, targetID, emoji string) error {
	session, _, err := connect(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer session.Close()

	go func() {
		_ = session.Serve(xmpp.HandlerFunc(func(t xmlstream.TokenReadEncoder, start *xml.StartElement) error {
			_, err := xmlstream.Copy(xmlstream.Discard(), t)
			return err
		}))
	}()

	return encodeReaction(ctx, session, to, targetID, emoji, stanza.ChatMessage)
}

// sendRoomReaction is like sendReaction but posts to cfg.Room.
func sendRoomReaction(ctx context.Context, cfg *Config, targetID, emoji string) error {
	if cfg.Room == "" {
		return fmt.Errorf("XMPP_ROOM is not set in .env")
	}
	session, _, err := connect(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer session.Close()

	go func() {
		_ = session.Serve(xmpp.HandlerFunc(func(t xmlstream.TokenReadEncoder, start *xml.StartElement) error {
			_, err := xmlstream.Copy(xmlstream.Discard(), t)
			return err
		}))
	}()

	if err := joinRoom(ctx, session, cfg.Room, cfg.Nick); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)

	return encodeReaction(ctx, session, cfg.Room, targetID, emoji, stanza.GroupChatMessage)
}

// sendFile uploads the file at path (XEP-0363) and messages the resulting
// URL to to, then disconnects.
func sendFile(ctx context.Context, cfg *Config, path, to string) (string, error) {
	session, _, err := connect(ctx, cfg)
	if err != nil {
		return "", fmt.Errorf("connect: %w", err)
	}
	defer session.Close()

	go func() {
		_ = session.Serve(xmpp.HandlerFunc(func(t xmlstream.TokenReadEncoder, start *xml.StartElement) error {
			_, err := xmlstream.Copy(xmlstream.Discard(), t)
			return err
		}))
	}()

	getURL, err := uploadAndGetURL(ctx, cfg, session, path)
	if err != nil {
		return "", err
	}
	if err := encodeFileMessage(ctx, session, to, getURL, stanza.ChatMessage); err != nil {
		return "", err
	}
	return getURL, nil
}

// sendRoomFile is like sendFile but uploads and posts to cfg.Room.
func sendRoomFile(ctx context.Context, cfg *Config, path string) (string, error) {
	if cfg.Room == "" {
		return "", fmt.Errorf("XMPP_ROOM is not set in .env")
	}
	session, _, err := connect(ctx, cfg)
	if err != nil {
		return "", fmt.Errorf("connect: %w", err)
	}
	defer session.Close()

	go func() {
		_ = session.Serve(xmpp.HandlerFunc(func(t xmlstream.TokenReadEncoder, start *xml.StartElement) error {
			_, err := xmlstream.Copy(xmlstream.Discard(), t)
			return err
		}))
	}()

	if err := joinRoom(ctx, session, cfg.Room, cfg.Nick); err != nil {
		return "", err
	}
	time.Sleep(500 * time.Millisecond)

	getURL, err := uploadAndGetURL(ctx, cfg, session, path)
	if err != nil {
		return "", err
	}
	if err := encodeFileMessage(ctx, session, cfg.Room, getURL, stanza.GroupChatMessage); err != nil {
		return "", err
	}
	return getURL, nil
}

// resolveUploadHosts opens a short-lived connection to discover the upload
// service and returns candidate HTTP hosts for its download URLs, for use
// as a download allowlist by the listen daemon. It includes both the upload
// component's own XMPP domain and the account's server domain, since
// XEP-0363 GET URLs are often served from the bare domain behind a reverse
// proxy rather than from the (sub)domain of the disco'd component JID.
func resolveUploadHosts(cfg *Config) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, _, err := connect(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer session.Close()

	go func() {
		_ = session.Serve(xmpp.HandlerFunc(func(t xmlstream.TokenReadEncoder, start *xml.StartElement) error {
			_, err := xmlstream.Copy(xmlstream.Discard(), t)
			return err
		}))
	}()

	uploadJID, err := resolveUploadService(ctx, cfg, session)
	if err != nil {
		return nil, err
	}
	accountJID, err := jid.Parse(cfg.JID)
	if err != nil {
		return nil, err
	}

	hosts := []string{uploadJID.Domain().String()}
	if accountDomain := accountJID.Domain().String(); accountDomain != hosts[0] {
		hosts = append(hosts, accountDomain)
	}
	return hosts, nil
}

// extractBody returns the text content of the first <body> child element
// found among toks, or "" if there is none.
func extractBody(toks []xml.Token) string {
	for i, tok := range toks {
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "body" {
			continue
		}
		if i+1 < len(toks) {
			if cd, ok := toks[i+1].(xml.CharData); ok {
				return string(cd)
			}
		}
	}
	return ""
}

// extractOOBURL returns the URL from the first XEP-0066 <x><url>...</url></x>
// out-of-band data element found among toks, or "" if there is none.
func extractOOBURL(toks []xml.Token) string {
	for i, tok := range toks {
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "url" {
			continue
		}
		if i+1 < len(toks) {
			if cd, ok := toks[i+1].(xml.CharData); ok {
				return string(cd)
			}
		}
	}
	return ""
}

// extractReactions returns the text of every XEP-0444 <reaction> child
// element found among toks (the sender's full current reaction set).
func extractReactions(toks []xml.Token) []string {
	var out []string
	for i, tok := range toks {
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "reaction" || se.Name.Space != reactionsNS {
			continue
		}
		if i+1 < len(toks) {
			if cd, ok := toks[i+1].(xml.CharData); ok {
				out = append(out, string(cd))
			}
		}
	}
	return out
}

// attrVal returns the value of the first attribute named local, or "".
func attrVal(attrs []xml.Attr, local string) string {
	for _, a := range attrs {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

// parseForwardedMessage extracts an incoming from a XEP-0313 archived
// message: toks starts at the outer <message><result><forwarded> wrapper: it
// finds the second <message> start element (the original archived stanza),
// its <delay/> timestamp, and its body/attachment, mirroring the fields
// listen()'s live handler would have produced when the message first went by.
func parseForwardedMessage(toks []xml.Token) *incoming {
	count := 0
	archIdx := -1
	var archStart xml.StartElement
	for i, tok := range toks {
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "message" {
			continue
		}
		count++
		if count == 2 {
			archStart = se
			archIdx = i
			break
		}
	}
	if archIdx < 0 {
		return nil
	}
	rest := toks[archIdx:]

	body := extractBody(rest)
	attachmentURL := extractOOBURL(rest)
	if body == "" && attachmentURL == "" {
		return nil
	}

	ts := time.Now().UTC().Format(time.RFC3339)
	if se, ok := findElement(toks, "urn:xmpp:delay", "delay"); ok {
		if stamp := attrVal(se.Attr, "stamp"); stamp != "" {
			if parsed, err := time.Parse(time.RFC3339, stamp); err == nil {
				ts = parsed.UTC().Format(time.RFC3339)
			}
		}
	}

	return &incoming{
		Time:          ts,
		From:          attrVal(archStart.Attr, "from"),
		Body:          body,
		ID:            attrVal(archStart.Attr, "id"),
		Type:          attrVal(archStart.Attr, "type"),
		AttachmentURL: attachmentURL,
	}
}

// fetchDirectHistorySince queries the account's own XEP-0313 message archive
// for direct messages from cfg.To sent at or after since, so the listen
// daemon can backfill anything that arrived during downtime rather than
// relying solely on the server's best-effort offline-message delivery.
// Returns (nil, nil) if the archive has nothing new — a missing/unsupported
// archive is a real error, not silently swallowed, so callers can log it.
func fetchDirectHistorySince(ctx context.Context, cfg *Config, since time.Time) ([]incoming, error) {
	session, _, err := connect(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer session.Close()

	withJID, err := jid.Parse(cfg.To)
	if err != nil {
		return nil, fmt.Errorf("invalid XMPP_TO %q: %w", cfg.To, err)
	}
	accountJID, err := jid.Parse(cfg.JID)
	if err != nil {
		return nil, err
	}

	// Collect archive results inside the Serve read-loop rather than via
	// history.Handler's Fetch iterator. The iterator hands the live session
	// reader to the consuming goroutine while Serve keeps reading the same
	// stream to drain the stanza, so two goroutines read the one xml.Decoder
	// at once and corrupt its buffer offsets — the "slice bounds out of range"
	// panic. Reading each result here, in the goroutine that runs Serve, keeps
	// all stream reads on a single goroutine.
	var (
		mu  sync.Mutex
		out []incoming
	)
	m := mux.New(stanza.NSClient, mux.MessageFunc(
		stanza.NormalMessage,
		xml.Name{Space: history.NS, Local: "result"},
		func(_ stanza.Message, r xmlstream.TokenReadEncoder) error {
			toks, err := xmlstream.ReadAll(r)
			if err != nil {
				return err
			}
			if msg := parseForwardedMessage(toks); msg != nil {
				mu.Lock()
				out = append(out, *msg)
				mu.Unlock()
			}
			return nil
		},
	))
	go func() {
		_ = session.Serve(m)
	}()

	// Fetch sends the MAM query and blocks until the archive's IQ result
	// arrives; the server delivers every result <message> ahead of that IQ, so
	// by the time this returns the handler above has collected them all.
	if _, err := history.Fetch(ctx, history.Query{With: withJID, Start: since}, accountJID.Bare(), session); err != nil {
		return nil, err
	}

	mu.Lock()
	defer mu.Unlock()
	return out, nil
}
