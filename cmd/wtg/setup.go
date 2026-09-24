package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/smallstep/truststore"
	"github.com/spf13/cobra"

	"github.com/chryzxc/worktree-gateway/internal/api"
	"github.com/chryzxc/worktree-gateway/internal/config"
	"github.com/chryzxc/worktree-gateway/internal/gitwt"
	"github.com/chryzxc/worktree-gateway/internal/proxy"
)

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use: "doctor", Short: "Check the environment and configuration", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			problems := 0
			check := func(ok bool, label, detail string) {
				mark := "ok  "
				if !ok {
					mark = "FAIL"
					problems++
				}
				fmt.Printf("[%s] %s", mark, label)
				if detail != "" {
					fmt.Printf(" — %s", detail)
				}
				fmt.Println()
			}
			info := func(label, detail string) { fmt.Printf("[info] %s — %s\n", label, detail) }

			out, err := exec.Command("git", "version").Output()
			check(err == nil, "git", strings.TrimSpace(string(out)))

			g, gerr := config.LoadGlobal(config.GlobalConfigPath())
			check(gerr == nil, "global config "+config.GlobalConfigPath(), errString(gerr))
			if gerr != nil {
				g = config.DefaultGlobal()
			}

			addrs, err := net.LookupHost("wtg-doctor.example.localhost")
			loop := err == nil && len(addrs) > 0
			for _, a := range addrs {
				loop = loop && net.ParseIP(a).IsLoopback()
			}
			check(loop, "*.localhost resolves to loopback", fmt.Sprint(addrs, " ", errString(err)))

			c := api.NewClient(config.SocketPath())
			var st api.Status
			running := c.Do("GET", "/v1/status", nil, nil, &st) == nil
			if running {
				check(st.ProxyError == "", "daemon running (pid "+strconv.Itoa(st.PID)+")", st.ProxyError)
			} else {
				info("daemon", "not running; starts automatically on `wtg up`")
				for _, p := range []int{g.HTTPPort, g.HTTPSPort, g.Ingress.Port} {
					l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
					if err == nil {
						l.Close()
					}
					check(err == nil, fmt.Sprintf("port %d free", p), errString(err))
				}
			}

			if g.HTTPS {
				root := proxy.CARootPath(config.StateDir())
				if _, err := os.Stat(root); err != nil {
					info("local CA", "not created yet (created on first HTTPS request)")
				} else {
					info("local CA", root+" — run `wtg trust` once to trust it")
				}
			}

			for _, bin := range []string{"cloudflared", "ngrok", "wt"} {
				if p, err := exec.LookPath(bin); err == nil {
					info(bin, p)
				} else {
					info(bin, "not installed")
				}
			}

			if wd, err := os.Getwd(); err == nil {
				if repo, err := gitwt.Resolve(context.Background(), wd); err == nil {
					cfg, err := config.LoadProject(repo.TopLevel)
					name := "(no wtg.yaml; defaults)"
					if cfg != nil && cfg.Path() != "" {
						name = cfg.Path()
					}
					check(err == nil, "project config "+name, errString(err))
				}
			}
			if problems > 0 {
				return fmt.Errorf("%d problem(s) found", problems)
			}
			return nil
		},
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func trustCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "trust",
		Short: "Trust the gateway's local CA (explicit, may prompt for your password)",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			root, err := ensureCA()
			if err != nil {
				return err
			}
			if err := truststore.InstallFile(root, truststore.WithFirefox(), truststore.WithJava()); err != nil {
				return fmt.Errorf("installing %s: %w", root, err)
			}
			fmt.Println("trusted", root)
			return nil
		},
	}
}

func untrustCmd() *cobra.Command {
	return &cobra.Command{
		Use: "untrust", Short: "Remove the gateway's local CA from trust stores", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			root := proxy.CARootPath(config.StateDir())
			if _, err := os.Stat(root); err != nil {
				return errors.New("no local CA found")
			}
			if err := truststore.UninstallFile(root, truststore.WithFirefox(), truststore.WithJava()); err != nil {
				return err
			}
			fmt.Println("untrusted", root)
			return nil
		},
	}
}

// ensureCA makes the embedded Caddy create its CA by starting the daemon,
// then returns the root certificate path.
func ensureCA() (string, error) {
	root := proxy.CARootPath(config.StateDir())
	if _, err := os.Stat(root); err == nil {
		return root, nil
	}
	c, err := client()
	if err != nil {
		return "", err
	}
	var st api.Status
	if err := c.Do("GET", "/v1/status", nil, nil, &st); err != nil {
		return "", err
	}
	if st.HTTPS == "" {
		return "", errors.New("https is disabled in the global config")
	}
	if st.Proxy != "embedded" {
		return "", errors.New("the local CA belongs to your external Caddy; use `caddy trust`")
	}
	// Caddy creates the CA when the TLS app provisions; poke it with a request.
	conn, err := net.Dial("tcp", st.HTTPS)
	if err == nil {
		conn.Close()
	}
	if _, err := os.Stat(root); err != nil {
		return "", fmt.Errorf("CA root not found at %s", root)
	}
	return root, nil
}

func hostsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "hosts",
		Short: "Print /etc/hosts lines for tools that do not resolve *.localhost",
		Long: `Browsers and most resolvers map *.localhost to loopback (RFC 6761). For
tools that do not, append this output to /etc/hosts yourself; wtg never
edits system files.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			c := api.NewClient(config.SocketPath())
			var st api.Status
			if err := c.Do("GET", "/v1/status", nil, nil, &st); err != nil {
				return err
			}
			seen := map[string]bool{}
			var hosts []string
			for _, w := range st.Worktrees {
				for _, s := range w.Services {
					for _, u := range s.URLs {
						h := strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
						if host, _, err := net.SplitHostPort(h); err == nil {
							h = host
						}
						if !seen[h] {
							seen[h] = true
							hosts = append(hosts, h)
						}
					}
				}
			}
			sort.Strings(hosts)
			fmt.Println("# worktree-gateway")
			for _, h := range hosts {
				fmt.Printf("127.0.0.1 %s\n", h)
			}
			return nil
		},
	}
}

const worktrunkHooks = `[post-start]
gateway = "wtg up --path {{ worktree_path }}"

[pre-remove]
gateway = "wtg down --path {{ worktree_path }} --forget"
`

func hooksCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "hooks", Short: "Integrations with worktree managers"}
	var write bool
	wt := &cobra.Command{
		Use:   "worktrunk",
		Short: "Print (or --write) Worktrunk hooks to .config/wt.toml",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if !write {
				fmt.Print(worktrunkHooks)
				return nil
			}
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			repo, err := gitwt.Resolve(context.Background(), wd)
			if err != nil {
				return err
			}
			file := filepath.Join(repo.TopLevel, ".config", "wt.toml")
			existing, err := os.ReadFile(file)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			s := string(existing)
			if strings.Contains(s, "wtg up") {
				fmt.Println(file, "already contains wtg hooks")
				return nil
			}
			if strings.Contains(s, "[post-start]") || strings.Contains(s, "[pre-remove]") {
				return fmt.Errorf("%s already has [post-start]/[pre-remove] sections; add these entries manually:\n\n%s", file, worktrunkHooks)
			}
			if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
				return err
			}
			if s != "" && !strings.HasSuffix(s, "\n") {
				s += "\n"
			}
			if s != "" {
				s += "\n"
			}
			if err := os.WriteFile(file, []byte(s+worktrunkHooks), 0o644); err != nil {
				return err
			}
			fmt.Println("wrote", file)
			return nil
		},
	}
	wt.Flags().BoolVar(&write, "write", false, "append the hooks to the repository's .config/wt.toml")
	cmd.AddCommand(wt)
	return cmd
}
