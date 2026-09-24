package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chryzxc/worktree-gateway/internal/config"
	"github.com/chryzxc/worktree-gateway/internal/registry"
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

func snapshot(t *testing.T, upstreamPort int) registry.Snapshot {
	t.Helper()
	r, _ := registry.Open("")
	cfg, err := config.ParseProject([]byte("project: myapp\nservices:\n  web: {}\n  api: {}\n  docs: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := r.UpsertWorktree(registry.WorktreeInput{CommonDir: "/r/.git", AdminID: "a", Branch: "feature/auth", Path: "/a", Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Register(registry.RegisterInput{WorktreeID: w.ID, Name: "web", Port: upstreamPort}); err != nil {
		t.Fatal(err)
	}
	down := freePort(t)
	r.Register(registry.RegisterInput{WorktreeID: w.ID, Name: "api", Port: down})
	r.SetStatus(w.ID+"/api", registry.StatusDown)
	return r.Snapshot()
}

func TestBuildConfigShape(t *testing.T) {
	g := config.DefaultGlobal()
	raw, err := BuildConfig(snapshot(t, 3000), Options{Global: g, StateDir: "/state", Embedded: true})
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{
		`"feature-auth.myapp.localhost"`, `"127.0.0.1:3000"`, `"api.feature-auth.myapp.localhost"`,
		`"127.0.0.1:8780"`, `"127.0.0.1:8743"`, `"install_trust": false`, `"disabled": true`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("config missing %s", want)
		}
	}
	ext, _ := BuildConfig(snapshot(t, 3000), Options{Global: g})
	if strings.Contains(string(ext), `"storage"`) || strings.Contains(string(ext), `"admin"`) {
		t.Fatal("external config must not carry process-local settings")
	}
}

func TestEmbeddedCaddyRoutes(t *testing.T) {
	if testing.Short() {
		t.Skip("boots caddy")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello from %s%s", r.Host, r.URL.Path)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	var upPort int
	fmt.Sscan(u.Port(), &upPort)

	state, err := os.MkdirTemp("", "wtgp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { t.Log(readLog(state)); os.RemoveAll(state) })
	g := config.DefaultGlobal()
	g.HTTPPort, g.HTTPSPort, g.Ingress.Port = freePort(t), freePort(t), freePort(t)

	p := &Embedded{}
	raw, err := BuildConfig(snapshot(t, upPort), Options{Global: g, StateDir: state, Embedded: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()

	get := func(client *http.Client, base, host string) (int, string, http.Header) {
		t.Helper()
		req, _ := http.NewRequest("GET", base+"/x", nil)
		req.Host = host
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s (%s): %v", base, host, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}
	httpBase := fmt.Sprintf("http://127.0.0.1:%d", g.HTTPPort)
	plain := &http.Client{Timeout: 5 * time.Second}

	code, body, hdr := get(plain, httpBase, "feature-auth.myapp.localhost")
	if code != 200 || body != "hello from feature-auth.myapp.localhost/x" || hdr.Get("X-Wg-Worktree") != "feature-auth" {
		t.Fatalf("routed: %d %q %v", code, body, hdr)
	}
	if code, body, _ := get(plain, httpBase, "api.feature-auth.myapp.localhost"); code != 503 || !strings.Contains(body, "not answering") {
		t.Fatalf("down: %d %q", code, body)
	}
	if code, body, _ := get(plain, httpBase, "docs.feature-auth.myapp.localhost"); code != 503 || !strings.Contains(body, "wtg run docs") {
		t.Fatalf("missing: %d %q", code, body)
	}
	if code, _, _ := get(plain, httpBase, "nope.myapp.localhost"); code != 404 {
		t.Fatalf("unknown: %d", code)
	}

	// HTTPS through Caddy's internal CA; trust only that root.
	var root []byte
	for i := 0; i < 50; i++ {
		if root, err = os.ReadFile(CARootPath(state)); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("CA root not created: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(root)
	host := "feature-auth.myapp.localhost"
	tlsClient := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: host},
	}}
	// Caddy issues certificates in the background right after a load.
	httpsURL := fmt.Sprintf("https://127.0.0.1:%d/x", g.HTTPSPort)
	for i := 0; i < 50; i++ {
		var resp *http.Response
		if resp, err = tlsClient.Get(httpsURL); err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	code, body, _ = get(tlsClient, fmt.Sprintf("https://127.0.0.1:%d", g.HTTPSPort), host)
	if code != 200 || !strings.HasPrefix(body, "hello from") {
		t.Fatalf("https: %d %q", code, body)
	}

	// Re-applying the same config is a no-op; a changed config reloads.
	if err := p.Apply(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
}

func readLog(state string) string {
	b, _ := os.ReadFile(state + "/caddy.log")
	return string(b)
}
