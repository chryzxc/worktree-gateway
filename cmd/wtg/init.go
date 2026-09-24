package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/chryzxc/worktree-gateway/internal/api"
	"github.com/chryzxc/worktree-gateway/internal/config"
	"github.com/chryzxc/worktree-gateway/internal/gitwt"
	"github.com/chryzxc/worktree-gateway/internal/slug"
)

func initCmd() *cobra.Command {
	var force, worktrunk bool
	var command string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a starter wtg.yaml in this repository",
		Long: `Writes wtg.yaml at the root of the current worktree with one "web" service.
The dev command is guessed from the project (package.json, go.mod, …); override
it with --command. Commit the file so every worktree shares it.`,
		Example: `  wtg init
  wtg init --command "bin/rails server -p $PORT" --worktrunk`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			repo, err := gitwt.Resolve(context.Background(), wd)
			if err != nil {
				return err
			}
			if existing, _ := config.LoadProject(repo.TopLevel); existing != nil && existing.Path() != "" && !force {
				return fmt.Errorf("%s already exists (use --force to overwrite)", existing.Path())
			}
			if command == "" {
				command = guessCommand(repo.TopLevel)
			}
			name := slug.Sanitize(filepath.Base(repo.MainPath))
			if name == "" {
				name = "app"
			}
			file := filepath.Join(repo.TopLevel, "wtg.yaml")
			content := starterConfig(name, command)
			if _, err := config.ParseProject([]byte(content)); err != nil {
				return fmt.Errorf("generated config is invalid (%v); pass --command", err)
			}
			if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
				return err
			}
			fmt.Println("wrote", file)
			if worktrunk {
				if err := writeWorktrunkHooks(repo.TopLevel); err != nil {
					return err
				}
			}
			fmt.Printf(`
next:
  wtg run            start the web service with $PORT and a stable URL
  wtg open           open it in the browser
  wtg trust          (once) trust the local HTTPS certificate
`)
			if !worktrunk {
				fmt.Println("  wtg hooks worktrunk --write   register new Worktrunk worktrees automatically")
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "overwrite an existing wtg.yaml")
	cmd.Flags().BoolVar(&worktrunk, "worktrunk", false, "also add Worktrunk hooks to .config/wt.toml")
	cmd.Flags().StringVar(&command, "command", "", "dev server command (receives $PORT)")
	return cmd
}

// guessCommand picks a dev command from common project markers.
func guessCommand(dir string) string {
	has := func(f string) bool { _, err := os.Stat(filepath.Join(dir, f)); return err == nil }
	switch {
	case has("package.json"):
		pm := "npm"
		switch {
		case has("pnpm-lock.yaml"):
			pm = "pnpm"
		case has("yarn.lock"):
			pm = "yarn"
		case has("bun.lockb"), has("bun.lock"):
			pm = "bun"
		}
		return pm + " run dev"
	case has("manage.py"):
		return "python manage.py runserver 127.0.0.1:$PORT"
	case has("bin/rails"):
		return "bin/rails server -p $PORT"
	case has("mix.exs"):
		return "mix phx.server"
	case has("go.mod"):
		return "go run ."
	case has("Cargo.toml"):
		return "cargo run"
	}
	return ""
}

func starterConfig(project, command string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `# Worktree Gateway: https://github.com/chryzxc/worktree-gateway
# Every worktree of this repo gets https://{worktree}.%s.localhost:8743
# Reference: docs/configuration.md

project: %s

services:
  web:
`, project, project)
	if command != "" {
		fmt.Fprintf(&b, "    command: %s   # must listen on $PORT\n", yamlScalar(command))
	} else {
		b.WriteString("    # command: npm run dev   # used by `wtg run`; must listen on $PORT\n")
	}
	b.WriteString(`    # health: /healthz
    # public:                  # reachable through ` + "`wtg tunnel start`" + `, only these paths
    #   paths: [/webhooks]
    # oauth_callbacks: [/auth/callback]

  # api:
  #   command: go run ./cmd/api
  #   # → https://api.{worktree}.` + project + `.localhost:8743
`)
	return b.String()
}

func yamlScalar(s string) string {
	if strings.ContainsAny(s, ":#{}[]&*!|>'\"%@`") || strings.HasPrefix(s, "-") {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return s
}

func openCmd() *cobra.Command {
	var path string
	var printOnly bool
	cmd := &cobra.Command{
		Use:   "open [service]",
		Short: "Open this worktree's URL in the browser",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			p, err := pathArg(path)
			if err != nil {
				return err
			}
			c, err := client()
			if err != nil {
				return err
			}
			var v api.WorktreeView
			if err := c.Do("POST", "/v1/up", nil, api.UpRequest{Path: p}, &v); err != nil {
				return err
			}
			var svc *api.ServiceView
			for i := range v.Services {
				s := &v.Services[i]
				if (len(args) == 1 && s.Name == args[0]) || (len(args) == 0 && svc == nil) {
					svc = s
				}
			}
			if len(args) == 0 {
				if cfg, err := projectConfig(p); err == nil {
					for i := range v.Services {
						if v.Services[i].Name == cfg.Default() {
							svc = &v.Services[i]
						}
					}
				}
			}
			if svc == nil || len(svc.URLs) == 0 {
				return errors.New("no such service in this worktree (see `wtg status`)")
			}
			u := svc.URLs[0]
			if svc.Status != "up" {
				state := svc.Status
				if state == "missing" {
					state = "not running"
				}
				fmt.Fprintf(os.Stderr, "note: %s is %s; start it with `wtg run %s`\n", svc.Name, state, svc.Name)
			}
			if printOnly {
				fmt.Println(u)
				return nil
			}
			fmt.Println(u)
			return openBrowser(u)
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "worktree path (default: cwd)")
	cmd.Flags().BoolVarP(&printOnly, "print", "p", false, "print the URL instead of opening it")
	return cmd
}

func openBrowser(u string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", u).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	default:
		return exec.Command("xdg-open", u).Start()
	}
}
