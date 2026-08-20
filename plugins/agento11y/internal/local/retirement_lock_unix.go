//go:build !windows

package local

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func withRetirementReadLock(dir string, run func() error) error {
	return withRetirementLock(filepath.Join(dir, RetirementLockFile), syscall.LOCK_SH, run)
}

func withRetirementWriteLock(dir string, run func() error) error {
	return withRetirementLock(filepath.Join(dir, RetirementLockFile), syscall.LOCK_EX, run)
}

func withRetirementLock(path string, mode int, run func() error) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open retirement lockfile: %w", err)
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), mode); err != nil {
		return fmt.Errorf("lock retirement file: %w", err)
	}
	defer func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN) }()
	return run()
}
