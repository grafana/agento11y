// Package harbor launches the Python Harbor integration behind `agento11y harbor`.
package harbor

import (
	"errors"
	"io"
	"os"
	"os/exec"

	"github.com/grafana/agento11y/plugins/agento11y/internal/launcher"
)

const bridgeBin = "agento11y-harbor"

// Test seams.
var (
	lookPath = exec.LookPath
	execFn   = launcher.DefaultExec
)

// Run hands off to the installed agento11y-harbor bridge.
func Run(args []string, _ io.Writer) error {
	bin, err := lookPath(bridgeBin)
	if err != nil {
		return errors.New("Harbor integration is not installed; install `agento11y-harbor` in Harbor's Python environment and ensure its scripts directory is on PATH")
	}
	return launcher.Exec(execFn, bin, bridgeBin, args, os.Environ())
}
