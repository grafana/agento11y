package local

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

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

func newStorage(t *testing.T) *Storage {
	t.Helper()
	dir := t.TempDir()
	storage, err := NewStorage(filepath.Join(dir, "local"))
	require.NoError(t, err)
	return storage
}
