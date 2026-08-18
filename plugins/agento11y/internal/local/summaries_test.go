package local

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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

// conversationFileFor stats one conversation file the way the walk does, so
// a cache test can ask for an entry without going through a read path.
func conversationFileFor(t *testing.T, s *Storage, convID string) conversationFile {
	t.Helper()
	path := filepath.Join(s.Dir(), ConversationsDir, convID+".jsonl")
	info, err := os.Stat(path)
	require.NoError(t, err)
	return conversationFile{id: convID, path: path, size: info.Size(), modTime: info.ModTime()}
}

// recordDecodes makes the cache report which files it decoded, in order.
// The paths a read decodes are otherwise invisible: a hit and a miss return
// the same entry.
func recordDecodes(c *summaryCache) *[]string {
	var mu sync.Mutex
	var decoded []string
	c.decode = func(f conversationFile) (*fileSummary, error) {
		mu.Lock()
		decoded = append(decoded, f.id)
		mu.Unlock()
		return decodeFileSummary(f)
	}
	return &decoded
}

// TestSummaryCache covers what makes an entry valid: an unchanged file is
// answered without reopening it, a changed one is decoded again, and an
// explicit invalidation forces a decode whatever the file looks like.
func TestSummaryCache(t *testing.T) {
	cases := []struct {
		name string
		// change runs between the two gets. It returns the file to ask for
		// the second time.
		change      func(t *testing.T, s *Storage, first conversationFile) conversationFile
		wantDecodes int
	}{
		{
			name: "unchanged file is not decoded again",
			change: func(_ *testing.T, _ *Storage, first conversationFile) conversationFile {
				return first
			},
			wantDecodes: 1,
		},
		{
			name: "a grown file is decoded again",
			change: func(t *testing.T, s *Storage, _ conversationFile) conversationFile {
				writeGen(t, s, "conv-A", "g2", agento11y.Generation{
					Model:     agento11y.ModelRef{Name: "m"},
					StartedAt: mustParse(t, "2026-08-03T11:00:00Z"),
					Usage:     agento11y.TokenUsage{InputTokens: 5},
				}, "2026-08-03T11:00:00Z")
				return conversationFileFor(t, s, "conv-A")
			},
			wantDecodes: 2,
		},
		{
			name: "a rewritten modification time is decoded again",
			change: func(t *testing.T, s *Storage, _ conversationFile) conversationFile {
				setConversationModTime(t, s, "conv-A", mustParse(t, "2026-08-04T09:00:00Z"))
				return conversationFileFor(t, s, "conv-A")
			},
			wantDecodes: 2,
		},
		{
			name: "an invalidated entry is decoded again",
			change: func(t *testing.T, s *Storage, first conversationFile) conversationFile {
				s.summaries.invalidate(first.path)
				return first
			},
			wantDecodes: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStorage(t)
			writeGen(t, s, "conv-A", "g1", agento11y.Generation{
				Model:     agento11y.ModelRef{Name: "m"},
				StartedAt: mustParse(t, "2026-08-03T10:00:00Z"),
				Usage:     agento11y.TokenUsage{InputTokens: 10, OutputTokens: 3},
			}, "2026-08-03T10:00:01Z")

			decoded := recordDecodes(&s.summaries)
			first := conversationFileFor(t, s, "conv-A")
			entry, err := s.summaries.get(first)
			require.NoError(t, err)
			require.True(t, entry.ok)
			assert.Equal(t, 1, entry.summary.Calls)
			require.Len(t, entry.points, 1)
			assert.Equal(t, mustParse(t, "2026-08-03T10:00:00Z"), entry.first)
			assert.Equal(t, mustParse(t, "2026-08-03T10:00:00Z"), entry.last)

			next := tc.change(t, s, first)
			again, err := s.summaries.get(next)
			require.NoError(t, err)
			require.True(t, again.ok)
			assert.Len(t, *decoded, tc.wantDecodes)
		})
	}
}

// TestSummaryCacheHitDoesNotOpenTheFile proves the validation is what saves
// the read: the file is made unreadable after the first decode, and the
// second get still answers.
func TestSummaryCacheHitDoesNotOpenTheFile(t *testing.T) {
	requireUnreadableFilesSupported(t)
	s := newStorage(t)
	writeGen(t, s, "conv-A", "g1", agento11y.Generation{
		Model:     agento11y.ModelRef{Name: "m"},
		StartedAt: mustParse(t, "2026-08-03T10:00:00Z"),
		Usage:     agento11y.TokenUsage{InputTokens: 10},
	}, "2026-08-03T10:00:01Z")

	f := conversationFileFor(t, s, "conv-A")
	first, err := s.summaries.get(f)
	require.NoError(t, err)

	blockConversationFile(t, s, "conv-A")
	again, err := s.summaries.get(f)
	require.NoError(t, err)
	assert.Same(t, first, again, "a valid entry is returned as-is")
}

// TestSummaryCachePrunesDeletedFiles checks that a conversation removed
// from the store leaves no entry behind. Pruning only runs once the cache
// holds more paths than the walk found, which is the point at which an
// entry is provably dead.
func TestSummaryCachePrunesDeletedFiles(t *testing.T) {
	s := newStorage(t)
	for _, id := range []string{"conv-A", "conv-B"} {
		writeGen(t, s, id, "g-"+id, agento11y.Generation{
			Model:     agento11y.ModelRef{Name: "m"},
			StartedAt: mustParse(t, "2026-08-03T10:00:00Z"),
			Usage:     agento11y.TokenUsage{InputTokens: 1},
		}, "2026-08-03T10:00:01Z")
	}
	_, _, err := s.ListConversations(ConversationListOptions{})
	require.NoError(t, err)
	require.Len(t, s.summaries.entries, 2)

	gone := filepath.Join(s.Dir(), ConversationsDir, "conv-B.jsonl")
	require.NoError(t, os.Remove(gone))
	_, _, err = s.ListConversations(ConversationListOptions{})
	require.NoError(t, err)

	assert.Len(t, s.summaries.entries, 1)
	_, held := s.summaries.entries[gone]
	assert.False(t, held, "the deleted conversation must not stay cached")
}

// TestSummaryCacheConcurrentGet runs the read path from several goroutines
// at once. Entries are immutable and replaced whole, so every caller must
// see one consistent snapshot; the race detector covers the rest.
func TestSummaryCacheConcurrentGet(t *testing.T) {
	s := newStorage(t)
	writeGen(t, s, "conv-A", "g1", agento11y.Generation{
		Model:     agento11y.ModelRef{Name: "m"},
		StartedAt: mustParse(t, "2026-08-03T10:00:00Z"),
		Usage:     agento11y.TokenUsage{InputTokens: 10, OutputTokens: 3},
	}, "2026-08-03T10:00:01Z")
	f := conversationFileFor(t, s, "conv-A")

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			entry, err := s.summaries.get(f)
			assert.NoError(t, err)
			require.NotNil(t, entry)
			assert.Equal(t, 1, entry.summary.Calls)
			assert.Equal(t, int64(10), entry.summary.InputTokens)
			require.Len(t, entry.points, 1)
		})
	}
	wg.Wait()
}

// TestSummaryCacheDecodesOncePerFile covers the in-flight dedup: callers
// arriving while a file is being decoded wait for that decode rather than
// repeating it. summaryCache.get documents why a request racing the
// background warm depends on this.
func TestSummaryCacheDecodesOncePerFile(t *testing.T) {
	s := newStorage(t)
	writeGen(t, s, "conv-A", "g1", agento11y.Generation{
		Model:     agento11y.ModelRef{Name: "m"},
		StartedAt: mustParse(t, "2026-08-03T10:00:00Z"),
		Usage:     agento11y.TokenUsage{InputTokens: 10, OutputTokens: 3},
	}, "2026-08-03T10:00:01Z")
	f := conversationFileFor(t, s, "conv-A")

	started := make(chan struct{})
	release := make(chan struct{})
	var decodes atomic.Int64
	s.summaries.decode = func(f conversationFile) (*fileSummary, error) {
		if decodes.Add(1) == 1 {
			close(started)
		}
		<-release
		return decodeFileSummary(f)
	}

	entries := make([]*fileSummary, 8)
	var wg sync.WaitGroup
	wg.Go(func() {
		entry, err := s.summaries.get(f)
		assert.NoError(t, err)
		entries[0] = entry
	})
	// The rest only join an existing decode once the first one is running.
	<-started
	for i := 1; i < len(entries); i++ {
		wg.Go(func() {
			entry, err := s.summaries.get(f)
			assert.NoError(t, err)
			entries[i] = entry
		})
	}
	// The joiners have to be parked on the decode, not spinning on their
	// own copies of it, before it is allowed to finish.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	assert.Equal(t, int64(1), decodes.Load(), "one decode answers every caller")
	for _, entry := range entries {
		assert.Same(t, entries[0], entry, "every caller sees the same entry")
	}
}

// TestSummaryCacheDecodesAgainAfterAFailedDecode checks that a failure is
// not cached and does not wedge the path: the caller waiting on it gets the
// error, and a later read decodes the file again.
func TestSummaryCacheDecodesAgainAfterAFailedDecode(t *testing.T) {
	s := newStorage(t)
	writeGen(t, s, "conv-A", "g1", agento11y.Generation{
		Model:     agento11y.ModelRef{Name: "m"},
		StartedAt: mustParse(t, "2026-08-03T10:00:00Z"),
		Usage:     agento11y.TokenUsage{InputTokens: 1},
	}, "2026-08-03T10:00:01Z")
	f := conversationFileFor(t, s, "conv-A")

	var decodes atomic.Int64
	failing := errors.New("decode failed")
	s.summaries.decode = func(f conversationFile) (*fileSummary, error) {
		if decodes.Add(1) == 1 {
			return nil, failing
		}
		return decodeFileSummary(f)
	}

	_, err := s.summaries.get(f)
	require.ErrorIs(t, err, failing)
	assert.Empty(t, s.summaries.entries)

	entry, err := s.summaries.get(f)
	require.NoError(t, err)
	assert.True(t, entry.ok)
	assert.Equal(t, int64(2), decodes.Load())
}

// TestWarmSummaries covers the cold-start warmer: it visits the newest
// conversation first, fills both projections, and leaves the store readable
// with no file opened afterwards.
func TestWarmSummaries(t *testing.T) {
	requireUnreadableFilesSupported(t)
	s := newStorage(t)
	modTimes := map[string]string{
		"conv-new": "2026-08-03T12:00:00Z",
		"conv-old": "2026-08-03T10:00:00Z",
	}
	for _, id := range []string{"conv-old", "conv-new"} {
		writeGen(t, s, id, "g-"+id, agento11y.Generation{
			Model:     agento11y.ModelRef{Name: "m"},
			StartedAt: mustParse(t, modTimes[id]),
			Usage:     agento11y.TokenUsage{InputTokens: 4, OutputTokens: 1},
		}, modTimes[id])
		setConversationModTime(t, s, id, mustParse(t, modTimes[id]))
	}

	decoded := recordDecodes(&s.summaries)
	s.warmSummaries(context.Background())
	assert.Equal(t, []string{"conv-new", "conv-old"}, *decoded,
		"the newest conversation is warmed first: it is the page the viewer opens on")

	for _, id := range []string{"conv-new", "conv-old"} {
		blockConversationFile(t, s, id)
	}
	convs, _, err := s.ListConversations(ConversationListOptions{})
	require.NoError(t, err, "a warmed store serves the list without opening a file")
	assert.Len(t, convs, 2)

	points, _, err := s.TokenUsagePoints(TokenUsageOptions{Interval: time.Hour})
	require.NoError(t, err, "one warm fills the token projection too")
	require.Len(t, points, 2)
	assert.Equal(t, TokenBuckets{FreshInput: 4, Output: 1}, points[0].TokenBuckets)

	again, _, err := s.TokenUsagePoints(TokenUsageOptions{Interval: time.Hour})
	require.NoError(t, err)
	assert.Equal(t, points, again, "two identical requests return identical totals")
	assert.Equal(t, []string{"conv-new", "conv-old"}, *decoded, "neither request decoded a file")
}

// cachedSummaryCount reads the entry count under the cache lock, which a
// test racing a background warm has to do.
func cachedSummaryCount(c *summaryCache) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// TestWarmStartsOnFirstViewerRead pins warming to the viewer. EnsureRunning
// starts a daemon for every --local agent session and for `history import`,
// and neither opens the viewer, so a daemon nobody looks at must not decode
// the whole store.
func TestWarmStartsOnFirstViewerRead(t *testing.T) {
	srv, storage, _ := newTestServerStorage(t)
	for _, id := range []string{"conv-A", "conv-B", "conv-C"} {
		writeGen(t, storage, id, "g-"+id, agento11y.Generation{
			Model:     agento11y.ModelRef{Name: "m"},
			StartedAt: mustParse(t, "2026-08-03T10:00:00Z"),
			Usage:     agento11y.TokenUsage{InputTokens: 1},
		}, "2026-08-03T10:00:01Z")
	}
	srv.WarmSummariesOnFirstRead(context.Background())
	warm := srv.warmStore
	var warms atomic.Int64
	srv.warmStore = func() {
		warms.Add(1)
		warm()
	}

	postDiscard(t, srv, "/api/v1/generations:export", "application/json",
		`{"generations":[{"id":"g-ingest","conversation_id":"conv-A"}]}`)
	assert.Never(t, func() bool { return warms.Load() > 0 }, 50*time.Millisecond, 5*time.Millisecond,
		"recording a generation is not a reason to read the whole store")

	// The page stops after one conversation, so only the warm can account
	// for the other two entries.
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, newLocalRequest(http.MethodGet, "/api/v1/conversations?limit=1", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	require.Eventually(t, func() bool { return cachedSummaryCount(&storage.summaries) == 3 },
		2*time.Second, 5*time.Millisecond, "the first viewer read starts the warm")

	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, newLocalRequest(http.MethodGet, "/api/v1/metrics/tokens", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Never(t, func() bool { return warms.Load() > 1 }, 50*time.Millisecond, 5*time.Millisecond,
		"the store is warmed once, not once per request")
}

// TestWarmSummariesLeavesUndecodableFiles checks the warmer is best effort:
// a file it cannot read stays uncached, so a later request decodes it and
// reports the failure rather than inheriting a wrong answer.
func TestWarmSummariesLeavesUndecodableFiles(t *testing.T) {
	requireUnreadableFilesSupported(t)
	s := newStorage(t)
	writeGen(t, s, "conv-A", "g1", agento11y.Generation{
		Model:     agento11y.ModelRef{Name: "m"},
		StartedAt: mustParse(t, "2026-08-03T10:00:00Z"),
		Usage:     agento11y.TokenUsage{InputTokens: 1},
	}, "2026-08-03T10:00:01Z")
	blockConversationFile(t, s, "conv-A")

	s.warmSummaries(context.Background())
	assert.Empty(t, s.summaries.entries)

	_, _, err := s.ListConversations(ConversationListOptions{})
	assert.Error(t, err, "the unreadable file is reported, not served from a warm entry")
}
