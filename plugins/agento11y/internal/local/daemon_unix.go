//go:build !windows

package local

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ReceiverSupported reports whether this platform can run the local
// capture daemon. Unix does, so hook dispatch may point agents at the
// loopback endpoint instead of leaving Cloud credentials in place.
func ReceiverSupported() bool { return true }

// daemonSysProcAttr detaches the daemon from the process that spawns it by
// giving it its own session, so it survives the parent exiting.
func daemonSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// childExited reports whether the daemon this process spawned has already
// exited, and reaps it when it has. Only the parent may call it: Wait4
// answers for its own children.
func childExited(pid int) bool {
	var ws syscall.WaitStatus
	reaped, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
	return err == nil && reaped == pid
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

type unixDaemonProcess struct {
	status Status
}

func openDaemonProcess(status Status) (daemonProcess, error) {
	if !pidAlive(status.PID) {
		return nil, nil
	}
	matches, err := processMatchesDaemon(status)
	if err != nil {
		return nil, err
	}
	if !matches {
		return nil, nil
	}
	return &unixDaemonProcess{status: status}, nil
}

func (p *unixDaemonProcess) alive() bool { return pidAlive(p.status.PID) }

func (p *unixDaemonProcess) terminate() error {
	if !p.alive() {
		return nil
	}
	matches, err := processMatchesDaemon(p.status)
	if err != nil {
		if !p.alive() {
			return nil
		}
		return err
	}
	if !matches {
		return nil
	}
	return terminateProcess(p.status.PID)
}

func (p *unixDaemonProcess) close() {}

// processCommandLineFn is a test seam for reading a live process's command
// line.
var processCommandLineFn = processCommandLine

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

// processMatchesDaemon reports whether the live process behind s.PID is the
// daemon s describes. Unix reads the process command line, which names the
// binary and the "local serve" arguments it was started with, so the
// recorded start time adds nothing here.
func processMatchesDaemon(s Status) (bool, error) {
	cmdline, err := processCommandLineFn(s.PID)
	if err != nil {
		return false, err
	}
	cmdline = strings.TrimSpace(cmdline)
	const daemonArgs = " local serve"
	if !strings.HasSuffix(cmdline, daemonArgs) {
		return false, nil
	}
	exe := strings.TrimSpace(strings.TrimSuffix(cmdline, daemonArgs))
	if exe == "" {
		return false, nil
	}
	return imageIsDaemon(exe), nil
}
