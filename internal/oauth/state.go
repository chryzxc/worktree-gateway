// Package oauth signs and verifies the routing envelope that lets one stable
// OAuth callback URL serve every worktree.
//
// Format: base64url(json(Payload)) + "." + base64url(HMAC-SHA256(key, first part))
//
// The key is derived per worktree: HMAC-SHA256(master, "wg-oauth-v1:" +
// project + "/" + worktree). A worktree's app can therefore be handed its own
// key (WG_OAUTH_STATE_KEY) and sign envelopes locally, but can only ever route
// callbacks to itself; editing the worktree inside the payload changes the
// verification key.
package oauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const version = 1

// Payload is the signed routing information.
type Payload struct {
	V        int    `json:"v"`
	Project  string `json:"p"`
	Worktree string `json:"w"`
	Service  string `json:"s"`
	Path     string `json:"path"`
	State    string `json:"st,omitempty"` // the application's own state value
	Exp      int64  `json:"exp"`
	Nonce    string `json:"n"`
}

var (
	ErrMalformed = errors.New("malformed state")
	ErrSignature = errors.New("invalid state signature")
	ErrExpired   = errors.New("state expired")
	ErrReplayed  = errors.New("state already used")
)

var b64 = base64.RawURLEncoding

// Signer holds the master secret and the single-use nonce cache.
type Signer struct {
	master []byte
	now    func() time.Time

	mu   sync.Mutex
	used map[string]int64 // nonce → exp
}

// NewSigner uses the given master secret.
func NewSigner(master []byte) *Signer {
	return &Signer{master: master, now: time.Now, used: map[string]int64{}}
}

// LoadOrCreate reads the master secret from path, creating a new random one
// (0600) if missing.
func LoadOrCreate(path string) (*Signer, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		key, derr := hex.DecodeString(strings.TrimSpace(string(data)))
		if derr != nil || len(key) < 32 {
			return nil, fmt.Errorf("%s: invalid oauth secret", path)
		}
		return NewSigner(key), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return NewSigner(key), nil
}

// WorktreeKey derives the signing key for one worktree.
func (s *Signer) WorktreeKey(project, worktree string) []byte {
	m := hmac.New(sha256.New, s.master)
	m.Write([]byte("wg-oauth-v1:" + project + "/" + worktree))
	return m.Sum(nil)
}

// WorktreeKeyHex is the form injected as WG_OAUTH_STATE_KEY.
func (s *Signer) WorktreeKeyHex(project, worktree string) string {
	return hex.EncodeToString(s.WorktreeKey(project, worktree))
}

// Sign creates an envelope for p, filling version, expiry and nonce.
func (s *Signer) Sign(p Payload, ttl time.Duration) (string, error) {
	p.V = version
	if p.Exp == 0 {
		p.Exp = s.now().Add(ttl).Unix()
	}
	if p.Nonce == "" {
		n := make([]byte, 12)
		if _, err := rand.Read(n); err != nil {
			return "", err
		}
		p.Nonce = b64.EncodeToString(n)
	}
	return SignWithKey(s.WorktreeKey(p.Project, p.Worktree), p)
}

// SignWithKey signs p with an already derived worktree key. This is exactly
// what an application holding WG_OAUTH_STATE_KEY does.
func SignWithKey(key []byte, p Payload) (string, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	head := b64.EncodeToString(body)
	m := hmac.New(sha256.New, key)
	m.Write([]byte(head))
	return head + "." + b64.EncodeToString(m.Sum(nil)), nil
}

// Verify checks signature, expiry and single use. On success the nonce is
// consumed.
func (s *Signer) Verify(token string) (Payload, error) {
	var p Payload
	head, sig, ok := strings.Cut(token, ".")
	if !ok || len(token) > 4096 {
		return p, ErrMalformed
	}
	body, err := b64.DecodeString(head)
	if err != nil {
		return p, ErrMalformed
	}
	if err := json.Unmarshal(body, &p); err != nil || p.V != version || p.Project == "" || p.Worktree == "" || p.Nonce == "" {
		return Payload{}, ErrMalformed
	}
	gotSig, err := b64.DecodeString(sig)
	if err != nil {
		return Payload{}, ErrMalformed
	}
	m := hmac.New(sha256.New, s.WorktreeKey(p.Project, p.Worktree))
	m.Write([]byte(head))
	if !hmac.Equal(gotSig, m.Sum(nil)) {
		return Payload{}, ErrSignature
	}
	now := s.now().Unix()
	if now > p.Exp {
		return Payload{}, ErrExpired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for n, exp := range s.used {
		if now > exp {
			delete(s.used, n)
		}
	}
	if _, dup := s.used[p.Nonce]; dup {
		return Payload{}, ErrReplayed
	}
	s.used[p.Nonce] = p.Exp
	return p, nil
}
