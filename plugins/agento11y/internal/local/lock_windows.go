//go:build windows

package local

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// LockFileEx locks byte ranges. All callers use the maximum range from byte
// zero so their locks exclude each other.
const lockRegionLow, lockRegionHigh = ^uint32(0), ^uint32(0)

// daemonLock is a held file lock. The lock lives on the file handle, so
// Windows drops it when the process dies without calling release.
type daemonLock struct {
	f *os.File
}

// acquireFileLock takes an exclusive LockFileEx range lock on path, creating
// the file when it is missing. wait blocks until the lock is free; without
// it a lock another handle holds returns errLockBusy straight away.
//
// A second handle in the same process contends with the first. This matches
// flock and lets the shared contention test use two handles in one process.
func acquireFileLock(path string, wait bool) (*daemonLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lockfile: %w", err)
	}
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	if !wait {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	if err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, lockRegionLow, lockRegionHigh, new(windows.Overlapped)); err != nil {
		_ = f.Close()
		// A LOCKFILE_FAIL_IMMEDIATELY request that cannot take the range
		// fails with ERROR_LOCK_VIOLATION, the counterpart of EWOULDBLOCK.
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errLockBusy
		}
		return nil, fmt.Errorf("LockFileEx: %w", err)
	}
	return &daemonLock{f: f}, nil
}

func (l *daemonLock) release() {
	_ = windows.UnlockFileEx(windows.Handle(l.f.Fd()), 0, lockRegionLow, lockRegionHigh, new(windows.Overlapped))
	_ = l.f.Close()
}
