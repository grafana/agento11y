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

// interrupts absorbs console interrupts for the launcher. It is never read and
// never stopped: see DefaultExec.
var interrupts = make(chan os.Signal, 1)

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

	// Windows sends Ctrl+C and Ctrl+Break to every process on the console, so
	// the agent gets them without help. The launcher must survive them to
	// report the agent's exit code. signal.Ignore does not do that on Windows:
	// the runtime passes an unwatched event to the default handler, which ends
	// the process. The registration outlives this call because a late event
	// would otherwise end the launcher as it exits with the agent's code.
	signal.Notify(interrupts, os.Interrupt)
	if err := cmd.Run(); err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
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
