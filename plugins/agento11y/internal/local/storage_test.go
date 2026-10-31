package local

import (
	"bufio"
	"encoding/json"
	"errors"
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

func TestAppendGenerationsValidatesConversationID(t *testing.T) {
	storage := newStorage(t)
	for _, tc := range []struct {
		name   string
		convID string
		record generationRecord
	}{
		{name: "empty id", record: generationRecord{}},
		{name: "unsafe id", convID: "../runs", record: generationRecord{ConversationID: "../runs"}},
		{name: "mixed batch", convID: "conv-A", record: generationRecord{ConversationID: "conv-B"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			written, err := storage.AppendGenerations(tc.convID, []generationRecord{tc.record})
			assert.Zero(t, written)
			require.Error(t, err)
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

// countingOpener simulates a short JSONL batch write.
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
