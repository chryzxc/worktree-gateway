//go:build !windows

package daemon

import (
	"errors"
	"os"
	"syscall"
)

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
