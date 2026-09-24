// Package proxy turns the routing table into Caddy configuration. The
// gateway never implements local HTTP proxying itself: Caddy does.
package proxy

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/chryzxc/worktree-gateway/internal/config"
	"github.com/chryzxc/worktree-gateway/internal/registry"
)

// Options controls config generation.
type Options struct {
	Global   config.Global
	StateDir string
	// Embedded adds process-local settings (storage, logging, admin off)
	// that must not be pushed into a user's external Caddy.
	Embedded bool
}

type obj = map[string]any

// CARootPath is where Caddy's internal CA root certificate lives.
func CARootPath(stateDir string) string {
	return filepath.Join(stateDir, "caddy", "pki", "authorities", "local", "root.crt")
}

// BuildConfig renders a complete Caddy JSON config for the snapshot.
func BuildConfig(snap registry.Snapshot, opt Options) ([]byte, error) {
	g := opt.Global
	routes := snap.Routes()

	var hostRoutes []any
	var hosts []string
	seen := map[string]bool{}
	for _, rt := range routes {
		if rt.Conflict != "" || seen[rt.Hostname] {
			continue
		}
		seen[rt.Hostname] = true
		hosts = append(hosts, rt.Hostname)
		hostRoutes = append(hostRoutes, routeFor(rt))
	}
	// Project apex domains serve the stable OAuth callback even when no
	// service claims them, so they need certificates too.
	for _, p := range snap.Projects {
		if !seen[p.Domain] {
			seen[p.Domain] = true
			hosts = append(hosts, p.Domain)
		}
	}
	sort.Strings(hosts)

	// Stable OAuth callback endpoint on every gateway hostname; handled by
	// the wtg ingress so the same signed-state logic serves local and public.
	callback := obj{
		"match": []any{obj{"path": []string{"/_wg/oauth/*"}}},
		"handle": []any{obj{
			"handler":   "reverse_proxy",
			"upstreams": []any{obj{"dial": net.JoinHostPort(loopback(g.ListenHost), strconv.Itoa(g.Ingress.Port))}},
			"headers": obj{"request": obj{"set": obj{
				"X-Forwarded-Proto": []string{"{http.request.scheme}"},
				"X-Wg-Via":          []string{"local"},
			}}},
		}},
		"terminal": true,
	}

	fallback := obj{
		"handle": []any{staticPage(404, "Unknown gateway host",
			"No worktree service is registered for {http.request.host}.\nRun `wtg status` to list routes.", nil)},
	}

	all := append([]any{callback}, hostRoutes...)
	all = append(all, fallback)

	errorRoutes := []any{obj{
		"handle": []any{staticPage(502, "Service unreachable",
			"The gateway could not reach the service behind {http.request.host} ({http.error.status_code}).\n"+
				"It may be restarting. Check `wtg status`.", nil)},
	}}

	listen := func(port int) []string {
		return []string{net.JoinHostPort(loopback(g.ListenHost), strconv.Itoa(port))}
	}
	servers := obj{
		"wtg_http": obj{
			"listen":          listen(g.HTTPPort),
			"routes":          all,
			"errors":          obj{"routes": errorRoutes},
			"automatic_https": obj{"disable": true},
		},
	}
	httpApp := obj{"http_port": g.HTTPPort, "servers": servers}
	apps := obj{"http": httpApp}

	if g.HTTPS {
		httpApp["https_port"] = g.HTTPSPort
		servers["wtg_https"] = obj{
			"listen":                  listen(g.HTTPSPort),
			"routes":                  all,
			"errors":                  obj{"routes": errorRoutes},
			"tls_connection_policies": []any{obj{}},
			"automatic_https":         obj{"disable_redirects": true},
		}
		policy := obj{"issuers": []any{obj{"module": "internal"}}}
		if len(hosts) > 0 {
			policy["subjects"] = hosts
		}
		apps["tls"] = obj{"automation": obj{"policies": []any{policy}}}
		apps["pki"] = obj{"certificate_authorities": obj{"local": obj{
			"name":          "Worktree Gateway Local CA",
			"install_trust": false,
		}}}
	}

	cfg := obj{"apps": apps}
	if opt.Embedded {
		cfg["admin"] = obj{"disabled": true}
		cfg["storage"] = obj{"module": "file_system", "root": filepath.Join(opt.StateDir, "caddy")}
		cfg["logging"] = obj{"logs": obj{"default": obj{
			"level":  "WARN",
			"writer": obj{"output": "file", "filename": filepath.Join(opt.StateDir, "caddy.log")},
		}}}
	}
	return json.MarshalIndent(cfg, "", "  ")
}

func loopback(h string) string {
	if h == "localhost" || h == "" {
		return "127.0.0.1"
	}
	return h
}

func routeFor(rt registry.Route) any {
	match := []any{obj{"host": []string{rt.Hostname}}}
	ident := obj{
		"handler": "headers",
		"response": obj{"set": obj{
			"X-Wg-Worktree": []string{rt.Worktree},
			"X-Wg-Service":  []string{rt.Service},
		}},
	}
	var handle []any
	switch rt.Status {
	case registry.StatusUp, registry.StatusStarting:
		handle = []any{ident, obj{
			"handler":        "reverse_proxy",
			"upstreams":      []any{obj{"dial": rt.Upstream}},
			"flush_interval": -1, // stream SSE / HMR without buffering
		}}
	case registry.StatusDown:
		handle = []any{ident, staticPage(503, "Service down",
			fmt.Sprintf("Service %q of worktree %q is registered on %s but is not answering.\n"+
				"It may be restarting; this page will be replaced as soon as it is back.", rt.Service, rt.Worktree, rt.Upstream),
			obj{"Retry-After": []string{"2"}})}
	default: // configured but not registered
		handle = []any{ident, staticPage(503, "Service not running",
			fmt.Sprintf("Service %q of worktree %q is configured but not registered.\n"+
				"Start it with `wtg run %s` from the worktree, or register an existing process with `wtg register %s --port <port>`.",
				rt.Service, rt.Worktree, rt.Service, rt.Service),
			obj{"Retry-After": []string{"5"}})}
	}
	return obj{"match": match, "handle": handle, "terminal": true}
}

func staticPage(status int, title, body string, headers obj) obj {
	h := obj{"Content-Type": []string{"text/plain; charset=utf-8"}, "X-Wg-Gateway": []string{"1"}}
	for k, v := range headers {
		h[k] = v
	}
	return obj{
		"handler":     "static_response",
		"status_code": status,
		"headers":     h,
		"body":        "worktree-gateway: " + title + "\n\n" + strings.TrimSpace(body) + "\n",
	}
}
