package daemon

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chryzxc/worktree-gateway/internal/api"
	"github.com/chryzxc/worktree-gateway/internal/config"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestDaemonEndToEnd boots a real daemon (embedded Caddy) against a real
// repository with two worktrees and drives it through the socket API.
func TestDaemonEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("boots caddy")
	}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	main := filepath.Join(root, "shop")
	os.MkdirAll(main, 0o755)
	git(t, main, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(main, "wtg.yaml"), []byte(`project: shop
services:
  web:
    public:
      paths: [/webhooks]
`), 0o644)
	git(t, main, "add", ".")
	git(t, main, "commit", "-qm", "init")
	feat := filepath.Join(root, "shop.feat")
	git(t, main, "worktree", "add", "-q", "-b", "feature/pay", feat)

	sockDir, _ := os.MkdirTemp("", "wtg")
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	g := config.DefaultGlobal()
	g.HTTPS = false
	g.HTTPPort = freePort(t)
	g.Ingress.Port = freePort(t)
	g.HealthInterval = 100 * time.Millisecond
	g.DiscoveryInterval = 200 * time.Millisecond
	d, err := New(Options{StateDir: filepath.Join(root, "state"), SocketPath: filepath.Join(sockDir, "s"), Global: g})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Run(context.Background()) }()
	t.Cleanup(func() {
		d.Stop()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	c := api.NewClient(filepath.Join(sockDir, "s"))
	eventually(t, "socket", c.Ping)

	var up api.WorktreeView
	if err := c.Do("POST", "/v1/up", nil, api.UpRequest{Path: feat}, &up); err != nil {
		t.Fatal(err)
	}
	if up.Slug != "feature-pay" || up.Project != "shop" {
		t.Fatalf("up = %+v", up)
	}

	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "feat %s %s", r.Host, r.URL.Path)
	}))
	defer app.Close()
	port := app.Listener.Addr().(*net.TCPAddr).Port
	var reg api.RegisterResponse
	if err := c.Do("POST", "/v1/register", nil, api.RegisterRequest{Path: feat, Service: "web", Port: port}, &reg); err != nil {
		t.Fatal(err)
	}
	if reg.Service.Status != "up" || len(reg.Service.URLs) == 0 || !strings.Contains(reg.Service.URLs[0], "feature-pay.shop.localhost") {
		t.Fatalf("register = %+v", reg.Service)
	}

	get := func(host, path string) (int, string) {
		req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d%s", g.HTTPPort, path), nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	eventually(t, "route", func() bool {
		code, body := get("feature-pay.shop.localhost", "/x")
		return code == 200 && strings.HasPrefix(body, "feat feature-pay.shop.localhost /x")
	})
	if code, _ := get("nope.shop.localhost", "/"); code != 404 {
		t.Fatalf("unknown host = %d", code)
	}

	// Env contains identity only.
	var env map[string]string
	if err := c.Do("POST", "/v1/env", nil, api.EnvRequest{Path: feat, Service: "web", Port: 1234}, &env); err != nil {
		t.Fatal(err)
	}
	if env["WG_WORKTREE"] != "feature-pay" || env["PORT"] != "1234" || env["WG_URL"] == "" || env["WG_PUBLIC_URL"] != "" {
		t.Fatalf("env = %v", env)
	}

	// Ingress refuses to forward without an active tunnel.
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/w/feature-pay/webhooks/x", g.Ingress.Port), "text/plain", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("ingress forwarded while not exposed")
	}

	// Discovery picks up the main worktree without being asked.
	eventually(t, "discovery of main", func() bool {
		var st api.Status
		c.Do("GET", "/v1/status", nil, nil, &st)
		return len(st.Worktrees) == 2
	})

	// Removing the worktree with plain git removes routes.
	app.Close()
	git(t, main, "worktree", "remove", "--force", feat)
	eventually(t, "discovery removal", func() bool {
		var st api.Status
		c.Do("GET", "/v1/status", nil, nil, &st)
		for _, w := range st.Worktrees {
			if w.Slug == "feature-pay" {
				return false
			}
		}
		return true
	})
	eventually(t, "route removal", func() bool {
		code, _ := get("feature-pay.shop.localhost", "/")
		return code == 404
	})

	// A service registered with a PID disappears when that process exits.
	proc := exec.Command("sleep", "30")
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	if err := c.Do("POST", "/v1/register", nil, api.RegisterRequest{Path: main, Service: "web", Port: freePort(t), PID: proc.Process.Pid}, nil); err != nil {
		t.Fatal(err)
	}
	proc.Process.Kill()
	proc.Wait()
	eventually(t, "pid deregistration", func() bool {
		var st api.Status
		c.Do("GET", "/v1/status", nil, nil, &st)
		for _, w := range st.Worktrees {
			for _, s := range w.Services {
				if s.Name == "web" && s.Status != "missing" {
					return false
				}
			}
		}
		return true
	})

	// Non-loopback registrations are refused.
	if err := c.Do("POST", "/v1/register", nil, api.RegisterRequest{Path: main, Service: "web", Host: "10.0.0.5", Port: 80}, nil); err == nil {
		t.Fatal("non-loopback upstream accepted")
	}
	// OAuth state requires an allowlisted callback path.
	if err := c.Do("POST", "/v1/oauth/state", nil, api.OAuthStateRequest{Path: main, Service: "web", CallbackPath: "/cb"}, nil); err == nil {
		t.Fatal("oauth state issued for unlisted callback")
	}
}
