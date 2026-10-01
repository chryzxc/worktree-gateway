//go:build !windows

package daemon

import (
	"io"
	"log"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/chryzxc/worktree-gateway/internal/config"
	"github.com/chryzxc/worktree-gateway/internal/registry"
)

// The registered PID is `sh -c`; its dev server is a child sharing the test's
// process group (as with `wtg run`), and must die with it.
func TestStopProcessKillsTree(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 60; true") // `; true` keeps sh from exec'ing sleep
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait() // reap, as `wtg run` does
	pid := cmd.Process.Pid
	var kids []int
	for i := 0; i < 50 && len(kids) == 0; i++ {
		time.Sleep(20 * time.Millisecond)
		kids = descendants(pid)
	}
	if len(kids) != 1 {
		t.Fatalf("descendants(sh) = %v, want the sleep", kids)
	}
	stopProcess(pid)
	if ProcessAlive(kids[0]) {
		syscall.Kill(kids[0], syscall.SIGKILL)
		t.Fatal("child still alive after stopProcess")
	}
}

func TestEnforceLimits(t *testing.T) {
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := reg.UpsertWorktree(registry.WorktreeInput{CommonDir: "/r/.git", MainPath: "/r", AdminID: ".", Branch: "main", Path: "/r", IsMain: true})
	if err != nil {
		t.Fatal(err)
	}
	start := func(name string, port int) int {
		cmd := exec.Command("sh", "-c", "sleep 60; true")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go cmd.Wait()
		t.Cleanup(func() { cmd.Process.Kill() })
		if _, _, err := reg.Register(registry.RegisterInput{WorktreeID: wt.ID, Name: name, Port: port, PID: cmd.Process.Pid}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond) // distinct RegisteredAt
		return cmd.Process.Pid
	}
	old, young := start("api", 20001), start("web", 20002)
	d := &Daemon{reg: reg, log: log.New(io.Discard, "", 0), g: config.Global{MaxServices: 1}}

	d.enforceLimits() // over the cap: the oldest goes
	if ProcessAlive(old) || !ProcessAlive(young) {
		t.Fatal("max_services should stop only the oldest service")
	}
	d.g = config.Global{ServiceTTL: time.Nanosecond}
	d.enforceLimits() // past its ttl
	if ProcessAlive(young) {
		t.Fatal("service_ttl should stop the service")
	}
	if n := len(reg.Snapshot().Services); n != 0 {
		t.Fatalf("%d services still registered", n)
	}
}
