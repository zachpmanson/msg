package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// AccountConfig uses pi-msg's connection field names. Room and WebSocketURL
// retain msg's single-room and explicit WebSocket override capabilities.
type AccountConfig struct {
	JID           string `json:"jid"`
	Password      string `json:"password"`
	Owner         string `json:"owner,omitempty"`
	Service       string `json:"service,omitempty"`
	Room          string `json:"room,omitempty"`
	Nick          string `json:"nick,omitempty"`
	UploadService string `json:"uploadService,omitempty"`
	WebSocketURL  string `json:"websocketUrl,omitempty"`
}

type FileConfig struct {
	Accounts       map[string]AccountConfig `json:"accounts"`
	AccountAliases map[string]string        `json:"accountAliases,omitempty"`
}

// configPath follows existing directory discovery unless explicitly overridden.
// MSG_DIR still controls runtime state independently of MSG_CONFIG.
func configPath() string {
	if path := os.Getenv("MSG_CONFIG"); path != "" {
		return path
	}
	return filepath.Join(dataDir(), "config.json")
}

// loadFileConfig returns nil only if the implicit config is absent. Explicit,
// unreadable, and malformed configs never silently select legacy credentials.
func loadFileConfig() (*FileConfig, error) {
	path := configPath()
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) && os.Getenv("MSG_CONFIG") == "" {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var cfg FileConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("invalid config at %s: %w", path, err)
	}
	if cfg.Accounts == nil {
		return nil, fmt.Errorf("%s must have an accounts object", path)
	}
	for alias, target := range cfg.AccountAliases {
		if strings.TrimSpace(alias) == "" {
			return nil, fmt.Errorf("%s: account alias must not be blank", path)
		}
		if _, exists := cfg.Accounts[alias]; exists {
			return nil, fmt.Errorf("%s: account alias %q conflicts with an account", path, alias)
		}
		if _, exists := cfg.Accounts[target]; !exists {
			return nil, fmt.Errorf("%s: account alias %q refers to unknown account %q", path, alias, target)
		}
	}
	return &cfg, nil
}

func (cfg *FileConfig) accountName(requested string) (string, error) {
	name := requested
	if name == "" {
		name = "default"
	} else if target, ok := cfg.AccountAliases[name]; ok {
		name = target
	}
	if _, ok := cfg.Accounts[name]; !ok {
		names := make([]string, 0, len(cfg.Accounts))
		for name := range cfg.Accounts {
			names = append(names, name)
		}
		sort.Strings(names)
		return "", fmt.Errorf("%s: account %q not found (configured accounts: %s)", configPath(), name, strings.Join(names, ", "))
	}
	// Names become runtime filenames. Legacy --as already rejects these.
	if strings.ContainsAny(name, "/.") || strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("%s: invalid account name %q", configPath(), name)
	}
	return name, nil
}

// selectAccount canonicalizes aliases before any command touches runtime state.
// The default keeps its historical unnamespaced filenames; selecting it by
// alias or explicitly uses the same files rather than a second daemon/inbox.
func selectAccount() error {
	cfg, err := loadFileConfig()
	if err != nil || cfg == nil {
		return err
	}
	name, err := cfg.accountName(account)
	if err != nil {
		return err
	}
	account = name
	if name == "default" {
		account = ""
	}
	return nil
}

func loadConfig() (*Config, error) {
	file, err := loadFileConfig()
	if err != nil {
		return nil, err
	}
	if file == nil {
		return loadLegacyConfig()
	}
	name, err := file.accountName(account)
	if err != nil {
		return nil, err
	}
	a := file.Accounts[name]
	if a.JID == "" || a.Password == "" {
		return nil, fmt.Errorf("%s: account %q must set jid and password", configPath(), name)
	}
	cfg := &Config{
		JID: a.JID, Password: a.Password, To: a.Owner,
		Server: strings.TrimPrefix(a.Service, "xmpp://"),
		Room:   a.Room, Nick: a.Nick, UploadService: a.UploadService,
		WebSocket: a.WebSocketURL,
	}
	if strings.HasPrefix(a.Service, "wss://") && cfg.WebSocket == "" {
		cfg.WebSocket = a.Service
		cfg.Server = ""
	}
	if cfg.Nick == "" {
		cfg.Nick = strings.SplitN(cfg.JID, "@", 2)[0]
	}
	return cfg, nil
}

// stageJSONAccount prepares a protected replacement before any remote side
// effect. The caller owns the staged file: retain it after uncertain provisioning
// or commit failures so generated credentials remain recoverable.
func stageJSONAccount(name string, a AccountConfig) (staged string, err error) {
	path := configPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return "", err
	}
	var accounts map[string]json.RawMessage
	if err := json.Unmarshal(root["accounts"], &accounts); err != nil {
		return "", err
	}
	if accounts == nil {
		return "", fmt.Errorf("%s must have an accounts object", path)
	}
	if _, exists := accounts[name]; exists {
		return "", fmt.Errorf("account %q already exists in %s; not overwriting", name, path)
	}
	entry, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	accounts[name] = entry
	root["accounts"], err = json.Marshal(accounts)
	if err != nil {
		return "", err
	}
	raw, err = json.MarshalIndent(root, "", "  ")
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".msg-config-*")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			os.Remove(f.Name())
		}
	}()
	if _, err = f.Write(append(raw, '\n')); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	return f.Name(), nil
}

// addJSONAccount preserves unknown fields and atomically replaces the config.
func addJSONAccount(name string, a AccountConfig) error {
	staged, err := stageJSONAccount(name, a)
	if err != nil {
		return err
	}
	defer os.Remove(staged)
	return os.Rename(staged, configPath())
}
