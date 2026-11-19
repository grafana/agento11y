//go:build !windows

package entry

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func cleanupAgentProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// Codex can exit successfully while its marketplace Git clone is still running.
	err := cmd.Cancel()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func configureAgentProcess(cmd *exec.Cmd) {
	// The npm launcher and its native child must share the cancellation target.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
