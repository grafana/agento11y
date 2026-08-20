//go:build !windows

package local

import "os"

// readShared reads path. The Windows build needs its own opener; on unix a
// plain read already lets a writer rename over the file mid-read.
func readShared(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// replaceFile renames tmp onto path, replacing whatever is there.
func replaceFile(tmp, path string) error {
	return os.Rename(tmp, path)
}
