package slug

import (
	"strings"
	"testing"
)

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"feature-auth":         "feature-auth",
		"Feature/Auth":         "feature-auth",
		"feat/JIRA-123_login!": "feat-jira-123-login",
		"--weird--":            "weird",
		"café/crème":           "cafe-creme",
		"a..b":                 "a-b",
		"UPPER":                "upper",
		"dependabot/npm/x@1.2": "dependabot-npm-x-1-2",
	}
	for in, want := range cases {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeEmptyAndLong(t *testing.T) {
	if got := Sanitize("///"); !strings.HasPrefix(got, "wt-") || len(got) != 7 {
		t.Fatalf("empty fallback = %q", got)
	}
	if Sanitize("日本") == Sanitize("中文") {
		t.Fatal("distinct non-ascii names must not collide")
	}
	long1 := strings.Repeat("a", 80) + "1"
	long2 := strings.Repeat("a", 80) + "2"
	g1, g2 := Sanitize(long1), Sanitize(long2)
	if len(g1) > MaxLabel || len(g2) > MaxLabel {
		t.Fatalf("too long: %d %d", len(g1), len(g2))
	}
	if g1 == g2 {
		t.Fatal("long names must stay distinct")
	}
	if !ValidLabel(g1) {
		t.Fatalf("result not valid: %q", g1)
	}
}

func TestWithSuffix(t *testing.T) {
	s := WithSuffix("feature-auth", "key")
	if !strings.HasPrefix(s, "feature-auth-") || len(s) != len("feature-auth")+5 {
		t.Fatalf("got %q", s)
	}
	long := WithSuffix(strings.Repeat("b", 63), "k")
	if len(long) > MaxLabel || !ValidLabel(long) {
		t.Fatalf("bad long suffix %q", long)
	}
}

func TestValidHostname(t *testing.T) {
	if !ValidHostname("api.feature-auth.myapp.localhost") {
		t.Fatal("expected valid")
	}
	for _, h := range []string{"", "a..b", "-a.b", "A.b", "a_b.c"} {
		if ValidHostname(h) {
			t.Errorf("%q should be invalid", h)
		}
	}
}
