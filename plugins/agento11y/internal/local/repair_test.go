package local

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRepairStoreModTimes covers the modification-time pass over an
// existing store: a file written before appends stamped their activity gets
// the time its own records describe, a file that already agrees is not
// rewritten, and a file whose records are dated in the future is clamped to
// now rather than left at the time it was written.
func TestRepairStoreModTimes(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	fixtures := []struct {
		id string
		// activity is the newest completed_at in the file.
		activity time.Duration
		// modTime is what the file's modification time starts as, offset
		// from now, and want is what the pass must leave behind.
		modTime time.Duration
		want    time.Duration
	}{
		{id: "conv-imported", activity: -90 * 24 * time.Hour, modTime: 0, want: -90 * 24 * time.Hour},
		{id: "conv-live", activity: -time.Hour, modTime: -time.Hour, want: -time.Hour},
		{id: "conv-future", activity: 48 * time.Hour, modTime: -time.Minute, want: 0},
	}

	s := newStorage(t)
	for _, f := range fixtures {
		writeGen(t, s, f.id, "g-"+f.id, agento11y.Generation{
			Model:       agento11y.ModelRef{Name: "m"},
			StartedAt:   now.Add(f.activity).Add(-time.Minute),
			CompletedAt: now.Add(f.activity),
			Usage:       agento11y.TokenUsage{InputTokens: 1},
		}, now.Add(f.activity).Format(time.RFC3339Nano))
		setConversationModTime(t, s, f.id, now.Add(f.modTime))
	}
	stamps := countStamps(s)

	repaired, err := repairStoreModTimes(context.Background(), s)
	require.NoError(t, err)
	assert.True(t, repaired)

	for _, f := range fixtures {
		info, err := os.Stat(filepath.Join(s.Dir(), ConversationsDir, f.id+".jsonl"))
		require.NoError(t, err)
		assert.WithinDuration(t, now.Add(f.want), info.ModTime(), stampTolerance, f.id)
	}
	assert.Equal(t, int32(2), stamps.Load(), "only the files that disagreed are rewritten")

	marker, err := os.ReadFile(filepath.Join(s.Dir(), StoreFile))
	require.NoError(t, err)
	assert.JSONEq(t, `{"mtime_stamped": true}`, string(marker))
}

// errStampRefused stands in for a filesystem that will not set a
// modification time.
var errStampRefused = errors.New("read-only file system")

// TestRepairStoreModTimes_Outcomes covers what one pass over a store with
// two files leaves behind. Only a pass that stamped every file it walked
// writes the marker, so any other outcome runs the pass again on the next
// daemon start.
func TestRepairStoreModTimes_Outcomes(t *testing.T) {
	cases := []struct {
		name string
		// arrange decides what the pass runs into.
		arrange func(t *testing.T, s *Storage)
		// stampErr fails every stamp with this error.
		stampErr error
		// wantErr is what the pass must return, matched with errors.Is.
		wantErr error
		// wantWalked reports whether the pass walked the store, which is
		// what tells an open viewer to refetch.
		wantWalked bool
		wantMarker bool
		wantStamps int32
	}{
		{
			name: "a marked store is not walked again",
			arrange: func(t *testing.T, s *Storage) {
				require.NoError(t, writeStoreMeta(s.Dir(), storeMeta{MtimeStamped: true}))
			},
			wantMarker: true,
		},
		{
			name:       "a store no daemon has stamped repairs and marks itself",
			wantWalked: true,
			wantMarker: true,
			wantStamps: 2,
		},
		{
			name:     "a file that cannot be stamped leaves the marker unset",
			stampErr: errStampRefused,
			wantErr:  errStampRefused,
			// Every file is tried, so one bad file does not leave the rest
			// ordered by the time they were written.
			wantWalked: true,
			wantStamps: 2,
		},
		{
			name: "another daemon holding the lock skips this pass",
			arrange: func(t *testing.T, s *Storage) {
				held, err := acquireFileLock(filepath.Join(s.Dir(), RepairLockFile), false)
				require.NoError(t, err)
				t.Cleanup(held.release)
			},
			wantErr: errLockBusy,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStorage(t)
			for _, id := range []string{"conv-A", "conv-B"} {
				seedConversation(t, s, id, time.Now().Add(-90*24*time.Hour))
			}
			var stamps atomic.Int32
			s.chtimes = func(path string, atime, mtime time.Time) error {
				stamps.Add(1)
				if tc.stampErr != nil {
					return tc.stampErr
				}
				return os.Chtimes(path, atime, mtime)
			}
			if tc.arrange != nil {
				tc.arrange(t, s)
			}

			walked, err := repairStoreModTimes(context.Background(), s)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantWalked, walked)
			assert.Equal(t, tc.wantStamps, stamps.Load(), "files stamped")

			meta, err := readStoreMeta(s.Dir())
			require.NoError(t, err)
			assert.Equal(t, tc.wantMarker, meta.MtimeStamped)
		})
	}
}

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
			path := filepath.Join(t.TempDir(), RepairLockFile)
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

// TestRepairStoreModTimes_ExcludesConcurrentAppend covers the pass holding
// a file's append lock while it scans and stamps it, so a live export
// cannot write between the two.
func TestRepairStoreModTimes_ExcludesConcurrentAppend(t *testing.T) {
	s := newStorage(t)
	activity := time.Now().Add(-time.Hour).Truncate(time.Second)
	seedConversation(t, s, "conv-A", activity.Add(-90*24*time.Hour))

	// Hold the pass inside its stamp, which is inside the file's append
	// lock. The append below stamps too, so only the first call blocks.
	scanning := make(chan struct{})
	release := make(chan struct{})
	var hold sync.Once
	s.chtimes = func(path string, atime, mtime time.Time) error {
		hold.Do(func() {
			close(scanning)
			<-release
		})
		return os.Chtimes(path, atime, mtime)
	}

	repairDone := make(chan error, 1)
	go func() {
		_, err := repairStoreModTimes(context.Background(), s)
		repairDone <- err
	}()
	<-scanning

	appendDone := make(chan error, 1)
	go func() {
		_, err := s.AppendGenerations("conv-A", []generationRecord{{
			ConversationID: "conv-A",
			GenerationID:   "gen-live",
			Generation:     json.RawMessage(`{"id":"gen-live"}`),
		}}, []time.Time{activity})
		appendDone <- err
	}()

	select {
	case <-appendDone:
		t.Fatal("append ran while the pass held the file lock")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)

	require.NoError(t, <-repairDone)
	require.NoError(t, <-appendDone)

	info, err := os.Stat(filepath.Join(s.Dir(), ConversationsDir, "conv-A.jsonl"))
	require.NoError(t, err)
	assert.WithinDuration(t, activity, info.ModTime(), stampTolerance,
		"the append that waited leaves the newer activity")
}

// TestRepairStoreOnStartup_BroadcastsOnce covers what an open viewer sees:
// one refresh when the pass reorders the list, and nothing on a later start
// that skips the pass.
func TestRepairStoreOnStartup_BroadcastsOnce(t *testing.T) {
	s := newStorage(t)
	seedConversation(t, s, "conv-A", time.Now().Add(-90*24*time.Hour))
	hub := newEventHub()
	sub := hub.subscribe()
	require.NotNil(t, sub)

	repairStoreOnStartup(context.Background(), s, hub)
	assert.Len(t, sub.ch, 1)
	<-sub.ch

	repairStoreOnStartup(context.Background(), s, hub)
	assert.Empty(t, sub.ch, "a marked store emits nothing")
}

// seedConversation writes one conversation whose records end at activity,
// with the modification time a history import would leave: the wall clock
// of the import rather than the session's own date.
func seedConversation(t *testing.T, s *Storage, convID string, activity time.Time) {
	t.Helper()
	writeGen(t, s, convID, "g-"+convID, agento11y.Generation{
		Model:       agento11y.ModelRef{Name: "m"},
		StartedAt:   activity.Add(-time.Minute),
		CompletedAt: activity,
		Usage:       agento11y.TokenUsage{InputTokens: 1},
	}, activity.Format(time.RFC3339Nano))
	setConversationModTime(t, s, convID, time.Now())
}

// countStamps counts the modification times a Storage rewrites, so a test
// can tell "left alone" from "rewritten to the same value".
func countStamps(s *Storage) *atomic.Int32 {
	var n atomic.Int32
	s.chtimes = func(path string, atime, mtime time.Time) error {
		n.Add(1)
		return os.Chtimes(path, atime, mtime)
	}
	return &n
}
