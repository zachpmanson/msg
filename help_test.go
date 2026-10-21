package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCLIHelp(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "msg")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build msg: %v\n%s", err, output)
	}

	for _, scenario := range []struct {
		name            string
		legacy          bool
		json            string
		explicitMissing bool
	}{
		{name: "unconfigured"},
		{name: "legacy configured", legacy: true},
		{name: "malformed JSON", json: "{"},
		{name: "explicit missing JSON", explicitMissing: true},
		{name: "unknown selected JSON account", json: `{"accounts":{"default":{"jid":"bot@example.invalid","password":"test"}}}`},
		{name: "known JSON account", json: `{"accounts":{"default":{"jid":"bot@example.invalid","password":"test"},"test":{"jid":"test@example.invalid","password":"test"}}}`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			dir := t.TempDir()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			var connections atomic.Int32
			done := make(chan struct{})
			go func() {
				defer close(done)
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					connections.Add(1)
					conn.Close()
				}
			}()
			defer func() {
				listener.Close()
				<-done
				if got := connections.Load(); got != 0 {
					t.Errorf("help attempted %d XMPP connections; want none", got)
				}
			}()

			if scenario.legacy {
				config := fmt.Sprintf("XMPP_JID=bot@example.invalid\nXMPP_PASSWORD=test\nXMPP_TO=recipient@example.invalid\nXMPP_ROOM=room@example.invalid\nXMPP_SERVER=%s\n", listener.Addr())
				for _, name := range []string{".env", ".env.test"} {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(config), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}

			if scenario.json != "" {
				if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(scenario.json), 0600); err != nil {
					t.Fatal(err)
				}
			}

			for _, args := range [][]string{
				{"--help"},
				{"-h"},
				{"--as", "test", "--help"},
				{"--as=test", "-h"},
				{"--help", "--as", "test"},
			} {
				t.Run(strings.Join(args, " "), func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, binary, args...)
					cmd.Dir = dir
					// Never discover real account credentials or use a real proxy.
					cmd.Env = []string{"MSG_DIR=" + dir, "HOME=" + dir, "NO_PROXY=*"}
					if scenario.explicitMissing {
						cmd.Env = append(cmd.Env, "MSG_CONFIG="+filepath.Join(dir, "missing.json"))
					}
					var stdout, stderr bytes.Buffer
					cmd.Stdout, cmd.Stderr = &stdout, &stderr
					if err := cmd.Run(); err != nil {
						t.Fatalf("help must exit successfully: %v\nstdout: %s\nstderr: %s", err, &stdout, &stderr)
					}
					for _, want := range []string{"usage:", "msg send", "msg room", "--as <account>", "msg --help", "alias: -h"} {
						if !strings.Contains(stdout.String(), want) {
							t.Errorf("help missing %q: %s", want, &stdout)
						}
					}
					if stderr.Len() != 0 {
						t.Errorf("unexpected stderr: %s", &stderr)
					}
				})
			}
		})
	}
}
