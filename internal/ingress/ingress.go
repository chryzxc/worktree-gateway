// Package ingress is the only entry point for traffic arriving through a
// public tunnel. It resolves (worktree, service) strictly from the registry,
// enforces the per-service path allowlist, records a bounded redacted log and
// forwards with the standard library reverse proxy. Requests can never choose
// an upstream host or port.
package ingress

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/chryzxc/worktree-gateway/internal/capture"
	"github.com/chryzxc/worktree-gateway/internal/config"
	"github.com/chryzxc/worktree-gateway/internal/oauth"
	"github.com/chryzxc/worktree-gateway/internal/registry"
)

// Ingress serves public (tunnel) traffic and the OAuth callback endpoint.
type Ingress struct {
	Reg     *registry.Registry
	Global  func() config.Global
	Capture *capture.Log
	Signer  *oauth.Signer
	// Exposed reports whether public exposure is switched on (a tunnel was
	// started). Webhook forwarding is refused otherwise.
	Exposed func() bool
	// Transport is used for upstream requests (nil = default).
	Transport http.RoundTripper
	Logger    *log.Logger
}

// CallbackPath is the stable OAuth redirect path.
const CallbackPath = "/_wg/oauth/callback"

// target is a resolved destination.
type target struct {
	project  registry.Project
	worktree registry.Worktree
	service  string
	path     string // path forwarded to the service
	prefix   string // stripped public prefix
}

func (in *Ingress) logf(format string, args ...any) {
	if in.Logger != nil {
		in.Logger.Printf(format, args...)
	}
}

func (in *Ingress) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == CallbackPath:
		in.serveOAuth(w, r)
		return
	case r.URL.Path == "/_wg/health":
		w.Write([]byte("ok\n"))
		return
	case strings.HasPrefix(r.URL.Path, "/_wg/"):
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("X-Wg-Via") == "local" {
		// Only the OAuth endpoint is reachable through the local proxy.
		http.NotFound(w, r)
		return
	}
	if in.Exposed != nil && !in.Exposed() {
		http.Error(w, "public exposure is off (run `wtg tunnel start`)", http.StatusNotFound)
		return
	}
	t, status, msg := in.resolve(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	in.forward(w, r, t)
}

// resolve maps a public request to a target, or returns an HTTP error.
func (in *Ingress) resolve(r *http.Request) (target, int, string) {
	g := in.Global()
	snap := in.Reg.Snapshot()
	var ref, rest, prefix string
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	pd := strings.ToLower(strings.Trim(g.Tunnel.PublicDomain, "."))
	switch {
	case pd != "" && strings.HasSuffix(host, "."+pd):
		ref = strings.TrimSuffix(host, "."+pd)
		rest = r.URL.Path
	case strings.HasPrefix(r.URL.Path, "/w/"):
		after := strings.TrimPrefix(r.URL.Path, "/w/")
		ref, rest, _ = strings.Cut(after, "/")
		rest = "/" + rest
		prefix = "/w/" + ref
	default:
		return target{}, http.StatusNotFound, "not found"
	}
	// Normalize before matching the allowlist so "/webhooks/../admin" cannot
	// reach "/admin"; the cleaned path is also what gets forwarded.
	rest = cleanPath(rest)
	hasPublic := func(p registry.Project) bool {
		for _, w := range snap.Worktrees {
			if w.ProjectID == p.ID {
				for _, s := range w.Cfg().Services {
					if s.Public != nil {
						return true
					}
				}
			}
		}
		return false
	}
	wt, proj, err := snap.FindWorktree(ref, hasPublic)
	if err != nil {
		return target{}, http.StatusNotFound, "not found"
	}
	// Longest allowlisted prefix wins.
	best, bestLen := "", -1
	names := make([]string, 0, len(wt.Cfg().Services))
	for n := range wt.Cfg().Services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		s := wt.Cfg().Services[name]
		if s.Public == nil {
			continue
		}
		for _, p := range s.Public.Paths {
			if pathAllowed(rest, p) && len(p) > bestLen {
				best, bestLen = name, len(p)
			}
		}
	}
	if best == "" {
		return target{}, http.StatusNotFound, "not found"
	}
	t := target{project: proj, worktree: wt, service: best, path: rest, prefix: prefix}
	if sp := wt.Cfg().Services[best].Public.StripPrefix; sp != nil && !*sp && prefix != "" {
		t.path = cleanPath(r.URL.Path)
		t.prefix = ""
	}
	return t, 0, ""
}

func cleanPath(p string) string {
	c := path.Clean("/" + p)
	if strings.HasSuffix(p, "/") && c != "/" {
		c += "/"
	}
	return c
}

// pathAllowed matches an allowlist prefix on segment boundaries: "/webhooks"
// and "/webhooks/" both allow "/webhooks/stripe" but not "/webhooksx".
func pathAllowed(path, prefix string) bool {
	if prefix == "/" {
		return true
	}
	if strings.HasSuffix(prefix, "/") {
		return strings.HasPrefix(path, prefix) || path == strings.TrimSuffix(prefix, "/")
	}
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// waitForService polls the registry until the service is up or hold elapses,
// so webhooks delivered during a restart still land.
func (in *Ingress) waitForService(ctx context.Context, wtID, name string, hold time.Duration) (registry.Service, bool) {
	deadline := time.Now().Add(hold)
	for {
		if s, ok := in.Reg.Snapshot().Service(wtID, name); ok && (s.Status == registry.StatusUp) {
			return s, true
		}
		if time.Now().After(deadline) {
			return registry.Service{}, false
		}
		select {
		case <-ctx.Done():
			return registry.Service{}, false
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (in *Ingress) forward(w http.ResponseWriter, r *http.Request, t target) {
	g := in.Global()
	start := time.Now()
	id := capture.NewID()
	entry := in.newEntry(id, "public", r, t)

	// Capture the body up to the limit without altering the bytes that are
	// forwarded (signature verification depends on the raw body).
	if in.Capture != nil && in.Capture.Enabled() && in.Capture.Settings().Bodies && r.Body != nil {
		max := in.Capture.Settings().MaxBody
		buf, err := io.ReadAll(io.LimitReader(r.Body, max+1))
		if err != nil {
			http.Error(w, "error reading request body", http.StatusBadRequest)
			return
		}
		if int64(len(buf)) > max {
			entry.BodyTruncated = true
			r.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(buf), r.Body), r.Body}
		} else {
			entry.Body = buf
			entry.BodyCaptured = true
			entry.BodySize = int64(len(buf))
			r.Body = io.NopCloser(bytes.NewReader(buf))
		}
	}

	svc, ok := in.waitForService(r.Context(), t.worktree.ID, t.service, g.Ingress.Hold)
	if !ok {
		entry.Status = http.StatusServiceUnavailable
		entry.Error = "service offline"
		entry.DurationMS = time.Since(start).Milliseconds()
		in.record(entry)
		w.Header().Set("Retry-After", "5")
		w.Header().Set("X-Wg-Request-Id", id)
		http.Error(w, "worktree service temporarily unavailable", http.StatusServiceUnavailable)
		return
	}

	counter := &countingBody{}
	if !entry.BodyCaptured && r.Body != nil {
		counter.rc = r.Body
		r.Body = counter
	}
	rp := &httputil.ReverseProxy{
		Transport: in.Transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = svc.Upstream()
			pr.Out.URL.Path = t.path
			pr.Out.URL.RawPath = ""
			pr.Out.Host = r.Host // keep the public host: some signatures cover the URL
			pr.SetXForwarded()
			if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
				pr.Out.Header.Set("X-Forwarded-Proto", proto)
			} else {
				pr.Out.Header.Set("X-Forwarded-Proto", "https") // tunnels terminate TLS
			}
			if t.prefix != "" {
				pr.Out.Header.Set("X-Forwarded-Prefix", t.prefix)
			}
			pr.Out.Header.Set("X-Wg-Request-Id", id)
			pr.Out.Header.Set("X-Wg-Worktree", t.worktree.Slug)
		},
		ModifyResponse: func(resp *http.Response) error {
			entry.Status = resp.StatusCode
			resp.Header.Set("X-Wg-Request-Id", id)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			entry.Status = http.StatusBadGateway
			entry.Error = err.Error()
			w.Header().Set("Retry-After", "5")
			w.Header().Set("X-Wg-Request-Id", id)
			http.Error(w, "worktree service unreachable", http.StatusBadGateway)
		},
		FlushInterval: -1,
	}
	rp.ServeHTTP(w, r)
	if !entry.BodyCaptured {
		entry.BodySize += counter.n
	}
	entry.DurationMS = time.Since(start).Milliseconds()
	in.record(entry)
	in.logf("ingress %s %s %s → %s/%s %d", id, r.Method, r.URL.Path, t.worktree.Slug, t.service, entry.Status)
}

type countingBody struct {
	rc io.ReadCloser
	n  int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	c.n += int64(n)
	return n, err
}
func (c *countingBody) Close() error { return c.rc.Close() }

func (in *Ingress) newEntry(id, via string, r *http.Request, t target) capture.Entry {
	e := capture.Entry{
		ID: id, Time: time.Now(), Via: via,
		Project: t.project.Slug, Worktree: t.worktree.Slug, WorktreeID: t.worktree.ID, Service: t.service,
		Source: capture.DetectSource(r.Header), Method: r.Method, Host: r.Host,
		PublicPath: r.URL.Path, Path: t.path,
	}
	if in.Capture != nil {
		e.Header, e.Redactions = in.Capture.RedactHeader(r.Header)
	}
	if q, redacted := capture.RedactQuery(r.URL.RawQuery); redacted {
		e.RawQuery = q
		e.Redactions = append(e.Redactions, "query")
	} else {
		e.RawQuery = r.URL.RawQuery
	}
	return e
}

func (in *Ingress) record(e capture.Entry) {
	if in.Capture != nil {
		in.Capture.Add(e)
	}
}

// ---------------------------------------------------------------------------
// Replay

// Idempotent reports whether replaying method is considered safe.
func Idempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

var (
	ErrNotFound      = errors.New("request not found")
	ErrNeedsConfirm  = errors.New("request is not idempotent; confirm replay with --yes")
	ErrBodyMissing   = errors.New("request body was not captured (capture disabled or body over the size limit); cannot replay")
	ErrServiceAbsent = errors.New("target service is not running")
)

// ReplayResult describes a replay.
type ReplayResult struct {
	ID       string `json:"id"`
	ReplayOf string `json:"replay_of"`
	Worktree string `json:"worktree"`
	Service  string `json:"service"`
	Status   int    `json:"status"`
	Error    string `json:"error,omitempty"`
}

// Replay re-sends a captured request to its worktree (or to toRef). The copy
// carries X-Wg-Replay and X-Wg-Replay-Of; redacted headers are dropped rather
// than sent with placeholder values.
func (in *Ingress) Replay(ctx context.Context, id, toRef string, confirmed bool) (ReplayResult, error) {
	if in.Capture == nil {
		return ReplayResult{}, ErrNotFound
	}
	e, ok := in.Capture.Get(id)
	if !ok {
		return ReplayResult{}, ErrNotFound
	}
	if !Idempotent(e.Method) && !confirmed {
		return ReplayResult{}, ErrNeedsConfirm
	}
	if !e.BodyCaptured && e.BodySize > 0 {
		return ReplayResult{}, ErrBodyMissing
	}
	snap := in.Reg.Snapshot()
	var wt registry.Worktree
	var proj registry.Project
	var err error
	if toRef != "" {
		wt, proj, err = snap.FindWorktree(toRef, nil)
	} else {
		var found bool
		wt, found = snap.Worktree(e.WorktreeID)
		if !found {
			err = fmt.Errorf("worktree %s no longer exists; use --to", e.Worktree)
		} else {
			proj, _ = snap.Project(wt.ProjectID)
		}
	}
	if err != nil {
		return ReplayResult{}, err
	}
	svc, ok := snap.Service(wt.ID, e.Service)
	if !ok || svc.Status != registry.StatusUp {
		return ReplayResult{}, fmt.Errorf("%w: %s/%s", ErrServiceAbsent, wt.Slug, e.Service)
	}
	u := url.URL{Scheme: "http", Host: svc.Upstream(), Path: e.Path, RawQuery: e.RawQuery}
	req, err := http.NewRequestWithContext(ctx, e.Method, u.String(), bytes.NewReader(e.Body))
	if err != nil {
		return ReplayResult{}, err
	}
	redacted := map[string]bool{}
	for _, h := range e.Redactions {
		redacted[http.CanonicalHeaderKey(h)] = true
	}
	for k, v := range e.Header {
		if redacted[http.CanonicalHeaderKey(k)] || isHopHeader(k) {
			continue
		}
		req.Header[k] = append([]string(nil), v...)
	}
	req.Host = e.Host
	newID := capture.NewID()
	req.Header.Set("X-Wg-Replay", "1")
	req.Header.Set("X-Wg-Replay-Of", e.ID)
	req.Header.Set("X-Wg-Request-Id", newID)
	req.Header.Set("X-Wg-Worktree", wt.Slug)

	rec := e
	rec.ID, rec.Time, rec.Via, rec.ReplayOf = newID, time.Now(), "replay", e.ID
	rec.Project, rec.Worktree, rec.WorktreeID = proj.Slug, wt.Slug, wt.ID
	start := time.Now()
	client := &http.Client{Transport: in.Transport, Timeout: 60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	res := ReplayResult{ID: newID, ReplayOf: e.ID, Worktree: wt.Slug, Service: e.Service}
	if err != nil {
		rec.Status, rec.Error = http.StatusBadGateway, err.Error()
		res.Status, res.Error = rec.Status, rec.Error
	} else {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		rec.Status, res.Status = resp.StatusCode, resp.StatusCode
	}
	rec.DurationMS = time.Since(start).Milliseconds()
	in.record(rec)
	return res, nil
}

func isHopHeader(k string) bool {
	switch http.CanonicalHeaderKey(k) {
	case "Connection", "Keep-Alive", "Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "Content-Length":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// OAuth callback routing

var formPostTmpl = template.Must(template.New("f").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>Redirecting…</title></head>
<body onload="document.forms[0].submit()">
<form method="post" action="{{.Action}}">
{{range .Fields}}<input type="hidden" name="{{.K}}" value="{{.V}}">
{{end}}<noscript><button type="submit">Continue</button></noscript>
</form></body></html>`))

func (in *Ingress) serveOAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var params url.Values
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		params = r.PostForm
	} else {
		params = r.URL.Query()
	}
	token := params.Get("state")
	if token == "" || in.Signer == nil {
		http.Error(w, "missing state", http.StatusBadRequest)
		return
	}
	p, err := in.Signer.Verify(token)
	if err != nil {
		in.logf("oauth: rejected callback: %v", err)
		http.Error(w, "invalid or expired OAuth routing state: "+err.Error(), http.StatusBadRequest)
		return
	}
	snap := in.Reg.Snapshot()
	wt, proj, err := snap.FindWorktree(p.Worktree+"."+p.Project, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf("worktree %s of project %s no longer exists", p.Worktree, p.Project), http.StatusGone)
		return
	}
	allowed := false
	for _, cb := range wt.ServiceConfig(p.Service).OAuthCallbacks {
		if cb == p.Path {
			allowed = true
		}
	}
	if !allowed {
		http.Error(w, "callback path is not in the service's oauth_callbacks allowlist", http.StatusForbidden)
		return
	}
	if svc, ok := snap.Service(wt.ID, p.Service); !ok || svc.Status != registry.StatusUp {
		w.Header().Set("Retry-After", "5")
		http.Error(w, fmt.Sprintf("worktree %q is offline: service %q is not running. Start it and retry the callback (reload this page).", wt.Slug, p.Service), http.StatusServiceUnavailable)
		return
	}
	g := in.Global()
	dest := registry.URL(registry.Hostnames(proj, wt, p.Service)[0], g) + p.Path
	fwd := url.Values{}
	for k, v := range params {
		fwd[k] = v
	}
	if p.State != "" {
		fwd.Set("state", p.State)
	} else {
		fwd.Del("state")
	}
	in.logf("oauth: callback routed to %s/%s%s", wt.Slug, p.Service, p.Path)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Method == http.MethodGet {
		http.Redirect(w, r, dest+"?"+fwd.Encode(), http.StatusFound)
		return
	}
	type kv struct{ K, V string }
	var fields []kv
	keys := make([]string, 0, len(fwd))
	for k := range fwd {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range fwd[k] {
			fields = append(fields, kv{k, v})
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	formPostTmpl.Execute(w, map[string]any{"Action": dest, "Fields": fields})
}
