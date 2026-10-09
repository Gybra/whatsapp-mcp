//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// spawnDetached starts binary in its own session, so it outlives the terminal or Pi that started it.
func spawnDetached(binary string, args ...string) error {
	cmd := exec.Command(binary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
