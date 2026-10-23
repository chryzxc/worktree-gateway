package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/chryzxc/worktree-gateway/internal/api"
	"github.com/chryzxc/worktree-gateway/internal/config"
)

// Preparation must work before `wtg up`, and replace only the requested service.
func TestPrepareRun(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git(t, root, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "wtg.yaml"), []byte("project: shop\nservices:\n  web: {}\n  api: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := New(Options{StateDir: t.TempDir(), Global: config.DefaultGlobal()})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(d.apiHandler())
	defer server.Close()
	prepare := func(service string, wantStatus int) (out api.PrepareRunResponse) {
		t.Helper()
		body, err := json.Marshal(api.PortRequest{Path: root, Service: service})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.Post(server.URL+"/v1/run/prepare", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != wantStatus {
			t.Fatalf("prepare: HTTP %d, want %d", resp.StatusCode, wantStatus)
		}
		if wantStatus == http.StatusOK {
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	first := prepare("web", http.StatusOK)
	if first.Port <= 0 || first.Env["PORT"] != strconv.Itoa(first.Port) || first.Env["WG_PROJECT"] != "shop" || first.Env["WG_SERVICE"] != "web" || first.Env["WG_URL"] == "" || len(first.Stopped) != 0 {
		t.Fatalf("first prepare = %+v", first)
	}

	proc := exec.Command("sleep", "30")
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- proc.Wait() }()
	t.Cleanup(func() { proc.Process.Kill(); <-done })
	if _, err := d.register(context.Background(), api.RegisterRequest{Path: root, Service: "web", Port: first.Port, PID: proc.Process.Pid}); err != nil {
		t.Fatal(err)
	}
	// An independently managed sibling must not be stopped or deregistered.
	if _, err := d.register(context.Background(), api.RegisterRequest{Path: root, Service: "api", Port: freePort(t)}); err != nil {
		t.Fatal(err)
	}
	prepare("", http.StatusBadRequest)
	if len(d.reg.Snapshot().Services) != 2 {
		t.Fatal("empty service stopped existing services")
	}
	next := prepare("web", http.StatusOK)
	if next.Port != first.Port || len(next.Stopped) != 1 || next.Stopped[0] != "web" || ProcessAlive(proc.Process.Pid) {
		t.Fatalf("replacement = %+v; old process alive = %v", next, ProcessAlive(proc.Process.Pid))
	}
	snap := d.reg.Snapshot()
	if len(snap.Services) != 1 || snap.Services[0].Name != "api" {
		t.Fatalf("sibling registration changed: %+v", snap.Services)
	}
}
