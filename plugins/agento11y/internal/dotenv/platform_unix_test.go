//go:build !windows

package dotenv

import (
	"io/fs"
	"os"
	"testing"
)

// wantWrittenPerm is the mode WriteDotenv leaves on the config file.
const wantWrittenPerm fs.FileMode = 0o600

// makeUnreadable drops every permission bit so os.Open fails with EACCES.
func makeUnreadable(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
}
