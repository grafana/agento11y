//go:build windows

package local

// The local receiver is unsupported on Windows, so no daemon migrates a
// shared store here.
func withMigrationLock(_ string, run func() error) error {
	return run()
}
