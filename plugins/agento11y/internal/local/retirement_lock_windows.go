//go:build windows

package local

func withRetirementReadLock(_ string, run func() error) error {
	return run()
}

func withRetirementWriteLock(_ string, run func() error) error {
	return run()
}
