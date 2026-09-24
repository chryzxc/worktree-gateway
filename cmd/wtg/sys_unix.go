//go:build !windows

package main

import (
	"os"
	"syscall"
)

func detachAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// forwardedSignals are relayed to `wtg run` children. SIGINT is not: the
// child shares the terminal's foreground process group and already gets it.
var forwardedSignals = []os.Signal{syscall.SIGTERM, syscall.SIGHUP}

var interruptSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}
