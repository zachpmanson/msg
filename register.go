package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// apiResponse is ejabberd's mod_http_api response shape for register/unregister.
type apiResponse struct {
	Status  string `json:"status"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// registerAccount calls ejabberd's HTTP Admin API to provision a new XMPP
// account, scoped (per api_permissions on the server) to the register
// command only.
func registerAccount(ctx context.Context, api *APIConfig, user, password string) error {
	body, err := json.Marshal(map[string]string{
		"user":     user,
		"host":     api.Host,
		"password": password,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(api.URL, "/")+"/register", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(api.User, api.Password)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling register API: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("register API returned %s: %s", resp.Status, string(respBody))
	}

	var parsed apiResponse
	if err := json.Unmarshal(respBody, &parsed); err == nil && parsed.Status == "error" {
		return fmt.Errorf("register API error (code %d): %s", parsed.Code, parsed.Message)
	}
	return nil
}

// randomPassword generates a URL-safe random password for a newly
// provisioned account.
func randomPassword() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func cmdRegister(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: msg register <name> [password]")
	}
	name := args[0]
	if strings.ContainsAny(name, "/.@ ") {
		return fmt.Errorf("register %q: account name must not contain '/', '.', '@', or spaces", name)
	}

	api, err := loadAPIConfig()
	if err != nil {
		return err
	}

	password := ""
	if len(args) > 1 {
		password = args[1]
	} else {
		password, err = randomPassword()
		if err != nil {
			return fmt.Errorf("generating password: %w", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := registerAccount(ctx, api, name, password); err != nil {
		return err
	}

	envPath := filepath.Join(dataDir(), ".env."+name)
	if _, err := os.Stat(envPath); err == nil {
		fmt.Printf("registered %s@%s, but %s already exists — not overwriting. Password: %s\n", name, api.Host, envPath, password)
		return nil
	}

	contents := fmt.Sprintf(`XMPP_JID=%s@%s
XMPP_PASSWORD=%s
XMPP_TO=%s
`, name, api.Host, password, "")
	if err := os.WriteFile(envPath, []byte(contents), 0600); err != nil {
		return fmt.Errorf("registered %s@%s but failed to write %s: %w", name, api.Host, envPath, err)
	}

	fmt.Printf("registered %s@%s, wrote %s (fill in XMPP_TO before use)\n", name, api.Host, envPath)
	return nil
}
