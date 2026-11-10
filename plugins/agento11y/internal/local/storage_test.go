package local

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStorage_AppendsJSONL(t *testing.T) {
	s := newStorage(t)
	type rec struct {
		Name string `json:"name"`
		N    int    `json:"n"`
	}
	for i, name := range []string{"alpha", "beta", "gamma"} {
		if err := s.Append("test.jsonl", rec{Name: name, N: i}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	lines := readLines(t, filepath.Join(s.Dir(), "test.jsonl"))
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(lines))
	}
	for i, line := range lines {
		var got rec
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("decode line %d: %v", i, err)
		}
		if got.N != i {
			t.Errorf("line %d N = %d, want %d", i, got.N, i)
		}
	}
}

func TestStorage_FilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix-only permission check")
	}
	s := newStorage(t)
	if err := s.Append("file.jsonl", map[string]string{"x": "y"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	info, err := os.Stat(filepath.Join(s.Dir(), "file.jsonl"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %v, want 0600", mode)
	}
	dirInfo, err := os.Stat(s.Dir())
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if mode := dirInfo.Mode().Perm(); mode != 0o700 {
		t.Errorf("dir mode = %v, want 0700", mode)
	}
}

func TestNewStorageLeavesOptionalToolSpansDirectoryForFirstWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "local")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	// A non-directory at the optional sidecar path must not prevent startup.
	require.NoError(t, os.WriteFile(filepath.Join(dir, ToolSpansDir), []byte("reserved"), 0o600))

	storage, err := NewStorage(dir)
	require.NoError(t, err)
	assert.Equal(t, dir, storage.Dir())
	info, err := os.Stat(filepath.Join(dir, ConversationsDir))
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

// TestAppendGeneration covers generation storage: populated
// conversation IDs go to conversations/<id>.jsonl, missing or path-shaped
// IDs are rejected, and a conversations directory that disappeared under a
// running daemon is recreated so ingest and export recover.
func TestAppendGeneration(t *testing.T) {
	cases := []struct {
		name      string
		convID    string
		removeDir bool
		wantPath  string
		wantErr   bool
	}{
		{name: "populated id writes per-conversation file", convID: "conv-A", wantPath: "conv-A.jsonl"},
		{name: "UUID writes per-conversation file", convID: "9f2c4a1e-3b7d-4c58-9a10-2f6e8b4d1c07", wantPath: "9f2c4a1e-3b7d-4c58-9a10-2f6e8b4d1c07.jsonl"},
		{name: "missing conversations dir is recreated", convID: "conv-A", removeDir: true, wantPath: "conv-A.jsonl"},
		{name: "empty id rejected", convID: "", wantErr: true},
		{name: "path id rejected", convID: "../runs", wantErr: true},
		{name: "colon rejected", convID: "a:b", wantErr: true},
		{name: "other reserved characters rejected", convID: `a<bad>|name?*"`, wantErr: true},
		{name: "control character rejected", convID: "a\x1fb", wantErr: true},
		{name: "reserved device name rejected", convID: "CON", wantErr: true},
		{name: "reserved device name is case insensitive", convID: "com1", wantErr: true},
		{name: "reserved device name with extension rejected", convID: "LPT9.txt", wantErr: true},
		{name: "superscript COM device name rejected", convID: "COM¹", wantErr: true},
		{name: "superscript LPT device name with extension rejected", convID: "LPT³.jsonl", wantErr: true},
		{name: "trailing dot rejected", convID: "conversation.", wantErr: true},
		{name: "trailing space rejected", convID: "conversation ", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStorage(t)
			if tc.removeDir {
				require.NoError(t, os.RemoveAll(filepath.Join(s.Dir(), ConversationsDir)))
			}
			rec := generationRecord{
				ConversationID: tc.convID,
				GenerationID:   "gen-1",
				Generation:     json.RawMessage(`{"id":"gen-1"}`),
			}
			err := s.AppendGeneration(rec)
			if tc.wantErr {
				if err == nil {
					t.Fatal("AppendGeneration returned nil, want error")
				}
				assertConversationDirEmpty(t, s)
				return
			}
			if err != nil {
				t.Fatalf("AppendGeneration: %v", err)
			}
			assert.Len(t, readLines(t, filepath.Join(s.Dir(), ConversationsDir, tc.wantPath)), 1)
		})
	}
}

// TestAppendGenerations covers the batch append path the ingest handler
// uses: every record for one conversation goes through a single file open
// and close cycle, a mid-batch write failure keeps the records written
// before it, and a record from another conversation is refused.
//
// Each record carries one more minute of activity than the one before it,
// so the modification time the append leaves names the last record that
// reached the file.
func TestAppendGenerations(t *testing.T) {
	cases := []struct {
		name           string
		records        int
		mixConvID      bool
		failWriteAfter int
		wantWritten    int
		wantLines      int
		wantErr        bool
		wantOpens      int
	}{
		{name: "five records share one open cycle", records: 5, wantWritten: 5, wantLines: 5, wantOpens: 1},
		{name: "third write fails", records: 5, failWriteAfter: 2, wantWritten: 2, wantLines: 2, wantErr: true, wantOpens: 1},
		{name: "foreign conversation id refused", records: 2, mixConvID: true, wantErr: true, wantOpens: 0},
	}
	base := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStorage(t)
			opener := &countingOpener{failWriteAfter: tc.failWriteAfter}
			s.openAppend = opener.open

			recs := make([]generationRecord, 0, tc.records)
			for i := range tc.records {
				rec := generationRecord{
					ReceivedAt:     base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano),
					ConversationID: "conv-batch",
					GenerationID:   "gen-" + strconv.Itoa(i),
					Generation:     json.RawMessage(`{"id":"gen-` + strconv.Itoa(i) + `"}`),
				}
				if tc.mixConvID && i == 1 {
					rec.ConversationID = "conv-other"
				}
				recs = append(recs, rec)
			}

			written, err := s.AppendGenerations("conv-batch", recs)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantWritten, written)

			path := filepath.Join(s.Dir(), ConversationsDir, "conv-batch.jsonl")
			if tc.wantLines == 0 {
				_, statErr := os.Stat(path)
				assert.True(t, os.IsNotExist(statErr), "no file expected, stat err = %v", statErr)
			} else {
				assert.Len(t, readLines(t, path), tc.wantLines)
				info, statErr := os.Stat(path)
				require.NoError(t, statErr)
				assert.WithinDuration(t, base.Add(time.Duration(tc.wantWritten-1)*time.Minute), info.ModTime(), time.Second,
					"the stamp names the last record that reached the file")
			}
			opens, closes := opener.counts()
			assert.Equal(t, tc.wantOpens, opens, "file opens")
			assert.Equal(t, opens, closes, "every open must be closed")
		})
	}
}

// TestAppendGenerations_ConcurrentStampKeepsNewest drives appends at one
// conversation file from several goroutines. Whatever order they run in,
// the file must end up stamped with the newest activity any of them
// carried, or a later backfill could sink a live conversation.
func TestAppendGenerations_ConcurrentStampKeepsNewest(t *testing.T) {
	s := newStorage(t)
	const writers = 8
	base := time.Now().Add(-2 * time.Hour).Truncate(time.Second)

	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			genID := "gen-" + strconv.Itoa(w)
			_, err := s.AppendGenerations("conv-race", []generationRecord{{
				ReceivedAt:     base.Add(time.Duration(w) * time.Minute).Format(time.RFC3339Nano),
				ConversationID: "conv-race",
				GenerationID:   genID,
				Generation:     json.RawMessage(`{"id":"` + genID + `"}`),
			}})
			if err != nil {
				t.Errorf("writer %d append: %v", w, err)
			}
		})
	}
	wg.Wait()

	info, err := os.Stat(filepath.Join(s.Dir(), ConversationsDir, "conv-race.jsonl"))
	require.NoError(t, err)
	assert.WithinDuration(t, base.Add((writers-1)*time.Minute), info.ModTime(), time.Second)
}

func TestStorage_ConcurrentAppends(t *testing.T) {
	s := newStorage(t)
	const writers = 8
	const each = 50

	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range each {
				if err := s.Append("concurrent.jsonl", map[string]int{"w": id, "i": i}); err != nil {
					t.Errorf("worker %d append: %v", id, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	lines := readLines(t, filepath.Join(s.Dir(), "concurrent.jsonl"))
	if got, want := len(lines), writers*each; got != want {
		t.Fatalf("lines = %d, want %d", got, want)
	}
	// Every line must be valid JSON — interleaved writes would corrupt
	// at least one of them.
	for i, line := range lines {
		var m map[string]int
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %d not valid JSON: %v (%q)", i, err, line)
		}
	}
}

func TestAppendGenerationsStampsActivity(t *testing.T) {
	latest := time.Now().UTC().Truncate(time.Second).Add(48 * time.Hour)
	older := latest.Add(-time.Hour)
	received := latest.Add(time.Hour)
	latestJSON := latest.Format(time.RFC3339Nano)
	olderJSON := older.Format(time.RFC3339Nano)

	for _, tc := range []struct {
		name        string
		generations []string
		previous    time.Time
		want        time.Time
		wantCalls   int
	}{
		{
			name:        "malformed input keeps later started time",
			generations: []string{fmt.Sprintf(`{"started_at":%q,"completed_at":%q,"input":42}`, latestJSON, olderJSON)},
			want:        latest,
			wantCalls:   1,
		},
		{
			name:        "malformed output keeps completed time",
			generations: []string{fmt.Sprintf(`{"completed_at":%q,"output":"not-a-message"}`, latestJSON)},
			want:        latest,
			wantCalls:   1,
		},
		{
			name:        "no core times uses received time",
			generations: []string{`{"input":42,"output":"not-a-message"}`},
			want:        received,
			wantCalls:   1,
		},
		{
			name:        "invalid core time discards partial decoding and uses received time",
			generations: []string{fmt.Sprintf(`{"started_at":%q,"completed_at":"invalid"}`, received.Add(time.Hour).Format(time.RFC3339Nano))},
			want:        received,
		},
		{
			name:        "invalid core field uses received time",
			generations: []string{fmt.Sprintf(`{"started_at":%q,"tags":42}`, latestJSON)},
			want:        received,
		},
		{
			name:        "old malformed append preserves prior mtime",
			generations: []string{fmt.Sprintf(`{"started_at":%q,"input":42}`, olderJSON)},
			previous:    latest,
			want:        latest,
			wantCalls:   2,
		},
		{
			name: "batch keeps newest activity rather than last record",
			generations: []string{
				fmt.Sprintf(`{"started_at":%q,"input":42}`, latestJSON),
				fmt.Sprintf(`{"completed_at":%q}`, olderJSON),
			},
			want:      latest,
			wantCalls: 2,
		},
		{
			name: "batch compares received fallback with core times",
			generations: []string{
				fmt.Sprintf(`{"started_at":%q}`, latestJSON),
				`{"output":"not-a-message"}`,
				fmt.Sprintf(`{"completed_at":%q}`, olderJSON),
			},
			want:      received,
			wantCalls: 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storage := newStorage(t)
			if !tc.previous.IsZero() {
				written, err := storage.AppendGenerations("c1", []generationRecord{{
					ConversationID: "c1", GenerationID: "previous",
					Generation: json.RawMessage(fmt.Sprintf(`{"started_at":%q}`, tc.previous.Format(time.RFC3339Nano))),
				}})
				require.NoError(t, err)
				require.Equal(t, 1, written)
			}
			records := make([]generationRecord, len(tc.generations))
			for i, gen := range tc.generations {
				records[i] = generationRecord{
					ConversationID: "c1", GenerationID: strconv.Itoa(i),
					ReceivedAt: received.Format(time.RFC3339Nano), Generation: json.RawMessage(gen),
				}
			}
			written, err := storage.AppendGenerations("c1", records)
			require.NoError(t, err)
			assert.Equal(t, len(records), written)
			info, err := os.Stat(storage.Path(filepath.Join(ConversationsDir, "c1.jsonl")))
			require.NoError(t, err)
			assert.True(t, tc.want.Equal(info.ModTime()), "mtime = %v, want %v", info.ModTime(), tc.want)

			if tc.wantCalls > 0 {
				rows, _, err := storage.ListConversations(ConversationListOptions{Since: tc.want, Exact: true})
				require.NoError(t, err)
				require.Len(t, rows, 1)
				assert.Equal(t, "c1", rows[0].ID)
				assert.Equal(t, tc.wantCalls, rows[0].Calls)
				assert.True(t, tc.want.Equal(rows[0].LastActivity))
			}
		})
	}
}

func TestAppendGenerationsSQLiteOnly(t *testing.T) {
	store, dir := newTestSQLStore(t)
	storage, err := NewStorage(dir)
	require.NoError(t, err)
	storage.sql = store
	storage.setSQLiteOnly()

	records := []generationRecord{
		{ReceivedAt: "2026-08-20T08:00:00Z", GenerationID: "g1", ConversationID: "c1", Generation: json.RawMessage(`{"id":"g1","conversation_id":"c1"}`)},
		{ReceivedAt: "2026-08-20T08:00:01Z", GenerationID: "g2", ConversationID: "c1", Generation: json.RawMessage(`{"id":"g2","conversation_id":"c1"}`)},
	}
	written, err := storage.AppendGenerations("c1", records)
	require.NoError(t, err)
	assert.Equal(t, len(records), written)

	var generations int64
	require.NoError(t, store.db.Model(&sqlGeneration{}).Where("conv_id = ?", "c1").Count(&generations).Error)
	assert.EqualValues(t, 2, generations)
	var conversation sqlConversation
	require.NoError(t, store.db.Where("conv_id = ?", "c1").Take(&conversation).Error)
	assert.Equal(t, 2, conversation.Calls)
}

func TestAppendGenerationsSQLiteOnlyFailureRejectsBatch(t *testing.T) {
	store, dir := newTestSQLStore(t)
	storage, err := NewStorage(dir)
	require.NoError(t, err)
	storage.sql = store
	storage.setSQLiteOnly()
	require.NoError(t, store.Close())

	written, err := storage.AppendGenerations("c1", []generationRecord{{
		ReceivedAt: "2026-08-20T08:00:00Z", GenerationID: "g1", ConversationID: "c1", Generation: json.RawMessage(`{"id":"g1","conversation_id":"c1"}`),
	}})
	assert.Zero(t, written)
	require.Error(t, err)
}

func TestAppendGenerationsSQLiteOnlyConcurrent(t *testing.T) {
	store, dir := newTestSQLStore(t)
	storage, err := NewStorage(dir)
	require.NoError(t, err)
	storage.sql = store
	storage.setSQLiteOnly()

	const writers = 8
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			id := "g" + strconv.Itoa(i)
			written, err := storage.AppendGenerations("c1", []generationRecord{{
				ReceivedAt: "2026-08-20T08:00:00Z", GenerationID: id, ConversationID: "c1",
				Generation: json.RawMessage(`{"id":"` + id + `","conversation_id":"c1"}`),
			}})
			assert.NoError(t, err)
			assert.Equal(t, 1, written)
		})
	}
	wg.Wait()

	var count int64
	require.NoError(t, store.db.Model(&sqlGeneration{}).Where("conv_id = ?", "c1").Count(&count).Error)
	assert.EqualValues(t, writers, count)
}

type countingOpener struct {
	mu             sync.Mutex
	opens          int
	closes         int
	writes         int
	failWriteAfter int
}

func (o *countingOpener) open(path string) (io.WriteCloser, error) {
	f, err := openAppendFile(path)
	if err != nil {
		return nil, err
	}
	o.mu.Lock()
	o.opens++
	o.mu.Unlock()
	return &countingFile{owner: o, file: f}, nil
}

func (o *countingOpener) counts() (opens, closes int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.opens, o.closes
}

type countingFile struct {
	owner *countingOpener
	file  io.WriteCloser
}

func (f *countingFile) Write(p []byte) (int, error) {
	f.owner.mu.Lock()
	f.owner.writes++
	n := f.owner.writes
	failAfter := f.owner.failWriteAfter
	f.owner.mu.Unlock()
	if failAfter > 0 && n > failAfter {
		return 0, errors.New("no space left on device")
	}
	return f.file.Write(p)
}

func (f *countingFile) Close() error {
	f.owner.mu.Lock()
	f.owner.closes++
	f.owner.mu.Unlock()
	return f.file.Close()
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	require.NoError(t, sc.Err())
	return out
}

func assertConversationDirEmpty(t *testing.T, s *Storage) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(s.Dir(), ConversationsDir))
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func setConversationModTime(t *testing.T, s *Storage, convID string, when time.Time) {
	t.Helper()
	path := filepath.Join(s.Dir(), ConversationsDir, convID+".jsonl")
	require.NoError(t, os.Chtimes(path, when, when))
}

func blockConversationFile(t *testing.T, s *Storage, convID string) {
	t.Helper()
	path := filepath.Join(s.Dir(), ConversationsDir, convID+".jsonl")
	require.NoError(t, os.Chmod(path, 0o000))
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
}

func requireUnreadableFilesSupported(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file permissions do not prevent reads")
	}
}

func newStorage(t *testing.T) *Storage {
	t.Helper()
	dir := t.TempDir()
	storage, err := NewStorage(filepath.Join(dir, "local"))
	require.NoError(t, err)
	return storage
}
