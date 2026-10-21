package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudePresence(t *testing.T) {
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for _, tt := range []struct {
		name       string
		record     *string
		wantShow   string
		wantStatus string
	}{
		{name: "missing poll record", wantShow: "xa", wantStatus: "not being actively watched"},
		{name: "empty poll record", record: new(string), wantShow: "xa", wantStatus: "not being actively watched"},
		{name: "invalid poll record", record: stringPtr("invalid"), wantShow: "xa", wantStatus: "not being actively watched"},
		{name: "expired poll record", record: &past, wantShow: "xa", wantStatus: "not being actively watched"},
		{name: "active polling", record: &future, wantStatus: "listening for messages"},
		{name: "active polling with whitespace", record: stringPtr(" \n" + future + "\n"), wantStatus: "listening for messages"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("MSG_DIR", dir)
			path, err := dataFile("next_poll_due")
			if err != nil {
				t.Fatal(err)
			}
			if filepath.Dir(path) != dir {
				t.Fatalf("poll record path %q is outside test directory", path)
			}
			if tt.record != nil {
				if err := os.WriteFile(path, []byte(*tt.record), 0600); err != nil {
					t.Fatal(err)
				}
			}
			show, status := claudePresence()
			if show != tt.wantShow || status != tt.wantStatus {
				t.Errorf("claudePresence() = (%q, %q), want (%q, %q)", show, status, tt.wantShow, tt.wantStatus)
			}
			if strings.Contains(strings.ToLower(status), "claude") {
				t.Errorf("presence status still references Claude: %q", status)
			}
		})
	}
}

func stringPtr(s string) *string { return &s }
