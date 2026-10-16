//go:build !windows

package guardeval

import (
	"os"
	"testing"
)

// makeUnreadable drops every permission bit so os.Open fails with EACCES.
func makeUnreadable(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
}
