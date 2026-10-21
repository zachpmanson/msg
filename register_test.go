package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRegisterSavesSelectedConfigFormat(t *testing.T) {
	for _, useJSON := range []bool{false, true} {
		name := "dotenv"
		if useJSON {
			name = "json"
		}
		t.Run(name, func(t *testing.T) {
			dir := configTestDir(t)
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				user, password, ok := r.BasicAuth()
				if !ok || user != "admin@example.com" || password != "api-secret" || r.URL.Path != "/register" {
					t.Errorf("unexpected registration authentication/path")
				}
				var got map[string]string
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Error(err)
				}
				if got["host"] != "example.com" || got["user"] != "droid" || got["password"] != "droid-secret" {
					t.Errorf("unexpected registration body: %#v", got)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"status":"ok"}`))
			}))
			defer srv.Close()
			writeTestConfig(t, filepath.Join(dir, ".env"), "XMPP_API_URL="+srv.URL+"\nXMPP_API_USER=admin@example.com\nXMPP_API_PASSWORD=api-secret\n")
			if useJSON {
				// Registration does not require a default account or --as to exist.
				writeTestConfig(t, filepath.Join(dir, "config.json"), `{"accounts":{},"future":"preserve"}`)
			}
			account = "not-an-account"
			if err := cmdRegister([]string{"droid", "droid-secret"}); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("registration calls = %d", calls)
			}
			account = "droid"
			cfg, err := loadConfig()
			if err != nil || cfg.JID != "droid@example.com" || cfg.Password != "droid-secret" {
				t.Fatalf("registered account = %#v, %v", cfg, err)
			}
			if useJSON {
				if _, err := os.Stat(filepath.Join(dir, ".env.droid")); !os.IsNotExist(err) {
					t.Fatalf("JSON registration wrote dotenv: %v", err)
				}
				if err := cmdRegister([]string{"droid", "replacement"}); err == nil {
					t.Fatal("existing JSON account must not be overwritten")
				}
				if calls != 1 {
					t.Fatal("duplicate JSON registration contacted server")
				}
			}
		})
	}
}

func TestRegisterDoesNotOverwriteJSONAlias(t *testing.T) {
	dir := configTestDir(t)
	writeTestConfig(t, filepath.Join(dir, "config.json"), `{"accounts":{"default":{"jid":"bot@example.com","password":"s"}},"accountAliases":{"droid":"default"}}`)
	if err := cmdRegister([]string{"droid", "secret"}); err == nil {
		t.Fatal("alias must not be overwritten")
	}
}

func TestJSONRegistrationRecovery(t *testing.T) {
	for _, outcome := range []string{"success", "preflight failure", "commit failure", "API error", "ambiguous response"} {
		t.Run(outcome, func(t *testing.T) {
			if os.Geteuid() == 0 && (outcome == "preflight failure" || outcome == "commit failure") {
				t.Skip("permissions test requires non-root user")
			}
			dir := configTestDir(t)
			// Keep API credentials separate from the protected JSON destination.
			configDir := t.TempDir()
			path := filepath.Join(configDir, "config.json")
			t.Setenv("MSG_CONFIG", path)
			original := `{"accounts":{"default":{"jid":"old@example.com","password":"old","extra":true}},"future":{"keep":true},"accountAliases":{"primary":"default"}}`
			writeTestConfig(t, path, original)
			t.Cleanup(func() { os.Chmod(configDir, 0700) })
			calls := 0
			generated := ""
			handled := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(handled)
				calls++
				var request map[string]string
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				generated = request["password"]
				// Credentials must already be recoverable before provisioning.
				pending, _ := filepath.Glob(filepath.Join(configDir, ".msg-config-*"))
				if len(pending) != 1 {
					t.Errorf("staged configs before API = %d", len(pending))
				}
				if outcome == "commit failure" {
					if err := os.Chmod(configDir, 0500); err != nil {
						t.Error(err)
					}
				}
				if outcome == "API error" {
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte(generated)) // Server output must not leak credentials.
					return
				}
				if outcome == "ambiguous response" {
					connection, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					connection.Close() // Remote may have provisioned before disconnecting.
					return
				}
				w.Write([]byte(`{"status":"ok"}`))
			}))
			defer srv.Close()
			writeTestConfig(t, filepath.Join(dir, ".env"), "XMPP_API_URL="+srv.URL+"\nXMPP_API_USER=admin@example.com\nXMPP_API_PASSWORD=api-secret\n")
			if outcome == "preflight failure" {
				if err := os.Chmod(configDir, 0500); err != nil {
					t.Fatal(err)
				}
			}
			err := cmdRegister([]string{"newbot"})
			if outcome != "preflight failure" {
				// A hijacked connection can close before its handler returns.
				// Synchronize captured request fields before inspecting them.
				select {
				case <-handled:
				case <-time.After(5 * time.Second):
					t.Fatal("registration handler did not finish")
				}
			}
			if outcome == "success" && err != nil || outcome != "success" && err == nil {
				t.Fatalf("unexpected result: %v", err)
			}
			pending, _ := filepath.Glob(filepath.Join(configDir, ".msg-config-*"))
			if outcome == "preflight failure" {
				if calls != 0 || len(pending) != 0 {
					t.Fatalf("preflight failure: calls=%d artifacts=%d", calls, len(pending))
				}
			} else {
				if calls != 1 || len(generated) != 32 {
					t.Fatalf("calls=%d generated password length=%d", calls, len(generated))
				}
				artifact := path
				if outcome == "success" {
					if len(pending) != 0 {
						t.Fatal("successful rename left staging file")
					}
				} else {
					if len(pending) != 1 {
						t.Fatalf("expected recovery file, got %d", len(pending))
					}
					artifact = pending[0]
					if !strings.Contains(err.Error(), artifact) || strings.Contains(err.Error(), generated) {
						t.Fatal("error must report recovery path, not password")
					}
				}
				info, statErr := os.Stat(artifact)
				if statErr != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("artifact permissions: %v, %v", info, statErr)
				}
				raw, readErr := os.ReadFile(artifact)
				if readErr != nil {
					t.Fatal(readErr)
				}
				var recovered struct {
					Accounts map[string]AccountConfig
				}
				if err := json.Unmarshal(raw, &recovered); err != nil {
					t.Fatal(err)
				}
				if recovered.Accounts["newbot"].Password != generated || recovered.Accounts["newbot"].JID != "newbot@example.com" {
					t.Fatal("generated credentials not recoverable")
				}
				for _, field := range []string{`"extra": true`, `"future"`, `"primary"`} {
					if !strings.Contains(string(raw), field) {
						t.Fatalf("unknown field/alias lost: %s", field)
					}
				}
			}
			if outcome != "success" {
				after, readErr := os.ReadFile(path)
				if readErr != nil || string(after) != original {
					t.Fatalf("original config changed: %v", readErr)
				}
			}
		})
	}
}
