//go:build windows

package local

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// unstatablePath returns a path whose os.Stat fails with something other than
// "not found". A parent that is a file answers ERROR_PATH_NOT_FOUND here,
// which Go reports as absence, so the path carries a character no Windows
// name may hold and stat fails with ERROR_INVALID_NAME.
func unstatablePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "bad?name", "config.env")
}

// makeUnreadable denies reads of path and returns the undo. Windows has no
// chmod, so the file is held open with no sharing, which fails every other
// opener with a sharing violation and leaves size and mtime untouched.
func makeUnreadable(t *testing.T, path string) func() {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	require.NoError(t, err)
	h, err := windows.CreateFile(p, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	require.NoError(t, err)
	return func() { require.NoError(t, windows.CloseHandle(h)) }
}
