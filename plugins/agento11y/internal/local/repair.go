package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"
)

// storeMeta records what a daemon has already done to the store on disk.
// It lives in StoreFile so the modification-time pass over every
// conversation file runs once per store and not on every start.
type storeMeta struct {
	// MtimeStamped reports that every conversation file's modification time
	// has been set to its own last activity. A file the pass has not reached
	// carries the time it was written, which for a history import is the
	// import's wall clock and not the session's own date.
	MtimeStamped bool `json:"mtime_stamped,omitempty"`
}

func readStoreMeta(dir string) (storeMeta, error) {
	data, err := readShared(filepath.Join(dir, StoreFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return storeMeta{}, nil
		}
		return storeMeta{}, err
	}
	var meta storeMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return storeMeta{}, err
	}
	return meta, nil
}

func writeStoreMeta(dir string, meta storeMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(dir, StoreFile, data)
}

// stampTolerance is how far a file's modification time may sit from its
// last activity before the repair rewrites it. A filesystem that keeps
// coarser timestamps than the records do would otherwise make every pass
// rewrite every file.
const stampTolerance = time.Second

// repairStoreOnStartup runs the modification-time pass and tells connected
// viewers once it finished, so a viewer left open refetches a list the pass
// has just reordered. The pass reads every record in the store, so the
// daemon runs it in the background after publishing its status file.
func repairStoreOnStartup(ctx context.Context, s *Storage, hub *eventHub) {
	repaired, err := repairStoreModTimes(ctx, s)
	// A pass that stamped some files and failed on others still reordered
	// the list, so an open viewer has to refetch either way.
	if repaired {
		hub.broadcast(changeEvent{})
	}
	switch {
	case err == nil:
	case errors.Is(err, errLockBusy):
		s.logf("local: another daemon holds %s, leaving the modification-time pass to it", RepairLockFile)
	case errors.Is(err, context.Canceled):
		s.logf("local: modification-time pass stopped at shutdown, the next start repeats it")
	default:
		s.logf("local: modification-time pass over %s: %v", ConversationsDir, err)
	}
}

// repairStoreModTimes sets every conversation file's modification time to
// the newest activity that file holds, and records that it did so. It
// returns true when it walked the store, a walk that failed on some of the
// files included, and false for a store already marked as stamped.
//
// The marker is only written when every file was scanned and stamped
// without error, so a failed pass runs again on the next start. Repeating
// the pass changes nothing, because each target is recomputed from the
// file's own records. The exception is a file holding a record dated in the
// future, which every pass stamps again with its own now.
func repairStoreModTimes(ctx context.Context, s *Storage) (bool, error) {
	meta, err := readStoreMeta(s.Dir())
	if err != nil {
		return false, err
	}
	if meta.MtimeStamped {
		return false, nil
	}

	// More than one daemon can share a store, and two passes stamping the
	// same files would fight over the per-file lock for no gain.
	lock, err := acquireFileLock(filepath.Join(s.Dir(), RepairLockFile), false)
	if err != nil {
		return false, err
	}
	defer lock.release()

	// The daemon that held the lock may have finished the pass since the
	// read above.
	if meta, err := readStoreMeta(s.Dir()); err == nil && meta.MtimeStamped {
		return false, nil
	}

	files, err := s.conversationFiles()
	if err != nil {
		return false, err
	}
	// A first launch has no files to stamp, so mark the store and skip the
	// pass on every later start.
	if len(files) == 0 {
		return true, writeStoreMeta(s.Dir(), storeMeta{MtimeStamped: true})
	}
	// A pass over a large store runs for seconds, so log the start as well
	// as the summary below.
	s.logf("local: stamping %d conversation files with their last activity", len(files))
	started := time.Now()
	var stamped, failed int
	var firstErr error
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return stamped > 0, err
		}
		changed, err := s.stampConversationActivity(f.path, f.modTime)
		switch {
		case err != nil:
			// A file this daemon cannot read or stamp is counted and skipped.
			// Stopping here would leave every remaining file ordered by the
			// time it was written.
			failed++
			if firstErr == nil {
				firstErr = err
			}
			s.logf("local: stamp %s: %v", f.path, err)
		case changed:
			stamped++
		}
	}
	s.logf("local: modification-time pass: %d files, %d stamped, %d failed, took %s",
		len(files), stamped, failed, time.Since(started).Round(time.Millisecond))
	if failed > 0 {
		// No marker, so the next start walks the store again and picks up
		// the files this pass could not stamp.
		return true, fmt.Errorf("%d of %d conversation files could not be stamped: %w", failed, len(files), firstErr)
	}
	if err := writeStoreMeta(s.Dir(), storeMeta{MtimeStamped: true}); err != nil {
		return true, err
	}
	return true, nil
}

// stampConversationActivity sets one conversation file's modification time
// to the newest activity it holds, clamped to now, and reports whether it
// changed anything. modTime comes from the directory walk, so nothing
// re-stats the file.
//
// It takes the same per-path lock an append takes, so the scan and the
// stamp cannot interleave with a live export writing to the same file.
func (s *Storage) stampConversationActivity(path string, modTime time.Time) (bool, error) {
	lock := s.lockFor(path)
	lock.Lock()
	defer lock.Unlock()

	var newest time.Time
	skipped, err := scanLatestSummaryRecords(path, func(rec summaryRecord) {
		if when := recordActivity(rec.Generation, rec.ReceivedAt); when.After(newest) {
			newest = when
		}
	})
	s.logSkipped("stamp "+filepath.Base(path), skipped)
	if err != nil {
		return false, err
	}
	// A file with no usable timestamp keeps the time it was written, the one
	// case an append also leaves alone.
	if newest.IsZero() {
		return false, nil
	}
	// A record dated in the future would outrank every real activity until
	// that date passes, so now is as close as the file can sit to it. An
	// append leaves such a file at now as well. Keeping the import time
	// instead sinks the file below its own newest record, out of Since ranges
	// its records do cover.
	if now := time.Now(); newest.After(now) {
		newest = now
	}
	if diff := modTime.Sub(newest); diff < stampTolerance && diff > -stampTolerance {
		return false, nil
	}
	if err := s.setModTime(path, newest); err != nil {
		return false, err
	}
	// The stat validation would catch the new modification time on its own.
	// Dropping the entry keeps the cache from holding one no read can serve.
	s.summaries.invalidate(path)
	return true, nil
}
