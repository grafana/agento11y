package local

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSQLStoreConfig(t *testing.T) {
	store, _ := newTestSQLStore(t)
	for _, tc := range []struct {
		name  string
		query string
		want  string
	}{
		{name: "WAL", query: "PRAGMA journal_mode", want: "wal"},
		{name: "normal synchronous writes", query: "PRAGMA synchronous", want: "1"},
		{name: "five second busy timeout", query: "PRAGMA busy_timeout", want: "5000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			require.NoError(t, store.db.Raw(tc.query).Scan(&got).Error)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestSQLStoreKeepsGenerationsWithoutIDs(t *testing.T) {
	store, _ := newTestSQLStore(t)
	recs := []generationRecord{
		{ReceivedAt: "2026-08-20T08:00:00Z", ConversationID: "c1", Generation: json.RawMessage(`{"conversation_id":"c1","input":[{"role":"user","parts":[{"kind":"text","text":"first"}]}]}`)},
		{ReceivedAt: "2026-08-20T08:00:01Z", ConversationID: "c1", Generation: json.RawMessage(`{"conversation_id":"c1","input":[{"role":"user","parts":[{"kind":"text","text":"second"}]}]}`)},
	}
	require.NoError(t, store.appendGenerations("conversations/c1.jsonl", 0, 2, time.Time{}, time.Now(), recs))

	var rows []sqlGeneration
	require.NoError(t, store.db.Order("rowid").Find(&rows).Error)
	require.Len(t, rows, 2)
	assert.Nil(t, rows[0].GenID)
	assert.Nil(t, rows[1].GenID)

	var conversation sqlConversation
	require.NoError(t, store.db.Take(&conversation, "conv_id = ?", "c1").Error)
	assert.Equal(t, 2, conversation.Calls)
}

func TestSQLStoreUpsertPreservesRowIDAndFTSJoin(t *testing.T) {
	store, _ := newTestSQLStore(t)
	first := generationRecord{
		ReceivedAt:     "2026-08-20T08:00:00Z",
		GenerationID:   "g1",
		ConversationID: "c1",
		Generation:     json.RawMessage(`{"id":"g1","conversation_id":"c1","output":[{"role":"assistant","parts":[{"kind":"text","text":"old wording"}]}]}`),
	}
	require.NoError(t, store.appendGenerations("conversations/c1.jsonl", 0, 1, time.Time{}, time.Now(), []generationRecord{first}))

	var before sqlGeneration
	require.NoError(t, store.db.Where("gen_id = ?", "g1").Take(&before).Error)

	second := first
	second.ReceivedAt = "2026-08-20T08:01:00Z"
	second.Generation = json.RawMessage(`{"id":"g1","conversation_id":"c1","output":[{"role":"assistant","parts":[{"kind":"text","text":"replacement deadline"}]}]}`)
	require.NoError(t, store.appendGenerations("conversations/c1.jsonl", 1, 2, time.Time{}, time.Now(), []generationRecord{second}))

	var after sqlGeneration
	require.NoError(t, store.db.Where("gen_id = ?", "g1").Take(&after).Error)
	assert.Equal(t, before.RowID, after.RowID)

	var matches int64
	require.NoError(t, store.db.Raw("SELECT count(*) FROM gen_fts WHERE gen_fts MATCH ?", `"deadline"*`).Scan(&matches).Error)
	assert.EqualValues(t, 1, matches)
}

func TestSQLiteFailureDoesNotRejectJSONLAppend(t *testing.T) {
	for _, convID := range []string{"c1", ".", ".."} {
		t.Run(convID, func(t *testing.T) {
			srv, storage, dir := newTestServerStorage(t)
			store, err := openSQLStore(dir, nil)
			require.NoError(t, err)
			storage.sql = store
			require.NoError(t, store.Close())

			body := fmt.Sprintf(`{"generations":[{"id":"g1","conversation_id":%q}]}`, convID)
			resp := post(t, srv, "/api/v1/generations:export", "application/json", body)
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
			var out generationsResponse
			decodeJSON(t, resp.Body, &out)
			require.Len(t, out.Results, 1)
			assert.True(t, out.Results[0].Accepted)
			assert.Empty(t, out.Results[0].Error)
			assert.True(t, storage.remigrationRequested(convID))

			storage.sql = nil
			detail, err := storage.ConversationDetail(convID)
			require.NoError(t, err)
			require.NotNil(t, detail)
			assert.Len(t, detail.Generations, 1)
		})
	}
}

func newTestSQLStore(t *testing.T) (*sqlStore, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "local")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	store, err := openSQLStore(dir, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store, dir
}
