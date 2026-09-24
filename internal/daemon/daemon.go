// Package daemon runs the long-lived gateway process: registry, Caddy,
// ingress, tunnel and the lifecycle loops. It exposes a JSON API on a unix
// socket only.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/chryzxc/worktree-gateway/internal/capture"
	"github.com/chryzxc/worktree-gateway/internal/config"
	"github.com/chryzxc/worktree-gateway/internal/ingress"
	"github.com/chryzxc/worktree-gateway/internal/oauth"
	"github.com/chryzxc/worktree-gateway/internal/proxy"
	"github.com/chryzxc/worktree-gateway/internal/registry"
	"github.com/chryzxc/worktree-gateway/internal/tunnel"
	"github.com/chryzxc/worktree-gateway/internal/version"
)

// Options configure a daemon instance.
type Options struct {
	StateDir   string
	SocketPath string
	Global     config.Global
	Logger     *log.Logger
	// Proxy overrides the provider chosen from Global (tests).
	Proxy proxy.Provider
}

// Daemon is the running gateway.
type Daemon struct {
	opt     Options
	g       config.Global
	log     *log.Logger
	reg     *registry.Registry
	proxy   proxy.Provider
	capture *capture.Log
	signer  *oauth.Signer
	ingress *ingress.Ingress

	reloadCh chan struct{}
	started  time.Time

	mu         sync.Mutex
	tun        *tunnel.Tunnel
	lastErr    string
	tombstones map[string]time.Time // worktree id → do not auto-add until

	servers []*http.Server
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// New prepares a daemon; call Run.
func New(opt Options) (*Daemon, error) {
	if opt.Logger == nil {
		opt.Logger = log.New(io.Discard, "", 0)
	}
	if err := os.MkdirAll(opt.StateDir, 0o700); err != nil {
		return nil, err
	}
	reg, err := registry.Open(filepath.Join(opt.StateDir, "registry.json"))
	if err != nil {
		return nil, err
	}
	capLog, err := capture.Open(filepath.Join(opt.StateDir, "requests.jsonl"), opt.Global.Capture)
	if err != nil {
		return nil, err
	}
	signer, err := oauth.LoadOrCreate(filepath.Join(opt.StateDir, "oauth.key"))
	if err != nil {
		return nil, err
	}
	p := opt.Proxy
	if p == nil {
		if p, err = proxy.NewProvider(opt.Global.Proxy.Provider, opt.Global.Proxy.AdminURL); err != nil {
			return nil, err
		}
	}
	d := &Daemon{
		opt: opt, g: opt.Global, log: opt.Logger, reg: reg, proxy: p, capture: capLog, signer: signer,
		reloadCh: make(chan struct{}, 1), tombstones: map[string]time.Time{},
	}
	d.ingress = &ingress.Ingress{
		Reg: reg, Global: func() config.Global { return d.g }, Capture: capLog, Signer: signer,
		Exposed: d.exposed, Logger: opt.Logger,
	}
	reg.OnChange(d.requestReload)
	return d, nil
}

// Registry exposes the registry (tests).
func (d *Daemon) Registry() *registry.Registry { return d.reg }

func (d *Daemon) requestReload() {
	select {
	case d.reloadCh <- struct{}{}:
	default:
	}
}

// Run starts all listeners and loops and blocks until ctx is done.
func (d *Daemon) Run(ctx context.Context) error {
	ctx, d.cancel = context.WithCancel(ctx)
	d.started = time.Now()

	sock, err := listenSocket(d.opt.SocketPath)
	if err != nil {
		return err
	}
	ingressAddr := net.JoinHostPort(loopback(d.g.ListenHost), strconv.Itoa(d.g.Ingress.Port))
	il, err := net.Listen("tcp", ingressAddr)
	if err != nil {
		sock.Close()
		return fmt.Errorf("ingress listener %s: %w (set ingress.port in %s)", ingressAddr, err, config.GlobalConfigPath())
	}
	if err := d.applyProxy(ctx); err != nil {
		sock.Close()
		il.Close()
		return err
	}
	api := &http.Server{Handler: d.apiHandler(), ReadHeaderTimeout: 10 * time.Second}
	ing := &http.Server{Handler: d.ingress, ReadHeaderTimeout: 10 * time.Second}
	d.servers = []*http.Server{api, ing}
	pidFile := filepath.Join(d.opt.StateDir, "daemon.pid")
	os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600)

	d.log.Printf("wtg daemon %s started (http :%d, https :%d, ingress %s, proxy %s)",
		version.Version, d.g.HTTPPort, d.g.HTTPSPort, ingressAddr, d.proxy.Name())

	errCh := make(chan error, 2)
	for srv, l := range map[*http.Server]net.Listener{api: sock, ing: il} {
		d.wg.Add(1)
		go func(srv *http.Server, l net.Listener) {
			defer d.wg.Done()
			if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}(srv, l)
	}
	d.loop(ctx, "reload", 0, func() {
		if err := d.applyProxy(ctx); err != nil {
			d.log.Printf("proxy reload failed: %v", err)
		}
	})
	d.loop(ctx, "health", d.g.HealthInterval, func() { d.checkHealth(ctx) })
	d.loop(ctx, "discovery", d.g.DiscoveryInterval, func() { d.discover(ctx) })

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errCh:
	}
	d.shutdown()
	os.Remove(pidFile)
	return runErr
}

// Stop asks Run to return.
func (d *Daemon) Stop() {
	if d.cancel != nil {
		d.cancel()
	}
}

func (d *Daemon) shutdown() {
	if d.cancel != nil {
		d.cancel()
	}
	d.mu.Lock()
	tun := d.tun
	d.tun = nil
	d.mu.Unlock()
	if tun != nil {
		tun.Stop()
	}
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, s := range d.servers {
		s.Shutdown(sctx)
	}
	d.wg.Wait()
	if err := d.proxy.Stop(); err != nil {
		d.log.Printf("stopping proxy: %v", err)
	}
	os.Remove(d.opt.SocketPath)
	d.log.Printf("wtg daemon stopped")
}

// loop runs f every interval (or on reload requests when interval is 0).
func (d *Daemon) loop(ctx context.Context, name string, every time.Duration, f func()) {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		var tick <-chan time.Time
		if every > 0 {
			t := time.NewTicker(every)
			defer t.Stop()
			tick = t.C
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick:
				f()
			case <-d.reloadChIf(every == 0):
				// Debounce bursts of registry changes into one reload.
				time.Sleep(50 * time.Millisecond)
				select {
				case <-d.reloadCh:
				default:
				}
				f()
			}
		}
	}()
}

func (d *Daemon) reloadChIf(ok bool) <-chan struct{} {
	if ok {
		return d.reloadCh
	}
	return nil
}

func (d *Daemon) applyProxy(ctx context.Context) error {
	cfg, err := proxy.BuildConfig(d.reg.Snapshot(), proxy.Options{Global: d.g, StateDir: d.opt.StateDir, Embedded: d.proxy.Embedded()})
	if err != nil {
		return err
	}
	actx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	err = d.proxy.Apply(actx, cfg)
	d.mu.Lock()
	if err != nil {
		d.lastErr = err.Error()
	} else {
		d.lastErr = ""
	}
	d.mu.Unlock()
	return err
}

func loopback(h string) string {
	if h == "" || h == "localhost" {
		return "127.0.0.1"
	}
	return h
}

// listenSocket creates the admin socket, refusing to start when another
// daemon already answers on it.
func listenSocket(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if c, err := net.DialTimeout("unix", path, 500*time.Millisecond); err == nil {
		c.Close()
		return nil, fmt.Errorf("a wtg daemon is already running on %s", path)
	}
	os.Remove(path) // stale socket from a crashed daemon
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("admin socket %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// ---------------------------------------------------------------------------
// Tunnel

func (d *Daemon) exposed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tun != nil
}

// TunnelStatus returns the current tunnel status.
func (d *Daemon) TunnelStatus() tunnel.Status {
	d.mu.Lock()
	tun := d.tun
	d.mu.Unlock()
	if tun == nil {
		return tunnel.Status{Provider: d.g.Tunnel.Provider, State: tunnel.StateStopped}
	}
	return tun.Status()
}

func (d *Daemon) startTunnel(provider string) (tunnel.Status, error) {
	d.mu.Lock()
	if d.tun != nil {
		st := d.tun.Status()
		d.mu.Unlock()
		return st, nil
	}
	d.mu.Unlock()
	set := d.g.Tunnel
	if provider != "" {
		set.Provider = provider
	}
	local := fmt.Sprintf("http://%s", net.JoinHostPort(loopback(d.g.ListenHost), strconv.Itoa(d.g.Ingress.Port)))
	spec, err := tunnel.BuildSpec(set, local)
	if err != nil {
		return tunnel.Status{}, err
	}
	tun := tunnel.New(spec, local, func(st tunnel.Status) {
		d.log.Printf("tunnel %s: %s %s %s", st.Provider, st.State, st.PublicURL, st.LastError)
	})
	if err := tun.Start(); err != nil {
		return tunnel.Status{}, err
	}
	d.mu.Lock()
	d.tun = tun
	d.mu.Unlock()
	// Wait briefly for the public URL so the CLI can print it.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if st := tun.Status(); st.PublicURL != "" {
			return st, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return tun.Status(), nil
}

func (d *Daemon) stopTunnel() {
	d.mu.Lock()
	tun := d.tun
	d.tun = nil
	d.mu.Unlock()
	if tun != nil {
		tun.Stop()
	}
}
