package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config holds the account details loaded from .env.
type Config struct {
	JID      string // full JID of the bot account, e.g. bot@example.com
	Password string
	Server   string // optional host:port override; defaults to the JID domain
	To       string // JID to message by default (the human's account)
	Room     string // optional MUC room bare JID, e.g. testing@conference.example.com
	Nick     string // nickname to use in Room; defaults to the JID's local part
	// UploadService optionally overrides the XEP-0363 HTTP upload component
	// JID, skipping disco discovery for send-file/room-file.
	UploadService string

	// WebSocket optionally sets a wss:// URL to connect via XMPP-over-WebSocket
	// (RFC 7395) instead of a raw TCP connection. Useful when port 5222 is
	// blocked but 443 is open (e.g. cloud sandbox environments).
	// Example: wss://chat.example.com/ws
	WebSocket string
}

func loadEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	vals := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		val = strings.Trim(val, `"'`)
		vals[key] = val
	}
	return vals, scanner.Err()
}

// envFileName returns the .env filename to load: plain ".env" normally, or
// ".env.<account>" when --as selected a named account sharing this directory.
func envFileName() string {
	if account == "" {
		return ".env"
	}
	return ".env." + account
}

func loadConfig() (*Config, error) {
	path := filepath.Join(dataDir(), envFileName())
	vals, err := loadEnv(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	cfg := &Config{
		JID:      vals["XMPP_JID"],
		Password: vals["XMPP_PASSWORD"],
		Server:   vals["XMPP_SERVER"],
		To:       vals["XMPP_TO"],
		Room:     vals["XMPP_ROOM"],
		Nick:     vals["XMPP_NICK"],

		UploadService: vals["XMPP_UPLOAD_SERVICE"],
		WebSocket:     vals["XMPP_WEBSOCKET_URL"],
	}

	if cfg.JID == "" || cfg.Password == "" {
		return nil, fmt.Errorf("%s must set XMPP_JID and XMPP_PASSWORD", path)
	}
	if cfg.Nick == "" {
		if at := strings.IndexByte(cfg.JID, '@'); at >= 0 {
			cfg.Nick = cfg.JID[:at]
		} else {
			cfg.Nick = cfg.JID
		}
	}
	return cfg, nil
}

// APIConfig holds credentials for ejabberd's HTTP Admin API (mod_http_api),
// used by "msg register" to provision new persona accounts. Unlike the
// per-persona .env.<account> files, these always come from the base ".env"
// regardless of --as, since the API credentials are a shared admin resource,
// not tied to any one persona.
type APIConfig struct {
	URL      string // base URL, e.g. https://chat.zachmanson.com/api
	User     string // full JID used for HTTP Basic Auth, e.g. msg-api@chat.zachmanson.com
	Password string
	Host     string // XMPP domain new accounts are registered under
}

func loadAPIConfig() (*APIConfig, error) {
	path := filepath.Join(dataDir(), ".env")
	vals, err := loadEnv(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	cfg := &APIConfig{
		URL:      vals["XMPP_API_URL"],
		User:     vals["XMPP_API_USER"],
		Password: vals["XMPP_API_PASSWORD"],
		Host:     vals["XMPP_API_HOST"],
	}
	if cfg.URL == "" || cfg.User == "" || cfg.Password == "" {
		return nil, fmt.Errorf("%s must set XMPP_API_URL, XMPP_API_USER, and XMPP_API_PASSWORD", path)
	}
	if cfg.Host == "" {
		if at := strings.IndexByte(cfg.User, '@'); at >= 0 {
			cfg.Host = cfg.User[at+1:]
		} else {
			return nil, fmt.Errorf("%s must set XMPP_API_HOST (could not infer domain from XMPP_API_USER)", path)
		}
	}
	return cfg, nil
}

// dataDir returns the directory holding .env and this account's runtime
// state (inbox.jsonl, cursor, pidfile). MSG_DIR overrides discovery, so one
// approved binary can act as multiple accounts (e.g. for self-testing)
// without spawning a second, unapproved binary path.
func dataDir() string {
	if override := os.Getenv("MSG_DIR"); override != "" {
		return override
	}
	return envDir()
}

// envDir locates the directory holding the .env, checking the current working
// directory first, then the running binary's directory, then the XDG config
// dir (~/.config/msg), then ~/projects/msg, so the tool works whether invoked
// via `go run`, a built binary on PATH, or from an unrelated cwd. This is the
// discovery used when MSG_DIR is unset; MSG_DIR overrides all of it.
//
// A directory qualifies if it contains the env file this invocation will
// actually load — ".env.<account>" under --as, else ".env". (A dir holding
// only ".env" also qualifies under --as, so shared API creds still resolve.)
// Keying on the account file matters for persona-only setups that have no
// plain ".env" at all.
func envDir() string {
	// XDG config dir: ~/.config/msg (or $XDG_CONFIG_HOME/msg). This is the
	// intended home for the global install — config decoupled from any dev
	// checkout — and is checked first so a stray ".env"/".env.<account>" left
	// in whatever directory msg happens to be invoked from can't shadow the
	// real stored account config.
	if dir := xdgConfigDir(); dir != "" && dirHasEnv(dir) {
		return dir
	}
	if wd, err := os.Getwd(); err == nil {
		if dirHasEnv(wd) {
			return wd
		}
	}
	if exe, err := os.Executable(); err == nil {
		if dir := filepath.Dir(exe); dirHasEnv(dir) {
			return dir
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if dir := filepath.Join(home, "projects", "msg"); dirHasEnv(dir) {
			return dir
		}
	}
	wd, _ := os.Getwd()
	return wd
}

// dirHasEnv reports whether dir contains the env file this invocation would
// load: the account-specific ".env.<account>" when --as is set, or a plain
// ".env" (also accepted under --as, for shared XMPP_API_* register creds).
func dirHasEnv(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, envFileName())); err == nil {
		return true
	}
	if account != "" {
		if _, err := os.Stat(filepath.Join(dir, ".env")); err == nil {
			return true
		}
	}
	return false
}

// xdgConfigDir returns the msg config directory under the XDG base dir spec:
// $XDG_CONFIG_HOME/msg if set, else ~/.config/msg. Returns "" if the home
// directory can't be determined and XDG_CONFIG_HOME is unset.
func xdgConfigDir() string {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, "msg")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "msg")
	}
	return ""
}
