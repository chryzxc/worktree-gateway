// Package config loads the per-project service file (wtg.yaml) and the
// per-user gateway settings.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ProjectFileNames are looked up, in order, at the root of a worktree.
var ProjectFileNames = []string{"wtg.yaml", "wtg.yml", ".wtg.yaml", ".wtg.yml"}

// Project is the optional per-repository file describing services.
type Project struct {
	Project        string             `yaml:"project"`
	Domain         string             `yaml:"domain"`
	DefaultService string             `yaml:"default_service"`
	MainAlias      *bool              `yaml:"main_alias"`
	Services       map[string]Service `yaml:"services"`

	path string
}

// Service describes one runtime endpoint of a worktree.
type Service struct {
	// Command is used only by the `wtg run` convenience wrapper.
	Command string `yaml:"command"`
	// Port is "auto" (default) or a fixed port.
	Port PortSpec `yaml:"port"`
	// Hostname template. Variables: {worktree} {project} {domain} {service}.
	Hostname string `yaml:"hostname"`
	// Health is an optional HTTP path; when set, "up" means a non-5xx answer.
	Health string `yaml:"health"`
	// Public, when set, allows the service to be reached through the public
	// ingress — but only on the listed path prefixes.
	Public *Public `yaml:"public"`
	// OAuthCallbacks lists local callback paths that signed OAuth state may
	// route to.
	OAuthCallbacks []string `yaml:"oauth_callbacks"`
}

// Public is the exposure allowlist of a service.
type Public struct {
	Paths       []string `yaml:"paths"`
	StripPrefix *bool    `yaml:"strip_prefix"`
}

// PortSpec is either auto (0) or a fixed port.
type PortSpec int

func (p *PortSpec) UnmarshalYAML(n *yaml.Node) error {
	v := strings.TrimSpace(n.Value)
	if v == "" || v == "auto" {
		*p = 0
		return nil
	}
	i, err := strconv.Atoi(v)
	if err != nil || i < 1 || i > 65535 {
		return fmt.Errorf("line %d: port must be \"auto\" or 1-65535, got %q", n.Line, v)
	}
	*p = PortSpec(i)
	return nil
}

func (p PortSpec) MarshalYAML() (any, error) {
	if p == 0 {
		return "auto", nil
	}
	return int(p), nil
}

// Path returns the file the project config was loaded from ("" if none).
func (p *Project) Path() string { return p.path }

// LoadProject reads the project file from dir. A missing file is not an
// error: an empty config is returned.
func LoadProject(dir string) (*Project, error) {
	for _, name := range ProjectFileNames {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		p, err := ParseProject(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		p.path = path
		return p, nil
	}
	return &Project{Services: map[string]Service{}}, nil
}

// ParseProject parses and validates a project file.
func ParseProject(data []byte) (*Project, error) {
	var p Project
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if p.Services == nil {
		p.Services = map[string]Service{}
	}
	return &p, p.Validate()
}

// Validate checks templates and paths.
func (p *Project) Validate() error {
	for name, s := range p.Services {
		if name == "" || strings.ContainsAny(name, " ./") {
			return fmt.Errorf("invalid service name %q", name)
		}
		if s.Hostname != "" {
			if err := checkTemplate(s.Hostname); err != nil {
				return fmt.Errorf("service %s: %w", name, err)
			}
		}
		if s.Public != nil {
			if len(s.Public.Paths) == 0 {
				return fmt.Errorf("service %s: public.paths must list at least one path prefix (use \"/\" to expose everything)", name)
			}
			for _, pp := range s.Public.Paths {
				if !strings.HasPrefix(pp, "/") {
					return fmt.Errorf("service %s: public path %q must start with /", name, pp)
				}
			}
		}
		for _, cb := range s.OAuthCallbacks {
			if !strings.HasPrefix(cb, "/") {
				return fmt.Errorf("service %s: oauth callback %q must start with /", name, cb)
			}
		}
	}
	if p.DefaultService != "" && len(p.Services) > 0 {
		if _, ok := p.Services[p.DefaultService]; !ok {
			return fmt.Errorf("default_service %q is not defined in services", p.DefaultService)
		}
	}
	return nil
}

var templateVars = []string{"{worktree}", "{project}", "{domain}", "{service}"}

func checkTemplate(t string) error {
	rest := t
	for _, v := range templateVars {
		rest = strings.ReplaceAll(rest, v, "x")
	}
	if strings.ContainsAny(rest, "{}") {
		return fmt.Errorf("hostname template %q has unknown variables (allowed: %s)", t, strings.Join(templateVars, " "))
	}
	if !strings.Contains(t, "{worktree}") {
		return fmt.Errorf("hostname template %q must contain {worktree}", t)
	}
	return nil
}

// Default returns the name of the service that gets the bare
// {worktree}.{domain} hostname.
func (p *Project) Default() string {
	if p.DefaultService != "" {
		return p.DefaultService
	}
	if _, ok := p.Services["web"]; ok {
		return "web"
	}
	if len(p.Services) == 1 {
		for k := range p.Services {
			return k
		}
	}
	return "web"
}

// MainAliasEnabled reports whether the main worktree also answers on the
// bare project domain.
func (p *Project) MainAliasEnabled() bool { return p.MainAlias == nil || *p.MainAlias }

// HostnameTemplate returns the template used for service name.
func (p *Project) HostnameTemplate(service string) string {
	if s, ok := p.Services[service]; ok && s.Hostname != "" {
		return s.Hostname
	}
	if service == p.Default() {
		return "{worktree}.{domain}"
	}
	return "{service}.{worktree}.{domain}"
}

// ---------------------------------------------------------------------------
// Global (per-user) settings

// Global holds per-user gateway settings.
type Global struct {
	ListenHost string `yaml:"listen_host"`
	HTTPPort   int    `yaml:"http_port"`
	HTTPSPort  int    `yaml:"https_port"`
	HTTPS      bool   `yaml:"https"`

	Proxy   ProxySettings   `yaml:"proxy"`
	Ingress IngressSettings `yaml:"ingress"`
	Tunnel  TunnelSettings  `yaml:"tunnel"`
	Capture CaptureSettings `yaml:"capture"`
	OAuth   OAuthSettings   `yaml:"oauth"`

	HealthInterval    time.Duration `yaml:"health_interval"`
	DiscoveryInterval time.Duration `yaml:"discovery_interval"`
	StaleAfter        time.Duration `yaml:"stale_after"`
	PortRange         [2]int        `yaml:"port_range"`
}

type ProxySettings struct {
	// Provider is "embedded" (Caddy linked into wtg) or "caddy-admin"
	// (push config to an existing Caddy's admin API).
	Provider string `yaml:"provider"`
	AdminURL string `yaml:"admin_url"`
}

type IngressSettings struct {
	Port int `yaml:"port"`
	// Hold is how long a public request waits for a restarting service.
	Hold time.Duration `yaml:"hold"`
}

type TunnelSettings struct {
	// Provider is "cloudflared", "ngrok" or "external".
	Provider string `yaml:"provider"`
	// PublicURL is required for "external" and for named cloudflared/ngrok
	// tunnels with a fixed hostname.
	PublicURL string `yaml:"public_url"`
	// PublicDomain enables host-based routing: {worktree}.{project}.<domain>
	// or {worktree}.<domain>. Requires a wildcard hostname on the tunnel.
	PublicDomain string `yaml:"public_domain"`
	// Name is the named cloudflared tunnel to run (optional).
	Name string   `yaml:"name"`
	Args []string `yaml:"args"`
}

type CaptureSettings struct {
	Enabled       bool          `yaml:"enabled"`
	Bodies        bool          `yaml:"bodies"`
	MaxEntries    int           `yaml:"max_entries"`
	MaxAge        time.Duration `yaml:"max_age"`
	MaxBody       int64         `yaml:"max_body"`
	RedactHeaders []string      `yaml:"redact_headers"`
}

type OAuthSettings struct {
	StateTTL time.Duration `yaml:"state_ttl"`
}

// DefaultGlobal returns the built-in defaults.
func DefaultGlobal() Global {
	return Global{
		ListenHost: "127.0.0.1",
		HTTPPort:   8780,
		HTTPSPort:  8743,
		HTTPS:      true,
		Proxy:      ProxySettings{Provider: "embedded", AdminURL: "http://localhost:2019"},
		Ingress:    IngressSettings{Port: 8790, Hold: 10 * time.Second},
		Tunnel:     TunnelSettings{Provider: "cloudflared"},
		Capture: CaptureSettings{
			Enabled:    true,
			Bodies:     true,
			MaxEntries: 500,
			MaxAge:     7 * 24 * time.Hour,
			MaxBody:    1 << 20,
		},
		OAuth:             OAuthSettings{StateTTL: 15 * time.Minute},
		HealthInterval:    2 * time.Second,
		DiscoveryInterval: 10 * time.Second,
		StaleAfter:        10 * time.Minute,
		PortRange:         [2]int{20000, 29999},
	}
}

// LoadGlobal reads the global config file, filling unspecified fields with
// defaults. A missing file yields the defaults.
func LoadGlobal(path string) (Global, error) {
	g := DefaultGlobal()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return g, nil
	}
	if err != nil {
		return g, err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&g); err != nil && !errors.Is(err, io.EOF) {
		return g, fmt.Errorf("%s: %w", path, err)
	}
	return g, g.Validate()
}

// Validate checks the global settings.
func (g Global) Validate() error {
	if g.ListenHost != "127.0.0.1" && g.ListenHost != "::1" && g.ListenHost != "localhost" {
		return fmt.Errorf("listen_host must be a loopback address (got %q); the gateway is local-only by design", g.ListenHost)
	}
	switch g.Proxy.Provider {
	case "embedded", "caddy-admin":
	default:
		return fmt.Errorf("proxy.provider must be embedded or caddy-admin, got %q", g.Proxy.Provider)
	}
	switch g.Tunnel.Provider {
	case "cloudflared", "ngrok", "external":
	default:
		return fmt.Errorf("tunnel.provider must be cloudflared, ngrok or external, got %q", g.Tunnel.Provider)
	}
	if g.PortRange[0] < 1024 || g.PortRange[1] > 65535 || g.PortRange[0] >= g.PortRange[1] {
		return fmt.Errorf("port_range %v is invalid", g.PortRange)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Paths

// StateDir is where the daemon keeps its registry, socket, CA and logs.
func StateDir() string {
	if d := os.Getenv("WTG_HOME"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "worktree-gateway")
	}
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "worktree-gateway")
		}
	}
	return filepath.Join(home, ".local", "state", "worktree-gateway")
}

// GlobalConfigPath is the per-user settings file.
func GlobalConfigPath() string {
	if p := os.Getenv("WTG_CONFIG"); p != "" {
		return p
	}
	if d := os.Getenv("WTG_HOME"); d != "" {
		return filepath.Join(d, "config.yaml")
	}
	d, err := os.UserConfigDir()
	if err != nil || runtime.GOOS == "darwin" {
		// Prefer the XDG location on macOS as well: it is what CLI users expect.
		home, _ := os.UserHomeDir()
		d = filepath.Join(home, ".config")
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
			d = x
		}
	}
	return filepath.Join(d, "worktree-gateway", "config.yaml")
}

// SocketPath is the daemon's admin socket.
func SocketPath() string {
	if p := os.Getenv("WTG_SOCKET"); p != "" {
		return p
	}
	return filepath.Join(StateDir(), "wtg.sock")
}
