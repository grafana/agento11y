//go:build windows

package local

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

func withRetirementReadLock(dir string, run func() error) error {
	return withRetirementLock(filepath.Join(dir, RetirementLockFile), 0, run)
}

func withRetirementWriteLock(dir string, run func() error) error {
	return withRetirementLock(filepath.Join(dir, RetirementLockFile), windows.LOCKFILE_EXCLUSIVE_LOCK, run)
}

func withRetirementLock(path string, flags uint32, run func() error) error {
	lock, err := acquireWindowsFileLock(path, true, flags)
	if err != nil {
		return err
	}
	defer lock.release()
	return run()
}
