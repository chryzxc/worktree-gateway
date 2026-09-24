package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	// Standard Caddy HTTP modules (reverse_proxy, static_response, headers, …)
	// and the internal PKI/TLS apps.
	_ "github.com/caddyserver/caddy/v2/modules/standard"
)

// Provider applies generated configuration to a running proxy.
type Provider interface {
	Name() string
	// Embedded reports whether process-local settings should be generated.
	Embedded() bool
	Apply(ctx context.Context, cfg []byte) error
	Stop() error
}

// NewProvider returns the configured provider.
func NewProvider(name, adminURL string) (Provider, error) {
	switch name {
	case "", "embedded":
		return &Embedded{}, nil
	case "caddy-admin":
		return &External{AdminURL: strings.TrimRight(adminURL, "/"), Client: &http.Client{Timeout: 15 * time.Second}}, nil
	default:
		return nil, fmt.Errorf("unknown proxy provider %q", name)
	}
}

// Embedded runs Caddy inside the wtg daemon process.
type Embedded struct {
	mu      sync.Mutex
	last    []byte
	running bool
}

func (e *Embedded) Name() string   { return "embedded" }
func (e *Embedded) Embedded() bool { return true }

// Apply loads cfg with zero downtime; Caddy rolls back on failure.
func (e *Embedded) Apply(_ context.Context, cfg []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running && bytes.Equal(cfg, e.last) {
		return nil
	}
	if err := caddy.Load(cfg, false); err != nil {
		return fmt.Errorf("caddy: %w", err)
	}
	e.last = append(e.last[:0], cfg...)
	e.running = true
	return nil
}

func (e *Embedded) Stop() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.running {
		return nil
	}
	e.running = false
	return caddy.Stop()
}

// External pushes configuration to an already running Caddy through its
// admin API. That Caddy instance should be dedicated to the gateway: /load
// replaces its whole configuration.
type External struct {
	AdminURL string
	Client   *http.Client

	mu   sync.Mutex
	last []byte
}

func (x *External) Name() string   { return "caddy-admin" }
func (x *External) Embedded() bool { return false }

func (x *External) Apply(ctx context.Context, cfg []byte) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if bytes.Equal(cfg, x.last) {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, x.AdminURL+"/load", bytes.NewReader(cfg))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := x.Client.Do(req)
	if err != nil {
		return fmt.Errorf("caddy admin API %s unreachable: %w", x.AdminURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("caddy admin API: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	x.last = append(x.last[:0], cfg...)
	return nil
}

func (x *External) Stop() error { return nil }
