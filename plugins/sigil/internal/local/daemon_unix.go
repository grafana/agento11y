//go:build !windows

package local

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type daemonLock struct {
	f *os.File
}

func acquireDaemonLock(dir string) (*daemonLock, error) {
	path := filepath.Join(dir, LockFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lockfile: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("flock: %w", err)
	}
	return &daemonLock{f: f}, nil
}

func (l *daemonLock) release() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}

// startDaemon launches `sigil local serve` as a detached child process.
// The parent waits for the child to write its status file, then returns
// the recorded endpoint. The child detaches by setting its own session
// (SysProcAttr.Setsid) so it survives the parent exiting.
func startDaemon(ctx context.Context, dir string, logger *log.Logger) (*Status, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}
	bin, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve sigil binary: %w", err)
	}

	logPath := filepath.Join(dir, "server.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}

	cmd := exec.Command(bin, "local", "serve")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Inherit env so SIGIL_DEBUG and XDG_* flow through.
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("start daemon: %w", err)
	}
	// Close the log handle in this process; the child has its own copy.
	_ = logFile.Close()

	// Wait up to ~5s for the child to write its status file.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			_ = cmd.Process.Kill()
			return nil, ctx.Err()
		}
		if s, err := IsRunning(dir); err == nil && s != nil {
			if logger != nil {
				logger.Printf("local: daemon started pid=%d port=%d", s.PID, s.Port)
			}
			return s, nil
		}
		// Check the child exited prematurely so we don't block forever.
		var ws syscall.WaitStatus
		pid, _ := syscall.Wait4(cmd.Process.Pid, &ws, syscall.WNOHANG, nil)
		if pid == cmd.Process.Pid {
			body, _ := os.ReadFile(logPath)
			return nil, fmt.Errorf("daemon exited prematurely: %s", strings.TrimSpace(string(body)))
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	return nil, fmt.Errorf("daemon did not become ready within 5s")
}

// pidAlive reports whether a process with the given PID exists by
// sending signal 0 (no-op probe).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

// terminateProcess sends SIGTERM to pid for a graceful shutdown.
func terminateProcess(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.SIGTERM)
}

func processCommandLine(pid int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "command=")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
