package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/chryzxc/worktree-gateway/internal/api"
	"github.com/chryzxc/worktree-gateway/internal/capture"
	"github.com/chryzxc/worktree-gateway/internal/config"
	"github.com/chryzxc/worktree-gateway/internal/ingress"
	"github.com/chryzxc/worktree-gateway/internal/tunnel"
)

func tunnelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tunnel",
		Short: "Expose allowlisted public paths through a tunnel (opt-in)",
		Long: `Starts a tunnel (cloudflared, ngrok or an external one you run) pointed at
the gateway ingress. Only paths listed under services.<name>.public.paths are
reachable, as /w/<worktree>/<path> or <worktree>.<public_domain>/<path>.
Nothing is reachable from outside until this is started.`,
	}
	var provider string
	start := &cobra.Command{
		Use: "start", Short: "Start the tunnel", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var st tunnel.Status
			if err := c.Do("POST", "/v1/tunnel/start", nil, api.TunnelRequest{Provider: provider}, &st); err != nil {
				return err
			}
			printTunnel(st)
			if st.PublicURL == "" {
				fmt.Println("waiting for a public URL; check `wtg tunnel status`")
			}
			return nil
		},
	}
	start.Flags().StringVar(&provider, "provider", "", "cloudflared | ngrok | external (default from config)")
	cmd.AddCommand(start,
		&cobra.Command{
			Use: "stop", Short: "Stop the tunnel", Args: cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error {
				c := api.NewClient(config.SocketPath())
				if !c.Ping() {
					return nil
				}
				var st tunnel.Status
				if err := c.Do("POST", "/v1/tunnel/stop", nil, nil, &st); err != nil {
					return err
				}
				printTunnel(st)
				return nil
			},
		},
		&cobra.Command{
			Use: "status", Short: "Show the tunnel state", Args: cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error {
				c := api.NewClient(config.SocketPath())
				var st tunnel.Status
				if err := c.Do("GET", "/v1/tunnel", nil, nil, &st); err != nil {
					return err
				}
				printTunnel(st)
				return nil
			},
		},
	)
	return cmd
}

func printTunnel(st tunnel.Status) {
	fmt.Printf("provider  %s\nstate     %s\n", st.Provider, st.State)
	if st.PublicURL != "" {
		fmt.Printf("public    %s\n", st.PublicURL)
	}
	if st.LocalURL != "" {
		fmt.Printf("ingress   %s\n", st.LocalURL)
	}
	if st.Restarts > 0 {
		fmt.Printf("restarts  %d\n", st.Restarts)
	}
	if st.LastError != "" {
		fmt.Printf("error     %s\n", st.LastError)
	}
}

func requestsCmd() *cobra.Command {
	var wt string
	var limit int
	var asJSON, clear bool
	cmd := &cobra.Command{
		Use:   "requests [id]",
		Short: "List captured public requests, or show one",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := api.NewClient(config.SocketPath())
			if clear {
				return c.Do("DELETE", "/v1/requests", nil, nil, nil)
			}
			if len(args) == 1 {
				var e capture.Entry
				if err := c.Do("GET", "/v1/requests/"+url.PathEscape(args[0]), nil, nil, &e); err != nil {
					return err
				}
				if asJSON {
					return json.NewEncoder(os.Stdout).Encode(e)
				}
				printEntry(e)
				return nil
			}
			q := url.Values{}
			if wt != "" {
				q.Set("worktree", wt)
			}
			q.Set("limit", strconv.Itoa(limit))
			var list api.RequestList
			if err := c.Do("GET", "/v1/requests", q, nil, &list); err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(os.Stdout).Encode(list)
			}
			if len(list.Requests) == 0 {
				fmt.Println("no captured requests")
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tTIME\tWORKTREE\tSOURCE\tMETHOD\tPATH\tSTATUS\tMS")
			for _, e := range list.Requests {
				src := e.Source
				if e.ReplayOf != "" {
					src = "replay"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\n", e.ID, e.Time.Local().Format("15:04:05"),
					e.Worktree+"/"+e.Service, src, e.Method, e.Path, e.Status, e.DurationMS)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVarP(&wt, "worktree", "w", "", "filter by worktree")
	cmd.Flags().IntVarP(&limit, "limit", "n", 50, "maximum entries")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	cmd.Flags().BoolVar(&clear, "clear", false, "delete all captured requests")
	return cmd
}

func printEntry(e capture.Entry) {
	fmt.Printf("%s  %s  %s/%s  via %s\n", e.ID, e.Time.Local().Format(time.RFC3339), e.Worktree, e.Service, e.Via)
	if e.ReplayOf != "" {
		fmt.Printf("replay of %s\n", e.ReplayOf)
	}
	q := ""
	if e.RawQuery != "" {
		q = "?" + e.RawQuery
	}
	fmt.Printf("\n%s %s%s\nHost: %s\n", e.Method, e.Path, q, e.Host)
	for k, vs := range e.Header {
		for _, v := range vs {
			fmt.Printf("%s: %s\n", k, v)
		}
	}
	fmt.Println()
	switch {
	case !e.BodyCaptured:
		fmt.Printf("[body not captured, %d bytes]\n", e.BodySize)
	case len(e.Body) > 0:
		fmt.Println(string(e.Body))
		if e.BodyTruncated {
			fmt.Printf("[truncated, %d bytes total]\n", e.BodySize)
		}
	}
	fmt.Printf("\n→ %d in %dms", e.Status, e.DurationMS)
	if e.Error != "" {
		fmt.Printf(" (%s)", e.Error)
	}
	fmt.Println()
	if len(e.Redactions) > 0 {
		fmt.Printf("redacted: %s\n", strings.Join(e.Redactions, ", "))
	}
}

func replayCmd() *cobra.Command {
	var to string
	var yes bool
	cmd := &cobra.Command{
		Use:   "replay <id>",
		Short: "Re-send a captured request to its worktree (or --to another)",
		Long: `Replays a captured request with its original method, path, headers and
body. Replays carry X-Wg-Replay: 1 and X-Wg-Replay-Of: <id>. Redacted headers
are not re-sent, so signature checks may need a test-mode bypass.
Non-idempotent methods (POST, PATCH, DELETE…) require --yes.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := api.NewClient(config.SocketPath())
			var r ingress.ReplayResult
			if err := c.Do("POST", "/v1/requests/"+url.PathEscape(args[0])+"/replay", nil, api.ReplayRequest{To: to, Yes: yes}, &r); err != nil {
				return err
			}
			fmt.Printf("%s → %s/%s: %d", r.ID, r.Worktree, r.Service, r.Status)
			if r.Error != "" {
				fmt.Printf(" (%s)", r.Error)
			}
			fmt.Println()
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "target worktree (slug or slug.project)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "confirm replaying a non-idempotent request")
	return cmd
}

func oauthCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "oauth", Short: "OAuth callback routing helpers"}
	var path, svc, cb, inner string
	var ttl time.Duration
	state := &cobra.Command{
		Use:   "state",
		Short: "Mint a signed state value routing a provider callback to this worktree",
		Long: `Prints a signed state token and the shared redirect_uri to register with the
provider. After login, the gateway verifies the signature, expiry and single
use of the state, then redirects the browser to <worktree URL><callback path>
with the original state restored. Apps can also mint states themselves with
WG_OAUTH_STATE_KEY (see README).`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := pathArg(path)
			if err != nil {
				return err
			}
			if svc == "" {
				if svc, err = serviceArg(p, nil); err != nil {
					return err
				}
			}
			c, err := client()
			if err != nil {
				return err
			}
			var r api.OAuthStateResponse
			req := api.OAuthStateRequest{Path: p, Service: svc, CallbackPath: cb, State: inner, TTL: ttl}
			if err := c.Do("POST", "/v1/oauth/state", nil, req, &r); err != nil {
				return err
			}
			fmt.Printf("state         %s\nredirect_uri  %s\ndelivers to   %s\n", r.State, r.RedirectURI, r.Destination)
			return nil
		},
	}
	state.Flags().StringVar(&path, "path", "", "worktree path (default: cwd)")
	state.Flags().StringVar(&svc, "service", "", "service (default: project default)")
	state.Flags().StringVar(&cb, "callback", "", "callback path listed in oauth_callbacks (required)")
	state.Flags().StringVar(&inner, "state", "", "the app's own state value to restore")
	state.Flags().DurationVar(&ttl, "ttl", 0, "lifetime (default from config)")
	state.MarkFlagRequired("callback")
	cmd.AddCommand(state)
	return cmd
}
