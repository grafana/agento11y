//go:build !windows

package local

import "path/filepath"

func withMigrationLock(dir string, run func() error) error {
	lock, err := acquireFileLock(filepath.Join(dir, MigrationLockFile), false)
	if err != nil {
		return err
	}
	defer lock.release()
	return run()
}
