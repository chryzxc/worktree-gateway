// Package registry holds the runtime-identity model: projects, worktrees and
// the services registered inside them. Routes, URLs and environment
// variables are derived from it; they are never stored.
package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chryzxc/worktree-gateway/internal/config"
	"github.com/chryzxc/worktree-gateway/internal/slug"
)

// Service statuses.
const (
	StatusStarting = "starting"
	StatusUp       = "up"
	StatusDown     = "down"
)

// Registration sources.
const (
	SourceRegister = "register"
	SourceRun      = "run"
)

type Project struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	CommonDir string    `json:"common_dir"`
	MainPath  string    `json:"main_path"`
	Domain    string    `json:"domain"`
	MainAlias bool      `json:"main_alias"`
	CreatedAt time.Time `json:"created_at"`
}

type Worktree struct {
	ID         string `json:"id"`
	ProjectID  string `json:"project_id"`
	AdminID    string `json:"admin_id"`
	Slug       string `json:"slug"`
	Branch     string `json:"branch"`
	Path       string `json:"path"`
	IsMain     bool   `json:"is_main"`
	PinnedName string `json:"pinned_name,omitempty"`
	// Parked worktrees keep their identity but publish no routes (`wtg down`).
	Parked    bool            `json:"parked,omitempty"`
	Config    *config.Project `json:"config,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

type Service struct {
	WorktreeID   string    `json:"worktree_id"`
	Name         string    `json:"name"`
	Host         string    `json:"host"`
	Port         int       `json:"port"`
	PID          int       `json:"pid,omitempty"`
	Source       string    `json:"source"`
	Status       string    `json:"status"`
	RegisteredAt time.Time `json:"registered_at"`
	LastUp       time.Time `json:"last_up,omitempty"`
	DownSince    time.Time `json:"down_since,omitempty"`
}

// Key returns the registry key of a service.
func (s *Service) Key() string { return s.WorktreeID + "/" + s.Name }

// Upstream is the dial address of the service.
func (s *Service) Upstream() string { return net.JoinHostPort(s.Host, fmt.Sprint(s.Port)) }

type state struct {
	Projects  map[string]*Project  `json:"projects"`
	Worktrees map[string]*Worktree `json:"worktrees"`
	Services  map[string]*Service  `json:"services"`
}

// Registry is safe for concurrent use.
type Registry struct {
	mu       sync.RWMutex
	path     string
	st       state
	onChange func()
	now      func() time.Time
}

// Open loads the registry persisted at path (a missing file is fine).
func Open(path string) (*Registry, error) {
	r := &Registry{path: path, now: time.Now, st: state{
		Projects: map[string]*Project{}, Worktrees: map[string]*Worktree{}, Services: map[string]*Service{},
	}}
	if path == "" {
		return r, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &r.st); err != nil {
		return nil, fmt.Errorf("registry %s is corrupt: %w", path, err)
	}
	if r.st.Projects == nil {
		r.st.Projects = map[string]*Project{}
	}
	if r.st.Worktrees == nil {
		r.st.Worktrees = map[string]*Worktree{}
	}
	if r.st.Services == nil {
		r.st.Services = map[string]*Service{}
	}
	return r, nil
}

// OnChange registers a callback invoked (outside the lock) after every
// mutation that affects routing.
func (r *Registry) OnChange(f func()) { r.mu.Lock(); r.onChange = f; r.mu.Unlock() }

func (r *Registry) changed() {
	if err := r.saveLocked(); err != nil {
		fmt.Fprintf(os.Stderr, "wtg: saving registry: %v\n", err)
	}
}

func (r *Registry) notify() {
	r.mu.RLock()
	f := r.onChange
	r.mu.RUnlock()
	if f != nil {
		f()
	}
}

func (r *Registry) saveLocked() error {
	if r.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(r.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

func hashID(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

// ProjectID is derived from the git common dir, which is shared by every
// worktree of a repository.
func ProjectID(commonDir string) string { return hashID(commonDir) }

// WorktreeID combines the project with git's own admin id for the worktree.
func WorktreeID(projectID, adminID string) string { return projectID + ":" + adminID }

// ---------------------------------------------------------------------------
// Worktree sync

// WorktreeInput is what discovery knows about a worktree.
type WorktreeInput struct {
	CommonDir   string
	MainPath    string
	AdminID     string
	Branch      string
	Path        string
	IsMain      bool
	PinnedName  string // optional explicit name; "" keeps any existing pin
	Config      *config.Project
	MainConfig  *config.Project // config of the main worktree (project-level settings)
	ClearPinned bool
}

// UpsertWorktree creates or updates the identity of a worktree and returns
// a copy of it.
func (r *Registry) UpsertWorktree(in WorktreeInput) (Worktree, error) {
	r.mu.Lock()
	wt, err := r.upsertWorktreeLocked(in)
	var out Worktree
	if err == nil {
		out = *wt
		r.changed()
	}
	r.mu.Unlock()
	if err == nil {
		r.notify()
	}
	return out, err
}

func (r *Registry) upsertWorktreeLocked(in WorktreeInput) (*Worktree, error) {
	proj, err := r.upsertProjectLocked(in)
	if err != nil {
		return nil, err
	}
	id := WorktreeID(proj.ID, in.AdminID)
	wt, ok := r.st.Worktrees[id]
	if !ok {
		wt = &Worktree{ID: id, ProjectID: proj.ID, AdminID: in.AdminID, CreatedAt: r.now()}
		r.st.Worktrees[id] = wt
	}
	if in.PinnedName != "" {
		wt.PinnedName = in.PinnedName
	}
	if in.ClearPinned {
		wt.PinnedName = ""
	}
	wt.Branch = in.Branch
	wt.Path = in.Path
	wt.IsMain = in.IsMain
	if in.Config != nil {
		wt.Config = in.Config
	}
	desired := desiredSlug(wt)
	if wt.Slug == "" || baseOf(wt.Slug, wt.ID) != desired {
		wt.Slug = r.allocSlugLocked(proj.ID, wt.ID, desired)
	}
	return wt, nil
}

func desiredSlug(wt *Worktree) string {
	switch {
	case wt.PinnedName != "":
		return slug.Sanitize(wt.PinnedName)
	case wt.Branch != "":
		return slug.Sanitize(wt.Branch)
	default:
		return slug.Sanitize(filepath.Base(wt.Path))
	}
}

// baseOf strips the disambiguation suffix this worktree may have received.
func baseOf(s, key string) string {
	suffix := "-" + slug.Hash4(key)
	return strings.TrimSuffix(s, suffix)
}

func (r *Registry) allocSlugLocked(projectID, wtID, desired string) string {
	taken := func(s string) bool {
		for _, o := range r.st.Worktrees {
			if o.ProjectID == projectID && o.ID != wtID && o.Slug == s {
				return true
			}
		}
		return false
	}
	if !taken(desired) {
		return desired
	}
	return slug.WithSuffix(desired, wtID)
}

func (r *Registry) upsertProjectLocked(in WorktreeInput) (*Project, error) {
	id := ProjectID(in.CommonDir)
	cfg := in.MainConfig
	if cfg == nil {
		cfg = in.Config
	}
	domain := ""
	if cfg != nil && cfg.Domain != "" {
		domain = strings.ToLower(strings.TrimSuffix(cfg.Domain, "."))
		if !slug.ValidHostname(domain) {
			return nil, fmt.Errorf("project domain %q is not a valid hostname", cfg.Domain)
		}
		for _, o := range r.st.Projects {
			if o.ID != id && o.Domain == domain {
				return nil, fmt.Errorf("domain %q is already used by project %s (%s); set a different `domain:` in wtg.yaml", domain, o.Name, o.MainPath)
			}
		}
	}
	p, ok := r.st.Projects[id]
	if !ok {
		p = &Project{ID: id, CommonDir: in.CommonDir, CreatedAt: r.now()}
		r.st.Projects[id] = p
	}
	if in.MainPath != "" {
		p.MainPath = in.MainPath
	}
	name := ""
	if cfg != nil {
		name = cfg.Project
	}
	if name == "" {
		base := p.MainPath
		if base == "" {
			base = strings.TrimSuffix(in.CommonDir, string(filepath.Separator)+".git")
		}
		name = filepath.Base(base)
	}
	p.Name = name
	p.MainAlias = cfg == nil || cfg.MainAliasEnabled()

	wantSlug := slug.Sanitize(name)
	for _, o := range r.st.Projects {
		if o.ID != id && o.Slug == wantSlug {
			wantSlug = slug.WithSuffix(wantSlug, id)
			break
		}
	}
	p.Slug = wantSlug
	if domain == "" {
		domain = p.Slug + ".localhost"
	}
	p.Domain = domain
	return p, nil
}

// RemoveWorktree deletes a worktree and its services.
func (r *Registry) RemoveWorktree(id string) bool {
	r.mu.Lock()
	_, ok := r.st.Worktrees[id]
	if ok {
		delete(r.st.Worktrees, id)
		for k, s := range r.st.Services {
			if s.WorktreeID == id {
				delete(r.st.Services, k)
			}
		}
		r.changed()
	}
	r.mu.Unlock()
	if ok {
		r.notify()
	}
	return ok
}

// SetParked parks (removing its services) or un-parks a worktree.
func (r *Registry) SetParked(id string, parked bool) bool {
	r.mu.Lock()
	wt, ok := r.st.Worktrees[id]
	if ok {
		wt.Parked = parked
		if parked {
			for k, s := range r.st.Services {
				if s.WorktreeID == id {
					delete(r.st.Services, k)
				}
			}
		}
		r.changed()
	}
	r.mu.Unlock()
	if ok {
		r.notify()
	}
	return ok
}

// ---------------------------------------------------------------------------
// Services

// RegisterInput describes a service registration.
type RegisterInput struct {
	WorktreeID string
	Name       string
	Host       string
	Port       int
	PID        int
	Source     string
	Status     string
}

// ErrNotLoopback is returned when a registration targets a non-loopback host.
var ErrNotLoopback = errors.New("services must listen on a loopback address (127.0.0.1 or ::1)")

// Register adds or replaces a service. Any other registration pointing at the
// same host:port is evicted: two live processes cannot both listen there, so
// the older entry is necessarily stale.
func (r *Registry) Register(in RegisterInput) (Service, []Service, error) {
	if in.Host == "" || in.Host == "localhost" {
		in.Host = "127.0.0.1"
	}
	ip := net.ParseIP(in.Host)
	if ip == nil || !ip.IsLoopback() {
		return Service{}, nil, ErrNotLoopback
	}
	if in.Port < 1 || in.Port > 65535 {
		return Service{}, nil, fmt.Errorf("invalid port %d", in.Port)
	}
	if !slug.ValidLabel(in.Name) {
		return Service{}, nil, fmt.Errorf("invalid service name %q (use lowercase letters, digits and -)", in.Name)
	}
	if in.Source == "" {
		in.Source = SourceRegister
	}
	if in.Status == "" {
		in.Status = StatusUp
	}
	r.mu.Lock()
	wt, ok := r.st.Worktrees[in.WorktreeID]
	if !ok {
		r.mu.Unlock()
		return Service{}, nil, fmt.Errorf("unknown worktree %s", in.WorktreeID)
	}
	wt.Parked = false
	now := r.now()
	svc := &Service{
		WorktreeID: in.WorktreeID, Name: in.Name, Host: in.Host, Port: in.Port, PID: in.PID,
		Source: in.Source, Status: in.Status, RegisteredAt: now,
	}
	if svc.Status == StatusUp {
		svc.LastUp = now
	}
	var evicted []Service
	for k, o := range r.st.Services {
		if k != svc.Key() && o.Port == svc.Port && sameLoopback(o.Host, svc.Host) {
			evicted = append(evicted, *o)
			delete(r.st.Services, k)
		}
	}
	r.st.Services[svc.Key()] = svc
	out := *svc
	r.changed()
	r.mu.Unlock()
	r.notify()
	return out, evicted, nil
}

func sameLoopback(a, b string) bool {
	return a == b || (net.ParseIP(a).IsLoopback() && net.ParseIP(b).IsLoopback())
}

// Deregister removes a service. If pid is non-zero, the registration is only
// removed when it still belongs to that process (so a late exit of an old
// process cannot remove its replacement).
func (r *Registry) Deregister(worktreeID, name string, pid int) bool {
	r.mu.Lock()
	key := worktreeID + "/" + name
	s, ok := r.st.Services[key]
	if ok && pid != 0 && s.PID != pid {
		ok = false
	}
	if ok {
		delete(r.st.Services, key)
		r.changed()
	}
	r.mu.Unlock()
	if ok {
		r.notify()
	}
	return ok
}

// SetStatus updates health information. It reports whether routing changed.
func (r *Registry) SetStatus(key, status string) bool {
	r.mu.Lock()
	s, ok := r.st.Services[key]
	if !ok || s.Status == status {
		if ok && status == StatusUp {
			s.LastUp = r.now()
		}
		r.mu.Unlock()
		return false
	}
	s.Status = status
	now := r.now()
	switch status {
	case StatusUp:
		s.LastUp = now
		s.DownSince = time.Time{}
	case StatusDown:
		s.DownSince = now
	}
	r.changed()
	r.mu.Unlock()
	r.notify()
	return true
}

// ---------------------------------------------------------------------------
// Queries

// Snapshot is an immutable copy of the registry.
type Snapshot struct {
	Projects  []Project  `json:"projects"`
	Worktrees []Worktree `json:"worktrees"`
	Services  []Service  `json:"services"`
}

// Snapshot returns sorted copies of all entries.
func (r *Registry) Snapshot() Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var s Snapshot
	for _, p := range r.st.Projects {
		s.Projects = append(s.Projects, *p)
	}
	for _, w := range r.st.Worktrees {
		s.Worktrees = append(s.Worktrees, *w)
	}
	for _, v := range r.st.Services {
		s.Services = append(s.Services, *v)
	}
	sort.Slice(s.Projects, func(i, j int) bool { return s.Projects[i].Slug < s.Projects[j].Slug })
	sort.Slice(s.Worktrees, func(i, j int) bool {
		a, b := s.Worktrees[i], s.Worktrees[j]
		if a.ProjectID != b.ProjectID {
			return a.ProjectID < b.ProjectID
		}
		if a.IsMain != b.IsMain {
			return a.IsMain
		}
		return a.Slug < b.Slug
	})
	sort.Slice(s.Services, func(i, j int) bool { return s.Services[i].Key() < s.Services[j].Key() })
	return s
}

// Project returns a project by id.
func (s Snapshot) Project(id string) (Project, bool) {
	for _, p := range s.Projects {
		if p.ID == id {
			return p, true
		}
	}
	return Project{}, false
}

// Worktree returns a worktree by id.
func (s Snapshot) Worktree(id string) (Worktree, bool) {
	for _, w := range s.Worktrees {
		if w.ID == id {
			return w, true
		}
	}
	return Worktree{}, false
}

// FindWorktree resolves "<worktree>" or "<worktree>.<project>" (by slug).
// The bare form is only accepted when it is unambiguous; restrictTo limits
// the search to projects for which it returns true (nil = all).
func (s Snapshot) FindWorktree(ref string, restrictTo func(Project) bool) (Worktree, Project, error) {
	var matches []Worktree
	wtSlug, projSlug, qualified := strings.Cut(ref, ".")
	for _, w := range s.Worktrees {
		p, _ := s.Project(w.ProjectID)
		if restrictTo != nil && !restrictTo(p) {
			continue
		}
		if w.Slug != wtSlug {
			continue
		}
		if qualified && p.Slug != projSlug {
			continue
		}
		matches = append(matches, w)
	}
	switch len(matches) {
	case 0:
		return Worktree{}, Project{}, fmt.Errorf("no worktree %q", ref)
	case 1:
		p, _ := s.Project(matches[0].ProjectID)
		return matches[0], p, nil
	default:
		return Worktree{}, Project{}, fmt.Errorf("worktree %q is ambiguous; use <worktree>.<project>", ref)
	}
}

// Service returns a service of a worktree.
func (s Snapshot) Service(worktreeID, name string) (Service, bool) {
	for _, v := range s.Services {
		if v.WorktreeID == worktreeID && v.Name == name {
			return v, true
		}
	}
	return Service{}, false
}

// ServiceConfig returns the configured settings for a service (zero value
// for ad-hoc registrations).
func (w Worktree) ServiceConfig(name string) config.Service {
	if w.Config == nil {
		return config.Service{}
	}
	return w.Config.Services[name]
}

// Cfg returns the worktree's project config, never nil.
func (w Worktree) Cfg() *config.Project {
	if w.Config == nil {
		return &config.Project{Services: map[string]config.Service{}}
	}
	return w.Config
}

// ServiceNames lists configured and registered services of a worktree.
func (s Snapshot) ServiceNames(w Worktree) []string {
	set := map[string]bool{}
	for n := range w.Cfg().Services {
		set[n] = true
	}
	for _, v := range s.Services {
		if v.WorktreeID == w.ID {
			set[v.Name] = true
		}
	}
	var out []string
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Hostnames returns the local hostnames for a service of a worktree.
func Hostnames(p Project, w Worktree, service string) []string {
	cfg := w.Cfg()
	tmpl := cfg.HostnameTemplate(service)
	h := strings.NewReplacer(
		"{worktree}", w.Slug,
		"{project}", p.Slug,
		"{domain}", p.Domain,
		"{service}", slug.Sanitize(service),
	).Replace(tmpl)
	hosts := []string{strings.ToLower(h)}
	if w.IsMain && p.MainAlias {
		// The main worktree also answers without the worktree label.
		alias := strings.NewReplacer(
			"{worktree}.", "", ".{worktree}", "", "{worktree}", "",
		).Replace(tmpl)
		alias = strings.NewReplacer(
			"{project}", p.Slug, "{domain}", p.Domain, "{service}", slug.Sanitize(service),
		).Replace(alias)
		alias = strings.Trim(strings.ToLower(alias), ".")
		if alias != "" && alias != hosts[0] && slug.ValidHostname(alias) {
			hosts = append(hosts, alias)
		}
	}
	return hosts
}

// Route maps one hostname to a service.
type Route struct {
	Hostname   string
	ProjectID  string
	WorktreeID string
	Worktree   string // slug
	Service    string
	Upstream   string // "" when not registered
	Status     string // service status, or "missing" when only configured
	Conflict   string // non-empty when another route won the hostname
}

// Routes derives the routing table. Configured-but-unregistered services get
// a route with Status "missing" so the proxy can answer with a helpful page.
// When two services claim the same hostname, the earlier registration wins.
func (s Snapshot) Routes() []Route {
	type cand struct {
		r  Route
		at time.Time
	}
	var cands []cand
	for _, w := range s.Worktrees {
		p, ok := s.Project(w.ProjectID)
		if !ok || w.Parked {
			continue
		}
		for _, name := range s.ServiceNames(w) {
			svc, registered := s.Service(w.ID, name)
			for _, h := range Hostnames(p, w, name) {
				r := Route{Hostname: h, ProjectID: p.ID, WorktreeID: w.ID, Worktree: w.Slug, Service: name, Status: "missing"}
				at := time.Unix(1<<40, 0) // unregistered routes lose to registered ones
				if registered {
					r.Upstream = svc.Upstream()
					r.Status = svc.Status
					at = svc.RegisteredAt
				}
				cands = append(cands, cand{r, at})
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].r.Hostname != cands[j].r.Hostname {
			return cands[i].r.Hostname < cands[j].r.Hostname
		}
		if !cands[i].at.Equal(cands[j].at) {
			return cands[i].at.Before(cands[j].at)
		}
		return cands[i].r.WorktreeID+cands[i].r.Service < cands[j].r.WorktreeID+cands[j].r.Service
	})
	var out []Route
	winner := map[string]Route{}
	for _, c := range cands {
		if w, ok := winner[c.r.Hostname]; ok {
			c.r.Conflict = fmt.Sprintf("%s/%s", w.Worktree, w.Service)
		} else {
			winner[c.r.Hostname] = c.r
		}
		out = append(out, c.r)
	}
	return out
}
