package local

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Both platform locks make two handles in one process contend, so this test
// does not need a helper process.
func TestAcquireFileLock(t *testing.T) {
	cases := []struct {
		name         string
		hold         bool
		releaseFirst bool
		wantErr      error
	}{
		{name: "a free lock is taken"},
		{name: "a held lock reports the holder", hold: true, wantErr: errLockBusy},
		{name: "a released lock is free again", hold: true, releaseFirst: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), MigrationLockFile)
			if tc.hold {
				held, err := acquireFileLock(path, false)
				require.NoError(t, err)
				if tc.releaseFirst {
					held.release()
				} else {
					t.Cleanup(held.release)
				}
			}

			lock, err := acquireFileLock(path, false)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				assert.Nil(t, lock, "a refused lock hands back nothing to release")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, lock)
			lock.release()
		})
	}
}
