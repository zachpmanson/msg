package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mellium.im/xmlstream"
	"mellium.im/xmpp"
	"mellium.im/xmpp/jid"
	"mellium.im/xmpp/stanza"
	"mellium.im/xmpp/stream"
)

var activityTestNow = time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)

func TestActivityClock(t *testing.T) {
	t.Setenv("MSG_DIR", t.TempDir())
	oldAccount := account
	defer func() { account = oldAccount }()
	account = "clock-test"
	now := activityTestNow
	fallback := now.Add(-time.Minute)
	if got := lastActivity(fallback, now); !got.Equal(fallback) {
		t.Fatalf("missing record: got %v, want %v", got, fallback)
	}
	path, _ := dataFile("last_activity")
	for _, raw := range []string{"", "not-a-time", now.Add(time.Hour).Format(time.RFC3339)} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if got := lastActivity(fallback, now); !got.Equal(fallback) {
			t.Errorf("invalid record %q: got %v", raw, got)
		}
	}
	since := now.Add(-time.Hour - 500*time.Millisecond)
	recordActivity(since)
	if got := lastActivity(fallback, now); !got.Equal(since) {
		t.Fatalf("persisted activity: got %v, want %v", got, since)
	}
	if got := activitySeconds(since, now); got != 3600 {
		t.Fatalf("elapsed seconds = %d", got)
	}
	// Refreshes and reconnects use the same record, not a new activity time.
	if got := lastActivity(now, now.Add(time.Minute)); !got.Equal(since) {
		t.Fatalf("reconnect reset activity: %v", got)
	}
	account = "another-account"
	if got := lastActivity(fallback, now); !got.Equal(fallback) {
		t.Fatalf("activity leaked between accounts: %v", got)
	}
	for _, since := range []time.Time{time.Time{}, now.Add(time.Hour)} {
		if got := activitySeconds(since, now); got != 0 {
			t.Errorf("invalid activity time %v: seconds = %d", since, got)
		}
	}
}

func TestIdleActivityTransitions(t *testing.T) {
	t.Setenv("MSG_DIR", t.TempDir())
	started := activityTestNow
	recordActivity(started)
	for _, age := range []time.Duration{idleThreshold - time.Second, idleThreshold, idleThreshold + presenceRefresh} {
		now := started.Add(age)
		since := lastActivity(started, now)
		idle := idleElement(since, now, true)
		if (idle != nil) != (age >= idleThreshold) {
			t.Fatalf("incorrect idle transition after %v: %+v", age, idle)
		}
		if idle != nil && idle.Since != started.Format(time.RFC3339Nano) {
			t.Fatalf("presence refresh moved since: %+v", idle)
		}
		if activitySeconds(since, now) != int64(age/time.Second) {
			t.Fatal("IQ and presence did not use the same clock")
		}
	}
	now := started.Add(time.Hour)
	recordActivity(now)
	since := lastActivity(started, now)
	if idleElement(since, now, true) != nil || activitySeconds(since, now) != 0 {
		t.Fatal("renewed activity did not clear idle and reset elapsed seconds")
	}
}

func TestCheckRecordsActualActivityNotDeadline(t *testing.T) {
	t.Setenv("MSG_DIR", t.TempDir())
	before := time.Now()
	touchLastPoll(3600)
	after := time.Now()
	got := lastActivity(time.Time{}, after)
	if got.Before(before) || got.After(after) {
		t.Fatalf("check did not record real activity: %v outside %v..%v", got, before, after)
	}
	path, _ := dataFile("next_poll_due")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	due, err := time.Parse(time.RFC3339, string(b))
	if err != nil || due.Sub(got) < time.Hour {
		t.Fatalf("poll deadline did not retain --in: %q (%v)", b, err)
	}
}

func TestIdlePresenceEmissionAndReturn(t *testing.T) {
	now := activityTestNow
	since := now.Add(-10 * time.Minute)
	for _, tt := range []struct {
		name     string
		since    time.Time
		wantIdle bool
		disabled bool
	}{
		{"idle", since, true, false},
		{"refresh keeps original since", since, true, false},
		{"threshold", now.Add(-idleThreshold), true, false},
		{"active again", now, false, false},
		{"before threshold", now.Add(-idleThreshold + time.Nanosecond), false, false},
		{"unknown", time.Time{}, false, false},
		{"future", now.Add(time.Hour), false, false},
		{"privacy opt out", since, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var wire bytes.Buffer
			session, err := xmpp.NewSession(context.Background(), jid.MustParse("example.com"), jid.MustParse("bot@example.com/msg"), &wire, xmpp.Ready, xmpp.NewNegotiator(func(*xmpp.Session, *xmpp.StreamConfig) xmpp.StreamConfig { return xmpp.StreamConfig{} }))
			if err != nil {
				t.Fatal(err)
			}
			if err := sendPresence(context.Background(), session, "xa", "not being actively watched", idleElement(tt.since, now, !tt.disabled)); err != nil {
				t.Fatal(err)
			}
			var p presencePayload
			if err := xml.Unmarshal(wire.Bytes(), &p); err != nil {
				t.Fatalf("invalid presence %q: %v", wire.String(), err)
			}
			if p.Show != "xa" || p.Status != "not being actively watched" || (p.Idle != nil) != tt.wantIdle {
				t.Fatalf("unexpected presence: %s", wire.String())
			}
			if tt.wantIdle && (p.Idle.XMLName.Space != "urn:xmpp:idle:1" || p.Idle.Since != tt.since.UTC().Format(time.RFC3339Nano)) {
				t.Fatalf("incorrect idle payload: %s", wire.String())
			}
		})
	}
}

func activityIQ(t *testing.T, request string, since time.Time, enabled bool) string {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(request))
	tok, err := d.Token()
	if err != nil {
		t.Fatal(err)
	}
	start := tok.(xml.StartElement)
	var wire bytes.Buffer
	e := xml.NewEncoder(&wire)
	rw := struct {
		xml.TokenReader
		xmlstream.Encoder
	}{d, e}
	if err := handleActivityIQ(rw, &start, jid.MustParse("bot@example.com/msg"), since, activityTestNow, enabled); err != nil {
		t.Fatal(err)
	}
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	return wire.String()
}

func TestLastActivityIQ(t *testing.T) {
	for _, tt := range []struct {
		name, typ, to, payload, condition string
		disabled                          bool
		seconds                           string
	}{
		{name: "idle", seconds: "600"},
		{name: "privacy opt out", disabled: true, condition: "service-unavailable"},
		{name: "bare JID belongs to server", to: "bot@example.com", condition: "service-unavailable"},
		{name: "other resource", to: "bot@example.com/other", condition: "service-unavailable"},
		{name: "absent to", to: "-", condition: "service-unavailable"},
		{name: "set cannot change clock", typ: "set", condition: "not-allowed"},
		{name: "wrong namespace", payload: "<query xmlns='other'/>", condition: "service-unavailable"},
		{name: "nested query not accepted", payload: "<x><query xmlns='jabber:iq:last'/></x>", condition: "bad-request"},
		{name: "two queries", payload: "<query xmlns='jabber:iq:last'/><query xmlns='jabber:iq:last'/>", condition: "bad-request"},
		{name: "empty IQ", payload: " ", condition: "bad-request"},
		{name: "unexpected content", payload: "<query xmlns='jabber:iq:last'>secret</query>", condition: "bad-request"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.typ == "" {
				tt.typ = "get"
			}
			if tt.to == "" {
				tt.to = "bot@example.com/msg"
			}
			if tt.to == "-" {
				tt.to = ""
			}
			if tt.payload == "" {
				tt.payload = "<query xmlns='jabber:iq:last'/>"
			}
			request := "<iq type='" + tt.typ + "' id='last-1' from='friend@example.net/client' to='" + tt.to + "'>" + tt.payload + "</iq>"
			wire := activityIQ(t, request, activityTestNow.Add(-10*time.Minute), !tt.disabled)
			var response struct {
				stanza.IQ
				Query struct {
					Seconds string `xml:"seconds,attr"`
				} `xml:"jabber:iq:last query"`
				Error *stanza.Error `xml:"error"`
			}
			if err := xml.Unmarshal([]byte(wire), &response); err != nil {
				t.Fatalf("invalid response %s: %v", wire, err)
			}
			if response.ID != "last-1" || response.To.String() != "friend@example.net/client" || response.From.String() != tt.to {
				t.Fatalf("incorrect IQ addressing/correlation: %s", wire)
			}
			if tt.condition != "" {
				if response.Type != stanza.ErrorIQ || response.Error == nil || string(response.Error.Condition) != tt.condition || strings.Contains(wire, "seconds=") {
					t.Fatalf("incorrect error or timing disclosure: %s", wire)
				}
			} else if response.Type != stanza.ResultIQ || response.Query.Seconds != tt.seconds || response.Error != nil {
				t.Fatalf("incorrect result: %s", wire)
			}
		})
	}
	request := "<iq type='get' id='active' from='friend@example.net' to='bot@example.com/msg'><query xmlns='jabber:iq:last'/></iq>"
	if wire := activityIQ(t, request, activityTestNow, true); !strings.Contains(wire, `seconds="0"`) {
		t.Fatalf("active resource: %s", wire)
	}
}

func TestActivityIQIgnoresResultsAndServerPrivacyErrors(t *testing.T) {
	for _, typ := range []string{"result", "error"} {
		request := "<iq type='" + typ + "' id='last-1' from='example.com'><query xmlns='jabber:iq:last'/><error type='auth'><forbidden xmlns='urn:ietf:params:xml:ns:xmpp-stanzas'/></error></iq>"
		if wire := activityIQ(t, request, activityTestNow.Add(-time.Hour), true); wire != "" {
			t.Fatalf("must not answer a server privacy denial/result: %s", wire)
		}
	}
}

func TestLastActivityDiscovery(t *testing.T) {
	request := "<iq type='get' id='disco-1' from='friend@example.net' to='bot@example.com/msg'><query xmlns='http://jabber.org/protocol/disco#info'/></iq>"
	for _, enabled := range []bool{true, false} {
		wire := activityIQ(t, request, activityTestNow, enabled)
		if strings.Contains(wire, `var="jabber:iq:last"`) != enabled || !strings.Contains(wire, `type="result"`) {
			t.Fatalf("incorrect feature advertisement (enabled=%v): %s", enabled, wire)
		}
	}
}

// Exercise the real Mellium Serve reader/writer framing, not just a standalone
// XML decoder. No credentials, live server, or infrastructure writes needed.
func TestActivityIQOnSession(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	if err := server.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	local := jid.MustParse("bot@example.com/msg")
	session, err := xmpp.NewSession(context.Background(), jid.MustParse("example.com"), local, client, 0,
		func(_ context.Context, in, out *stream.Info, _ *xmpp.Session, _ interface{}) (xmpp.SessionState, io.ReadWriter, interface{}, error) {
			in.XMLNS, out.XMLNS = stanza.NSClient, stanza.NSClient
			return xmpp.Ready, nil, nil, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- session.Serve(xmpp.HandlerFunc(func(rw xmlstream.TokenReadEncoder, start *xml.StartElement) error {
			return handleActivityIQ(rw, start, session.LocalAddr(), activityTestNow.Add(-600*time.Second), activityTestNow, true)
		}))
	}()
	defer func() {
		client.Close()
		<-done
	}()
	if _, err := io.WriteString(server, "<iq xmlns='jabber:client' type='get' id='live' to='bot@example.com/msg' from='friend@example.net/mobile'><query xmlns='jabber:iq:last'/></iq>"); err != nil {
		t.Fatal(err)
	}
	var response struct {
		stanza.IQ
		Query struct {
			Seconds string `xml:"seconds,attr"`
		} `xml:"jabber:iq:last query"`
	}
	if err := xml.NewDecoder(server).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Type != stanza.ResultIQ || response.ID != "live" || response.Query.Seconds != "600" || response.To.String() != "friend@example.net/mobile" {
		t.Fatalf("incorrect session reply: %+v", response)
	}
}

func TestFailedCommandsDoNotRecordActivity(t *testing.T) {
	dir := configTestDir(t)
	writeTestConfig(t, filepath.Join(dir, "config.json"), `{"accounts":{"default":{"jid":"bot@example.com","password":"s"}}}`)
	for _, args := range [][]string{{"send"}, {"room"}, {"send", "--help"}} {
		_ = testRun(t, args...)
		if _, err := os.Stat(filepath.Join(dir, "last_activity")); !os.IsNotExist(err) {
			t.Fatalf("invalid/help command recorded activity: %v", args)
		}
	}
}
