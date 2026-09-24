package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/spf13/cobra"

	"github.com/chryzxc/worktree-gateway/internal/api"
	"github.com/chryzxc/worktree-gateway/internal/config"
	"github.com/chryzxc/worktree-gateway/internal/daemon"
)

func daemonCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "daemon", Short: "Manage the gateway daemon (normally auto-started)"}
	cmd.AddCommand(
		&cobra.Command{
			Use: "run", Short: "Run the daemon in the foreground",
			RunE: func(cmd *cobra.Command, _ []string) error {
				g, err := config.LoadGlobal(config.GlobalConfigPath())
				if err != nil {
					return err
				}
				d, err := daemon.New(daemon.Options{
					StateDir: config.StateDir(), SocketPath: config.SocketPath(), Global: g,
					Logger: log.New(os.Stderr, "", log.LstdFlags),
				})
				if err != nil {
					return err
				}
				ctx, stop := signal.NotifyContext(context.Background(), interruptSignals...)
				defer stop()
				return d.Run(ctx)
			},
		},
		&cobra.Command{
			Use: "start", Short: "Start the daemon in the background",
			RunE: func(*cobra.Command, []string) error {
				if api.NewClient(config.SocketPath()).Ping() {
					fmt.Println("daemon already running")
					return nil
				}
				if err := startDaemon(); err != nil {
					return err
				}
				fmt.Println("daemon started")
				return nil
			},
		},
		&cobra.Command{
			Use: "stop", Short: "Stop the daemon (routes and tunnel go away)",
			RunE: func(*cobra.Command, []string) error {
				c := api.NewClient(config.SocketPath())
				if !c.Ping() {
					fmt.Println("daemon not running")
					return nil
				}
				if err := c.Do("POST", "/v1/shutdown", nil, nil, nil); err != nil {
					return err
				}
				for i := 0; i < 100 && c.Ping(); i++ {
					time.Sleep(50 * time.Millisecond)
				}
				fmt.Println("daemon stopped")
				return nil
			},
		},
		&cobra.Command{
			Use: "status", Short: "Show daemon information",
			RunE: func(*cobra.Command, []string) error {
				c := api.NewClient(config.SocketPath())
				var st api.Status
				if err := c.Do("GET", "/v1/status", nil, nil, &st); err != nil {
					return err
				}
				fmt.Printf("pid        %d (since %s)\nversion    %s\nstate      %s\nproxy      %s\nhttp       %s\n",
					st.PID, st.Started.Format(time.RFC3339), st.Version, st.StateDir, st.Proxy, st.HTTP)
				if st.HTTPS != "" {
					fmt.Printf("https      %s\n", st.HTTPS)
				}
				fmt.Printf("ingress    %s\ntunnel     %s %s\n", st.Ingress, st.Tunnel.State, st.Tunnel.PublicURL)
				if st.ProxyError != "" {
					fmt.Printf("proxy err  %s\n", st.ProxyError)
				}
				return nil
			},
		},
	)
	return cmd
}
