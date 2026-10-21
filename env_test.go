package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func configTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MSG_DIR", dir)
	t.Setenv("MSG_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	old := account
	account = ""
	t.Cleanup(func() { account = old })
	return dir
}

func writeTestConfig(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestJSONMultipleAccounts(t *testing.T) {
	dir := configTestDir(t)
	writeTestConfig(t, filepath.Join(dir, "config.json"), `{
		"accounts": {
			"default": {"jid":"primary@example.com", "password":"primary-secret", "owner":"human@example.com"},
			"droid": {"jid":"droid@example.net", "password":"droid-secret", "owner":"other@example.net", "service":"xmpp://localhost:5223", "room":"test@muc.example.net", "nick":"Droid", "uploadService":"upload.example.net", "websocketUrl":"wss://example.net/ws"}
		},
		"accountAliases":{"robot":"droid", "primary":"default"}
	}`)
	wantDefault := &Config{JID: "primary@example.com", Password: "primary-secret", To: "human@example.com", Nick: "primary"}
	wantDroid := &Config{JID: "droid@example.net", Password: "droid-secret", To: "other@example.net", Server: "localhost:5223", Room: "test@muc.example.net", Nick: "Droid", UploadService: "upload.example.net", WebSocket: "wss://example.net/ws"}
	for _, tc := range []struct {
		name  string
		want  *Config
		state string
	}{
		{"", wantDefault, "inbox.jsonl"},
		{"droid", wantDroid, "inbox.droid.jsonl"},
		{"robot", wantDroid, "inbox.droid.jsonl"},
		{"default", wantDefault, "inbox.jsonl"},
		{"primary", wantDefault, "inbox.jsonl"},
	} {
		t.Run("account="+tc.name, func(t *testing.T) {
			account = tc.name
			got, err := loadConfig()
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("loadConfig() = %#v, %v; want %#v", got, err, tc.want)
			}
			if err := selectAccount(); err != nil {
				t.Fatal(err)
			}
			path, err := dataFile("inbox.jsonl")
			if err != nil || path != filepath.Join(dir, tc.state) {
				t.Fatalf("dataFile = %q, %v", path, err)
			}
		})
	}
}

func TestLegacyConfigPreserved(t *testing.T) {
	dir := configTestDir(t)
	writeTestConfig(t, filepath.Join(dir, ".env"), `# comment
XMPP_JID='default@example.com'
XMPP_PASSWORD="old-secret"
XMPP_TO=human@example.com
XMPP_SERVER=localhost:5223
XMPP_ROOM=test@muc.example.com
XMPP_NICK=old-nick
XMPP_UPLOAD_SERVICE=upload.example.com
XMPP_WEBSOCKET_URL=wss://example.com/ws
XMPP_API_URL=https://example.com/api
XMPP_API_USER=admin@example.com
XMPP_API_PASSWORD=admin-secret
`)
	writeTestConfig(t, filepath.Join(dir, ".env.droid"), "XMPP_JID=droid@example.net\nXMPP_PASSWORD=droid-secret\n")
	want := &Config{JID: "default@example.com", Password: "old-secret", To: "human@example.com", Server: "localhost:5223", Room: "test@muc.example.com", Nick: "old-nick", UploadService: "upload.example.com", WebSocket: "wss://example.com/ws"}
	got, err := loadConfig()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("default = %#v, %v", got, err)
	}
	account = "droid"
	if err := selectAccount(); err != nil {
		t.Fatal(err)
	}
	got, err = loadConfig()
	if err != nil || got.JID != "droid@example.net" || got.Nick != "droid" || got.To != "" {
		t.Fatalf("named = %#v, %v", got, err)
	}
	p, _ := dataFile(".inbox.cursor")
	if p != filepath.Join(dir, ".inbox.droid.cursor") {
		t.Fatal(p)
	}
	api, err := loadAPIConfig()
	if err != nil || api.Host != "example.com" || api.User != "admin@example.com" {
		t.Fatalf("shared API = %#v, %v", api, err)
	}
	account = "missing"
	if _, err := loadConfig(); err == nil {
		t.Fatal("missing account must not fall back to default")
	}
}

func TestJSONErrorsNeverFallback(t *testing.T) {
	for _, tc := range []struct{ name, raw, requested, contains string }{
		{"malformed", `{`, "", "invalid config"},
		{"missing accounts", `{}`, "", "accounts object"},
		{"null accounts", `{"accounts":null}`, "", "accounts object"},
		{"empty accounts", `{"accounts":{}}`, "", "not found"},
		{"no default", `{"accounts":{"droid":{"jid":"d@example.com","password":"s"}}}`, "", "not found"},
		{"unknown explicit", `{"accounts":{"default":{"jid":"d@example.com","password":"s"}}}`, "unknown", "not found"},
		{"missing jid", `{"accounts":{"default":{"password":"s"}}}`, "", "jid and password"},
		{"missing password", `{"accounts":{"default":{"jid":"d@example.com"}}}`, "", "jid and password"},
		{"blank alias", `{"accounts":{"default":{}},"accountAliases":{" ":"default"}}`, "", "must not be blank"},
		{"shadow alias", `{"accounts":{"default":{}},"accountAliases":{"default":"default"}}`, "", "conflicts"},
		{"dangling alias", `{"accounts":{"default":{}},"accountAliases":{"robot":"missing"}}`, "", "unknown account"},
		{"unsafe alias target", `{"accounts":{"../bad":{"jid":"x","password":"s"}},"accountAliases":{"robot":"../bad"}}`, "robot", "invalid account name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := configTestDir(t)
			writeTestConfig(t, filepath.Join(dir, ".env"), "XMPP_JID=legacy@example.com\nXMPP_PASSWORD=legacy-secret\n")
			writeTestConfig(t, filepath.Join(dir, ".env.unknown"), "XMPP_JID=unknown@example.com\nXMPP_PASSWORD=legacy-secret\n")
			writeTestConfig(t, filepath.Join(dir, "config.json"), tc.raw)
			account = tc.requested
			_, err := loadConfig()
			if err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("error = %v; want %q", err, tc.contains)
			}
		})
	}
}

func TestJSONWebSocketServiceAndOptionalOwner(t *testing.T) {
	dir := configTestDir(t)
	writeTestConfig(t, filepath.Join(dir, "config.json"), `{"accounts":{"default":{"jid":"bot@example.com","password":"s","service":"wss://example.com/ws"}}}`)
	cfg, err := loadConfig()
	if err != nil || cfg.WebSocket != "wss://example.com/ws" || cfg.Server != "" || cfg.To != "" {
		t.Fatalf("config = %#v, %v", cfg, err)
	}
}

func TestJSONDiscoveryAndOverrides(t *testing.T) {
	dir := configTestDir(t)
	t.Setenv("MSG_DIR", "")
	xdg := filepath.Join(t.TempDir(), "msg")
	if err := os.MkdirAll(xdg, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Dir(xdg))
	writeTestConfig(t, filepath.Join(xdg, "config.json"), `{"accounts":{"default":{"jid":"xdg@example.com","password":"s"}}}`)
	if dataDir() != xdg {
		t.Fatalf("dataDir = %s", dataDir())
	}
	cfg, err := loadConfig()
	if err != nil || cfg.JID != "xdg@example.com" {
		t.Fatalf("XDG = %#v, %v", cfg, err)
	}
	t.Setenv("MSG_DIR", dir)
	writeTestConfig(t, filepath.Join(dir, "config.json"), `{"accounts":{"default":{"jid":"dir@example.com","password":"s"}}}`)
	cfg, err = loadConfig()
	if err != nil || cfg.JID != "dir@example.com" {
		t.Fatalf("MSG_DIR = %#v, %v", cfg, err)
	}
	explicit := filepath.Join(t.TempDir(), "accounts.json")
	writeTestConfig(t, explicit, `{"accounts":{"default":{"jid":"explicit@example.com","password":"s"}}}`)
	t.Setenv("MSG_CONFIG", explicit)
	cfg, err = loadConfig()
	if err != nil || cfg.JID != "explicit@example.com" || dataDir() != dir {
		t.Fatalf("explicit = %#v, %v, state=%s", cfg, err, dataDir())
	}
	t.Setenv("MSG_DIR", "")
	if dataDir() != filepath.Dir(explicit) {
		t.Fatalf("explicit state = %s", dataDir())
	}
	t.Setenv("MSG_CONFIG", filepath.Join(dir, "missing.json"))
	if _, err := loadConfig(); err == nil {
		t.Fatal("missing explicit config must fail")
	}
}

func TestLegacyDiscovery(t *testing.T) {
	configTestDir(t)
	dir := t.TempDir()
	account = "droid"
	writeTestConfig(t, filepath.Join(dir, ".env.droid"), "XMPP_JID=droid@example.com\nXMPP_PASSWORD=s\n")
	if !dirHasEnv(dir) {
		t.Fatal("persona-only directory must qualify")
	}
	account = "other"
	if dirHasEnv(dir) {
		t.Fatal("unrelated persona-only directory must not qualify")
	}
	writeTestConfig(t, filepath.Join(dir, ".env"), "XMPP_API_URL=https://example.com/api\n")
	if !dirHasEnv(dir) {
		t.Fatal("shared API directory must qualify")
	}
}

func TestAddJSONAccountPreservesConfig(t *testing.T) {
	dir := configTestDir(t)
	path := filepath.Join(dir, "config.json")
	writeTestConfig(t, path, `{"accounts":{"default":{"jid":"bot@example.com","password":"old","extra":{"keep":true}}},"accountAliases":{"primary":"default"},"future":{"keep":true}}`)
	newAccount := AccountConfig{JID: "droid@example.net", Password: "new"}
	if err := addJSONAccount("droid", newAccount); err != nil {
		t.Fatal(err)
	}
	account = "droid"
	cfg, err := loadConfig()
	if err != nil || cfg.JID != newAccount.JID || cfg.Password != "new" {
		t.Fatalf("new account = %#v, %v", cfg, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(root["accounts"]), `"extra"`) || !strings.Contains(string(root["future"]), `true`) || !strings.Contains(string(root["accountAliases"]), `"primary"`) {
		t.Fatalf("fields lost: %s", b)
	}
	if err := addJSONAccount("default", newAccount); err == nil {
		t.Fatal("must not overwrite existing account")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(b) {
		t.Fatal("failed update changed config")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("config permissions = %v, %v", info, err)
	}
}

func TestImplicitJSONSymlinks(t *testing.T) {
	dir := configTestDir(t)
	writeTestConfig(t, filepath.Join(dir, ".env"), "XMPP_JID=stale@example.com\nXMPP_PASSWORD=old\n")
	target := filepath.Join(dir, "managed.json")
	if err := os.Symlink(target, filepath.Join(dir, "config.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(); err == nil {
		t.Fatal("dangling JSON must not select legacy credentials")
	}
	writeTestConfig(t, target, `{"accounts":{"default":{"jid":"managed@example.com","password":"s"}}}`)
	cfg, err := loadConfig()
	if err != nil || cfg.JID != "managed@example.com" {
		t.Fatalf("valid JSON symlink = %#v, %v", cfg, err)
	}
	if os.Geteuid() != 0 {
		if err := os.Chmod(target, 0000); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(); err == nil {
			t.Fatal("unreadable JSON target must not select legacy credentials")
		}
	}
}

func TestDanglingXDGJSONDoesNotSelectCWD(t *testing.T) {
	cwd := configTestDir(t)
	t.Chdir(cwd)
	t.Setenv("MSG_DIR", "")
	writeTestConfig(t, filepath.Join(cwd, ".env"), "XMPP_JID=stale@example.com\nXMPP_PASSWORD=old\n")
	xdg := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "msg")
	if err := os.Mkdir(xdg, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing-managed-config", filepath.Join(xdg, "config.json")); err != nil {
		t.Fatal(err)
	}
	if dataDir() != xdg {
		t.Fatalf("selected %s instead of authoritative XDG %s", dataDir(), xdg)
	}
	if _, err := loadConfig(); err == nil {
		t.Fatal("dangling XDG JSON must fail, not select cwd credentials")
	}
}

func TestLegacyLFOnlyAndWhitespaceSemantics(t *testing.T) {
	dir := configTestDir(t)
	for _, value := range []string{"left\u2028right", "left\u0085right", "left\vright", "left\fright", "left\rright", "\x1csecret\x1f"} {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			writeTestConfig(t, filepath.Join(dir, ".env"), "XMPP_JID=bot@example.com\r\nXMPP_PASSWORD=\u2003 '"+value+"' \u3000\r\n")
			cfg, err := loadLegacyConfig()
			if err != nil || cfg.Password != value {
				t.Fatalf("legacy password bytes changed: %v", err)
			}
		})
	}
}
