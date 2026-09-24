package capture

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chryzxc/worktree-gateway/internal/config"
)

func settings() config.CaptureSettings {
	s := config.DefaultGlobal().Capture
	s.MaxEntries = 3
	s.RedactHeaders = []string{"X-Custom-Secret"}
	return s
}

func TestRedaction(t *testing.T) {
	l, _ := Open("", settings())
	h := http.Header{}
	h.Set("Authorization", "Bearer x")
	h.Set("Cookie", "a=b")
	h.Set("X-Custom-Secret", "s")
	h.Set("Stripe-Signature", "t=1,v1=abc")
	out, names := l.RedactHeader(h)
	if out.Get("Authorization") != Redacted || out.Get("Cookie") != Redacted || out.Get("X-Custom-Secret") != Redacted {
		t.Fatalf("not redacted: %v", out)
	}
	if out.Get("Stripe-Signature") != "t=1,v1=abc" || len(names) != 3 {
		t.Fatalf("signature header must be kept for replay: %v %v", out, names)
	}
	if h.Get("Authorization") != "Bearer x" {
		t.Fatal("original header mutated")
	}
	q, changed := RedactQuery("a=1&token=abc&Code=z")
	if !changed || strings.Contains(q, "abc") || strings.Contains(q, "=z") || !strings.Contains(q, "a=1") {
		t.Fatalf("query: %q", q)
	}
	if DetectSource(h) != "stripe" {
		t.Fatal("source detection")
	}
}

func TestBoundsAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	l, err := Open(path, settings())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		l.Add(Entry{ID: fmt.Sprintf("req_%d", i), Time: time.Now(), Worktree: "w", Body: []byte("x")})
	}
	list := l.List("", 0)
	if len(list) != 3 || list[0].ID != "req_4" || list[0].Body != nil {
		t.Fatalf("bounded list: %+v", list)
	}
	if _, ok := l.Get("req_0"); ok {
		t.Fatal("oldest should be evicted")
	}
	if e, ok := l.Get("req_4"); !ok || string(e.Body) != "x" {
		t.Fatal("get should include body")
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("log file mode %v", st.Mode())
	}
	l2, err := Open(path, settings())
	if err != nil || len(l2.List("", 0)) != 3 {
		t.Fatalf("reload: %v", err)
	}
	// Age-based pruning.
	s := settings()
	s.MaxAge = time.Hour
	l3, _ := Open("", s)
	l3.Add(Entry{ID: "old", Time: time.Now().Add(-2 * time.Hour)})
	l3.Add(Entry{ID: "new", Time: time.Now()})
	if got := l3.List("", 0); len(got) != 1 || got[0].ID != "new" {
		t.Fatalf("age prune: %+v", got)
	}
	// Disabled capture stores nothing.
	s.Enabled = false
	l4, _ := Open("", s)
	l4.Add(Entry{ID: "x", Time: time.Now()})
	if len(l4.List("", 0)) != 0 {
		t.Fatal("disabled capture stored an entry")
	}
}
