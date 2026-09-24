//go:build windows

package main

import (
	"os"
	"syscall"
)

func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

var forwardedSignals = []os.Signal{}

var interruptSignals = []os.Signal{os.Interrupt}
