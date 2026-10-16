package local

import (
	"errors"
	"path/filepath"
)

// errLockBusy reports that a non-blocking lock request found the lock held
// by another process.
var errLockBusy = errors.New("lock held by another process")

// acquireDaemonLock takes the lock that serialises daemon starts under dir.
// It waits for a concurrent EnsureRunning to finish rather than failing, so
// the caller that loses the race sees the daemon the winner started.
func acquireDaemonLock(dir string) (*daemonLock, error) {
	return acquireFileLock(filepath.Join(dir, LockFile), true)
}
