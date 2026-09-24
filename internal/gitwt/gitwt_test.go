package gitwt

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestParsePorcelain(t *testing.T) {
	out := `worktree /repo
HEAD 1111111111111111111111111111111111111111
branch refs/heads/main

worktree /repo.feature-auth
HEAD 2222222222222222222222222222222222222222
branch refs/heads/feature/auth
locked reason

worktree /gone
HEAD 3333333333333333333333333333333333333333
detached
prunable gitdir file points to non-existent location

`
	wts := ParsePorcelain(out)
	if len(wts) != 3 {
		t.Fatalf("got %d worktrees", len(wts))
	}
	if !wts[0].IsMain || wts[0].Branch != "main" {
		t.Fatalf("main parsed wrong: %+v", wts[0])
	}
	if wts[1].IsMain || wts[1].Branch != "feature/auth" || !wts[1].Locked {
		t.Fatalf("linked parsed wrong: %+v", wts[1])
	}
	if !wts[2].Detached || !wts[2].Prunable {
		t.Fatalf("prunable parsed wrong: %+v", wts[2])
	}
}

// NewTestRepo creates a repository with one commit and returns its path.
func newTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("commit", "-q", "--allow-empty", "-m", "init")
	return dir
}

func TestResolveAndMove(t *testing.T) {
	repo := newTestRepo(t)
	wt := filepath.Join(t.TempDir(), "wt-auth")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-q", "-b", "feature/auth", wt).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	ctx := context.Background()
	r, err := Resolve(ctx, filepath.Join(wt))
	if err != nil {
		t.Fatal(err)
	}
	if r.CurrentAdmin != "wt-auth" {
		t.Fatalf("admin id = %q", r.CurrentAdmin)
	}
	if r.MainPath != realpath(repo) {
		t.Fatalf("main = %q want %q", r.MainPath, realpath(repo))
	}
	main, err := Resolve(ctx, repo)
	if err != nil || main.CurrentAdmin != "." {
		t.Fatalf("main resolve: %v %+v", err, main)
	}

	moved := filepath.Join(t.TempDir(), "moved")
	if out, err := exec.Command("git", "-C", repo, "worktree", "move", wt, moved).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	wts, err := List(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(wts) != 2 || wts[1].AdminID != "wt-auth" || wts[1].Path != realpath(moved) {
		t.Fatalf("after move: %+v", wts)
	}

	if _, err := Resolve(ctx, t.TempDir()); err == nil {
		t.Fatal("expected ErrNotRepo")
	}
}
