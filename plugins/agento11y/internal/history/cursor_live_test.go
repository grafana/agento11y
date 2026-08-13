package history

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestCursorLiveTimes is an opt-in harness that imports the times of every
// Cursor session on this machine. It is the counterpart of the reader's
// TestLiveStores: that one checks the format still decodes, this one checks the
// sessions still land on a believable timeline.
//
// It reports counts and durations and never prints a prompt, a reply, or a
// provider ID, which is what makes it safe to run against a real ~/.cursor.
// Opening a store adds the write-ahead log's index beside it, as [chatstore.Open]
// describes, and writes nothing else.
//
//	CURSOR_LIVE_CHATS=~/.cursor/chats go test \
//	    ./plugins/agento11y/internal/history \
//	    -run TestCursorLiveTimes -v -count=1
func TestCursorLiveTimes(t *testing.T) {
	root := strings.TrimSpace(os.Getenv("CURSOR_LIVE_CHATS"))
	if root == "" {
		t.Skip("set CURSOR_LIVE_CHATS=~/.cursor/chats to run the live time harness")
	}

	var paths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(path) != cursorStoreName {
			return nil //nolint:nilerr // an unreadable entry is skipped, not fatal
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(paths)

	imp := &cursorImporter{roots: []string{root}}
	var (
		sessions, dated, interpolated int
		spans                         []time.Duration
	)
	for _, path := range paths {
		preview, ok, err := imp.Preview(context.Background(), path)
		if err != nil {
			t.Errorf("preview %s: %v", filepath.Base(filepath.Dir(path)), err)
			continue
		}
		if !ok || preview.TurnCount == 0 {
			continue
		}
		sessions++

		// A modification time is a record of the last process to open the store,
		// so a session that claims to have run for days is one this importer
		// took its times from the filesystem.
		span := preview.LastActivityAt.Sub(preview.StartedAt)
		spans = append(spans, span)
		if span > cursorMaxFileSpan {
			t.Errorf("%s: the session spans %s, over the %s a file time is trusted for",
				filepath.Base(filepath.Dir(path)), span.Round(time.Second), cursorMaxFileSpan)
		}

		var previous time.Time
		for turn, err := range imp.Turns(context.Background(), preview) {
			if err != nil {
				continue // reported by the import as a warning, not a failure here
			}
			if slices.Contains(turn.Quality.Notes, cursorNoteInterpolatedTimes) {
				interpolated++
			} else {
				dated++
			}
			if turn.Gen.CompletedAt.Before(turn.Gen.StartedAt) {
				t.Errorf("%s: a turn ends %s before it starts",
					filepath.Base(filepath.Dir(path)), turn.Gen.StartedAt.Sub(turn.Gen.CompletedAt))
			}
			if turn.Gen.StartedAt.Before(previous) {
				t.Errorf("%s: a turn starts %s before the one in front of it",
					filepath.Base(filepath.Dir(path)), previous.Sub(turn.Gen.StartedAt))
			}
			previous = turn.Gen.StartedAt
		}
	}

	slices.Sort(spans)
	if len(spans) == 0 {
		t.Fatalf("no session with a turn in it under %s", root)
	}
	t.Logf("sessions=%d turns dated=%d interpolated=%d", sessions, dated, interpolated)
	t.Logf("span median=%s p90=%s max=%s",
		spans[len(spans)/2].Round(time.Second),
		spans[len(spans)*9/10].Round(time.Second),
		spans[len(spans)-1].Round(time.Second))
}
