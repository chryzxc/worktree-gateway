package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/chryzxc/worktree-gateway/internal/api"
	"github.com/chryzxc/worktree-gateway/internal/gitwt"
	"github.com/chryzxc/worktree-gateway/internal/ingress"
	"github.com/chryzxc/worktree-gateway/internal/oauth"
	"github.com/chryzxc/worktree-gateway/internal/registry"
	"github.com/chryzxc/worktree-gateway/internal/version"
)

type httpError struct {
	code int
	msg  string
}

func (e httpError) Error() string { return e.msg }

func badRequest(format string, args ...any) error {
	return httpError{http.StatusBadRequest, fmt.Sprintf(format, args...)}
}

// handle adapts a typed handler: decode body into In, encode the result.
func handle[In any](f func(context.Context, In, *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in In
		if r.Body != nil && r.ContentLength != 0 {
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request: " + err.Error()})
				return
			}
		}
		out, err := f(r.Context(), in, r)
		if err != nil {
			code := http.StatusInternalServerError
			var he httpError
			switch {
			case errors.As(err, &he):
				code = he.code
			case errors.Is(err, gitwt.ErrNotRepo), errors.Is(err, registry.ErrNotLoopback),
				errors.Is(err, ingress.ErrNeedsConfirm), errors.Is(err, ingress.ErrBodyMissing):
				code = http.StatusBadRequest
			case errors.Is(err, ingress.ErrNotFound):
				code = http.StatusNotFound
			case errors.Is(err, ingress.ErrServiceAbsent):
				code = http.StatusConflict
			}
			writeJSON(w, code, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

type none struct{}

func (d *Daemon) apiHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", handle(func(ctx context.Context, _ none, _ *http.Request) (any, error) {
		return d.status(), nil
	}))
	mux.HandleFunc("POST /v1/up", handle(func(ctx context.Context, in api.UpRequest, _ *http.Request) (any, error) {
		wt, err := d.SyncPath(ctx, in.Path, in.Name)
		if err != nil {
			return nil, err
		}
		if wt.Parked {
			d.reg.SetParked(wt.ID, false)
			wt.Parked = false
		}
		return d.view(d.reg.Snapshot(), wt), nil
	}))
	mux.HandleFunc("POST /v1/down", handle(func(ctx context.Context, in api.DownRequest, _ *http.Request) (any, error) {
		wt, err := d.worktreeForPath(ctx, in.Path)
		if err != nil {
			return nil, err
		}
		if in.Forget {
			d.forget(wt.ID)
		} else {
			d.reg.SetParked(wt.ID, true)
		}
		return map[string]string{"worktree": wt.Slug}, nil
	}))
	mux.HandleFunc("POST /v1/register", handle(func(ctx context.Context, in api.RegisterRequest, _ *http.Request) (any, error) {
		return d.register(ctx, in)
	}))
	mux.HandleFunc("POST /v1/deregister", handle(func(ctx context.Context, in api.DeregisterRequest, _ *http.Request) (any, error) {
		wt, err := d.worktreeForPath(ctx, in.Path)
		if err != nil {
			return nil, err
		}
		return map[string]bool{"removed": d.reg.Deregister(wt.ID, in.Service, in.PID)}, nil
	}))
	mux.HandleFunc("POST /v1/port", handle(func(ctx context.Context, in api.PortRequest, _ *http.Request) (any, error) {
		wt, err := d.SyncPath(ctx, in.Path, "")
		if err != nil {
			return nil, err
		}
		p, err := d.AllocPort(wt, in.Service)
		return api.PortResponse{Port: p}, err
	}))
	mux.HandleFunc("POST /v1/env", handle(func(ctx context.Context, in api.EnvRequest, _ *http.Request) (any, error) {
		wt, err := d.SyncPath(ctx, in.Path, "")
		if err != nil {
			return nil, err
		}
		return d.Env(wt, in.Service, in.Port), nil
	}))
	mux.HandleFunc("GET /v1/tunnel", handle(func(ctx context.Context, _ none, _ *http.Request) (any, error) {
		return d.TunnelStatus(), nil
	}))
	mux.HandleFunc("POST /v1/tunnel/start", handle(func(ctx context.Context, in api.TunnelRequest, _ *http.Request) (any, error) {
		return d.startTunnel(in.Provider)
	}))
	mux.HandleFunc("POST /v1/tunnel/stop", handle(func(ctx context.Context, _ none, _ *http.Request) (any, error) {
		d.stopTunnel()
		return d.TunnelStatus(), nil
	}))
	mux.HandleFunc("GET /v1/requests", handle(func(ctx context.Context, _ none, r *http.Request) (any, error) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		return api.RequestList{Requests: d.capture.List(r.URL.Query().Get("worktree"), limit)}, nil
	}))
	mux.HandleFunc("GET /v1/requests/{id}", handle(func(ctx context.Context, _ none, r *http.Request) (any, error) {
		e, ok := d.capture.Get(r.PathValue("id"))
		if !ok {
			return nil, ingress.ErrNotFound
		}
		return e, nil
	}))
	mux.HandleFunc("POST /v1/requests/{id}/replay", handle(func(ctx context.Context, in api.ReplayRequest, r *http.Request) (any, error) {
		return d.ingress.Replay(ctx, r.PathValue("id"), in.To, in.Yes)
	}))
	mux.HandleFunc("DELETE /v1/requests", handle(func(ctx context.Context, _ none, _ *http.Request) (any, error) {
		return none{}, d.capture.Clear()
	}))
	mux.HandleFunc("POST /v1/oauth/state", handle(func(ctx context.Context, in api.OAuthStateRequest, _ *http.Request) (any, error) {
		return d.oauthState(ctx, in)
	}))
	mux.HandleFunc("POST /v1/shutdown", handle(func(ctx context.Context, _ none, _ *http.Request) (any, error) {
		go func() { time.Sleep(100 * time.Millisecond); d.Stop() }()
		return none{}, nil
	}))
	return mux
}

// worktreeForPath resolves a path to an already-known worktree without
// creating one.
func (d *Daemon) worktreeForPath(ctx context.Context, path string) (registry.Worktree, error) {
	repo, err := gitwt.Resolve(ctx, path)
	if err != nil {
		// The directory may already be gone (post-remove); match by path.
		for _, w := range d.reg.Snapshot().Worktrees {
			if w.Path == path {
				return w, nil
			}
		}
		return registry.Worktree{}, err
	}
	id := registry.WorktreeID(registry.ProjectID(repo.CommonDir), repo.CurrentAdmin)
	wt, ok := d.reg.Snapshot().Worktree(id)
	if !ok {
		return registry.Worktree{}, httpError{http.StatusNotFound, "worktree is not registered with the gateway"}
	}
	return wt, nil
}

func (d *Daemon) register(ctx context.Context, in api.RegisterRequest) (api.RegisterResponse, error) {
	if in.Service == "" || in.Port == 0 {
		return api.RegisterResponse{}, badRequest("service and port are required")
	}
	wt, err := d.SyncPath(ctx, in.Path, "")
	if err != nil {
		return api.RegisterResponse{}, err
	}
	host := in.Host
	if host == "" || host == "localhost" {
		host = "127.0.0.1"
	}
	status := registry.StatusStarting
	if c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(in.Port)), 300*time.Millisecond); err == nil {
		c.Close()
		status = registry.StatusUp
	}
	svc, evicted, err := d.reg.Register(registry.RegisterInput{
		WorktreeID: wt.ID, Name: in.Service, Host: host, Port: in.Port, PID: in.PID, Source: in.Source, Status: status,
	})
	if err != nil {
		return api.RegisterResponse{}, err
	}
	snap := d.reg.Snapshot()
	wt, _ = snap.Worktree(wt.ID)
	resp := api.RegisterResponse{Worktree: d.view(snap, wt)}
	for _, sv := range resp.Worktree.Services {
		if sv.Name == svc.Name {
			resp.Service = sv
		}
	}
	for _, e := range evicted {
		resp.Evicted = append(resp.Evicted, e.Key())
		d.log.Printf("registration %s replaced stale %s on port %d", svc.Key(), e.Key(), e.Port)
	}
	d.log.Printf("registered %s → %s (%s)", svc.Key(), svc.Upstream(), status)
	return resp, nil
}

func (d *Daemon) oauthState(ctx context.Context, in api.OAuthStateRequest) (api.OAuthStateResponse, error) {
	wt, err := d.SyncPath(ctx, in.Path, "")
	if err != nil {
		return api.OAuthStateResponse{}, err
	}
	allowed := false
	for _, cb := range wt.ServiceConfig(in.Service).OAuthCallbacks {
		allowed = allowed || cb == in.CallbackPath
	}
	if !allowed {
		return api.OAuthStateResponse{}, badRequest("%q is not listed in services.%s.oauth_callbacks", in.CallbackPath, in.Service)
	}
	snap := d.reg.Snapshot()
	p, _ := snap.Project(wt.ProjectID)
	ttl := in.TTL
	if ttl <= 0 {
		ttl = d.g.OAuth.StateTTL
	}
	tok, err := d.signer.Sign(oauth.Payload{
		Project: p.Slug, Worktree: wt.Slug, Service: in.Service, Path: in.CallbackPath, State: in.State,
	}, ttl)
	if err != nil {
		return api.OAuthStateResponse{}, err
	}
	env := d.Env(wt, "", 0)
	return api.OAuthStateResponse{
		State:       tok,
		RedirectURI: env["WG_OAUTH_CALLBACK_URL"],
		Destination: registry.URL(registry.Hostnames(p, wt, in.Service)[0], d.g) + in.CallbackPath,
	}, nil
}

func (d *Daemon) status() api.Status {
	snap := d.reg.Snapshot()
	d.mu.Lock()
	perr := d.lastErr
	d.mu.Unlock()
	st := api.Status{
		Version: version.Version, PID: os.Getpid(), Started: d.started, StateDir: d.opt.StateDir,
		HTTP:    net.JoinHostPort(loopback(d.g.ListenHost), strconv.Itoa(d.g.HTTPPort)),
		Ingress: net.JoinHostPort(loopback(d.g.ListenHost), strconv.Itoa(d.g.Ingress.Port)),
		Proxy:   d.proxy.Name(), ProxyError: perr, Tunnel: d.TunnelStatus(),
	}
	if d.g.HTTPS {
		st.HTTPS = net.JoinHostPort(loopback(d.g.ListenHost), strconv.Itoa(d.g.HTTPSPort))
	}
	for _, w := range snap.Worktrees {
		st.Worktrees = append(st.Worktrees, d.view(snap, w))
	}
	return st
}

func (d *Daemon) view(snap registry.Snapshot, w registry.Worktree) api.WorktreeView {
	p, _ := snap.Project(w.ProjectID)
	v := api.WorktreeView{
		ID: w.ID, Project: p.Slug, ProjectDomain: p.Domain, Slug: w.Slug, Branch: w.Branch, Path: w.Path,
		IsMain: w.IsMain, Parked: w.Parked, PublicURL: d.PublicBase(w),
	}
	if w.Config != nil {
		v.ConfigFile = w.Config.Path()
	}
	conflicts := map[string]string{}
	for _, rt := range snap.Routes() {
		if rt.WorktreeID == w.ID && rt.Conflict != "" {
			conflicts[rt.Service] = fmt.Sprintf("%s is served by %s", rt.Hostname, rt.Conflict)
		}
	}
	for _, name := range snap.ServiceNames(w) {
		sv := api.ServiceView{Name: name, Status: "missing", Conflict: conflicts[name]}
		_, sv.Configured = w.Cfg().Services[name]
		if pub := w.ServiceConfig(name).Public; pub != nil {
			sv.Public = pub.Paths
		}
		if s, ok := snap.Service(w.ID, name); ok {
			sv.Status, sv.Upstream, sv.Port, sv.PID, sv.Source = s.Status, s.Upstream(), s.Port, s.PID, s.Source
		}
		if !w.Parked {
			for _, h := range registry.Hostnames(p, w, name) {
				sv.URLs = append(sv.URLs, registry.URL(h, d.g))
			}
		}
		v.Services = append(v.Services, sv)
	}
	return v
}
