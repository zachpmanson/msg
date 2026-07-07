// Command msg is a minimal XMPP client for sending and receiving chat
// messages from an automated agent. See the "xmpp-messaging" Claude Code
// skill for usage instructions.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mellium.im/xmlstream"
	"mellium.im/xmpp"
	"mellium.im/xmpp/stanza"
)

// account holds the name passed via --as, if any. When set, the tool loads
// ".env.<account>" instead of ".env" and namespaces its runtime state files
// (inbox, cursor, pidfile) so multiple accounts can share one directory
// without a second binary or a second checkout.
var account string

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	args, err := extractAccountFlag(os.Args[1:])
	if err != nil {
		return err
	}
	if len(args) < 1 {
		usage()
		return fmt.Errorf("missing command")
	}

	cmd := args[0]
	args = args[1:]

	switch cmd {
	case "send":
		return cmdSend(args)
	case "room":
		return cmdRoom(args)
	case "typing":
		return cmdTyping(args)
	case "send-file":
		return cmdSendFile(args)
	case "room-file":
		return cmdRoomFile(args)
	case "react":
		return cmdReact(args)
	case "disco":
		return cmdDisco(args)
	case "register":
		return cmdRegister(args)
	case "check":
		return cmdCheck(args)
	case "listen":
		return cmdListen(args)
	case "stop":
		return cmdStop()
	case "status":
		return cmdStatus()
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  msg [--as <account>] <command> ...
  msg send "<text>" [to-jid]   send a direct message (defaults to XMPP_TO from .env)
  msg room "<text>"             send a groupchat message to XMPP_ROOM
  msg typing [to-jid]           send a "composing" chat state (defaults to XMPP_TO)
  msg typing --room             send a "composing" chat state to XMPP_ROOM
  msg send-file <path> [to-jid] upload a file (XEP-0363) and message the link
  msg room-file <path>          upload a file and post the link to XMPP_ROOM
  msg react <emoji> [to-jid]    react (XEP-0444) to the latest message from to-jid
  msg react <emoji> --room      react to the latest message in XMPP_ROOM
  msg register <name> [password]
                                 provision a new persona account via ejabberd's
                                 HTTP Admin API (needs XMPP_API_URL/USER/PASSWORD
                                 in .env) and write .env.<name> with its creds
  msg listen                    run the background daemon that logs incoming messages
                                 (also joins XMPP_ROOM, if set)
  msg check                     print unread messages received since the last check
  msg check --since <ts|date>   one-shot: print messages since an RFC3339 timestamp
                                 or YYYY-MM-DD date, instead of the persisted cursor
  msg check --in <seconds>      declare when the next check is expected, so presence
                                 stays "available" for about that long
  msg check --no-receipt        skip sending read-receipt markers for this check
  msg status                    show whether the listen daemon is running
  msg stop                      stop the listen daemon

--as <account>   load .env.<account> instead of .env, and namespace this
                 account's runtime state (inbox, cursor, pidfile) so it can
                 share a directory with the default account.

connection (.env):
  XMPP_JID, XMPP_PASSWORD          required account credentials
  XMPP_SERVER                      optional host:port override; defaults to
                                   <domain>:5222
  XMPP_WEBSOCKET_URL               connect via XMPP-over-WebSocket (RFC 7395)
                                   instead of raw TCP, e.g.
                                   wss://chat.example.com/ws — useful when 5222
                                   is blocked but 443 is open
  HTTPS_PROXY / HTTP_PROXY         tunnel the connection through an HTTP CONNECT
                                   proxy (standard Go env vars)`)
}

// extractAccountFlag scans args for a leading "--as <name>" or "--as=<name>"
// flag (in any position), sets the package-level account var, and returns
// the remaining args with the flag removed.
func extractAccountFlag(args []string) ([]string, error) {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--as":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("--as requires a value")
			}
			account = args[i+1]
			i++
		case strings.HasPrefix(a, "--as="):
			account = strings.TrimPrefix(a, "--as=")
		default:
			out = append(out, a)
		}
	}
	if account == "" {
		return out, nil
	}
	if strings.ContainsAny(account, "/.") {
		return nil, fmt.Errorf("--as %q: account name must not contain '/' or '.'", account)
	}
	return out, nil
}

func cmdSend(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: msg send \"<text>\" [to-jid]")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	body := args[0]
	to := cfg.To
	if len(args) > 1 {
		to = args[1]
	}
	if to == "" {
		return fmt.Errorf("no recipient given and XMPP_TO not set in .env")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := sendMessage(ctx, cfg, to, body); err != nil {
		return err
	}
	fmt.Printf("sent to %s: %s\n", to, body)
	return nil
}

func cmdRoom(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: msg room \"<text>\"")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	body := args[0]

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := sendRoomMessage(ctx, cfg, body); err != nil {
		return err
	}
	fmt.Printf("sent to room %s: %s\n", cfg.Room, body)
	return nil
}

// cmdTyping sends a single XEP-0085 "composing" chat state, to signal that
// the agent has started working on something, without a body message.
// Pass --room to send it to XMPP_ROOM instead of a direct chat.
func cmdTyping(args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if len(args) > 0 && args[0] == "--room" {
		if err := sendRoomTyping(ctx, cfg); err != nil {
			return err
		}
		fmt.Printf("sent typing indicator to room %s\n", cfg.Room)
		return nil
	}

	to := cfg.To
	if len(args) > 0 {
		to = args[0]
	}
	if to == "" {
		return fmt.Errorf("no recipient given and XMPP_TO not set in .env")
	}
	if err := sendTyping(ctx, cfg, to); err != nil {
		return err
	}
	fmt.Printf("sent typing indicator to %s\n", to)
	return nil
}

// cmdSendFile uploads a file via XEP-0363 and messages the resulting link
// to a direct chat (defaults to XMPP_TO).
func cmdSendFile(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: msg send-file <path> [to-jid]")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	path := args[0]
	to := cfg.To
	if len(args) > 1 {
		to = args[1]
	}
	if to == "" {
		return fmt.Errorf("no recipient given and XMPP_TO not set in .env")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	getURL, err := sendFile(ctx, cfg, path, to)
	if err != nil {
		return err
	}
	fmt.Printf("sent %s to %s: %s\n", path, to, getURL)
	return nil
}

// cmdRoomFile uploads a file via XEP-0363 and posts the resulting link to
// XMPP_ROOM.
func cmdRoomFile(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: msg room-file <path>")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	getURL, err := sendRoomFile(ctx, cfg, args[0])
	if err != nil {
		return err
	}
	fmt.Printf("sent %s to room %s: %s\n", args[0], cfg.Room, getURL)
	return nil
}

// cmdReact reacts (XEP-0444) to the most recent inbox message from the
// target (a direct contact's JID, or XMPP_ROOM with --room), since XMPP
// message IDs aren't otherwise convenient to type by hand.
func cmdReact(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: msg react <emoji> [to-jid|--room]")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	emoji := args[0]

	room := len(args) > 1 && args[1] == "--room"
	to := cfg.To
	matchPrefix := to
	if room {
		matchPrefix = cfg.Room
	} else if len(args) > 1 {
		to = args[1]
		matchPrefix = to
	}
	if matchPrefix == "" {
		return fmt.Errorf("no target given, and XMPP_TO/XMPP_ROOM not set in .env")
	}

	inboxPath, _ := dataFile("inbox.jsonl")
	targetID, err := lastMessageIDFrom(inboxPath, matchPrefix)
	if err != nil {
		return err
	}
	if targetID == "" {
		return fmt.Errorf("no message with an id found from %s in the inbox", matchPrefix)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if room {
		if err := sendRoomReaction(ctx, cfg, targetID, emoji); err != nil {
			return err
		}
		fmt.Printf("reacted %s to %s in room %s\n", emoji, targetID, cfg.Room)
		return nil
	}
	if err := sendReaction(ctx, cfg, to, targetID, emoji); err != nil {
		return err
	}
	fmt.Printf("reacted %s to %s from %s\n", emoji, targetID, to)
	return nil
}

// lastMessageIDFrom scans the inbox for the most recent message whose From
// starts with matchPrefix (a bare JID for direct chats, or the room JID for
// groupchat) and returns its stanza ID, or "" if none has one.
func lastMessageIDFrom(inboxPath, matchPrefix string) (string, error) {
	f, err := os.Open(inboxPath)
	if os.IsNotExist(err) {
		return "", fmt.Errorf("inbox empty; is the listen daemon running? see `msg status`")
	}
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var lastID string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var m incoming
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m.ID == "" || !strings.HasPrefix(m.From, matchPrefix) {
			continue
		}
		lastID = m.ID
	}
	return lastID, scanner.Err()
}

func cmdDisco(args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	target := cfg.To
	if len(args) > 0 {
		target = args[0]
	}
	if at := strings.IndexByte(target, '@'); at >= 0 {
		target = target[at+1:]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	items, err := discoItems(ctx, cfg, target)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Printf("no items found on %s\n", target)
		return nil
	}
	for _, it := range items {
		fmt.Printf("%s  %s\n", it.JID, it.Name)
	}
	return nil
}

// dataFile returns the path to a runtime state file (inbox, cursor,
// pidfile). When --as selected an account, the name is namespaced by
// inserting ".<account>" before its last extension (e.g. "inbox.jsonl" ->
// "inbox.beltino.jsonl") so accounts sharing a directory don't collide.
func dataFile(name string) (string, error) {
	if account != "" {
		if idx := strings.LastIndex(name, "."); idx >= 0 {
			name = name[:idx] + "." + account + name[idx:]
		} else {
			name = name + "." + account
		}
	}
	return filepath.Join(dataDir(), name), nil
}

// defaultPollWindow is how long presence stays "available" after a `msg
// check` that didn't declare its own --in, and is also the jitter buffer
// added on top of a declared --in. It's set well above a typical agent poll
// interval (the afk skill reschedules every ~60s) to absorb scheduling
// jitter without flapping presence.
const defaultPollWindow = 180 * time.Second

// touchLastPoll records that `msg check` just ran and when the caller expects
// to check again (inSeconds, or -1 if not declared via --in), so the listen
// daemon's presence (see claudePresence) can reflect whether an agent is
// actively polling right now rather than just whether the daemon process is
// up. Best-effort: a failure here shouldn't block `msg check` from doing its
// job.
func touchLastPoll(inSeconds int) {
	window := defaultPollWindow
	if inSeconds > 0 {
		window = time.Duration(inSeconds)*time.Second + defaultPollWindow
	}
	due := time.Now().Add(window).UTC().Format(time.RFC3339)
	p, _ := dataFile("next_poll_due")
	if err := os.WriteFile(p, []byte(due), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not record next poll time: %v\n", err)
	}
}

// claudePresence reports the (show, status) the listen daemon should
// announce, based on when the next `msg check` is due (see touchLastPoll).
// With no record yet, or one already past, it reports extended-away rather
// than available.
func claudePresence() (string, string) {
	p, _ := dataFile("next_poll_due")
	b, err := os.ReadFile(p)
	if err != nil {
		return "xa", "not actively watched by Claude"
	}
	due, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	if err != nil || time.Now().After(due) {
		return "xa", "not actively watched by Claude"
	}
	return "", "listening for messages"
}

// parseSince parses a --since value as either a full RFC3339 timestamp or a
// bare date (YYYY-MM-DD, taken as that date's UTC midnight).
func parseSince(spec string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, spec); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", spec); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("--since %q: expected an RFC3339 timestamp or a YYYY-MM-DD date", spec)
}

// offsetAtOrAfter scans the inbox from the start and returns the byte offset
// of the first message timestamped at or after cutoff (or the file size if
// none is), so `msg check --since` can start from an arbitrary point in
// history instead of the persisted cursor.
func offsetAtOrAfter(inboxPath string, cutoff time.Time) (int64, error) {
	f, err := os.Open(inboxPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var offset int64
	for scanner.Scan() {
		line := scanner.Bytes()
		lineLen := int64(len(line)) + 1 // + the newline the scanner stripped
		var m incoming
		if err := json.Unmarshal(line, &m); err == nil {
			if t, err := time.Parse(time.RFC3339, m.Time); err == nil && !t.Before(cutoff) {
				return offset, nil
			}
		}
		offset += lineLen
	}
	return offset, scanner.Err()
}

// lastMarkableMessage scans the inbox for the most recent direct, markable
// chat message, so a poll that found no new messages can still re-send a
// "displayed" heartbeat marker for it (see cmdCheck).
func lastMarkableMessage(inboxPath string) (incoming, bool, error) {
	f, err := os.Open(inboxPath)
	if os.IsNotExist(err) {
		return incoming{}, false, nil
	}
	if err != nil {
		return incoming{}, false, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var last incoming
	found := false
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var m incoming
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m.Markable && m.ID != "" && m.Type == "chat" {
			last = m
			found = true
		}
	}
	return last, found, scanner.Err()
}

// cmdCheck prints any inbox lines appended since the last check and advances
// the read cursor, so repeated calls only surface new messages.
//
// Flags:
//
//	--since <RFC3339|YYYY-MM-DD>   one-shot: read from this point in history
//	                                instead of the persisted cursor. The
//	                                cursor still advances to end-of-file
//	                                afterward, so a later plain check won't
//	                                replay this window again.
//	--in <seconds>                 how long until the next check is expected;
//	                                keeps presence "available" for roughly
//	                                that long instead of the default window.
//	--no-receipt                   skip sending XEP-0333 "displayed" markers
//	                                for this check (both for newly-surfaced
//	                                messages and the no-new-messages heartbeat).
func cmdCheck(args []string) error {
	var sinceSpec string
	var receiptsOff bool
	inSeconds := -1
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--since":
			i++
			if i >= len(args) {
				return fmt.Errorf("--since requires a value")
			}
			sinceSpec = args[i]
		case "--no-receipt":
			receiptsOff = true
		case "--in":
			i++
			if i >= len(args) {
				return fmt.Errorf("--in requires a value")
			}
			n, err := strconv.Atoi(args[i])
			if err != nil {
				return fmt.Errorf("--in: invalid seconds %q: %w", args[i], err)
			}
			inSeconds = n
		default:
			return fmt.Errorf("unknown check argument %q", args[i])
		}
	}
	touchLastPoll(inSeconds)

	inboxPath, _ := dataFile("inbox.jsonl")
	cursorPath, _ := dataFile(".inbox.cursor")

	f, err := os.Open(inboxPath)
	if os.IsNotExist(err) {
		fmt.Println("no messages (inbox empty; is the listen daemon running? see `msg status`)")
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	var offset int64
	if sinceSpec != "" {
		cutoff, err := parseSince(sinceSpec)
		if err != nil {
			return err
		}
		offset, err = offsetAtOrAfter(inboxPath, cutoff)
		if err != nil {
			return err
		}
	} else if b, err := os.ReadFile(cursorPath); err == nil {
		offset, _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	}

	info, err := f.Stat()
	if err != nil {
		return err
	}
	if offset > info.Size() {
		offset = 0 // inbox was rotated/truncated
	}

	if _, err := f.Seek(offset, 0); err != nil {
		return err
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	count := 0
	var toAck []incoming
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var m incoming
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m.ReactionToID != "" {
			fmt.Printf("[%s] %s reacted %s to %s\n", m.Time, m.From, strings.Join(m.Reactions, " "), m.ReactionToID)
			count++
			continue
		}
		fmt.Printf("[%s] %s: %s\n", m.Time, m.From, m.Body)
		if m.AttachmentPath != "" {
			fmt.Printf("    [attachment saved to %s]\n", m.AttachmentPath)
		} else if m.AttachmentURL != "" {
			fmt.Printf("    [attachment: %s]\n", m.AttachmentURL)
		}
		count++
		if m.Markable && m.ID != "" && m.Type == "chat" {
			toAck = append(toAck, m)
		}
	}
	if count == 0 {
		fmt.Println("no new messages")
	}

	newOffset, err := f.Seek(0, 1)
	if err != nil {
		return err
	}
	if err := os.WriteFile(cursorPath, []byte(strconv.FormatInt(newOffset, 10)), 0644); err != nil {
		return err
	}

	if !receiptsOff {
		if len(toAck) > 0 {
			sendDisplayedMarkers(toAck)
		} else if last, ok, err := lastMarkableMessage(inboxPath); err == nil && ok {
			sendDisplayedMarkers([]incoming{last})
		}
	}
	return nil
}

// sendDisplayedMarkers opens a short-lived connection and sends a XEP-0333
// "displayed" chat marker for each markable direct message just surfaced by
// `msg check`, so the sender can tell the message was actually processed
// rather than just delivered. Best-effort: failures are logged, not fatal,
// since the messages have already been printed and the cursor advanced.
func sendDisplayedMarkers(msgs []incoming) {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "skipping read receipts: %v\n", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, _, err := connect(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skipping read receipts: connect: %v\n", err)
		return
	}
	defer session.Close()

	go func() {
		_ = session.Serve(xmpp.HandlerFunc(func(t xmlstream.TokenReadEncoder, start *xml.StartElement) error {
			_, err := xmlstream.Copy(xmlstream.Discard(), t)
			return err
		}))
	}()

	for _, m := range msgs {
		if err := sendDisplayedMarker(ctx, session, m.From, m.ID); err != nil {
			fmt.Fprintf(os.Stderr, "failed to send read receipt to %s: %v\n", m.From, err)
		}
	}
}

// maxAttachmentBytes caps auto-downloaded attachments so a malicious or
// oversized link can't fill the disk; larger files are left as a link only.
const maxAttachmentBytes = 25 * 1024 * 1024

// downloadAttachment fetches rawURL into destDir and returns the local path.
// It only downloads over https from a host in allowedHosts, and refuses
// anything over maxAttachmentBytes, since rawURL comes from a message body
// an untrusted party could have sent.
func downloadAttachment(ctx context.Context, rawURL string, allowedHosts []string, destDir string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	allowed := false
	for _, h := range allowedHosts {
		if u.Host == h {
			allowed = true
			break
		}
	}
	if u.Scheme != "https" || !allowed {
		return "", fmt.Errorf("refusing to download from untrusted URL %s (expected https from one of %v)", rawURL, allowedHosts)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("GET %s: %s", rawURL, resp.Status)
	}
	if resp.ContentLength > maxAttachmentBytes {
		return "", fmt.Errorf("attachment too large (%d bytes, cap is %d)", resp.ContentLength, maxAttachmentBytes)
	}

	if err := os.MkdirAll(destDir, 0755); err != nil {
		return "", err
	}
	name := filepath.Base(u.Path)
	if name == "" || name == "/" || name == "." {
		name = "attachment"
	}
	dest := filepath.Join(destDir, name)
	if _, err := os.Stat(dest); err == nil {
		ext := filepath.Ext(name)
		dest = filepath.Join(destDir, strings.TrimSuffix(name, ext)+"-"+newID()[:8]+ext)
	}

	f, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer f.Close()

	n, err := io.Copy(f, io.LimitReader(resp.Body, maxAttachmentBytes+1))
	if err != nil {
		os.Remove(dest)
		return "", err
	}
	if n > maxAttachmentBytes {
		os.Remove(dest)
		return "", fmt.Errorf("attachment exceeds size cap (%d bytes)", maxAttachmentBytes)
	}
	return dest, nil
}

// listenState holds everything the listen daemon's message-processing path
// needs, shared between live delivery and MAM backfill so both go through
// the same dedup/download/persist logic.
type listenState struct {
	inbox        *os.File
	seenIDs      map[string]bool
	seenIDsFile  *os.File
	uploadHosts  []string
	downloadsDir string
	lastSeenPath string
}

// process dedups m by stanza ID (skipping it entirely if already seen —
// needed because MUC/offline history replays on every daemon restart),
// downloads any attachment, appends to the inbox, and advances the
// last-seen-time marker used to seed the next MAM backfill.
func (s *listenState) process(ctx context.Context, m incoming) {
	if m.ID != "" {
		if s.seenIDs[m.ID] {
			return
		}
		s.seenIDs[m.ID] = true
		fmt.Fprintln(s.seenIDsFile, m.ID)
	}

	if m.AttachmentURL != "" && len(s.uploadHosts) > 0 {
		if path, err := downloadAttachment(ctx, m.AttachmentURL, s.uploadHosts, s.downloadsDir); err != nil {
			fmt.Fprintf(os.Stderr, "failed to download attachment %s: %v\n", m.AttachmentURL, err)
		} else {
			m.AttachmentPath = path
		}
	}

	b, _ := json.Marshal(m)
	s.inbox.Write(append(b, '\n'))
	s.inbox.Sync()

	if t, err := time.Parse(time.RFC3339, m.Time); err == nil {
		if err := os.WriteFile(s.lastSeenPath, []byte(t.UTC().Format(time.RFC3339)), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "failed to persist last-seen time: %v\n", err)
		}
	}
}

// maxSeenIDs bounds the seen-message log so it doesn't grow forever; past
// this many entries it's rewritten keeping only the most recent half.
const maxSeenIDs = 5000

// loadSeenIDs reads path's newline-delimited stanza IDs into a set, trimming
// the file on disk if it's grown past maxSeenIDs.
func loadSeenIDs(path string) (map[string]bool, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	if len(lines) > maxSeenIDs {
		lines = lines[len(lines)-maxSeenIDs/2:]
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
			return nil, err
		}
	}

	seen := make(map[string]bool, len(lines))
	for _, id := range lines {
		if id != "" {
			seen[id] = true
		}
	}
	return seen, nil
}

func cmdListen(args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	pidPath, _ := dataFile(".listen.pid")
	if pid, ok := runningPID(pidPath); ok {
		return fmt.Errorf("listen daemon already running (pid %d); run `msg stop` first", pid)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0644); err != nil {
		return err
	}
	defer os.Remove(pidPath)

	inboxPath, _ := dataFile("inbox.jsonl")
	inbox, err := os.OpenFile(inboxPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer inbox.Close()

	uploadHosts, err := resolveUploadHosts(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not resolve upload service (%v); incoming attachments won't be auto-downloaded\n", err)
	}
	downloadsDir, _ := dataFile("downloads")

	seenIDsPath, _ := dataFile("seen_ids.jsonl")
	seenIDs, err := loadSeenIDs(seenIDsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not load seen-message log (%v); duplicates may resurface\n", err)
		seenIDs = map[string]bool{}
	}
	seenIDsFile, err := os.OpenFile(seenIDsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer seenIDsFile.Close()

	lastSeenPath, _ := dataFile("last_seen_time")

	state := &listenState{
		inbox:        inbox,
		seenIDs:      seenIDs,
		seenIDsFile:  seenIDsFile,
		uploadHosts:  uploadHosts,
		downloadsDir: downloadsDir,
		lastSeenPath: lastSeenPath,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.To != "" {
		if b, err := os.ReadFile(lastSeenPath); err == nil {
			if since, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b))); err == nil {
				backCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				backfilled, err := fetchDirectHistorySince(backCtx, cfg, since)
				cancel()
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: MAM backfill failed: %v\n", err)
				} else if len(backfilled) > 0 {
					fmt.Printf("backfilling %d message(s) from the archive\n", len(backfilled))
					for _, m := range backfilled {
						state.process(ctx, m)
					}
				}
			}
		}
	}

	fmt.Printf("listening as %s (Ctrl-C to stop, or `msg stop`)\n", cfg.JID)

	announced := false
	backoff := time.Second
	for {
		err := listen(ctx, cfg, func(m incoming) {
			state.process(ctx, m)
		}, func(session *xmpp.Session) {
			if announced {
				return
			}
			announced = true
			if cfg.Room != "" {
				time.Sleep(500 * time.Millisecond)
				if _, err := encodeMessage(ctx, session, cfg.Room, "listening for your replies now.", stanza.GroupChatMessage); err != nil {
					fmt.Fprintf(os.Stderr, "failed to send listening announcement: %v\n", err)
				}
				return
			}
			if cfg.To == "" {
				return
			}
			if err := sendOnSession(ctx, session, cfg.To, "listening for your replies now."); err != nil {
				fmt.Fprintf(os.Stderr, "failed to send listening announcement: %v\n", err)
			}
		}, claudePresence)
		if ctx.Err() != nil {
			return nil
		}
		fmt.Fprintf(os.Stderr, "connection lost: %v; reconnecting in %s\n", err, backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func cmdStop() error {
	pidPath, _ := dataFile(".listen.pid")
	pid, ok := runningPID(pidPath)
	if !ok {
		fmt.Println("listen daemon is not running")
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	fmt.Printf("stopped listen daemon (pid %d)\n", pid)
	return nil
}

func cmdStatus() error {
	pidPath, _ := dataFile(".listen.pid")
	if pid, ok := runningPID(pidPath); ok {
		fmt.Printf("listen daemon running (pid %d)\n", pid)
	} else {
		fmt.Println("listen daemon not running")
	}
	return nil
}

func runningPID(pidPath string) (int, bool) {
	b, err := os.ReadFile(pidPath)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return 0, false
	}
	return pid, true
}
