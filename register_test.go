package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
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
