//go:build windows

package cli

import "os/exec"

func setProcAttr(cmd *exec.Cmd) {}

// Interrupt kills the process: Windows has no SIGINT for console-less
// children.
func Interrupt(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// Signal kills the process regardless of name on Windows.
func Signal(cmd *exec.Cmd, name string) error { return Interrupt(cmd) }
