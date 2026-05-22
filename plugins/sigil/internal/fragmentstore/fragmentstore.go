// Package fragmentstore holds the on-disk JSON storage primitives shared by
// the agent fragment packages: atomic writes, tolerant reads, advisory file
// locking, and stale-file cleanup.
//
// Agent-specific policy stays in the agent packages. codex and copilot lock
// their writes and sweep stale files on a timer; cursor uses an unlocked,
// conversation-scoped layout with quarantine and whole-directory removal.
// Callers opt into locking by calling WithFileLock, and into cleanup by
// calling CleanupStaleDir. Everything is written private to the user:
// 0o700 directories, 0o600 files — fragments may carry prompt text.
package fragmentstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

const (
	// DefaultLockTimeout bounds how long WithFileLock waits to acquire a lock
	// before giving up. DefaultStaleLockAge is how old a lock file may be
	// before it's treated as abandoned and reclaimed.
	DefaultLockTimeout  = 2 * time.Second
	DefaultStaleLockAge = 2 * time.Minute

	dirMode  os.FileMode = 0o700
	fileMode os.FileMode = 0o600
)

// WriteJSON marshals v and writes it to target atomically: it writes a temp
// file in the same directory, fsync-free closes it, chmods it to 0o600, then
// renames it over target. Parent directories are created 0o700 on demand. A
// crash between write and rename can't leave a partial file under target's
// deterministic name.
func WriteJSON(target string, v any) error {
	if err := os.MkdirAll(filepath.Dir(target), dirMode); err != nil {
		return fmt.Errorf("fragment: mkdir: %w", err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("fragment: marshal: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), filepath.Base(target)+".*.tmp")
	if err != nil {
		return fmt.Errorf("fragment: create tmp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fragment: write tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("fragment: close tmp: %w", err)
	}
	if err := os.Chmod(tmpName, fileMode); err != nil {
		return fmt.Errorf("fragment: chmod tmp: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("fragment: rename: %w", err)
	}
	return nil
}

// ReadJSON reads target and unmarshals JSON into a fresh T. It separates the
// four outcomes tolerant callers care about so they can log and recover as
// they see fit:
//
//   - missing file:   (nil, false, nil)
//   - read failure:   (nil, false, err)
//   - corrupt JSON:   (nil, true, err)
//   - success:        (&v,  false, nil)
//
// The corrupt flag lets callers reproduce their existing "read" vs "corrupt"
// log wording exactly while sharing the read/unmarshal mechanics.
func ReadJSON[T any](target string) (*T, bool, error) {
	raw, err := os.ReadFile(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, true, err
	}
	return &v, false, nil
}

// LogLoadErr logs a tolerant-load failure from ReadJSON as either a "read" or
// "corrupt" message, keyed off the corrupt flag ReadJSON returns. label is an
// optional entity prefix inserted before the path ("session ", "subagent link ",
// or "" for a plain fragment). A nil logger is a no-op so callers can forward
// their logger unconditionally.
func LogLoadErr(logger *log.Logger, label, path string, corrupt bool, err error) {
	if logger == nil {
		return
	}
	if corrupt {
		logger.Printf("fragment: corrupt %s%s: %v", label, path, err)
		return
	}
	logger.Printf("fragment: read %s%s: %v", label, path, err)
}

// WithFileLock acquires an exclusive advisory lock for target by creating
// <target>.lock with O_EXCL, runs fn, then removes the lock. It waits up to
// DefaultLockTimeout, reclaiming a lock file older than DefaultStaleLockAge so
// a crashed holder can't wedge the path forever. The lock directory is created
// with 0o700.
func WithFileLock(target string, fn func() error) error {
	lockPath := target + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), dirMode); err != nil {
		return fmt.Errorf("fragment: mkdir lock dir: %w", err)
	}
	deadline := time.Now().Add(DefaultLockTimeout)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fileMode)
		if err == nil {
			_, _ = fmt.Fprintf(f, "pid=%d\ncreated=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
			_ = f.Close()
			defer func() { _ = os.Remove(lockPath) }()
			return fn()
		}
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("fragment: create lock: %w", err)
		}
		if isStaleLock(lockPath, time.Now()) {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("fragment: lock timeout: %s", lockPath)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func isStaleLock(path string, now time.Time) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return now.Sub(info.ModTime()) > DefaultStaleLockAge
}

// CleanupStaleDir removes regular .json files directly or recursively under dir
// whose mod time is at or before cutoff, skipping skipPath. A missing dir is a
// no-op. Walk, stat, and remove errors are logged via logger (when non-nil) and
// never abort the walk, so one bad entry can't block the rest of the sweep.
func CleanupStaleDir(dir string, cutoff time.Time, logger *log.Logger, skipPath string) error {
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if logger != nil {
				logger.Printf("fragment: cleanup walk %s: %v", path, err)
			}
			return nil
		}
		if path == dir || d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".json" {
			return nil
		}
		if skipPath != "" && path == skipPath {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if logger != nil {
				logger.Printf("fragment: cleanup stat %s: %v", path, err)
			}
			return nil
		}
		if info.ModTime().After(cutoff) {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) && logger != nil {
			logger.Printf("fragment: cleanup remove %s: %v", path, err)
		}
		return nil
	})
}
