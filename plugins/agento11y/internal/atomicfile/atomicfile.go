// Package atomicfile replaces an agent config file through a temp file in the
// same directory plus a rename, so no reader ever sees a half-written file,
// and skips the write when the content already matches.
package atomicfile

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteIfChanged replaces path with content through a temp file in the same
// directory plus a rename, so a crash or a failed write never leaves a
// partially written file or a stray temp file. The parent directory is created
// with 0755 before umask when it is missing, and the new file gets mode.
//
// When the file already holds exactly content, nothing is written, wrote is
// false, and the mode on disk is left alone. An existing file that cannot be
// read counts as different and is rewritten.
func WriteIfChanged(path string, content []byte, mode fs.FileMode) (wrote bool, err error) {
	if existing, readErr := os.ReadFile(path); readErr == nil && bytes.Equal(existing, content) {
		return false, nil
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return false, fmt.Errorf("temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		cleanup()
		return false, fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return false, fmt.Errorf("chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return false, fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return false, fmt.Errorf("rename to %s: %w", path, err)
	}
	return true, nil
}
