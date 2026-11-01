package entry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"
)

func configureAgentProcess(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).CombinedOutput()
		if err != nil {
			if errors.Is(cmd.Process.Kill(), os.ErrProcessDone) {
				return os.ErrProcessDone
			}
			return fmt.Errorf("kill agent process tree: %w: %s", err, out)
		}
		return nil
	}
}
