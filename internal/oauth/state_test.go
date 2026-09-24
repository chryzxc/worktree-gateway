package oauth

import (
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	s := NewSigner([]byte(strings.Repeat("k", 32)))
	tok, err := s.Sign(Payload{Project: "myapp", Worktree: "feature-auth", Service: "api", Path: "/auth/callback", State: "xyz"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Verify(tok)
	if err != nil || p.Worktree != "feature-auth" || p.State != "xyz" {
		t.Fatalf("%v %+v", err, p)
	}
	if _, err := s.Verify(tok); err != ErrReplayed {
		t.Fatalf("replay: %v", err)
	}
}

func TestTamperAndExpiry(t *testing.T) {
	s := NewSigner([]byte(strings.Repeat("k", 32)))
	// An app holding feature-auth's key cannot route to another worktree.
	key := s.WorktreeKey("myapp", "feature-auth")
	forged, _ := SignWithKey(key, Payload{V: 1, Project: "myapp", Worktree: "main", Path: "/", Exp: time.Now().Add(time.Hour).Unix(), Nonce: "n"})
	if _, err := s.Verify(forged); err != ErrSignature {
		t.Fatalf("forged: %v", err)
	}
	// But it can sign for itself.
	own, _ := SignWithKey(key, Payload{V: 1, Project: "myapp", Worktree: "feature-auth", Path: "/", Exp: time.Now().Add(time.Hour).Unix(), Nonce: "n2"})
	if _, err := s.Verify(own); err != nil {
		t.Fatalf("own: %v", err)
	}
	tok, _ := s.Sign(Payload{Project: "p", Worktree: "w"}, time.Minute)
	s.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if _, err := s.Verify(tok); err != ErrExpired {
		t.Fatalf("expiry: %v", err)
	}
	for _, bad := range []string{"", "abc", "a.b", tok[:len(tok)-2] + "xx"} {
		if _, err := s.Verify(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
	if hex.EncodeToString(key) != s.WorktreeKeyHex("myapp", "feature-auth") {
		t.Fatal("hex key mismatch")
	}
}

func TestLoadOrCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth.key")
	a, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if a.WorktreeKeyHex("p", "w") != b.WorktreeKeyHex("p", "w") {
		t.Fatal("secret not persisted")
	}
}
