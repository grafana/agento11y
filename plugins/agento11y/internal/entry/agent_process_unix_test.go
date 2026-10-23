//go:build !windows

package entry

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

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
