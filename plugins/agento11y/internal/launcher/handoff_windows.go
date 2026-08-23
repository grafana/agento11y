//go:build windows

package launcher

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

// DefaultExec runs the target agent as a child because Windows cannot replace
// the current process image. The caller exits with a non-zero child code.
func DefaultExec(argv0 string, argv []string, envv []string) error {
	cmd, err := handoffCommand(argv0, argv[1:])
	if err != nil {
		return err
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = envv

	// Windows sends Ctrl+C to every process attached to the console. Let the
	// child handle it while this launcher waits for the child's exit status.
	signal.Ignore(os.Interrupt)
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return &ExitError{Code: exitErr.ExitCode()}
		}
		return err
	}
	return nil
}

func handoffCommand(argv0 string, args []string) (*exec.Cmd, error) {
	ext := strings.ToLower(filepath.Ext(strings.TrimRight(argv0, ". ")))
	if ext != ".bat" && ext != ".cmd" {
		return exec.Command(argv0, args...), nil
	}

	commandLine, err := batchCommandLine(argv0, args)
	if err != nil {
		return nil, err
	}
	commandInterpreter := os.Getenv("ComSpec")
	if commandInterpreter == "" {
		commandInterpreter = "cmd.exe"
	}
	cmd := exec.Command(commandInterpreter)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: commandLine}
	return cmd, nil
}
