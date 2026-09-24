// Command wtg is the Worktree Gateway CLI and daemon.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/chryzxc/worktree-gateway/internal/api"
	"github.com/chryzxc/worktree-gateway/internal/config"
	"github.com/chryzxc/worktree-gateway/internal/version"
)

func main() {
	root := &cobra.Command{
		Use:   "wtg",
		Short: "Stable runtime identity and routing for Git worktrees",
		Long: `Worktree Gateway gives every Git worktree a stable hostname and routes
local and (opt-in) external traffic to that worktree's services.

It does not create worktrees (use Worktrunk or git), run your processes
(use your dev server) or implement a proxy (it drives Caddy).`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Version,
	}
	root.AddCommand(
		initCmd(), openCmd(), daemonCmd(), upCmd(), downCmd(), registerCmd(), deregisterCmd(), statusCmd(),
		portCmd(), envCmd(), runCmd(), tunnelCmd(), requestsCmd(), replayCmd(), oauthCmd(),
		doctorCmd(), trustCmd(), untrustCmd(), hostsCmd(), hooksCmd(), versionCmd(),
	)
	if err := root.Execute(); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			os.Exit(ee.code) // the child already reported its own failure
		}
		fmt.Fprintln(os.Stderr, "wtg:", err)
		os.Exit(1)
	}
}

type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func versionCmd() *cobra.Command {
	return &cobra.Command{Use: "version", Short: "Print the version", Run: func(*cobra.Command, []string) {
		fmt.Println("wtg", version.Version)
	}}
}

// client returns an API client, starting the daemon if needed.
func client() (*api.Client, error) {
	c := api.NewClient(config.SocketPath())
	if c.Ping() {
		return c, nil
	}
	if os.Getenv("WTG_NO_AUTOSTART") != "" {
		return nil, api.ErrDaemonDown
	}
	if err := startDaemon(); err != nil {
		return nil, err
	}
	return c, nil
}

// startDaemon launches `wtg daemon run` detached and waits for its socket.
func startDaemon() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	state := config.StateDir()
	if err := os.MkdirAll(state, 0o700); err != nil {
		return err
	}
	logPath := filepath.Join(state, "daemon.log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(self, "daemon", "run")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = detachAttr()
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	c := api.NewClient(config.SocketPath())
	deadline := time.After(15 * time.Second)
	for {
		select {
		case err := <-exited:
			return fmt.Errorf("daemon exited during startup (%v); see %s", err, logPath)
		case <-deadline:
			return fmt.Errorf("daemon did not start within 15s; see %s", logPath)
		case <-time.After(50 * time.Millisecond):
			if c.Ping() {
				cmd.Process.Release()
				return nil
			}
		}
	}
}

// pathArg resolves --path (default: the working directory).
func pathArg(p string) (string, error) {
	if p == "" {
		return os.Getwd()
	}
	return filepath.Abs(p)
}
