//go:build windows

package daemon

import (
	"os"

	"golang.org/x/sys/windows"
)

// ProcessAlive reports whether a process exists.
func ProcessAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == 259 // STILL_ACTIVE
}

// lockFile takes an exclusive, non-blocking lock on path for the lifetime of
// the process (the kernel drops it if the process dies).
func lockFile(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	ol := new(windows.Overlapped)
	h := windows.Handle(f.Fd())
	if err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol); err != nil {
		f.Close()
		if err == windows.ERROR_LOCK_VIOLATION {
			return nil, errLocked
		}
		return nil, err
	}
	return func() { windows.UnlockFileEx(h, 0, 1, 0, ol); f.Close() }, nil
}
