//go:build windows

package local

import (
	"context"
	"errors"
	"log"
)

// errLocalUnsupported is returned by every daemon entry point on Windows.
// The local receiver relies on Unix process semantics (flock, Setsid,
// signal-based liveness and termination) that have no portable equivalent
// here, so `--local` capture mode is unavailable. Default Cloud mode never
// touches this code path.
var errLocalUnsupported = errors.New("agento11y local receiver is not supported on Windows")

type daemonLock struct{}

func acquireDaemonLock(dir string) (*daemonLock, error) { return nil, errLocalUnsupported }

func (l *daemonLock) release() {}

func startDaemon(ctx context.Context, dir string, logger *log.Logger) (*Status, error) {
	return nil, errLocalUnsupported
}

func pidAlive(pid int) bool { return false }

func terminateProcess(pid int) error { return errLocalUnsupported }

func processCommandLine(pid int) (string, error) { return "", errLocalUnsupported }
