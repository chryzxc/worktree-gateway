package registry

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/chryzxc/worktree-gateway/internal/config"
)

func mustCfg(t *testing.T, doc string) *config.Project {
	t.Helper()
	p, err := config.ParseProject([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const cfgDoc = `
project: myapp
services:
  web: {}
  api: {}
`

func setup(t *testing.T) (*Registry, Worktree, Worktree) {
	t.Helper()
	r, err := Open(filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := mustCfg(t, cfgDoc)
	main, err := r.UpsertWorktree(WorktreeInput{CommonDir: "/r/.git", MainPath: "/r", AdminID: ".", Branch: "main", Path: "/r", IsMain: true, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	feat, err := r.UpsertWorktree(WorktreeInput{CommonDir: "/r/.git", MainPath: "/r", AdminID: "r.feature-auth", Branch: "feature/auth", Path: "/r.feature-auth", Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	return r, main, feat
}

func TestIdentity(t *testing.T) {
	r, main, feat := setup(t)
	if main.Slug != "main" || feat.Slug != "feature-auth" {
		t.Fatalf("slugs %q %q", main.Slug, feat.Slug)
	}
	s := r.Snapshot()
	p, _ := s.Project(feat.ProjectID)
	if p.Domain != "myapp.localhost" || p.Name != "myapp" {
		t.Fatalf("project %+v", p)
	}
	if h := Hostnames(p, feat, "web"); len(h) != 1 || h[0] != "feature-auth.myapp.localhost" {
		t.Fatalf("web hosts %v", h)
	}
	if h := Hostnames(p, feat, "api"); h[0] != "api.feature-auth.myapp.localhost" {
		t.Fatalf("api hosts %v", h)
	}
	h := Hostnames(p, main, "api")
	if len(h) != 2 || h[1] != "api.myapp.localhost" {
		t.Fatalf("main alias %v", h)
	}
	if h := Hostnames(p, main, "web"); len(h) != 2 || h[1] != "myapp.localhost" {
		t.Fatalf("main web alias %v", h)
	}
}

func TestSlugCollisionAndRename(t *testing.T) {
	r, _, feat := setup(t)
	// A second branch that sanitizes to the same slug gets a suffix.
	other, err := r.UpsertWorktree(WorktreeInput{CommonDir: "/r/.git", AdminID: "x", Branch: "feature-auth", Path: "/x"})
	if err != nil {
		t.Fatal(err)
	}
	if other.Slug == feat.Slug || !strings.HasPrefix(other.Slug, "feature-auth-") {
		t.Fatalf("collision not resolved: %q vs %q", other.Slug, feat.Slug)
	}
	// Re-syncing keeps the suffixed slug stable.
	again, _ := r.UpsertWorktree(WorktreeInput{CommonDir: "/r/.git", AdminID: "x", Branch: "feature-auth", Path: "/x"})
	if again.Slug != other.Slug {
		t.Fatalf("slug not stable: %q -> %q", other.Slug, again.Slug)
	}
	// Branch rename re-slugs; path move keeps identity.
	renamed, _ := r.UpsertWorktree(WorktreeInput{CommonDir: "/r/.git", AdminID: "x", Branch: "feature/sms", Path: "/moved"})
	if renamed.ID != other.ID || renamed.Slug != "feature-sms" || renamed.Path != "/moved" {
		t.Fatalf("rename: %+v", renamed)
	}
	// Pinned names win over branch names.
	pinned, _ := r.UpsertWorktree(WorktreeInput{CommonDir: "/r/.git", AdminID: "x", Branch: "feature/sms", Path: "/moved", PinnedName: "Demo Env"})
	if pinned.Slug != "demo-env" {
		t.Fatalf("pinned: %q", pinned.Slug)
	}
	// Detached HEAD falls back to the directory name.
	det, _ := r.UpsertWorktree(WorktreeInput{CommonDir: "/r/.git", AdminID: "d", Path: "/tmp/repo.hotfix"})
	if det.Slug != "repo-hotfix" {
		t.Fatalf("detached: %q", det.Slug)
	}
}

func TestProjectCollisions(t *testing.T) {
	r, _, _ := setup(t)
	cfg := mustCfg(t, "project: myapp\n")
	w, err := r.UpsertWorktree(WorktreeInput{CommonDir: "/other/.git", AdminID: ".", Path: "/other", IsMain: true, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := r.Snapshot().Project(w.ProjectID)
	if p.Slug == "myapp" || p.Domain == "myapp.localhost" {
		t.Fatalf("second project must not steal the domain: %+v", p)
	}
	dup := mustCfg(t, "domain: myapp.localhost\n")
	if _, err := r.UpsertWorktree(WorktreeInput{CommonDir: "/third/.git", AdminID: ".", Path: "/third", IsMain: true, Config: dup}); err == nil {
		t.Fatal("explicit duplicate domain must fail")
	}
	if len(r.Snapshot().Projects) != 2 {
		t.Fatal("failed upsert must not leave a project behind")
	}
}

func TestRegisterEvictAndRoutes(t *testing.T) {
	r, main, feat := setup(t)
	if _, _, err := r.Register(RegisterInput{WorktreeID: feat.ID, Name: "web", Host: "10.0.0.5", Port: 3000}); err != ErrNotLoopback {
		t.Fatalf("non-loopback must fail, got %v", err)
	}
	if _, _, err := r.Register(RegisterInput{WorktreeID: "nope", Name: "web", Port: 3000}); err == nil {
		t.Fatal("unknown worktree must fail")
	}
	if _, _, err := r.Register(RegisterInput{WorktreeID: feat.ID, Name: "web", Port: 3000}); err != nil {
		t.Fatal(err)
	}
	// Same port claimed by another worktree evicts the stale owner.
	_, ev, err := r.Register(RegisterInput{WorktreeID: main.ID, Name: "web", Port: 3000})
	if err != nil || len(ev) != 1 || ev[0].WorktreeID != feat.ID {
		t.Fatalf("eviction: %v %+v", err, ev)
	}
	// Same service twice replaces.
	r.Register(RegisterInput{WorktreeID: main.ID, Name: "web", Port: 3001})
	s := r.Snapshot()
	if len(s.Services) != 1 || s.Services[0].Port != 3001 {
		t.Fatalf("replace: %+v", s.Services)
	}

	routes := s.Routes()
	byHost := map[string]Route{}
	for _, rt := range routes {
		if rt.Conflict == "" {
			byHost[rt.Hostname] = rt
		}
	}
	if byHost["main.myapp.localhost"].Upstream != "127.0.0.1:3001" || byHost["myapp.localhost"].Upstream != "127.0.0.1:3001" {
		t.Fatalf("routes: %+v", byHost)
	}
	if byHost["api.feature-auth.myapp.localhost"].Status != "missing" {
		t.Fatalf("configured unregistered service should have a missing route")
	}

	if !r.Deregister(main.ID, "web", 0) {
		t.Fatal("deregister failed")
	}
	r.Register(RegisterInput{WorktreeID: main.ID, Name: "web", Port: 3002, PID: 42})
	if r.Deregister(main.ID, "web", 41) {
		t.Fatal("deregister with foreign pid must not remove")
	}
}

func TestHostnameConflict(t *testing.T) {
	r, _, _ := setup(t)
	cfg := mustCfg(t, "project: myapp\nservices:\n  web: {}\n  docs:\n    hostname: \"{worktree}.{domain}\"\n")
	w, _ := r.UpsertWorktree(WorktreeInput{CommonDir: "/r/.git", AdminID: "c", Branch: "c", Path: "/c", Config: cfg})
	r.Register(RegisterInput{WorktreeID: w.ID, Name: "web", Port: 5000})
	r.Register(RegisterInput{WorktreeID: w.ID, Name: "docs", Port: 5001})
	var conflicts int
	for _, rt := range r.Snapshot().Routes() {
		if rt.Hostname == "c.myapp.localhost" && rt.Conflict != "" {
			conflicts++
			if rt.Service != "docs" {
				t.Fatalf("earlier registration should win, loser = %s", rt.Service)
			}
		}
	}
	if conflicts != 1 {
		t.Fatalf("expected one conflict, got %d", conflicts)
	}
}

func TestPersistenceAndRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	r, _ := Open(path)
	w, _ := r.UpsertWorktree(WorktreeInput{CommonDir: "/r/.git", AdminID: ".", Branch: "main", Path: "/r", IsMain: true})
	r.Register(RegisterInput{WorktreeID: w.ID, Name: "web", Port: 3000})
	r2, err := Open(path)
	if err != nil || len(r2.Snapshot().Services) != 1 {
		t.Fatalf("reload: %v", err)
	}
	r2.RemoveWorktree(w.ID)
	if s := r2.Snapshot(); len(s.Worktrees) != 0 || len(s.Services) != 0 {
		t.Fatal("remove worktree must cascade")
	}
}

func TestFindWorktree(t *testing.T) {
	r, _, _ := setup(t)
	r.UpsertWorktree(WorktreeInput{CommonDir: "/o/.git", AdminID: "f", Branch: "feature/auth", Path: "/o2", Config: mustCfg(t, "project: other\n")})
	s := r.Snapshot()
	if _, _, err := s.FindWorktree("feature-auth", nil); err == nil {
		t.Fatal("expected ambiguity")
	}
	w, p, err := s.FindWorktree("feature-auth.other", nil)
	if err != nil || p.Slug != "other" || w.Slug != "feature-auth" {
		t.Fatalf("%v %+v", err, p)
	}
	if _, _, err := s.FindWorktree("feature-auth", func(p Project) bool { return p.Slug == "myapp" }); err != nil {
		t.Fatal(err)
	}
}

func TestParked(t *testing.T) {
	r, _, feat := setup(t)
	r.Register(RegisterInput{WorktreeID: feat.ID, Name: "web", Port: 3000})
	r.SetParked(feat.ID, true)
	s := r.Snapshot()
	for _, rt := range s.Routes() {
		if rt.WorktreeID == feat.ID {
			t.Fatal("parked worktree must not publish routes")
		}
	}
	if len(s.Services) != 0 {
		t.Fatal("parking removes services")
	}
	r.Register(RegisterInput{WorktreeID: feat.ID, Name: "web", Port: 3000})
	if w, _ := r.Snapshot().Worktree(feat.ID); w.Parked {
		t.Fatal("registering a service un-parks")
	}
}

// A health decision made on a snapshot must not touch a service that was
// re-registered while the probe ran.
func TestGuardedUpdatesIgnoreReplacedRegistration(t *testing.T) {
	r, main, _ := setup(t)
	old, _, err := r.Register(RegisterInput{WorktreeID: main.ID, Name: "web", Port: 4000})
	if err != nil {
		t.Fatal(err)
	}
	fresh, _, _ := r.Register(RegisterInput{WorktreeID: main.ID, Name: "web", Port: 4001})

	if r.SetStatusIfCurrent(old, StatusDown) {
		t.Fatal("stale probe changed the replacement's status")
	}
	if r.DeregisterIfCurrent(old) {
		t.Fatal("stale probe removed the replacement")
	}
	if s, ok := r.Snapshot().Service(main.ID, "web"); !ok || s.Port != 4001 || s.Status != StatusUp {
		t.Fatalf("replacement changed: %+v %v", s, ok)
	}

	if !r.SetStatusIfCurrent(fresh, StatusDown) {
		t.Fatal("current probe should update status")
	}
	if !r.DeregisterIfCurrent(fresh) {
		t.Fatal("current registration should be removable")
	}
}
