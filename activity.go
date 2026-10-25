package main

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mellium.im/xmlstream"
	"mellium.im/xmpp/disco"
	"mellium.im/xmpp/disco/info"
	"mellium.im/xmpp/jid"
	"mellium.im/xmpp/stanza"
)

const lastActivityNS = "jabber:iq:last"

// Use the existing poll grace period as the idle threshold. The last activity
// itself is recorded separately from next_poll_due: --in is not activity.
const idleThreshold = defaultPollWindow

// recordActivity is shared by separate CLI invocations and the daemon. Atomic
// replacement prevents the daemon from seeing a partially written timestamp.
func recordActivity(now time.Time) {
	path, _ := dataFile("last_activity")
	err := writeActivity(path, now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not record activity: %v\n", err)
	}
}

func writeActivity(path string, now time.Time) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".msg-activity-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(now.UTC().Format(time.RFC3339Nano))
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

// lastActivity falls back to daemon startup if there is no usable record. The
// caller retains that fallback across reconnects. Never infer activity from
// incoming messages, receipts, presence refreshes, or last_seen_time (MAM).
func lastActivity(fallback, now time.Time) time.Time {
	path, _ := dataFile("last_activity")
	if b, err := os.ReadFile(path); err == nil {
		if ts, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b))); err == nil && !ts.After(now) {
			return ts
		}
	}
	if fallback.After(now) {
		return now
	}
	return fallback
}

func activitySeconds(since, now time.Time) int64 {
	if since.IsZero() || since.After(now) {
		return 0
	}
	return int64(now.Sub(since) / time.Second)
}

type idlePresence struct {
	XMLName xml.Name `xml:"urn:xmpp:idle:1 idle"`
	Since   string   `xml:"since,attr"`
}

func idleElement(since, now time.Time, enabled bool) *idlePresence {
	if !enabled || since.IsZero() || now.Sub(since) < idleThreshold {
		return nil
	}
	return &idlePresence{Since: since.UTC().Format(time.RFC3339Nano)}
}

// handleActivityIQ handles only the online resource. The authenticated XMPP
// server MUST enforce presence subscriptions, privacy lists, and blocking
// before forwarding jabber:iq:last requests, even those to a full JID. A client
// cannot reconstruct these permissions from the incoming stanza or MUC status.
// Bare-JID/offline last activity remains the server's responsibility.
func handleActivityIQ(t xmlstream.TokenReadEncoder, start *xml.StartElement, local jid.JID, since, now time.Time, enabled bool) error {
	toks, err := xmlstream.ReadAll(xmlstream.Inner(t))
	if err != nil {
		return err
	}
	iq, err := stanza.NewIQ(*start)
	if err != nil {
		return err
	}
	// Never answer results/errors (including server-generated forbidden errors).
	if iq.Type != stanza.GetIQ && iq.Type != stanza.SetIQ {
		return nil
	}
	payload := make([]xml.TokenReader, 0, len(toks))
	var query xml.StartElement
	elements, depth := 0, 0
	empty := true
	for _, tok := range toks {
		payload = append(payload, xmlstream.Token(tok))
		switch tok := tok.(type) {
		case xml.StartElement:
			if depth == 0 {
				query = tok
				elements++
			} else {
				empty = false
			}
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			if strings.TrimSpace(string(tok)) != "" {
				empty = false
			}
		}
	}
	replyError := func(condition stanza.Condition, typ stanza.ErrorType) error {
		response := iq
		response.Type = stanza.ErrorIQ
		response.From, response.To = iq.To, iq.From
		_, err := xmlstream.Copy(t, response.Wrap(xmlstream.MultiReader(
			xmlstream.MultiReader(payload...),
			stanza.Error{Type: typ, Condition: condition}.TokenReader(),
		)))
		return err
	}
	if elements != 1 || !empty {
		return replyError(stanza.BadRequest, stanza.Modify)
	}
	if query.Name.Local != "query" || (query.Name.Space != lastActivityNS && query.Name.Space != disco.NSInfo) {
		return replyError(stanza.ServiceUnavailable, stanza.Cancel)
	}
	if iq.Type != stanza.GetIQ {
		return replyError(stanza.NotAllowed, stanza.Cancel)
	}
	if query.Name.Space == disco.NSInfo {
		if attrVal(query.Attr, "node") != "" {
			return replyError(stanza.ItemNotFound, stanza.Cancel)
		}
		features := []info.Feature{{Var: disco.NSInfo}}
		if enabled {
			features = append(features, info.Feature{Var: lastActivityNS})
		}
		_, err := xmlstream.Copy(t, iq.Result(disco.Info{
			Features: features,
			Identity: []info.Identity{{Category: "client", Type: "bot", Name: "msg"}},
		}.TokenReader()))
		return err
	}
	// Do not invent offline/bare-JID results if a server misroutes a query.
	if !enabled || local.Resourcepart() == "" || !iq.To.Equal(local) {
		return replyError(stanza.ServiceUnavailable, stanza.Cancel)
	}
	query.Attr = []xml.Attr{{Name: xml.Name{Local: "seconds"}, Value: strconv.FormatInt(activitySeconds(since, now), 10)}}
	_, err = xmlstream.Copy(t, iq.Result(xmlstream.Wrap(nil, query)))
	return err
}
