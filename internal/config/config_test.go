package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sample = `
project: innovacare
domain: innovacare.localhost
services:
  web:
    command: npm run dev
    port: auto
  api:
    command: npm run server
    port: 4000
    hostname: "api.{worktree}.{domain}"
    public:
      paths: ["/webhooks/"]
    oauth_callbacks: ["/auth/callback"]
  storybook:
    command: npm run storybook
`

func TestParseProject(t *testing.T) {
	p, err := ParseProject([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if p.Project != "innovacare" || len(p.Services) != 3 {
		t.Fatalf("%+v", p)
	}
	if p.Services["api"].Port != 4000 || p.Services["web"].Port != 0 {
		t.Fatal("ports parsed wrong")
	}
	if p.Default() != "web" {
		t.Fatalf("default = %s", p.Default())
	}
	if got := p.HostnameTemplate("storybook"); got != "{service}.{worktree}.{domain}" {
		t.Fatal(got)
	}
	if got := p.HostnameTemplate("web"); got != "{worktree}.{domain}" {
		t.Fatal(got)
	}
	if !p.MainAliasEnabled() {
		t.Fatal("main alias should default on")
	}
}

func TestParseProjectErrors(t *testing.T) {
	bad := map[string]string{
		"unknown field":    "servics: {}",
		"bad port":         "services: {web: {port: 99999}}",
		"template var":     "services: {web: {hostname: \"{branch}.x\"}}",
		"no worktree":      "services: {web: {hostname: \"web.x\"}}",
		"public w/o paths": "services: {web: {public: {}}}",
		"relative public":  "services: {web: {public: {paths: [hooks]}}}",
		"default missing":  "default_service: nope\nservices: {web: {}}",
	}
	for name, doc := range bad {
		if _, err := ParseProject([]byte(doc)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestLoadProjectMissingAndEmpty(t *testing.T) {
	dir := t.TempDir()
	p, err := LoadProject(dir)
	if err != nil || p.Path() != "" || p.Services == nil {
		t.Fatalf("missing file: %v %+v", err, p)
	}
	if err := os.WriteFile(filepath.Join(dir, "wtg.yaml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	p, err = LoadProject(dir)
	if err != nil || !strings.HasSuffix(p.Path(), "wtg.yaml") {
		t.Fatalf("empty file: %v", err)
	}
}

func TestLoadGlobal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	g, err := LoadGlobal(path)
	if err != nil || g.HTTPPort != 8780 {
		t.Fatalf("defaults: %v %+v", err, g)
	}
	os.WriteFile(path, []byte("http_port: 9000\ningress: {hold: 3s}\ntunnel: {provider: ngrok}\n"), 0o644)
	g, err = LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	if g.HTTPPort != 9000 || g.HTTPSPort != 8743 || g.Ingress.Hold != 3*time.Second || g.Ingress.Port != 8790 || g.Tunnel.Provider != "ngrok" {
		t.Fatalf("merge wrong: %+v", g)
	}
	os.WriteFile(path, []byte("listen_host: 0.0.0.0\n"), 0o644)
	if _, err := LoadGlobal(path); err == nil {
		t.Fatal("non-loopback listen host must be rejected")
	}
}
