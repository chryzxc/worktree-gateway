// Package gitwt reads worktree information from Git itself. It never creates,
// moves or removes worktrees — that is the job of Git or Worktrunk.
package gitwt

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Worktree is one entry of `git worktree list --porcelain`, enriched with the
// stable admin-dir identity.
type Worktree struct {
	Path     string // absolute, symlinks resolved
	Head     string
	Branch   string // short name, "" when detached
	Detached bool
	Bare     bool
	Locked   bool
	Prunable bool
	IsMain   bool
	// AdminID is the name of the directory under <common-dir>/worktrees that
	// Git uses for this worktree. It survives `git worktree move`. It is "."
	// for the main worktree.
	AdminID string
}

// Repo describes the repository a path belongs to.
type Repo struct {
	CommonDir    string // absolute git common dir (shared by all worktrees)
	MainPath     string // main worktree path ("" for bare repositories)
	TopLevel     string // top level of the worktree that contained the query path
	CurrentAdmin string // admin id of that worktree
}

var ErrNotRepo = errors.New("not inside a git worktree")

func git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

func realpath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// Resolve finds the repository and worktree containing dir.
func Resolve(ctx context.Context, dir string) (*Repo, error) {
	out, err := git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir", "--git-dir", "--show-toplevel")
	if err != nil {
		if _, statErr := os.Stat(dir); statErr != nil {
			return nil, fmt.Errorf("%w: %s", ErrNotRepo, dir)
		}
		return nil, fmt.Errorf("%w: %v", ErrNotRepo, err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 3 {
		return nil, fmt.Errorf("%w: %s", ErrNotRepo, dir)
	}
	r := &Repo{
		CommonDir: realpath(lines[0]),
		TopLevel:  realpath(lines[2]),
	}
	r.CurrentAdmin = AdminIDFromGitDir(r.CommonDir, realpath(lines[1]))
	wts, err := List(ctx, r.TopLevel)
	if err != nil {
		return nil, err
	}
	for _, w := range wts {
		if w.IsMain && !w.Bare {
			r.MainPath = w.Path
		}
	}
	return r, nil
}

// AdminIDFromGitDir maps a worktree's git dir to its admin id.
func AdminIDFromGitDir(commonDir, gitDir string) string {
	if gitDir == commonDir {
		return "."
	}
	prefix := filepath.Join(commonDir, "worktrees") + string(filepath.Separator)
	if strings.HasPrefix(gitDir, prefix) {
		return strings.TrimPrefix(gitDir, prefix)
	}
	return filepath.Base(gitDir)
}

// List returns all worktrees of the repository containing dir.
func List(ctx context.Context, dir string) ([]Worktree, error) {
	out, err := git(ctx, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	wts := ParsePorcelain(out)
	for i := range wts {
		wts[i].AdminID = adminIDForPath(wts[i])
	}
	return wts, nil
}

// adminIDForPath reads the `.git` file of a linked worktree
// ("gitdir: <common>/worktrees/<id>") to find its admin id.
func adminIDForPath(w Worktree) string {
	if w.IsMain {
		return "."
	}
	data, err := os.ReadFile(filepath.Join(w.Path, ".git"))
	if err == nil {
		line := strings.TrimSpace(string(data))
		if rest, ok := strings.CutPrefix(line, "gitdir:"); ok {
			return filepath.Base(strings.TrimSpace(rest))
		}
	}
	// Worktree directory missing (prunable) — fall back to its basename,
	// which is what git uses by default when creating the admin dir.
	return filepath.Base(w.Path)
}

// ParsePorcelain parses `git worktree list --porcelain` output. The first
// entry is always the main worktree.
func ParsePorcelain(out string) []Worktree {
	var (
		res []Worktree
		cur *Worktree
	)
	flush := func() {
		if cur != nil {
			res = append(res, *cur)
			cur = nil
		}
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			flush()
			continue
		}
		key, val, _ := strings.Cut(line, " ")
		switch key {
		case "worktree":
			flush()
			cur = &Worktree{Path: realpath(val), IsMain: len(res) == 0}
		case "HEAD":
			if cur != nil {
				cur.Head = val
			}
		case "branch":
			if cur != nil {
				cur.Branch = strings.TrimPrefix(val, "refs/heads/")
			}
		case "detached":
			if cur != nil {
				cur.Detached = true
			}
		case "bare":
			if cur != nil {
				cur.Bare = true
			}
		case "locked":
			if cur != nil {
				cur.Locked = true
			}
		case "prunable":
			if cur != nil {
				cur.Prunable = true
			}
		}
	}
	flush()
	return res
}
