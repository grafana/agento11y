//go:build windows

package dotenv

import (
	"io/fs"
	"syscall"
	"testing"
)

// wantWrittenPerm is what os.Stat reports for a writable file on Windows,
// where the mode only carries the read-only attribute.
const wantWrittenPerm fs.FileMode = 0o666

// makeUnreadable keeps an exclusive handle on path until the test ends, so
// every other open fails with a sharing violation. Windows has no equivalent
// of chmod 000: clearing permission bits still leaves the file readable.
func makeUnreadable(t *testing.T, path string) {
	t.Helper()
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := syscall.CreateFile(
		name,
		syscall.GENERIC_READ,
		0,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatalf("exclusive open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = syscall.CloseHandle(handle) })
}
