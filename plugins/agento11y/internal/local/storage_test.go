package local

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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

func assertConversationDirEmpty(t *testing.T, s *Storage) {
	t.Helper()
	convDir := filepath.Join(s.Dir(), ConversationsDir)
	entries, err := os.ReadDir(convDir)
	if err != nil {
		t.Fatalf("read %s: %v", convDir, err)
	}
	if len(entries) != 0 {
		t.Fatalf("%s not empty: %v", convDir, entries)
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
			activities := make([]time.Time, 0, tc.records)
			for i := range tc.records {
				rec := generationRecord{
					ConversationID: "conv-batch",
					GenerationID:   "gen-" + strconv.Itoa(i),
					Generation:     json.RawMessage(`{"id":"gen-` + strconv.Itoa(i) + `"}`),
				}
				if tc.mixConvID && i == 1 {
					rec.ConversationID = "conv-other"
				}
				recs = append(recs, rec)
				activities = append(activities, base.Add(time.Duration(i)*time.Minute))
			}

			written, err := s.AppendGenerations("conv-batch", recs, activities)
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

// TestAppendGenerations_StampsActivity covers the modification time an
// append leaves on a conversation file, which is what the list orders and
// bounds on. Offsets are relative to now so the rows read as "an hour of
// activity ago" rather than as fixed dates.
func TestAppendGenerations_StampsActivity(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	cases := []struct {
		name string
		// pin rewrites the file's modification time after the first append,
		// the way a history import into an existing file leaves it.
		pin           time.Duration
		pinAfterFirst bool
		// batches are the activity stamps to append with, in order.
		batches []time.Duration
		// want is the modification time the file must end up with. When
		// wantWriteTime is set the file must instead keep the time the
		// append itself ran.
		want          time.Duration
		wantWriteTime bool
	}{
		{
			name:    "the batch activity becomes the modification time",
			batches: []time.Duration{-2 * time.Hour},
			want:    -2 * time.Hour,
		},
		{
			name:    "a backfilled append does not sink the file",
			batches: []time.Duration{-time.Hour, -72 * time.Hour},
			want:    -time.Hour,
		},
		{
			name:          "an import-time modification time survives an older append",
			pin:           0,
			pinAfterFirst: true,
			batches:       []time.Duration{-72 * time.Hour, -73 * time.Hour},
			want:          0,
		},
		{
			name:          "a future activity leaves the write time",
			batches:       []time.Duration{48 * time.Hour},
			wantWriteTime: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStorage(t)
			path := filepath.Join(s.Dir(), ConversationsDir, "conv-A.jsonl")
			for i, offset := range tc.batches {
				genID := "gen-" + strconv.Itoa(i)
				rec := generationRecord{
					ConversationID: "conv-A",
					GenerationID:   genID,
					Generation:     json.RawMessage(`{"id":"` + genID + `"}`),
				}
				written, err := s.AppendGenerations("conv-A", []generationRecord{rec}, []time.Time{now.Add(offset)})
				require.NoError(t, err)
				require.Equal(t, 1, written)
				if i == 0 && tc.pinAfterFirst {
					setConversationModTime(t, s, "conv-A", now.Add(tc.pin))
				}
			}

			assert.Len(t, readLines(t, path), len(tc.batches), "every record reached the file")
			info, err := os.Stat(path)
			require.NoError(t, err)
			if tc.wantWriteTime {
				assert.WithinDuration(t, time.Now(), info.ModTime(), time.Minute)
				return
			}
			assert.WithinDuration(t, now.Add(tc.want), info.ModTime(), time.Second)
		})
	}
}

// TestAppendGenerations_StampFailureKeepsRecords covers a filesystem that
// refuses the stamp: the records are already written, so the append still
// reports them and only the ordering degrades.
func TestAppendGenerations_StampFailureKeepsRecords(t *testing.T) {
	s := newStorage(t)
	var logs strings.Builder
	s.SetLogger(log.New(&logs, "", 0))
	var stamps int
	s.chtimes = func(string, time.Time, time.Time) error {
		stamps++
		return errors.New("read-only file system")
	}

	written, err := s.AppendGenerations("conv-A", []generationRecord{{
		ConversationID: "conv-A",
		GenerationID:   "gen-1",
		Generation:     json.RawMessage(`{"id":"gen-1"}`),
	}}, []time.Time{time.Now().Add(-time.Hour)})

	require.NoError(t, err)
	assert.Equal(t, 1, written)
	assert.Len(t, readLines(t, filepath.Join(s.Dir(), ConversationsDir, "conv-A.jsonl")), 1)
	assert.Equal(t, 1, stamps, "one append attempts one stamp")
	assert.Contains(t, logs.String(), "read-only file system")
}

// TestAppendGenerations_FutureModTimeIsNotCarriedForward covers a file
// whose modification time already runs ahead of the clock, which a restore
// or a copy from a machine with a fast clock leaves behind. The append
// folds that time in as now instead of skipping the stamp, so the file
// stops claiming activity that has not happened yet.
func TestAppendGenerations_FutureModTimeIsNotCarriedForward(t *testing.T) {
	s := newStorage(t)
	activity := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	appendGen := func(genID string, when time.Time) {
		t.Helper()
		_, err := s.AppendGenerations("conv-A", []generationRecord{{
			ConversationID: "conv-A",
			GenerationID:   genID,
			Generation:     json.RawMessage(`{"id":"` + genID + `"}`),
		}}, []time.Time{when})
		require.NoError(t, err)
	}

	appendGen("gen-1", activity)
	setConversationModTime(t, s, "conv-A", time.Now().Add(72*time.Hour))

	var stamps []time.Time
	s.chtimes = func(path string, atime, mtime time.Time) error {
		stamps = append(stamps, mtime)
		return os.Chtimes(path, atime, mtime)
	}
	appendGen("gen-2", activity.Add(time.Hour))

	require.Len(t, stamps, 1, "the append stamps the file rather than leaving it")
	assert.False(t, stamps[0].After(time.Now()), "the stamp is not in the future")
	info, err := os.Stat(filepath.Join(s.Dir(), ConversationsDir, "conv-A.jsonl"))
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), info.ModTime(), time.Minute)
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
				ConversationID: "conv-race",
				GenerationID:   genID,
				Generation:     json.RawMessage(`{"id":"` + genID + `"}`),
			}}, []time.Time{base.Add(time.Duration(w) * time.Minute)})
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

// countingOpener wraps the real appender, counting open and close cycles
// so the batch tests can prove one request costs one open per
// conversation. failWriteAfter > 0 fails every write past that count.
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

func newStorage(t *testing.T) *Storage {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStorage(filepath.Join(dir, "local"))
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	return s
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return out
}
