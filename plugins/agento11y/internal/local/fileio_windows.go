//go:build windows

package local

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

// replaceRetries and replaceRetryDelay bound the wait for a reader that holds
// the destination open. A reader of ours is gone within microseconds; the
// budget covers a foreign one, such as an editor or a virus scanner.
const (
	replaceRetries    = 50
	replaceRetryDelay = 10 * time.Millisecond
)

// readShared reads path with delete sharing on, so a writer may rename a new
// version over the file while this read is in flight. os.ReadFile opens
// without FILE_SHARE_DELETE, which makes that writer's rename fail with
// "Access is denied".
func readShared(path string) ([]byte, error) {
	p, err := windows.UTF16PtrFromString(extendedPath(path))
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(
		p,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(h), path)
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

// extendedPath lifts the MAX_PATH limit on a long path, which the os package
// does for its own callers and CreateFile does not.
func extendedPath(path string) string {
	const maxPath = 260
	if len(path) < maxPath || strings.HasPrefix(path, `\\?\`) {
		return path
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if rest, ok := strings.CutPrefix(abs, `\\`); ok {
		return `\\?\UNC\` + rest
	}
	return `\\?\` + abs
}

// replaceFile renames tmp onto path, replacing whatever is there. Windows
// refuses the rename while another handle holds the destination open without
// delete sharing, so the attempt is repeated for a bounded time.
func replaceFile(tmp, path string) error {
	var err error
	for attempt := range replaceRetries {
		err = os.Rename(tmp, path)
		if err == nil || !heldByAnotherHandle(err) {
			return err
		}
		if attempt < replaceRetries-1 {
			time.Sleep(replaceRetryDelay)
		}
	}
	return err
}

// heldByAnotherHandle reports whether err is the rename failure Windows
// returns for a destination another handle still has open.
func heldByAnotherHandle(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
