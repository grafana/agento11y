//go:build !linux && !darwin && !windows

package local

import "math"

// freeDiskBytes does not measure free space here: Statfs_t differs across
// the BSDs, and NetBSD has no Statfs. An error would stop every migration
// pass, so report unlimited space.
func freeDiskBytes(string) (uint64, error) {
	return math.MaxUint64, nil
}
