//go:build !windows

package local

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// daemonLock is a held file lock. The lock lives on the open file
// description, so closing the file releases it even if the process dies
// without calling release.
type daemonLock struct {
	f *os.File
}

// acquireFileLock takes an exclusive flock on path, creating the file when
// it is missing. wait blocks until the lock is free; without it a lock
// another process holds returns errLockBusy straight away.
func acquireFileLock(path string, wait bool) (*daemonLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lockfile: %w", err)
	}
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLockBusy
		}
		return nil, fmt.Errorf("flock: %w", err)
	}
	return &daemonLock{f: f}, nil
}

func (l *daemonLock) release() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}
