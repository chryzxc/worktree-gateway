// Package api defines the daemon's unix-socket JSON API and a client for it.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/chryzxc/worktree-gateway/internal/capture"
	"github.com/chryzxc/worktree-gateway/internal/tunnel"
)

type ServiceView struct {
	Name       string   `json:"name"`
	Status     string   `json:"status"` // up | down | starting | missing
	Upstream   string   `json:"upstream,omitempty"`
	Port       int      `json:"port,omitempty"`
	PID        int      `json:"pid,omitempty"`
	Source     string   `json:"source,omitempty"`
	URLs       []string `json:"urls"`
	Public     []string `json:"public_paths,omitempty"`
	Configured bool     `json:"configured"`
	Conflict   string   `json:"conflict,omitempty"`
}

type WorktreeView struct {
	ID            string        `json:"id"`
	Project       string        `json:"project"`
	ProjectDomain string        `json:"project_domain"`
	Slug          string        `json:"slug"`
	Branch        string        `json:"branch"`
	Path          string        `json:"path"`
	IsMain        bool          `json:"is_main"`
	Parked        bool          `json:"parked,omitempty"`
	PublicURL     string        `json:"public_url,omitempty"`
	ConfigFile    string        `json:"config_file,omitempty"`
	Services      []ServiceView `json:"services"`
}

type Status struct {
	Version    string         `json:"version"`
	PID        int            `json:"pid"`
	Started    time.Time      `json:"started"`
	StateDir   string         `json:"state_dir"`
	HTTP       string         `json:"http"`
	HTTPS      string         `json:"https,omitempty"`
	Ingress    string         `json:"ingress"`
	Proxy      string         `json:"proxy"`
	ProxyError string         `json:"proxy_error,omitempty"`
	Tunnel     tunnel.Status  `json:"tunnel"`
	Worktrees  []WorktreeView `json:"worktrees"`
}

type VersionResponse struct {
	Version string `json:"version"`
}

type UpRequest struct {
	Path string `json:"path"`
	Name string `json:"name,omitempty"`
}

type DownRequest struct {
	Path   string `json:"path"`
	Forget bool   `json:"forget,omitempty"`
}

type RegisterRequest struct {
	Path    string `json:"path"`
	Service string `json:"service"`
	Host    string `json:"host,omitempty"`
	Port    int    `json:"port"`
	PID     int    `json:"pid,omitempty"`
	Source  string `json:"source,omitempty"`
}

type RegisterResponse struct {
	Worktree WorktreeView `json:"worktree"`
	Service  ServiceView  `json:"service"`
	Evicted  []string     `json:"evicted,omitempty"`
}

type DeregisterRequest struct {
	Path    string `json:"path"`
	Service string `json:"service"`
	PID     int    `json:"pid,omitempty"`
}

type PortRequest struct {
	Path    string `json:"path"`
	Service string `json:"service"`
}

type PortResponse struct {
	Port int `json:"port"`
}

type EnvRequest struct {
	Path    string `json:"path"`
	Service string `json:"service,omitempty"`
	Port    int    `json:"port,omitempty"`
}

type TunnelRequest struct {
	Provider string `json:"provider,omitempty"`
}

type ReplayRequest struct {
	To  string `json:"to,omitempty"`
	Yes bool   `json:"yes,omitempty"`
}

type OAuthStateRequest struct {
	Path         string        `json:"path"`
	Service      string        `json:"service"`
	CallbackPath string        `json:"callback_path"`
	State        string        `json:"state,omitempty"`
	TTL          time.Duration `json:"ttl,omitempty"`
}

type OAuthStateResponse struct {
	State       string `json:"state"`
	RedirectURI string `json:"redirect_uri"`
	Destination string `json:"destination"`
}

type RequestList struct {
	Requests []capture.Entry `json:"requests"`
}

type errorBody struct {
	Error string `json:"error"`
}

// ErrDaemonDown is returned when nothing listens on the socket.
var ErrDaemonDown = errors.New("wtg daemon is not running")

// Client talks to the daemon over its unix socket.
type Client struct {
	Socket string
	http   *http.Client
}

// NewClient returns a client for socket.
func NewClient(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{Socket: socket, http: &http.Client{Transport: tr, Timeout: 60 * time.Second}}
}

// Ping reports whether the daemon answers.
func (c *Client) Ping() bool {
	conn, err := net.DialTimeout("unix", c.Socket, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Do sends a request; in and out may be nil.
func (c *Client) Do(method, path string, query url.Values, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	u := "http://wtg" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		var ne *net.OpError
		if errors.As(err, &ne) {
			return ErrDaemonDown
		}
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e errorBody
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("daemon: %s", resp.Status)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
