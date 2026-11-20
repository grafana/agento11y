package local

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
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
		{name: "schema version", query: "SELECT v FROM meta WHERE k = 'schema_version'", want: "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			require.NoError(t, store.db.Raw(tc.query).Scan(&got).Error)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestSQLStoreRawCodec(t *testing.T) {
	const generation = `{"id":"g1","conversation_title":"codec","input":[{"role":"user","parts":[{"kind":"text","text":"needle text"}]}]}`
	for _, tc := range []struct {
		name  string
		codec string
	}{
		{name: "deflated", codec: rawCodecDeflate},
		{name: "plain row from before compression", codec: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storage, store := newMigrationTestStore(t)
			storage.sql = store
			require.NoError(t, store.setJSONLRetired())
			require.NoError(t, storage.AppendGeneration(generationRecord{
				ReceivedAt: "2026-08-20T08:00:00Z", GenerationID: "g1", ConversationID: "c1",
				Generation: json.RawMessage(generation),
			}))
			var row sqlGeneration
			require.NoError(t, store.db.Take(&row, "gen_id = ?", "g1").Error)
			require.Equal(t, rawCodecDeflate, row.RawCodec)
			require.NotEqual(t, generation, string(row.Raw))
			if tc.codec == "" {
				require.NoError(t, store.db.Model(&sqlGeneration{}).Where("gen_id = ?", "g1").
					Updates(map[string]any{"raw": []byte(generation), "raw_codec": ""}).Error)
			}

			detail, err := storage.ConversationDetail("c1")
			require.NoError(t, err)
			require.NotNil(t, detail)
			require.Len(t, detail.Generations, 1)
			assert.Equal(t, "needle text", detail.Generations[0].Input[0].Parts[0].Text)
			summary, err := storage.decodeSQLSummary(conversationFile{id: "c1", sql: true})
			require.NoError(t, err)
			assert.Equal(t, "codec", summary.summary.Title)
			hits, err := storage.SearchConversations("needle", 10)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			assert.Contains(t, hits[0].Snippet, "needle")
		})
	}
}

func TestSQLStoreFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no unix permission bits")
	}
	store, dir := newTestSQLStore(t)
	require.NoError(t, store.writeGenerations([]generationRecord{{
		ReceivedAt: "2026-08-20T08:00:00Z", GenerationID: "g1", ConversationID: "c1",
		Generation: json.RawMessage(`{"id":"g1"}`),
	}}))
	for _, name := range []string{DatabaseFile, DatabaseFile + "-wal", DatabaseFile + "-shm"} {
		info, err := os.Stat(filepath.Join(dir, name))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), name)
	}
}

func TestSQLStoreTransactionsBeginImmediate(t *testing.T) {
	first, dir := newTestSQLStore(t)
	second, err := openSQLStore(dir, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = second.Close() })

	writing := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- first.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(&sqlMeta{K: "first", V: "1"}).Error; err != nil {
				return err
			}
			close(writing)
			<-release
			return nil
		})
	}()
	<-writing

	// A deferred transaction would read a snapshot without "first" and then
	// fail its write with SQLITE_BUSY_SNAPSHOT.
	var seen int64
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- second.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&sqlMeta{}).Where("k = ?", "first").Count(&seen).Error; err != nil {
				return err
			}
			return tx.Create(&sqlMeta{K: "second", V: "1"}).Error
		})
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("second transaction did not wait for the first: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-firstDone)
	require.NoError(t, <-secondDone)
	assert.EqualValues(t, 1, seen)
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

func TestSQLStoreBatchPopulatesRowIDs(t *testing.T) {
	for _, size := range []int{1, 2, 201} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			store, _ := newTestSQLStore(t)
			recs := make([]generationRecord, size)
			for i := range recs {
				recs[i] = generationRecord{
					ConversationID: fmt.Sprintf("c%d", i),
					Generation: json.RawMessage(fmt.Sprintf(
						`{"input":[{"role":"user","parts":[{"kind":"text","text":"batchword%d"}]}]}`, i)),
				}
				if i%2 == 1 {
					recs[i].GenerationID = fmt.Sprintf("g%d", i)
				}
			}
			for round := 1; round <= 2; round++ {
				rows, err := sqlRowsFromRecords(recs)
				require.NoError(t, err)
				require.NoError(t, store.db.Transaction(func(tx *gorm.DB) error {
					_, err := upsertSQLGenerations(tx, rows)
					return err
				}))
				for i, row := range rows {
					require.Positive(t, row.RowID, "round %d, row %d", round, i)
					var stored sqlGeneration
					require.NoError(t, store.db.Take(&stored, "rowid = ?", row.RowID).Error)
					assert.Equal(t, row.GenID, stored.GenID)
					assert.Equal(t, row.ConvID, stored.ConvID)
					assert.Equal(t, row.Body, stored.Body)

					var wantRowIDs, ftsRowIDs []int64
					require.NoError(t, store.db.Model(&sqlGeneration{}).
						Where("conv_id = ?", row.ConvID).Order("rowid").Pluck("rowid", &wantRowIDs).Error)
					require.NoError(t, store.db.Raw(
						"SELECT rowid FROM gen_fts WHERE gen_fts MATCH ? ORDER BY rowid",
						fmt.Sprintf("batchword%d", i)).Scan(&ftsRowIDs).Error)
					assert.Equal(t, wantRowIDs, ftsRowIDs)
					if row.GenID == nil {
						assert.Len(t, wantRowIDs, round)
					} else {
						assert.Len(t, wantRowIDs, 1)
					}
				}
				var nullIDs int64
				require.NoError(t, store.db.Model(&sqlGeneration{}).Where("gen_id IS NULL").Count(&nullIDs).Error)
				assert.EqualValues(t, ((size+1)/2)*round, nullIDs)
			}
		})
	}
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
	for _, convID := range []string{"c1"} {
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
