// Package capture keeps a bounded, redacted history of public ingress
// requests so they can be inspected and replayed. It is deliberately small:
// no transformation, fan-out or long-term storage.
package capture

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chryzxc/worktree-gateway/internal/config"
)

// Redacted replaces sensitive values.
const Redacted = "[redacted]"

// DefaultRedactHeaders are always removed from stored requests.
var DefaultRedactHeaders = []string{
	"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie",
	"X-Api-Key", "X-Auth-Token", "X-Access-Token", "X-Csrf-Token", "X-Xsrf-Token",
}

var sensitiveQuery = []string{"token", "access_token", "id_token", "refresh_token", "api_key", "apikey", "key", "secret", "password", "code"}

// Entry is one captured request.
type Entry struct {
	ID            string      `json:"id"`
	Time          time.Time   `json:"time"`
	Via           string      `json:"via"` // public | replay
	Project       string      `json:"project"`
	Worktree      string      `json:"worktree"`
	WorktreeID    string      `json:"worktree_id"`
	Service       string      `json:"service"`
	Source        string      `json:"source"`
	Method        string      `json:"method"`
	Host          string      `json:"host"`
	PublicPath    string      `json:"public_path"`
	Path          string      `json:"path"` // as forwarded to the service
	RawQuery      string      `json:"raw_query,omitempty"`
	Header        http.Header `json:"header"`
	Body          []byte      `json:"body,omitempty"`
	BodySize      int64       `json:"body_size"`
	BodyCaptured  bool        `json:"body_captured"`
	BodyTruncated bool        `json:"body_truncated,omitempty"`
	Redactions    []string    `json:"redactions,omitempty"`
	Status        int         `json:"status"`
	DurationMS    int64       `json:"duration_ms"`
	Error         string      `json:"error,omitempty"`
	ReplayOf      string      `json:"replay_of,omitempty"`
}

// Log is a bounded request history persisted as JSON lines.
type Log struct {
	mu      sync.Mutex
	set     config.CaptureSettings
	path    string
	entries []Entry
	redact  map[string]bool
	now     func() time.Time
	lines   int
}

// Open loads the log at path (may be "" for memory only).
func Open(path string, set config.CaptureSettings) (*Log, error) {
	l := &Log{set: set, path: path, redact: map[string]bool{}, now: time.Now}
	for _, h := range append(append([]string{}, DefaultRedactHeaders...), set.RedactHeaders...) {
		l.redact[http.CanonicalHeaderKey(h)] = true
	}
	if path == "" {
		return l, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.ID != "" {
			l.entries = append(l.entries, e)
		}
		l.lines++
	}
	l.pruneLocked()
	return l, l.rewriteLocked()
}

// Enabled reports whether capturing is on.
func (l *Log) Enabled() bool { return l.set.Enabled }

// Settings returns the capture settings.
func (l *Log) Settings() config.CaptureSettings { return l.set }

// NewID returns a short request id.
func NewID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return "req_" + hex.EncodeToString(b)
}

// RedactHeader returns a copy of h with sensitive values replaced, and the
// list of header names that were redacted.
func (l *Log) RedactHeader(h http.Header) (http.Header, []string) {
	out := h.Clone()
	var names []string
	for k := range out {
		if l.redact[http.CanonicalHeaderKey(k)] {
			out[k] = []string{Redacted}
			names = append(names, k)
		}
	}
	sort.Strings(names)
	return out, names
}

// RedactQuery hides values of well-known credential parameters.
func RedactQuery(raw string) (string, bool) {
	if raw == "" {
		return raw, false
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		return "", true
	}
	changed := false
	for k := range q {
		lk := strings.ToLower(k)
		for _, s := range sensitiveQuery {
			if lk == s {
				q[k] = []string{Redacted}
				changed = true
			}
		}
	}
	if !changed {
		return raw, false
	}
	return q.Encode(), true
}

// DetectSource guesses the webhook sender from well-known headers.
func DetectSource(h http.Header) string {
	switch {
	case h.Get("Stripe-Signature") != "":
		return "stripe"
	case h.Get("X-Twilio-Signature") != "":
		return "twilio"
	case h.Get("X-GitHub-Event") != "":
		return "github"
	case h.Get("X-Gitlab-Event") != "":
		return "gitlab"
	case h.Get("X-Slack-Signature") != "":
		return "slack"
	case h.Get("X-Shopify-Hmac-Sha256") != "":
		return "shopify"
	case h.Get("Svix-Id") != "" || h.Get("Webhook-Id") != "":
		return "svix"
	case h.Get("Paddle-Signature") != "":
		return "paddle"
	}
	return "-"
}

// Add stores an entry (already redacted by the caller).
func (l *Log) Add(e Entry) {
	if !l.set.Enabled {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, e)
	before := len(l.entries)
	l.pruneLocked()
	if l.path == "" {
		return
	}
	if len(l.entries) < before || l.lines > 2*l.set.MaxEntries {
		_ = l.rewriteLocked()
		return
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	if b, err := json.Marshal(e); err == nil {
		f.Write(append(b, '\n'))
		l.lines++
	}
}

func (l *Log) pruneLocked() {
	cutoff := l.now().Add(-l.set.MaxAge)
	i := 0
	for i < len(l.entries) && l.set.MaxAge > 0 && l.entries[i].Time.Before(cutoff) {
		i++
	}
	l.entries = l.entries[i:]
	if l.set.MaxEntries > 0 && len(l.entries) > l.set.MaxEntries {
		l.entries = l.entries[len(l.entries)-l.set.MaxEntries:]
	}
}

func (l *Log) rewriteLocked() error {
	if l.path == "" {
		return nil
	}
	tmp := l.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, e := range l.entries {
		b, _ := json.Marshal(e)
		w.Write(append(b, '\n'))
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	l.lines = len(l.entries)
	return os.Rename(tmp, l.path)
}

// Get returns an entry by id.
func (l *Log) Get(id string) (Entry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.entries) - 1; i >= 0; i-- {
		if l.entries[i].ID == id {
			return l.entries[i], true
		}
	}
	return Entry{}, false
}

// List returns the most recent entries first, optionally filtered by
// worktree slug, without bodies.
func (l *Log) List(worktree string, limit int) []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Entry
	for i := len(l.entries) - 1; i >= 0 && (limit <= 0 || len(out) < limit); i-- {
		e := l.entries[i]
		if worktree != "" && e.Worktree != worktree {
			continue
		}
		e.Body = nil
		out = append(out, e)
	}
	return out
}

// Clear removes all entries.
func (l *Log) Clear() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = nil
	return l.rewriteLocked()
}
