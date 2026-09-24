package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/chryzxc/worktree-gateway/internal/api"
	"github.com/chryzxc/worktree-gateway/internal/config"
	"github.com/chryzxc/worktree-gateway/internal/gitwt"
	"github.com/chryzxc/worktree-gateway/internal/registry"
)

func upCmd() *cobra.Command {
	var path, name string
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Register the current worktree's identity and routes",
		Long: `Registers the worktree containing --path (default: cwd). Its hostname is
derived from the branch; --name (or WG_WORKTREE) pins a different one.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := pathArg(path)
			if err != nil {
				return err
			}
			if name == "" {
				name = os.Getenv("WG_WORKTREE")
			}
			c, err := client()
			if err != nil {
				return err
			}
			var v api.WorktreeView
			if err := c.Do("POST", "/v1/up", nil, api.UpRequest{Path: p, Name: name}, &v); err != nil {
				return err
			}
			printWorktree(v)
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "worktree path (default: cwd)")
	cmd.Flags().StringVar(&name, "name", "", "pin the worktree hostname label")
	return cmd
}

func downCmd() *cobra.Command {
	var path string
	var forget bool
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Remove the worktree's routes (--forget: drop its identity too)",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := pathArg(path)
			if err != nil {
				return err
			}
			c := api.NewClient(config.SocketPath())
			if !c.Ping() {
				return nil // nothing is routed when the daemon is not running
			}
			var out map[string]string
			if err := c.Do("POST", "/v1/down", nil, api.DownRequest{Path: p, Forget: forget}, &out); err != nil {
				return err
			}
			verb := "parked"
			if forget {
				verb = "forgot"
			}
			fmt.Printf("%s %s\n", verb, out["worktree"])
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "worktree path (default: cwd)")
	cmd.Flags().BoolVar(&forget, "forget", false, "remove the worktree identity (use before `git worktree remove`)")
	return cmd
}

func registerCmd() *cobra.Command {
	var path, host string
	var port, pid int
	cmd := &cobra.Command{
		Use:   "register <service>",
		Short: "Route a service that is already listening on a loopback port",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			p, err := pathArg(path)
			if err != nil {
				return err
			}
			c, err := client()
			if err != nil {
				return err
			}
			var r api.RegisterResponse
			req := api.RegisterRequest{Path: p, Service: args[0], Host: host, Port: port, PID: pid, Source: registry.SourceRegister}
			if err := c.Do("POST", "/v1/register", nil, req, &r); err != nil {
				return err
			}
			for _, e := range r.Evicted {
				fmt.Fprintf(os.Stderr, "replaced stale registration %s on the same port\n", e)
			}
			fmt.Printf("%s/%s → %s (%s)\n", r.Worktree.Slug, r.Service.Name, r.Service.Upstream, r.Service.Status)
			for _, u := range r.Service.URLs {
				fmt.Println("  " + u)
			}
			if r.Service.Conflict != "" {
				fmt.Fprintln(os.Stderr, "warning: hostname conflict:", r.Service.Conflict)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "worktree path (default: cwd)")
	cmd.Flags().IntVar(&port, "port", 0, "upstream port (required)")
	cmd.Flags().StringVar(&host, "host", "127.0.0.1", "upstream loopback address")
	cmd.Flags().IntVar(&pid, "pid", 0, "deregister automatically when this process exits")
	cmd.MarkFlagRequired("port")
	return cmd
}

func deregisterCmd() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "deregister <service>",
		Short: "Stop routing a service",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			p, err := pathArg(path)
			if err != nil {
				return err
			}
			c := api.NewClient(config.SocketPath())
			if !c.Ping() {
				return nil
			}
			return c.Do("POST", "/v1/deregister", nil, api.DeregisterRequest{Path: p, Service: args[0]}, nil)
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "worktree path (default: cwd)")
	return cmd
}

func statusCmd() *cobra.Command {
	var asJSON, all bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show worktrees, services, URLs and health",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			c := api.NewClient(config.SocketPath())
			var st api.Status
			if err := c.Do("GET", "/v1/status", nil, nil, &st); err != nil {
				if errors.Is(err, api.ErrDaemonDown) && !asJSON {
					fmt.Println("daemon not running (start with `wtg up` or `wtg daemon start`)")
					return nil
				}
				return err
			}
			if !all {
				// Default to the current project when run inside a repository.
				if wd, err := os.Getwd(); err == nil {
					if repo, err := gitwt.Resolve(context.Background(), wd); err == nil {
						pid := registry.ProjectID(repo.CommonDir)
						var keep []api.WorktreeView
						for _, w := range st.Worktrees {
							if strings.HasPrefix(w.ID, pid+":") {
								keep = append(keep, w)
							}
						}
						st.Worktrees = keep
					}
				}
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(st)
			}
			sort.Slice(st.Worktrees, func(i, j int) bool {
				a, b := st.Worktrees[i], st.Worktrees[j]
				if a.Project != b.Project {
					return a.Project < b.Project
				}
				return a.IsMain || (!b.IsMain && a.Slug < b.Slug)
			})
			if st.Tunnel.PublicURL != "" {
				fmt.Printf("tunnel: %s (%s, %s)\n\n", st.Tunnel.PublicURL, st.Tunnel.Provider, st.Tunnel.State)
			}
			if len(st.Worktrees) == 0 {
				fmt.Println("no worktrees registered (run `wtg up` in a worktree)")
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "WORKTREE\tBRANCH\tSERVICE\tSTATUS\tUPSTREAM\tURL")
			for _, w := range st.Worktrees {
				label := w.Slug
				if all {
					label = w.Slug + "." + w.Project
				}
				if w.IsMain {
					label += " (main)"
				}
				if w.Parked {
					fmt.Fprintf(tw, "%s\t%s\t-\tparked\t\t\n", label, w.Branch)
					continue
				}
				if len(w.Services) == 0 {
					fmt.Fprintf(tw, "%s\t%s\t-\tno services\t\t\n", label, w.Branch)
				}
				for i, s := range w.Services {
					u := ""
					if len(s.URLs) > 0 {
						u = s.URLs[0]
					}
					status := s.Status
					if s.Conflict != "" {
						status += " (conflict)"
					}
					l, b := label, w.Branch
					if i > 0 {
						l, b = "", ""
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", l, b, s.Name, status, s.Upstream, u)
				}
				if w.PublicURL != "" {
					fmt.Fprintf(tw, "\t\t\tpublic\t\t%s\n", w.PublicURL)
				}
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	cmd.Flags().BoolVarP(&all, "all", "a", false, "show all projects, not just the current one")
	return cmd
}

func printWorktree(v api.WorktreeView) {
	fmt.Printf("%s (%s) · project %s\n", v.Slug, v.Branch, v.Project)
	for _, s := range v.Services {
		u := ""
		if len(s.URLs) > 0 {
			u = s.URLs[0]
		}
		fmt.Printf("  %-10s %-9s %s\n", s.Name, s.Status, u)
	}
	if len(v.Services) == 0 {
		fmt.Printf("  no services yet: `wtg run <service> -- <cmd>` or `wtg register <service> --port N`\n")
	}
}

func portCmd() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "port [service]",
		Short: "Print a stable free port for a service in this worktree",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			p, err := pathArg(path)
			if err != nil {
				return err
			}
			svc, err := serviceArg(p, args)
			if err != nil {
				return err
			}
			c, err := client()
			if err != nil {
				return err
			}
			var r api.PortResponse
			if err := c.Do("POST", "/v1/port", nil, api.PortRequest{Path: p, Service: svc}, &r); err != nil {
				return err
			}
			fmt.Println(r.Port)
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "worktree path (default: cwd)")
	return cmd
}

func envCmd() *cobra.Command {
	var path string
	var port int
	cmd := &cobra.Command{
		Use:   "env [service]",
		Short: "Print identity variables as shell exports (eval \"$(wtg env)\")",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			p, err := pathArg(path)
			if err != nil {
				return err
			}
			svc := ""
			if len(args) == 1 {
				svc = args[0]
			}
			c, err := client()
			if err != nil {
				return err
			}
			var env map[string]string
			if err := c.Do("POST", "/v1/env", nil, api.EnvRequest{Path: p, Service: svc, Port: port}, &env); err != nil {
				return err
			}
			keys := make([]string, 0, len(env))
			for k := range env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Printf("export %s=%s\n", k, shellQuote(env[k]))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "worktree path (default: cwd)")
	cmd.Flags().IntVar(&port, "port", 0, "also export PORT")
	return cmd
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// serviceArg returns args[0] or the project's default service.
func serviceArg(path string, args []string) (string, error) {
	if len(args) == 1 {
		return args[0], nil
	}
	cfg, err := projectConfig(path)
	if err != nil {
		return "", err
	}
	return cfg.Default(), nil
}

func projectConfig(path string) (*config.Project, error) {
	repo, err := gitwt.Resolve(context.Background(), path)
	if err != nil {
		return nil, err
	}
	return config.LoadProject(repo.TopLevel)
}

func runCmd() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "run [service] [-- command...]",
		Short: "Run a dev server with an allocated PORT and identity env, routed while it lives",
		Long: `Allocates a stable port, exports PORT and the WG_* identity variables,
runs the command (or services.<name>.command from wtg.yaml), registers the
service while it runs and deregisters it on exit. Exits with the command's
status. wtg is not a supervisor: it does not restart the command.`,
		Example: `  wtg run web -- npm run dev
  wtg run api -- go run ./cmd/api
  wtg run            # default service, command from wtg.yaml`,
		RunE: func(cmd *cobra.Command, args []string) error {
			var svcArgs, command []string
			if at := cmd.ArgsLenAtDash(); at >= 0 {
				svcArgs, command = args[:at], args[at:]
			} else {
				svcArgs = args
			}
			if len(svcArgs) > 1 {
				return errors.New("usage: wtg run [service] [-- command...]")
			}
			p, err := pathArg(path)
			if err != nil {
				return err
			}
			cfg, err := projectConfig(p)
			if err != nil {
				return err
			}
			svc := cfg.Default()
			if len(svcArgs) == 1 {
				svc = svcArgs[0]
			}
			if len(command) == 0 {
				sc, ok := cfg.Services[svc]
				if !ok || sc.Command == "" {
					return fmt.Errorf("no command: pass one after -- or set services.%s.command in wtg.yaml", svc)
				}
				command = []string{sc.Command}
			}
			c, err := client()
			if err != nil {
				return err
			}
			var pr api.PortResponse
			if err := c.Do("POST", "/v1/port", nil, api.PortRequest{Path: p, Service: svc}, &pr); err != nil {
				return err
			}
			var env map[string]string
			if err := c.Do("POST", "/v1/env", nil, api.EnvRequest{Path: p, Service: svc, Port: pr.Port}, &env); err != nil {
				return err
			}
			return runService(c, p, svc, pr.Port, env, command)
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "worktree path (default: cwd)")
	return cmd
}

func runService(c *api.Client, path, svc string, port int, env map[string]string, command []string) error {
	var child *exec.Cmd
	if len(command) == 1 {
		// A single string is a shell command line (as in wtg.yaml).
		if runtime.GOOS == "windows" {
			child = exec.Command("cmd", "/C", command[0])
		} else {
			child = exec.Command("sh", "-c", command[0])
		}
	} else {
		child = exec.Command(command[0], command[1:]...)
	}
	child.Dir = path
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	child.Env = os.Environ()
	for k, v := range env {
		child.Env = append(child.Env, k+"="+v)
	}
	// Ignore SIGINT ourselves: the child gets it from the terminal and we
	// wait for it to exit so we can deregister.
	signal.Ignore(os.Interrupt)
	sigs := make(chan os.Signal, 1)
	if len(forwardedSignals) > 0 {
		signal.Notify(sigs, forwardedSignals...)
	}
	if err := child.Start(); err != nil {
		return err
	}
	go func() {
		for s := range sigs {
			child.Process.Signal(s)
		}
	}()
	pid := child.Process.Pid
	req := api.RegisterRequest{Path: path, Service: svc, Port: port, PID: pid, Source: registry.SourceRun}
	var r api.RegisterResponse
	if err := c.Do("POST", "/v1/register", nil, req, &r); err != nil {
		fmt.Fprintf(os.Stderr, "wtg: warning: could not register %s: %v\n", svc, err)
	} else if len(r.Service.URLs) > 0 {
		fmt.Fprintf(os.Stderr, "wtg: %s/%s on port %d → %s\n", r.Worktree.Slug, svc, port, r.Service.URLs[0])
	}
	err := child.Wait()
	signal.Stop(sigs)
	c.Do("POST", "/v1/deregister", nil, api.DeregisterRequest{Path: path, Service: svc, PID: pid}, nil)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code := ee.ExitCode()
		if code < 0 {
			code = 1
		}
		return &exitError{code}
	}
	return err
}
