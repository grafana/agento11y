package local

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreMigrator(t *testing.T) {
	t.Run("broadcasts when the migration completes", func(t *testing.T) {
		if !ReceiverSupported() {
			t.Skip("local receiver is unavailable on this platform")
		}
		storage, store := newMigrationTestStore(t)
		appendMigrationRecord(t, storage, "c1", "g1", map[string]any{"id": "g1", "conversation_id": "c1"})
		storage.sql = store
		hub := newEventHub()
		sub := hub.subscribe()
		require.NotNil(t, sub)

		migrateStoreOnStartup(context.Background(), storage, hub)

		select {
		case <-sub.ch:
		case <-time.After(time.Second):
			t.Fatal("migration completion was not broadcast")
		}
		complete, err := store.migrationComplete()
		require.NoError(t, err)
		assert.True(t, complete)
	})

	t.Run("resumes an interrupted file by row", func(t *testing.T) {
		storage, store := newMigrationTestStore(t)
		for i := range 3 {
			appendMigrationRecord(t, storage, "c1", "", map[string]any{
				"conversation_id": "c1",
				"agent_name":      "agent",
				"started_at":      time.Date(2026, 8, 20, 8, i, 0, 0, time.UTC),
			})
		}
		storage.sql = store

		ctx, cancel := context.WithCancel(context.Background())
		migrator := newStoreMigrator(storage)
		migrator.rows = 1
		sleepCalls := 0
		migrator.sleep = func(context.Context, time.Duration) error {
			sleepCalls++
			if sleepCalls == 1 {
				cancel()
			}
			return nil
		}
		changed, err := migrator.run(ctx)
		assert.False(t, changed)
		require.ErrorIs(t, err, context.Canceled)

		state, err := store.migrationState(filepath.Join(ConversationsDir, "c1.jsonl"))
		require.NoError(t, err)
		require.NotNil(t, state)
		assert.EqualValues(t, 1, state.Rows)
		assert.Nil(t, state.DoneAt)

		migrator = newStoreMigrator(storage)
		migrator.rows = 1
		migrator.sleep = noMigrationSleep
		changed, err = migrator.run(context.Background())
		require.NoError(t, err)
		assert.True(t, changed)

		var count int64
		require.NoError(t, store.db.Model(&sqlGeneration{}).Where("conv_id = ?", "c1").Count(&count).Error)
		assert.EqualValues(t, 3, count, "resuming must not duplicate rows with NULL generation ids")
	})

	t.Run("does not reread completed files", func(t *testing.T) {
		storage, store := newMigrationTestStore(t)
		appendMigrationRecord(t, storage, "c1", "g1", map[string]any{"id": "g1", "conversation_id": "c1"})
		appendMigrationRecord(t, storage, "c2", "g2", map[string]any{"id": "g2", "conversation_id": "c2"})
		storage.sql = store

		ctx, cancel := context.WithCancel(context.Background())
		first := newStoreMigrator(storage)
		sleepCalls := 0
		first.sleep = func(context.Context, time.Duration) error {
			sleepCalls++
			if sleepCalls == 1 {
				cancel()
			}
			return nil
		}
		var completedPath string
		first.scan = func(ctx context.Context, path string, size int64, visit func(generationRecord, storedGeneration) error) (int, error) {
			completedPath = path
			return scanLatestGenerationRecordsUpTo(ctx, path, size, visit)
		}
		_, err := first.run(ctx)
		require.ErrorIs(t, err, context.Canceled)
		require.NotEmpty(t, completedPath)

		second := newStoreMigrator(storage)
		second.sleep = noMigrationSleep
		var scanned []string
		second.scan = func(ctx context.Context, path string, size int64, visit func(generationRecord, storedGeneration) error) (int, error) {
			scanned = append(scanned, path)
			return scanLatestGenerationRecordsUpTo(ctx, path, size, visit)
		}
		_, err = second.run(context.Background())
		require.NoError(t, err)
		assert.NotContains(t, scanned, completedPath)
		assert.Len(t, scanned, 1)
	})

	t.Run("does not block appends while scanning a file", func(t *testing.T) {
		storage, store := newMigrationTestStore(t)
		appendMigrationRecord(t, storage, "c1", "g1", map[string]any{"id": "g1", "conversation_id": "c1"})
		storage.sql = store
		files, err := storage.conversationFiles()
		require.NoError(t, err)
		require.Len(t, files, 1)

		started := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		migrator := newStoreMigrator(storage)
		migrator.sleep = noMigrationSleep
		migrator.scan = func(ctx context.Context, path string, size int64, visit func(generationRecord, storedGeneration) error) (int, error) {
			once.Do(func() {
				close(started)
				<-release
			})
			return scanLatestGenerationRecordsUpTo(ctx, path, size, visit)
		}
		migrated := make(chan error, 1)
		go func() { migrated <- migrator.migrateFile(context.Background(), files[0]) }()
		<-started

		appended := make(chan error, 1)
		go func() {
			appended <- storage.AppendGeneration(generationRecord{
				ReceivedAt: "2026-08-20T08:01:00Z", GenerationID: "g2", ConversationID: "c1",
				Generation: json.RawMessage(`{"id":"g2","conversation_id":"c1"}`),
			})
		}()
		select {
		case err := <-appended:
			require.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("append waited for the full migration scan")
		}
		close(release)
		require.NoError(t, <-migrated)

		var count int64
		require.NoError(t, store.db.Model(&sqlGeneration{}).Where("conv_id = ?", "c1").Count(&count).Error)
		assert.EqualValues(t, 2, count)
	})

	t.Run("replaces a file whose recorded size is larger", func(t *testing.T) {
		storage, store := newMigrationTestStore(t)
		appendMigrationRecord(t, storage, "c1", "old-1", map[string]any{
			"id": "old-1", "conversation_id": "c1", "agent_name": "an-agent-name-that-makes-this-record-long",
		})
		appendMigrationRecord(t, storage, "c1", "old-2", map[string]any{
			"id": "old-2", "conversation_id": "c1", "agent_name": "another-agent-name-that-makes-this-record-long",
		})
		storage.sql = store
		runTestMigration(t, storage)

		path := filepath.Join(storage.Dir(), ConversationsDir, "c1.jsonl")
		oldInfo, err := os.Stat(path)
		require.NoError(t, err)
		replacement := migrationRecordLine(t, "c1", "new", map[string]any{"id": "new", "conversation_id": "c1"})
		require.NoError(t, os.WriteFile(path, replacement, 0o600))
		newInfo, err := os.Stat(path)
		require.NoError(t, err)
		require.Less(t, newInfo.Size(), oldInfo.Size())

		runTestMigration(t, storage)
		var ids []string
		require.NoError(t, store.db.Model(&sqlGeneration{}).Where("conv_id = ? AND gen_id IS NOT NULL", "c1").Order("gen_id").Pluck("gen_id", &ids).Error)
		assert.Equal(t, []string{"new"}, ids)
		var staleMatches int64
		require.NoError(t, store.db.Raw("SELECT count(*) FROM gen_fts WHERE gen_fts MATCH ?", `"another"*`).Scan(&staleMatches).Error)
		assert.Zero(t, staleMatches, "replacing a file must remove its old FTS rows")
	})

	t.Run("dual-write after replacement keeps migration pending", func(t *testing.T) {
		storage, store := newMigrationTestStore(t)
		appendMigrationRecord(t, storage, "c1", "old-1", map[string]any{
			"id": "old-1", "conversation_id": "c1", "agent_name": "an-agent-name-that-makes-this-record-long",
		})
		appendMigrationRecord(t, storage, "c1", "old-2", map[string]any{
			"id": "old-2", "conversation_id": "c1", "agent_name": "another-agent-name-that-makes-this-record-long",
		})
		storage.sql = store
		runTestMigration(t, storage)

		path := filepath.Join(storage.Dir(), ConversationsDir, "c1.jsonl")
		replacement := migrationRecordLine(t, "c1", "new-1", map[string]any{"id": "new-1", "conversation_id": "c1"})
		require.NoError(t, os.WriteFile(path, replacement, 0o600))
		require.NoError(t, storage.AppendGeneration(generationRecord{
			ReceivedAt: "2026-08-20T08:02:00Z", GenerationID: "new-2", ConversationID: "c1",
			Generation: json.RawMessage(`{"id":"new-2","conversation_id":"c1"}`),
		}))

		state, err := store.migrationState(filepath.Join(ConversationsDir, "c1.jsonl"))
		require.NoError(t, err)
		require.NotNil(t, state)
		assert.Nil(t, state.DoneAt, "a replacement must not inherit completed migration state")
		runTestMigration(t, storage)

		var ids []string
		require.NoError(t, store.db.Model(&sqlGeneration{}).Where("conv_id = ? AND gen_id IS NOT NULL", "c1").Order("gen_id").Pluck("gen_id", &ids).Error)
		assert.Equal(t, []string{"new-1", "new-2"}, ids)
	})

	t.Run("retires JSONL on the next clean start", func(t *testing.T) {
		storage, store := newMigrationTestStore(t)
		appendMigrationRecord(t, storage, "c1", "g1", map[string]any{"id": "g1", "conversation_id": "c1"})
		storage.sql = store

		changed, err := migrateStore(context.Background(), storage)
		require.NoError(t, err)
		assert.True(t, changed)
		retired, err := store.jsonlRetired()
		require.NoError(t, err)
		assert.False(t, retired, "the migration start keeps its rollback copy")
		files, err := storage.conversationFiles()
		require.NoError(t, err)
		require.Len(t, files, 1)
		otherStorage, err := NewStorage(storage.Dir())
		require.NoError(t, err)
		otherStorage.sql = store

		// A marker that appears after the clean check must postpone retirement.
		require.NoError(t, storage.markForRemigration("c1"))
		changed, err = retireJSONL(context.Background(), storage)
		require.NoError(t, err)
		assert.True(t, changed)
		retired, err = store.jsonlRetired()
		require.NoError(t, err)
		assert.False(t, retired)
		files, err = storage.conversationFiles()
		require.NoError(t, err)
		require.Len(t, files, 1)

		require.NoError(t, os.WriteFile(filepath.Join(storage.Dir(), "store.json"), []byte(`{"mtime_stamped":true}`), 0o600))
		changed, err = migrateStore(context.Background(), storage)
		require.NoError(t, err)
		assert.False(t, changed)
		retired, err = store.jsonlRetired()
		require.NoError(t, err)
		assert.True(t, retired)
		assert.True(t, storage.sqlOnly)
		files, err = storage.conversationFiles()
		require.NoError(t, err)
		assert.Empty(t, files)
		_, err = os.Stat(filepath.Join(storage.Dir(), "store.json"))
		assert.ErrorIs(t, err, os.ErrNotExist)

		require.NoError(t, otherStorage.AppendGeneration(generationRecord{
			ReceivedAt: "2026-08-20T08:01:00Z", GenerationID: "g2", ConversationID: "c1",
			Generation: json.RawMessage(`{"id":"g2","conversation_id":"c1"}`),
		}))
		assert.True(t, otherStorage.sqlOnly, "another store instance observes the retirement metadata")
		var count int64
		require.NoError(t, store.db.Model(&sqlGeneration{}).Where("conv_id = ?", "c1").Count(&count).Error)
		assert.EqualValues(t, 2, count)
		files, err = storage.conversationFiles()
		require.NoError(t, err)
		assert.Empty(t, files, "SQLite-only writes must not recreate the rollback copy")
	})

	t.Run("cross-process retirement waits for dual-write recovery", func(t *testing.T) {
		if !ReceiverSupported() {
			t.Skip("local receiver is unavailable on this platform")
		}
		storage, store := newMigrationTestStore(t)
		appendMigrationRecord(t, storage, "c1", "g1", map[string]any{"id": "g1", "conversation_id": "c1"})
		storage.sql = store
		changed, err := migrateStore(context.Background(), storage)
		require.NoError(t, err)
		require.True(t, changed)

		readLocked := make(chan struct{})
		addMarker := make(chan struct{})
		readDone := make(chan error, 1)
		go func() {
			readDone <- withRetirementReadLock(storage.Dir(), func() error {
				close(readLocked)
				<-addMarker
				return storage.markForRemigration("c1")
			})
		}()
		<-readLocked
		retireDone := make(chan struct {
			changed bool
			err     error
		}, 1)
		go func() {
			changed, err := retireJSONL(context.Background(), storage)
			retireDone <- struct {
				changed bool
				err     error
			}{changed: changed, err: err}
		}()
		select {
		case <-retireDone:
			t.Fatal("retirement did not wait for the cross-process dual-write lock")
		case <-time.After(50 * time.Millisecond):
		}
		close(addMarker)
		require.NoError(t, <-readDone)
		result := <-retireDone
		require.NoError(t, result.err)
		assert.True(t, result.changed)
		retired, err := store.jsonlRetired()
		require.NoError(t, err)
		assert.False(t, retired, "the recovery marker must postpone retirement")
		files, err := storage.conversationFiles()
		require.NoError(t, err)
		assert.Len(t, files, 1)
	})

	t.Run("skips a truncated final line", func(t *testing.T) {
		storage, store := newMigrationTestStore(t)
		appendMigrationRecord(t, storage, "c1", "g1", map[string]any{"id": "g1", "conversation_id": "c1"})
		path := filepath.Join(storage.Dir(), ConversationsDir, "c1.jsonl")
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		require.NoError(t, err)
		_, err = file.WriteString(`{"received_at":`)
		require.NoError(t, err)
		require.NoError(t, file.Close())
		storage.sql = store

		runTestMigration(t, storage)
		var count int64
		require.NoError(t, store.db.Model(&sqlGeneration{}).Where("conv_id = ?", "c1").Count(&count).Error)
		assert.EqualValues(t, 1, count)
		complete, err := store.migrationComplete()
		require.NoError(t, err)
		assert.True(t, complete)
	})
}

func newMigrationTestStore(t *testing.T) (*Storage, *sqlStore) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "local")
	storage, err := NewStorage(dir)
	require.NoError(t, err)
	store, err := openSQLStore(dir, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return storage, store
}

func appendMigrationRecord(t *testing.T, storage *Storage, convID, genID string, generation map[string]any) {
	t.Helper()
	raw, err := json.Marshal(generation)
	require.NoError(t, err)
	require.NoError(t, storage.AppendGeneration(generationRecord{
		ReceivedAt:     "2026-08-20T08:00:00Z",
		GenerationID:   genID,
		ConversationID: convID,
		Generation:     raw,
	}))
}

func migrationRecordLine(t *testing.T, convID, genID string, generation map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(generation)
	require.NoError(t, err)
	line, err := json.Marshal(generationRecord{
		ReceivedAt:     "2026-08-20T08:00:00Z",
		GenerationID:   genID,
		ConversationID: convID,
		Generation:     raw,
	})
	require.NoError(t, err)
	return append(line, '\n')
}

func runTestMigration(t *testing.T, storage *Storage) {
	t.Helper()
	migrator := newStoreMigrator(storage)
	migrator.sleep = noMigrationSleep
	_, err := migrator.run(context.Background())
	require.NoError(t, err)
}

func noMigrationSleep(ctx context.Context, _ time.Duration) error {
	return context.Cause(ctx)
}
