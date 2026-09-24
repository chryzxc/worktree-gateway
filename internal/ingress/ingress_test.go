package ingress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chryzxc/worktree-gateway/internal/capture"
	"github.com/chryzxc/worktree-gateway/internal/config"
	"github.com/chryzxc/worktree-gateway/internal/oauth"
	"github.com/chryzxc/worktree-gateway/internal/registry"
)

const projectCfg = `
project: myapp
services:
  web: {}
  api:
    public:
      paths: ["/webhooks/"]
    oauth_callbacks: ["/auth/callback"]
  admin:
    public:
      paths: ["/webhooks/admin"]
`

type seen struct {
	mu   sync.Mutex
	reqs []*http.Request
	body []string
}

func (s *seen) last() (*http.Request, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reqs) == 0 {
		return nil, ""
	}
	return s.reqs[len(s.reqs)-1], s.body[len(s.body)-1]
}

type fixture struct {
	in       *Ingress
	reg      *registry.Registry
	wt       registry.Worktree
	api      *seen
	apiPort  int
	exposed  bool
	g        config.Global
	upstream *httptest.Server
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{api: &seen{}, exposed: true, g: config.DefaultGlobal()}
	f.g.Ingress.Hold = 300 * time.Millisecond
	f.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.api.mu.Lock()
		f.api.reqs = append(f.api.reqs, r)
		f.api.body = append(f.api.body, string(b))
		f.api.mu.Unlock()
		w.WriteHeader(202)
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(f.upstream.Close)
	u, _ := url.Parse(f.upstream.URL)
	fmt.Sscan(u.Port(), &f.apiPort)

	f.reg, _ = registry.Open("")
	cfg, err := config.ParseProject([]byte(projectCfg))
	if err != nil {
		t.Fatal(err)
	}
	f.wt, _ = f.reg.UpsertWorktree(registry.WorktreeInput{CommonDir: "/r/.git", AdminID: "a", Branch: "feature/auth", Path: "/a", Config: cfg})
	f.reg.Register(registry.RegisterInput{WorktreeID: f.wt.ID, Name: "api", Port: f.apiPort})
	log, _ := capture.Open("", f.g.Capture)
	f.in = &Ingress{
		Reg: f.reg, Global: func() config.Global { return f.g }, Capture: log,
		Signer:  oauth.NewSigner([]byte(strings.Repeat("s", 32))),
		Exposed: func() bool { return f.exposed },
	}
	return f
}

func (f *fixture) do(method, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.in.ServeHTTP(rec, req)
	return rec
}

func TestWebhookPathRouting(t *testing.T) {
	f := newFixture(t)
	raw := `{"id":"evt_1",  "amount": 100}` // odd spacing must survive byte-for-byte
	rec := f.do("POST", "https://abc.trycloudflare.com/w/feature-auth/webhooks/stripe?x=1", raw,
		map[string]string{"Stripe-Signature": "t=1,v1=sig", "Authorization": "Bearer secret", "Content-Type": "application/json"})
	if rec.Code != 202 {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	r, body := f.api.last()
	if body != raw || r.URL.Path != "/webhooks/stripe" || r.URL.RawQuery != "x=1" {
		t.Fatalf("forwarded %q %q %q", body, r.URL.Path, r.URL.RawQuery)
	}
	if r.Host != "abc.trycloudflare.com" || r.Header.Get("X-Forwarded-Prefix") != "/w/feature-auth" ||
		r.Header.Get("Stripe-Signature") != "t=1,v1=sig" || r.Header.Get("X-Forwarded-Proto") != "https" {
		t.Fatalf("headers not preserved: host=%s %v", r.Host, r.Header)
	}
	if r.Header.Get("Authorization") != "Bearer secret" {
		t.Fatal("live request must not be redacted, only the stored copy")
	}
	list := f.in.Capture.List("", 0)
	if len(list) != 1 || list[0].Source != "stripe" || list[0].Status != 202 || list[0].Worktree != "feature-auth" {
		t.Fatalf("capture %+v", list)
	}
	e, _ := f.in.Capture.Get(list[0].ID)
	if e.Header.Get("Authorization") != capture.Redacted || string(e.Body) != raw {
		t.Fatalf("stored copy wrong: %v %q", e.Header, e.Body)
	}
}

func TestAllowlistAndSSRF(t *testing.T) {
	f := newFixture(t)
	for _, target := range []string{
		"/w/feature-auth/admin",            // not allowlisted on api
		"/w/feature-auth/webhooksx",        // segment boundary
		"/w/nope/webhooks/x",               // unknown worktree
		"/?host=127.0.0.1&port=22",         // no routing from query
		"/w/feature-auth/../../etc/passwd", // path games
		"/_wg/anything",
		"/w/feature-auth/webhooks/../admin",
		"/w/feature-auth/webhooks/%2e%2e/admin", // internal namespace
	} {
		if rec := f.do("GET", target, "", nil); rec.Code != 404 {
			t.Errorf("%s: got %d, want 404", target, rec.Code)
		}
	}
	if n := len(f.api.reqs); n != 0 {
		t.Fatalf("nothing should reach the service, got %d", n)
	}
	// Local proxy traffic may only use the OAuth endpoint.
	if rec := f.do("POST", "/w/feature-auth/webhooks/x", "", map[string]string{"X-Wg-Via": "local"}); rec.Code != 404 {
		t.Fatalf("local via: %d", rec.Code)
	}
	// Exposure off → nothing forwarded.
	f.exposed = false
	if rec := f.do("POST", "/w/feature-auth/webhooks/x", "", nil); rec.Code != 404 {
		t.Fatalf("exposure off: %d", rec.Code)
	}
}

func TestLongestPrefixAndHostRouting(t *testing.T) {
	f := newFixture(t)
	// admin has the more specific prefix but is not registered → held, then 503.
	rec := f.do("POST", "/w/feature-auth/webhooks/admin/x", "{}", nil)
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("offline service: %d", rec.Code)
	}
	f.g.Tunnel.PublicDomain = "dev.example.com"
	req := httptest.NewRequest("POST", "https://feature-auth.dev.example.com/webhooks/github", strings.NewReader("{}"))
	req.Host = "feature-auth.dev.example.com"
	w := httptest.NewRecorder()
	f.in.ServeHTTP(w, req)
	if w.Code != 202 {
		t.Fatalf("host routing: %d", w.Code)
	}
	r, _ := f.api.last()
	if r.URL.Path != "/webhooks/github" || r.Header.Get("X-Forwarded-Prefix") != "" {
		t.Fatalf("host routing path %q", r.URL.Path)
	}
}

func TestHoldDuringRestart(t *testing.T) {
	f := newFixture(t)
	f.g.Ingress.Hold = 3 * time.Second
	f.reg.SetStatus(f.wt.ID+"/api", registry.StatusDown)
	go func() {
		time.Sleep(400 * time.Millisecond)
		f.reg.SetStatus(f.wt.ID+"/api", registry.StatusUp)
	}()
	if rec := f.do("POST", "/w/feature-auth/webhooks/x", "{}", nil); rec.Code != 202 {
		t.Fatalf("should have been held until the service came back: %d", rec.Code)
	}
}

func TestReplay(t *testing.T) {
	f := newFixture(t)
	f.do("POST", "/w/feature-auth/webhooks/twilio", "Body=hi", map[string]string{"X-Twilio-Signature": "abc", "Cookie": "sid=1"})
	id := f.in.Capture.List("", 0)[0].ID
	ctx := context.Background()
	if _, err := f.in.Replay(ctx, id, "", false); !errors.Is(err, ErrNeedsConfirm) {
		t.Fatalf("expected confirm error, got %v", err)
	}
	res, err := f.in.Replay(ctx, id, "", true)
	if err != nil || res.Status != 202 || res.ReplayOf != id {
		t.Fatalf("replay: %v %+v", err, res)
	}
	r, body := f.api.last()
	if body != "Body=hi" || r.Header.Get("X-Wg-Replay") != "1" || r.Header.Get("X-Wg-Replay-Of") != id ||
		r.Header.Get("Cookie") != "" || r.Header.Get("X-Twilio-Signature") != "abc" || r.URL.Path != "/webhooks/twilio" {
		t.Fatalf("replayed request wrong: %q %v", body, r.Header)
	}
	if list := f.in.Capture.List("", 0); list[0].Via != "replay" || list[0].ReplayOf != id {
		t.Fatalf("replay not recorded: %+v", list[0])
	}
	if _, err := f.in.Replay(ctx, "req_nope", "", true); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	f.reg.Deregister(f.wt.ID, "api", 0)
	if _, err := f.in.Replay(ctx, id, "", true); !errors.Is(err, ErrServiceAbsent) {
		t.Fatalf("offline replay: %v", err)
	}
}

func TestReplayRefusesTruncated(t *testing.T) {
	f := newFixture(t)
	f.g.Capture.MaxBody = 4
	f.in.Capture, _ = capture.Open("", f.g.Capture)
	f.do("POST", "/w/feature-auth/webhooks/x", "0123456789", nil)
	if _, body := f.api.last(); body != "0123456789" {
		t.Fatalf("large body must still be forwarded intact, got %q", body)
	}
	e := f.in.Capture.List("", 0)[0]
	if !e.BodyTruncated || e.BodySize != 10 {
		t.Fatalf("truncation not recorded: %+v", e)
	}
	if _, err := f.in.Replay(context.Background(), e.ID, "", true); !errors.Is(err, ErrBodyMissing) {
		t.Fatalf("got %v", err)
	}
}

func TestOAuthCallback(t *testing.T) {
	f := newFixture(t)
	sign := func(p oauth.Payload) string {
		tok, err := f.in.Signer.Sign(p, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	tok := sign(oauth.Payload{Project: "myapp", Worktree: "feature-auth", Service: "api", Path: "/auth/callback", State: "app-state"})
	rec := f.do("GET", CallbackPath+"?code=abc&state="+url.QueryEscape(tok), "", nil)
	if rec.Code != 302 {
		t.Fatalf("oauth: %d %s", rec.Code, rec.Body)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Host != "api.feature-auth.myapp.localhost:8743" || loc.Path != "/auth/callback" ||
		loc.Query().Get("code") != "abc" || loc.Query().Get("state") != "app-state" {
		t.Fatalf("redirect %s", loc)
	}
	if len(f.in.Capture.List("", 0)) != 0 {
		t.Fatal("OAuth callbacks carry codes and must not be captured")
	}
	// Single use.
	if rec := f.do("GET", CallbackPath+"?state="+url.QueryEscape(tok), "", nil); rec.Code != 400 {
		t.Fatalf("replayed state: %d", rec.Code)
	}
	// Path not in allowlist.
	bad := sign(oauth.Payload{Project: "myapp", Worktree: "feature-auth", Service: "api", Path: "/admin"})
	if rec := f.do("GET", CallbackPath+"?state="+url.QueryEscape(bad), "", nil); rec.Code != 403 {
		t.Fatalf("allowlist: %d", rec.Code)
	}
	// Unsigned / tampered.
	if rec := f.do("GET", CallbackPath+"?state=forged.sig", "", nil); rec.Code != 400 {
		t.Fatalf("forged: %d", rec.Code)
	}
	// form_post response mode → auto-submitting form with the inner state.
	tok2 := sign(oauth.Payload{Project: "myapp", Worktree: "feature-auth", Service: "api", Path: "/auth/callback", State: "s2"})
	form := url.Values{"state": {tok2}, "code": {"c<script>"}}
	req := httptest.NewRequest("POST", CallbackPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	f.in.ServeHTTP(w, req)
	html := w.Body.String()
	if w.Code != 200 || !strings.Contains(html, `value="s2"`) || strings.Contains(html, "<script>") ||
		!strings.Contains(html, "https://api.feature-auth.myapp.localhost:8743/auth/callback") {
		t.Fatalf("form_post: %d %s", w.Code, html)
	}
	// Offline worktree.
	f.reg.Deregister(f.wt.ID, "api", 0)
	tok3 := sign(oauth.Payload{Project: "myapp", Worktree: "feature-auth", Service: "api", Path: "/auth/callback"})
	if rec := f.do("GET", CallbackPath+"?state="+url.QueryEscape(tok3), "", nil); rec.Code != 503 {
		t.Fatalf("offline: %d", rec.Code)
	}
}
