// Package tunnel adapts existing tunnel providers. It only starts the
// provider's own client and learns the public URL; the provider owns the
// Internet-facing edge, TLS and networking.
package tunnel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/chryzxc/worktree-gateway/internal/config"
)

// States reported by Status.
const (
	StateStopped      = "stopped"
	StateStarting     = "starting"
	StateConnected    = "connected"
	StateReconnecting = "reconnecting"
)

// Status is a point-in-time view of the tunnel.
type Status struct {
	Provider  string    `json:"provider"`
	State     string    `json:"state"`
	PublicURL string    `json:"public_url,omitempty"`
	LocalURL  string    `json:"local_url,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	Since     time.Time `json:"since,omitempty"`
	Restarts  int       `json:"restarts"`
}

// Spec describes how to run a provider client.
type Spec struct {
	Provider string
	Binary   string
	Args     []string
	// FixedURL is known up front (named tunnels, external).
	FixedURL string
	// ParseURL extracts the public URL from a line of client output.
	ParseURL func(line string) string
}

var quickTunnelRe = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

// BuildSpec turns settings into a runnable spec for the ingress at localURL.
func BuildSpec(set config.TunnelSettings, localURL string) (Spec, error) {
	fixed := strings.TrimRight(set.PublicURL, "/")
	switch set.Provider {
	case "cloudflared":
		s := Spec{Provider: "cloudflared", Binary: "cloudflared", FixedURL: fixed}
		if set.Name != "" {
			if fixed == "" {
				return s, errors.New("tunnel.public_url is required with a named cloudflared tunnel")
			}
			s.Args = append([]string{"tunnel", "--no-autoupdate", "run", "--url", localURL}, set.Args...)
			s.Args = append(s.Args, set.Name)
		} else {
			s.Args = append([]string{"tunnel", "--no-autoupdate", "--url", localURL}, set.Args...)
			s.ParseURL = func(line string) string { return quickTunnelRe.FindString(line) }
		}
		return s, nil
	case "ngrok":
		s := Spec{Provider: "ngrok", Binary: "ngrok", FixedURL: fixed,
			Args: []string{"http", strings.TrimPrefix(localURL, "http://"), "--log", "stdout", "--log-format", "json"}}
		if fixed != "" {
			s.Args = append(s.Args, "--url", fixed)
		}
		s.Args = append(s.Args, set.Args...)
		s.ParseURL = func(line string) string {
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) != nil {
				return ""
			}
			if u, ok := m["url"].(string); ok && strings.HasPrefix(u, "https://") {
				return u
			}
			return ""
		}
		return s, nil
	case "external":
		if fixed == "" {
			return Spec{}, errors.New("tunnel.public_url is required for the external provider (the URL your own tunnel forwards to " + localURL + ")")
		}
		return Spec{Provider: "external", FixedURL: fixed}, nil
	}
	return Spec{}, fmt.Errorf("unknown tunnel provider %q", set.Provider)
}

// Tunnel supervises one provider client process.
type Tunnel struct {
	spec     Spec
	localURL string
	onChange func(Status)

	mu     sync.Mutex
	st     Status
	cancel context.CancelFunc
	done   chan struct{}
}

// New prepares a tunnel; call Start to run it.
func New(spec Spec, localURL string, onChange func(Status)) *Tunnel {
	return &Tunnel{spec: spec, localURL: localURL, onChange: onChange,
		st: Status{Provider: spec.Provider, State: StateStopped, LocalURL: localURL}}
}

// Status returns the current status.
func (t *Tunnel) Status() Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.st
}

func (t *Tunnel) update(f func(*Status)) {
	t.mu.Lock()
	f(&t.st)
	st := t.st
	t.mu.Unlock()
	if t.onChange != nil {
		t.onChange(st)
	}
}

// Start launches the client and supervises it until Stop.
func (t *Tunnel) Start() error {
	if t.spec.Binary != "" {
		if _, err := exec.LookPath(t.spec.Binary); err != nil {
			return fmt.Errorf("%s not found in PATH; install it or choose another tunnel.provider", t.spec.Binary)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.mu.Lock()
	t.cancel = cancel
	t.done = make(chan struct{})
	t.mu.Unlock()
	if t.spec.Binary == "" { // external: nothing to run
		t.update(func(s *Status) {
			s.State, s.PublicURL, s.Since = StateConnected, t.spec.FixedURL, time.Now()
		})
		close(t.done)
		return nil
	}
	t.update(func(s *Status) { s.State, s.Since = StateStarting, time.Now() })
	go t.supervise(ctx)
	return nil
}

func (t *Tunnel) supervise(ctx context.Context) {
	defer close(t.done)
	backoff := time.Second
	for {
		start := time.Now()
		err := t.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		t.update(func(s *Status) {
			s.State = StateReconnecting
			s.Restarts++
			if err != nil {
				s.LastError = err.Error()
			}
		})
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (t *Tunnel) runOnce(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, t.spec.Binary, t.spec.Args...)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	// Don't wait forever on grandchildren still holding the output pipe.
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		pw.Close()
		return err
	}
	if t.spec.FixedURL != "" {
		t.update(func(s *Status) {
			s.State, s.PublicURL, s.Since = StateConnected, t.spec.FixedURL, time.Now()
		})
	}
	var lastLine string
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if strings.TrimSpace(line) != "" {
				lastLine = line
			}
			if t.spec.ParseURL != nil {
				if u := t.spec.ParseURL(line); u != "" {
					t.update(func(s *Status) {
						s.State, s.PublicURL, s.Since, s.LastError = StateConnected, u, time.Now(), ""
					})
				}
			}
		}
	}()
	err := cmd.Wait()
	pw.Close()
	<-scanned
	if err == nil {
		err = errors.New("tunnel client exited")
	}
	if lastLine != "" {
		err = fmt.Errorf("%w: %s", err, lastLine)
	}
	return err
}

// Stop terminates the client.
func (t *Tunnel) Stop() {
	t.mu.Lock()
	cancel, done := t.cancel, t.done
	t.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
	t.update(func(s *Status) { s.State, s.PublicURL = StateStopped, "" })
}
