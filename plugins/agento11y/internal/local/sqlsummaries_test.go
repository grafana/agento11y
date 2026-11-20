package local

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestSQLiteSummaryCache(t *testing.T) {
	when := mustParse(t, "2026-08-21T10:00:00Z")
	replacement := func() agento11y.Generation {
		return agento11y.Generation{StartedAt: when, Usage: agento11y.TokenUsage{InputTokens: 20}}
	}
	for _, tc := range []struct {
		name    string
		change  func(*testing.T, *Storage)
		want    map[string]int64
		changed map[string]bool
	}{
		{name: "unchanged", want: map[string]int64{"c1": 10, "c2": 1}},
		{
			name: "local upsert at unchanged activity",
			change: func(t *testing.T, storage *Storage) {
				writeGen(t, storage, "c1", "g1", replacement(), when.Format(time.RFC3339Nano))
			},
			want: map[string]int64{"c1": 20, "c2": 1}, changed: map[string]bool{"c1": true},
		},
		{
			name: "other writer upsert at unchanged activity",
			change: func(t *testing.T, storage *Storage) {
				other, err := openSQLStore(storage.Dir(), nil)
				require.NoError(t, err)
				t.Cleanup(func() { _ = other.Close() })
				writer, err := NewStorage(storage.Dir())
				require.NoError(t, err)
				writer.sql = other
				writeGen(t, writer, "c1", "g1", replacement(), when.Format(time.RFC3339Nano))
			},
			want: map[string]int64{"c1": 20, "c2": 1}, changed: map[string]bool{"c1": true},
		},
		{
			name: "older append leaves activity unchanged",
			change: func(t *testing.T, storage *Storage) {
				gen := replacement()
				gen.StartedAt = when.Add(-time.Hour)
				writeGen(t, storage, "c1", "older", gen, when.Format(time.RFC3339Nano))
			},
			want: map[string]int64{"c1": 30, "c2": 1}, changed: map[string]bool{"c1": true},
		},
		{
			name: "remigration rebuilds the conversation",
			change: func(t *testing.T, storage *Storage) {
				appendMigrationRecord(t, storage, "c1", "g1", map[string]any{
					"started_at": when, "usage": map[string]any{"input_tokens": 20},
				})
				runTestMigration(t, storage)
			},
			want: map[string]int64{"c1": 20, "c2": 1}, changed: map[string]bool{"c1": true},
		},
		{
			name: "schema upgrade preserves records and invalidation",
			change: func(t *testing.T, storage *Storage) {
				require.NoError(t, storage.sql.db.Migrator().DropColumn(&sqlConversation{}, "Revision"))
				require.NoError(t, storage.sql.Close())
				upgraded, err := openSQLStore(storage.Dir(), nil)
				require.NoError(t, err)
				t.Cleanup(func() { _ = upgraded.Close() })
				storage.sql = upgraded
				writeGen(t, storage, "c1", "g1", replacement(), when.Format(time.RFC3339Nano))
			},
			want: map[string]int64{"c1": 20, "c2": 1}, changed: map[string]bool{"c1": true, "c2": true},
		},
		{
			name: "retirement keeps cached projections",
			change: func(t *testing.T, storage *Storage) {
				_, err := migrateStore(context.Background(), storage)
				require.NoError(t, err)
				retired, err := storage.sql.jsonlRetired()
				require.NoError(t, err)
				require.True(t, retired)
			},
			want: map[string]int64{"c1": 10, "c2": 1},
		},
		{
			name: "SQLite-only generation moves between conversations",
			change: func(t *testing.T, storage *Storage) {
				_, err := migrateStore(context.Background(), storage)
				require.NoError(t, err)
				writeGen(t, storage, "c2", "g1", replacement(), when.Format(time.RFC3339Nano))
			},
			want: map[string]int64{"c2": 21}, changed: map[string]bool{"c1": true, "c2": true},
		},
		{
			name: "rolled back changes keep cached projections",
			change: func(t *testing.T, storage *Storage) {
				rollback := errors.New("rollback")
				row, err := sqlGenerationFromRecord(generationRecord{
					ConversationID: "c1", GenerationID: "g1", Generation: mustJSON(t, replacement()),
					ReceivedAt: when.Format(time.RFC3339Nano),
				})
				require.NoError(t, err)
				err = storage.sql.db.Transaction(func(tx *gorm.DB) error {
					_, err := upsertSQLGenerations(tx, []sqlGeneration{row})
					if err != nil {
						return err
					}
					if err := recomputeSQLConversation(tx, "c1"); err != nil {
						return err
					}
					return rollback
				})
				require.ErrorIs(t, err, rollback)
			},
			want: map[string]int64{"c1": 10, "c2": 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storage := newStorage(t)
			for _, seed := range []struct {
				conv, id string
				tokens   int64
			}{{"c1", "g1", 10}, {"c2", "g2", 1}} {
				writeGen(t, storage, seed.conv, seed.id, agento11y.Generation{
					StartedAt: when, Usage: agento11y.TokenUsage{InputTokens: seed.tokens},
				}, when.Format(time.RFC3339Nano))
			}
			store, err := openSQLStore(storage.Dir(), nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			storage.sql = store
			runTestMigration(t, storage)
			load := func() map[string]*fileSummary {
				files, err := storage.queryConversationFiles()
				require.NoError(t, err)
				entries := make(map[string]*fileSummary, len(files))
				for _, file := range files {
					entries[file.id], err = storage.conversationSummary(file)
					require.NoError(t, err)
				}
				return entries
			}
			first := load()
			if tc.change != nil {
				tc.change(t, storage)
			}
			second := load()
			require.Len(t, second, len(tc.want))
			assert.Equal(t, len(tc.want), cachedSummaryCount(&storage.sqlSummaries))
			for id, tokens := range tc.want {
				require.Contains(t, second, id)
				assert.Equal(t, tokens, second[id].summary.InputTokens)
				if tc.changed[id] {
					assert.NotSame(t, first[id], second[id], "changed conversation %s", id)
				} else {
					assert.Same(t, first[id], second[id], "unchanged conversation %s", id)
				}
				assert.Equal(t, first[id].summary.LastActivity, second[id].summary.LastActivity)
			}
		})
	}
}
