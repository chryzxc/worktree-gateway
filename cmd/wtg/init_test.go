package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chryzxc/worktree-gateway/internal/config"
)

func TestStarterConfigParses(t *testing.T) {
	for _, cmd := range []string{"", "npm run dev", "python manage.py runserver 127.0.0.1:$PORT",
		"bin/rails server -p $PORT", `node -e "x: 1" # it's`, "--weird"} {
		cfg, err := config.ParseProject([]byte(starterConfig("shop", cmd)))
		if err != nil {
			t.Fatalf("%q: %v", cmd, err)
		}
		if cfg.Project != "shop" || cfg.Services["web"].Command != cmd {
			t.Fatalf("%q: got %+v", cmd, cfg.Services["web"])
		}
	}
}

func TestGuessCommand(t *testing.T) {
	dir := t.TempDir()
	if got := guessCommand(dir); got != "" {
		t.Fatalf("empty dir: %q", got)
	}
	os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(dir, "pnpm-lock.yaml"), nil, 0o644)
	if got := guessCommand(dir); got != "pnpm run dev" {
		t.Fatalf("pnpm: %q", got)
	}
}
