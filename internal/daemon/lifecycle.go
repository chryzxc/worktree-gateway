package daemon

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chryzxc/worktree-gateway/internal/config"
	"github.com/chryzxc/worktree-gateway/internal/gitwt"
	"github.com/chryzxc/worktree-gateway/internal/ingress"
	"github.com/chryzxc/worktree-gateway/internal/registry"
)

// SyncPath resolves the worktree containing path from Git and upserts its
// identity. It is the single entry point used by `wtg up`, register, run…
func (d *Daemon) SyncPath(ctx context.Context, path, pinnedName string) (registry.Worktree, error) {
	repo, err := gitwt.Resolve(ctx, path)
	if err != nil {
		return registry.Worktree{}, err
	}
	wts, err := gitwt.List(ctx, repo.TopLevel)
	if err != nil {
		return registry.Worktree{}, err
	}
	var cur *gitwt.Worktree
	for i := range wts {
		if wts[i].AdminID == repo.CurrentAdmin {
			cur = &wts[i]
		}
	}
	if cur == nil {
		return registry.Worktree{}, fmt.Errorf("worktree %s not listed by git", repo.TopLevel)
	}
	in, err := d.worktreeInput(repo.CommonDir, repo.MainPath, *cur)
	if err != nil {
		return registry.Worktree{}, err
	}
	in.PinnedName = pinnedName
	d.mu.Lock()
	delete(d.tombstones, registry.WorktreeID(registry.ProjectID(repo.CommonDir), cur.AdminID))
	d.mu.Unlock()
	return d.reg.UpsertWorktree(in)
}

func (d *Daemon) worktreeInput(commonDir, mainPath string, w gitwt.Worktree) (registry.WorktreeInput, error) {
	cfg, err := config.LoadProject(w.Path)
	if err != nil {
		return registry.WorktreeInput{}, err
	}
	var mainCfg *config.Project
	if mainPath != "" && mainPath != w.Path {
		if mc, err := config.LoadProject(mainPath); err == nil && mc.Path() != "" {
			mainCfg = mc
		}
	}
	return registry.WorktreeInput{
		CommonDir: commonDir, MainPath: mainPath, AdminID: w.AdminID, Branch: w.Branch,
		Path: w.Path, IsMain: w.IsMain, Config: cfg, MainConfig: mainCfg,
	}, nil
}

// forget removes a worktree and prevents discovery from re-adding it for a
// minute (Worktrunk's pre-remove hook runs before the directory is gone).
func (d *Daemon) forget(id string) {
	d.mu.Lock()
	d.tombstones[id] = time.Now().Add(time.Minute)
	d.mu.Unlock()
	d.reg.RemoveWorktree(id)
}

// discover reconciles the registry with `git worktree list` for every known
// project: new worktrees appear, removed/prunable ones disappear, renamed
// branches re-slug and moved paths are followed.
func (d *Daemon) discover(ctx context.Context) {
	snap := d.reg.Snapshot()
	now := time.Now()
	d.mu.Lock()
	for id, until := range d.tombstones {
		if now.After(until) {
			delete(d.tombstones, id)
		}
	}
	tomb := make(map[string]bool, len(d.tombstones))
	for id := range d.tombstones {
		tomb[id] = true
	}
	d.mu.Unlock()

	for _, p := range snap.Projects {
		dir := p.MainPath
		if _, err := os.Stat(dir); dir == "" || err != nil {
			dir = ""
			for _, w := range snap.Worktrees {
				if w.ProjectID == p.ID {
					if _, err := os.Stat(w.Path); err == nil {
						dir = w.Path
						break
					}
				}
			}
		}
		var listed []gitwt.Worktree
		if dir != "" {
			var err error
			if listed, err = gitwt.List(ctx, dir); err != nil {
				d.log.Printf("discovery %s: %v", p.Name, err)
				continue // transient git failure: change nothing
			}
		}
		live := map[string]gitwt.Worktree{}
		for _, w := range listed {
			if !w.Bare && !w.Prunable {
				if _, err := os.Stat(w.Path); err == nil {
					live[w.AdminID] = w
				}
			}
		}
		for _, w := range snap.Worktrees {
			if w.ProjectID != p.ID {
				continue
			}
			if _, ok := live[w.AdminID]; !ok {
				d.log.Printf("worktree %s/%s removed", p.Slug, w.Slug)
				d.reg.RemoveWorktree(w.ID)
			}
		}
		mainPath := ""
		for _, w := range listed {
			if w.IsMain && !w.Bare {
				mainPath = w.Path
			}
		}
		for _, lw := range live {
			id := registry.WorktreeID(p.ID, lw.AdminID)
			if tomb[id] {
				continue
			}
			in, err := d.worktreeInput(p.CommonDir, mainPath, lw)
			if err != nil {
				d.log.Printf("discovery %s: %v", lw.Path, err)
				continue
			}
			if old, ok := snap.Worktree(id); ok && unchanged(old, in) {
				continue
			}
			if _, err := d.reg.UpsertWorktree(in); err != nil {
				d.log.Printf("discovery %s: %v", lw.Path, err)
			}
		}
	}
}

func unchanged(old registry.Worktree, in registry.WorktreeInput) bool {
	return old.Branch == in.Branch && old.Path == in.Path && old.IsMain == in.IsMain &&
		reflect.DeepEqual(old.Config, in.Config)
}

// checkHealth updates service status from PID liveness and TCP/HTTP probes.
// Probes run concurrently so one slow health endpoint cannot delay the rest,
// and every update is conditional on the registration being unchanged.
func (d *Daemon) checkHealth(ctx context.Context) {
	snap := d.reg.Snapshot()
	var wg sync.WaitGroup
	for _, s := range snap.Services {
		wt, _ := snap.Worktree(s.WorktreeID)
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.checkService(ctx, s, wt.ServiceConfig(s.Name).Health)
		}()
	}
	wg.Wait()
}

func (d *Daemon) checkService(ctx context.Context, s registry.Service, healthPath string) {
	if s.PID > 0 && !ProcessAlive(s.PID) {
		if d.reg.DeregisterIfCurrent(s) {
			d.log.Printf("service %s: process %d exited, deregistered", s.Key(), s.PID)
		}
		return
	}
	switch {
	case probe(ctx, s, healthPath):
		d.reg.SetStatusIfCurrent(s, registry.StatusUp)
	case s.Status == registry.StatusStarting:
		if s.PID == 0 && time.Since(s.RegisteredAt) > d.g.StaleAfter {
			d.reg.DeregisterIfCurrent(s)
		}
	default:
		if d.reg.SetStatusIfCurrent(s, registry.StatusDown) {
			d.log.Printf("service %s on %s is down", s.Key(), s.Upstream())
		}
		if s.PID == 0 && !s.DownSince.IsZero() && time.Since(s.DownSince) > d.g.StaleAfter {
			if d.reg.DeregisterIfCurrent(s) {
				d.log.Printf("service %s stale for %s, deregistered", s.Key(), d.g.StaleAfter)
			}
		}
	}
}

// probeClient never follows redirects: a 3xx from the health path means the
// app is up, and following it could leave loopback.
var probeClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func probe(ctx context.Context, s registry.Service, healthPath string) bool {
	c, err := net.DialTimeout("tcp", s.Upstream(), 500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	if healthPath == "" {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.Upstream()+healthPath, nil)
	if err != nil {
		return false
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}

// ---------------------------------------------------------------------------
// Ports

// AllocPort returns the configured port, or a deterministic free port for
// (project, worktree, service) inside the configured range, skipping ports
// already registered or bound.
func (d *Daemon) AllocPort(wt registry.Worktree, service string) (int, error) {
	if p := int(wt.ServiceConfig(service).Port); p != 0 {
		return p, nil
	}
	snap := d.reg.Snapshot()
	if s, ok := snap.Service(wt.ID, service); ok && !portFree(s.Port) {
		// Already running: keep it.
		return s.Port, nil
	}
	used := map[int]bool{}
	for _, s := range snap.Services {
		if !(s.WorktreeID == wt.ID && s.Name == service) {
			used[s.Port] = true
		}
	}
	lo, hi := d.g.PortRange[0], d.g.PortRange[1]
	span := hi - lo + 1
	h := fnv.New32a()
	h.Write([]byte(wt.ProjectID + "/" + wt.Slug + "/" + service))
	start := int(h.Sum32() % uint32(span))
	for i := 0; i < span && i < 2000; i++ {
		p := lo + (start+i)%span
		if !used[p] && portFree(p) {
			return p, nil
		}
	}
	return 0, errors.New("no free port in port_range")
}

func portFree(p int) bool {
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// ---------------------------------------------------------------------------
// Identity environment

// Env returns the runtime identity variables for a worktree (and optionally
// one of its services). Only gateway identity is injected — never app config.
func (d *Daemon) Env(wt registry.Worktree, service string, port int) map[string]string {
	snap := d.reg.Snapshot()
	p, _ := snap.Project(wt.ProjectID)
	env := map[string]string{
		"WG_GATEWAY":        "1",
		"WG_PROJECT":        p.Slug,
		"WG_PROJECT_DOMAIN": p.Domain,
		"WG_WORKTREE":       wt.Slug,
		"WG_WORKTREE_PATH":  wt.Path,
		"WG_BRANCH":         wt.Branch,
	}
	cfg := wt.Cfg()
	oauth := false
	for _, name := range snap.ServiceNames(wt) {
		u := registry.URL(registry.Hostnames(p, wt, name)[0], d.g)
		env["WG_"+registry.EnvName(name)+"_URL"] = u
		env["WG_"+registry.EnvName(name)+"_HOST"] = registry.Hostnames(p, wt, name)[0]
		if name == cfg.Default() {
			env["WG_URL"] = u
		}
		if len(wt.ServiceConfig(name).OAuthCallbacks) > 0 {
			oauth = true
		}
	}
	if service != "" {
		env["WG_SERVICE"] = service
		env["WG_SERVICE_URL"] = registry.URL(registry.Hostnames(p, wt, service)[0], d.g)
		if port > 0 {
			env["PORT"] = strconv.Itoa(port)
		}
	}
	pub := d.PublicBase(wt)
	if pub != "" {
		env["WG_PUBLIC_URL"] = pub
	}
	if oauth {
		cb := registry.URL(p.Domain, d.g) + ingress.CallbackPath
		if st := d.TunnelStatus(); st.PublicURL != "" {
			cb = strings.TrimRight(st.PublicURL, "/") + ingress.CallbackPath
		}
		env["WG_OAUTH_CALLBACK_URL"] = cb
		env["WG_OAUTH_STATE_KEY"] = d.signer.WorktreeKeyHex(p.Slug, wt.Slug)
	}
	return env
}

// PublicBase is the public URL prefix for a worktree, "" without a tunnel or
// without any publicly exposed service.
func (d *Daemon) PublicBase(wt registry.Worktree) string {
	st := d.TunnelStatus()
	if st.PublicURL == "" {
		return ""
	}
	public := false
	for _, s := range wt.Cfg().Services {
		if s.Public != nil {
			public = true
		}
	}
	if !public {
		return ""
	}
	snap := d.reg.Snapshot()
	p, _ := snap.Project(wt.ProjectID)
	ref := wt.Slug
	if _, _, err := snap.FindWorktree(ref, nil); err != nil {
		ref = wt.Slug + "." + p.Slug
	}
	if pd := strings.Trim(d.g.Tunnel.PublicDomain, "."); pd != "" {
		return "https://" + ref + "." + pd
	}
	return strings.TrimRight(st.PublicURL, "/") + "/w/" + ref
}
