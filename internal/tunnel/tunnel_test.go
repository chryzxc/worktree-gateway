package tunnel

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chryzxc/worktree-gateway/internal/config"
)

func TestBuildSpec(t *testing.T) {
	local := "http://127.0.0.1:8790"
	s, err := BuildSpec(config.TunnelSettings{Provider: "cloudflared"}, local)
	if err != nil || s.Binary != "cloudflared" || !strings.Contains(strings.Join(s.Args, " "), "--url "+local) {
		t.Fatalf("quick: %v %+v", err, s)
	}
	if got := s.ParseURL("2026 INF |  https://abc-def-1.trycloudflare.com  |"); got != "https://abc-def-1.trycloudflare.com" {
		t.Fatalf("parse quick url: %q", got)
	}
	if _, err := BuildSpec(config.TunnelSettings{Provider: "cloudflared", Name: "dev"}, local); err == nil {
		t.Fatal("named tunnel without public_url must fail")
	}
	s, _ = BuildSpec(config.TunnelSettings{Provider: "cloudflared", Name: "dev", PublicURL: "https://dev.example.com/"}, local)
	if s.FixedURL != "https://dev.example.com" || s.Args[len(s.Args)-1] != "dev" {
		t.Fatalf("named: %+v", s)
	}
	s, _ = BuildSpec(config.TunnelSettings{Provider: "ngrok"}, local)
	if s.ParseURL(`{"lvl":"info","msg":"started tunnel","url":"https://x.ngrok-free.app"}`) != "https://x.ngrok-free.app" {
		t.Fatal("ngrok parse")
	}
	if _, err := BuildSpec(config.TunnelSettings{Provider: "external"}, local); err == nil {
		t.Fatal("external without url must fail")
	}
}

func TestExternal(t *testing.T) {
	s, _ := BuildSpec(config.TunnelSettings{Provider: "external", PublicURL: "https://dev.example.com"}, "http://127.0.0.1:1")
	tn := New(s, "http://127.0.0.1:1", nil)
	if err := tn.Start(); err != nil {
		t.Fatal(err)
	}
	if st := tn.Status(); st.State != StateConnected || st.PublicURL != "https://dev.example.com" {
		t.Fatalf("%+v", st)
	}
	tn.Stop()
	if tn.Status().State != StateStopped {
		t.Fatal("not stopped")
	}
}

func TestSupervisedFakeClient(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-cloudflared")
	os.WriteFile(bin, []byte("#!/bin/sh\necho 'INF | https://fake-one.trycloudflare.com |' >&2\nsleep 30\n"), 0o755)
	s, _ := BuildSpec(config.TunnelSettings{Provider: "cloudflared"}, "http://127.0.0.1:1")
	s.Binary = bin
	changes := make(chan Status, 10)
	tn := New(s, "http://127.0.0.1:1", func(st Status) { changes <- st })
	if err := tn.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case st := <-changes:
			if st.State == StateConnected && st.PublicURL == "https://fake-one.trycloudflare.com" {
				tn.Stop()
				return
			}
		case <-deadline:
			t.Fatalf("never connected: %+v", tn.Status())
		}
	}
}
