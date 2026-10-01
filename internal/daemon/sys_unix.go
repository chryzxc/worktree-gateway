//go:build !windows

package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// stopProcess sends SIGTERM to pid and all its descendants (the registered
// PID is `sh -c`; the dev server is a child of it, sharing `wtg run`'s process
// group, so neither a single kill nor a group kill would reach only that
// tree), then SIGKILL to whatever is still alive after 5s.
// ponytail: trusts the registry PID; a PID reused after the daemon missed the
// exit would be signalled too (the health loop prunes dead PIDs within seconds).
func stopProcess(pid int) {
	tree := append(descendants(pid), pid)
	alive := func() bool {
		for _, p := range tree {
			if ProcessAlive(p) {
				return true
			}
		}
		return false
	}
	for _, p := range tree {
		syscall.Kill(p, syscall.SIGTERM)
	}
	for deadline := time.Now().Add(5 * time.Second); alive() && time.Now().Before(deadline); {
		time.Sleep(100 * time.Millisecond)
	}
	for _, p := range tree {
		syscall.Kill(p, syscall.SIGKILL)
	}
}

// descendants lists every process below pid, from one `ps` snapshot.
func descendants(pid int) []int {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=").Output()
	if err != nil {
		return nil
	}
	children := map[int][]int{}
	for _, line := range strings.Split(string(out), "\n") {
		var p, pp int
		if n, _ := fmt.Sscan(line, &p, &pp); n == 2 {
			children[pp] = append(children[pp], p)
		}
	}
	var all []int
	for queue := children[pid]; len(queue) > 0; queue = queue[1:] {
		all = append(all, queue[0])
		queue = append(queue, children[queue[0]]...)
	}
	return all
}

// ProcessAlive reports whether a process exists (EPERM means it exists but
// belongs to someone else).
func ProcessAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// lockFile takes an exclusive, non-blocking lock on path for the lifetime of
// the process (the kernel drops it if the process dies).
func lockFile(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}
