//go:build !windows

package local

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// unstatablePath returns a path whose os.Stat fails with something other than
// "not found". A parent that is a file answers ENOTDIR.
func unstatablePath(t *testing.T) string {
	t.Helper()
	parent := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(parent, []byte("x"), 0o600))
	return filepath.Join(parent, "config.env")
}

// makeUnreadable denies reads of path and returns the undo, which restores
// access without changing the file's size or mtime.
func makeUnreadable(t *testing.T, path string) func() {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	require.NoError(t, os.Chmod(path, 0o000))
	return func() { require.NoError(t, os.Chmod(path, 0o600)) }
}
