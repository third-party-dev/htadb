//go:build !windows

package cli

import (
	"os/exec"
	"syscall"
)

func setProcAttr(cmd *exec.Cmd) {}

// Interrupt sends SIGINT (Ctrl+C) to the process.
func Interrupt(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Signal(syscall.SIGINT)
}

// Signal sends a named signal ("SIGINT", "SIGTERM", "SIGKILL", "SIGQUIT").
func Signal(cmd *exec.Cmd, name string) error {
	if cmd.Process == nil {
		return nil
	}
	sig := map[string]syscall.Signal{"SIGINT": syscall.SIGINT, "SIGTERM": syscall.SIGTERM, "SIGKILL": syscall.SIGKILL, "SIGQUIT": syscall.SIGQUIT, "SIGHUP": syscall.SIGHUP}[name]
	if sig == 0 {
		sig = syscall.SIGINT
	}
	return cmd.Process.Signal(sig)
}
