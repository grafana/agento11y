package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreMigrator(t *testing.T) {
	t.Run("migration lock excludes concurrent migrations", func(t *testing.T) {
		dir := t.TempDir()
		callbackErr := errors.New("migration failed")
		err := withMigrationLock(dir, func() error {
			err := withMigrationLock(dir, func() error {
				t.Error("concurrent migration acquired the lock")
				return nil
			})
			require.ErrorIs(t, err, errLockBusy)
			return callbackErr
		})
		require.ErrorIs(t, err, callbackErr)

		called := false
		require.NoError(t, withMigrationLock(dir, func() error {
			called = true
			return nil
		}))
		assert.True(t, called, "failed migration releases its lock")
	})

	t.Run("keeps latest payloads at first positions", func(t *testing.T) {
		for _, tc := range []struct {
			name, separator string
			finalNewline    bool
		}{
			{"LF", "\n", true},
			{"CRLF", "\r\n", true},
			{"no final newline", "\n", false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "conversation.jsonl")
				lines := []string{"", "not-json"}
				for _, record := range []struct{ id, genID, title string }{
					{"g1", "g1", "original"},
					{"", "", "anonymous"},
					{"g2", "g2", "second"},
					{"", "g1", "updated"},
				} {
					line := migrationRecordLine(t, "c1", record.id, map[string]any{
						"id": record.genID, "conversation_title": record.title,
					})
					lines = append(lines, strings.TrimSuffix(string(line), "\n"))
				}
				data := strings.Join(lines, tc.separator)
				if tc.finalNewline {
					data += tc.separator
				}
				size := int64(len(data))
				data += tc.separator + string(migrationRecordLine(t, "c1", "g1", map[string]any{
					"id": "g1", "conversation_title": "outside snapshot",
				}))
				require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
				var titles []string
				result, err := scanLatestGenerationRecordsUpTo(context.Background(), path, size, func(_ generationRecord, gen storedGeneration) error {
					titles = append(titles, gen.title())
					return nil
				})
				require.NoError(t, err)
				assert.Equal(t, jsonlScanResult{skipped: 1, unreadable: 1}, result)
				assert.Equal(t, []string{"updated", "anonymous", "second"}, titles)
			})
		}
	})

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
		first.scan = func(ctx context.Context, path string, size int64, visit func(generationRecord, storedGeneration) error) (jsonlScanResult, error) {
			completedPath = path
			return scanLatestGenerationRecordsUpTo(ctx, path, size, visit)
		}
		_, err := first.run(ctx)
		require.ErrorIs(t, err, context.Canceled)
		require.NotEmpty(t, completedPath)

		second := newStoreMigrator(storage)
		second.sleep = noMigrationSleep
		var scanned []string
		second.scan = func(ctx context.Context, path string, size int64, visit func(generationRecord, storedGeneration) error) (jsonlScanResult, error) {
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
		migrator.scan = func(ctx context.Context, path string, size int64, visit func(generationRecord, storedGeneration) error) (jsonlScanResult, error) {
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

	t.Run("retirement allows concurrent dual-writes", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, withRetirementReadLock(dir, func() error {
			done := make(chan error, 1)
			go func() {
				done <- withRetirementReadLock(dir, func() error { return nil })
			}()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("dual-write lock blocked another reader")
			}
			return nil
		}))
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

	t.Run("counts unreadable lines except a truncated final line", func(t *testing.T) {
		for _, tc := range []struct {
			name           string
			tail           string
			wantUnreadable int64
		}{
			{"truncated final line", `{"received_at":`, 0},
			{"mid-file line", "not-json\n", 1},
			{"mid-file and truncated final line", "not-json\n{\"received_at\":", 1},
		} {
			t.Run(tc.name, func(t *testing.T) {
				storage, store := newMigrationTestStore(t)
				appendMigrationRecord(t, storage, "c1", "g1", map[string]any{"id": "g1", "conversation_id": "c1"})
				path := filepath.Join(storage.Dir(), ConversationsDir, "c1.jsonl")
				appendRaw(t, path, tc.tail)
				if strings.HasSuffix(tc.tail, "\n") {
					appendRaw(t, path, string(migrationRecordLine(t, "c1", "g2", map[string]any{"id": "g2"})))
				}
				storage.sql = store

				runTestMigration(t, storage)
				state, err := store.migrationState(filepath.Join(ConversationsDir, "c1.jsonl"))
				require.NoError(t, err)
				require.NotNil(t, state)
				assert.Equal(t, tc.wantUnreadable, state.Unreadable)
				complete, err := store.migrationComplete()
				require.NoError(t, err)
				assert.True(t, complete)
			})
		}
	})

	t.Run("migrates a record the JSONL readers accept through the summary fallback", func(t *testing.T) {
		storage, store := newMigrationTestStore(t)
		path := filepath.Join(storage.Dir(), ConversationsDir, "c2.jsonl")
		appendRaw(t, path, string(migrationRecordLine(t, "c2", "g2", map[string]any{"id": "g2", "input": "not-an-array"})))
		detail, err := storage.ConversationDetail("c2")
		require.NoError(t, err)
		require.NotNil(t, detail, "JSONL reads accept the record")
		storage.sql = store

		_, err = migrateStore(context.Background(), storage)
		require.NoError(t, err)
		complete, err := store.migrationComplete()
		require.NoError(t, err)
		assert.True(t, complete)
		assert.Equal(t, []string{"c2/g2"}, generationKeys(t, store))
	})

	t.Run("files rows under the file's conversation id", func(t *testing.T) {
		storage, store := newMigrationTestStore(t)
		path := filepath.Join(storage.Dir(), ConversationsDir, "c1.jsonl")
		appendRaw(t, path, string(migrationRecordLine(t, "elsewhere", "g1", map[string]any{"id": "g1"})))
		storage.sql = store

		runTestMigration(t, storage)
		assert.Equal(t, []string{"c1/g1"}, generationKeys(t, store))
	})

	t.Run("a failing file does not stop the pass", func(t *testing.T) {
		storage, store := newMigrationTestStore(t)
		appendMigrationRecord(t, storage, "c1", "g1", map[string]any{"id": "g1"})
		appendMigrationRecord(t, storage, "c2", "g2", map[string]any{"id": "g2"})
		storage.sql = store

		migrator := newStoreMigrator(storage)
		migrator.sleep = noMigrationSleep
		scanErr := errors.New("scan failed")
		migrator.scan = func(ctx context.Context, path string, size int64, visit func(generationRecord, storedGeneration) error) (jsonlScanResult, error) {
			if filepath.Base(path) == "c1.jsonl" {
				return jsonlScanResult{}, scanErr
			}
			return scanLatestGenerationRecordsUpTo(ctx, path, size, visit)
		}
		changed, err := migrator.run(context.Background())
		require.Error(t, err)
		assert.True(t, changed)

		state, err := store.migrationState(filepath.Join(ConversationsDir, "c2.jsonl"))
		require.NoError(t, err)
		require.NotNil(t, state)
		assert.NotNil(t, state.DoneAt)
		assertNoMeta(t, store, migrationCompleteKey)
	})

	t.Run("limits a transaction by bytes", func(t *testing.T) {
		storage, store := newMigrationTestStore(t)
		for _, id := range []string{"g1", "g2", "g3"} {
			appendMigrationRecord(t, storage, "c1", id, map[string]any{"id": id})
		}
		storage.sql = store

		migrator := newStoreMigrator(storage)
		migrator.bytes = 1
		writes := 0
		migrator.sleep = func(context.Context, time.Duration) error {
			writes++
			return nil
		}
		_, err := migrator.run(context.Background())
		require.NoError(t, err)
		assert.Equal(t, 4, writes, "one transaction per row plus the completing one")
	})

	t.Run("skips the pass without enough free disk space", func(t *testing.T) {
		storage, store := newMigrationTestStore(t)
		appendMigrationRecord(t, storage, "c1", "g1", map[string]any{"id": "g1"})
		storage.sql = store
		var logs strings.Builder
		storage.SetLogger(log.New(&logs, "", 0))
		storage.freeBytes = func(string) (uint64, error) { return 1, nil }
		info, err := os.Stat(filepath.Join(storage.Dir(), ConversationsDir, "c1.jsonl"))
		require.NoError(t, err)

		migrateStoreOnStartup(context.Background(), storage, newEventHub())

		assert.Empty(t, generationKeys(t, store))
		assertNoMeta(t, store, migrationCompleteKey)
		need := uint64(float64(info.Size())*migrationDiskFactor) + migrationDiskReserve
		assert.Contains(t, logs.String(), fmt.Sprintf("needs %d free bytes in %s and 1 are available", need, storage.Dir()))
	})

	t.Run("stops when free disk space runs out mid-pass", func(t *testing.T) {
		storage, store := newMigrationTestStore(t)
		appendMigrationRecord(t, storage, "c1", "g1", map[string]any{"id": "g1"})
		appendMigrationRecord(t, storage, "c2", "g2", map[string]any{"id": "g2"})
		storage.sql = store
		calls := 0
		storage.freeBytes = func(string) (uint64, error) {
			calls++
			if calls <= 2 { // the pass, then the first file
				return 2 * migrationDiskReserve, nil
			}
			return migrationDiskReserve - 1, nil
		}

		migrator := newStoreMigrator(storage)
		migrator.sleep = noMigrationSleep
		_, err := migrator.run(context.Background())
		var spaceErr *diskSpaceError
		require.ErrorAs(t, err, &spaceErr)
		first, err := store.migrationState(filepath.Join(ConversationsDir, "c1.jsonl"))
		require.NoError(t, err)
		require.NotNil(t, first)
		assert.NotNil(t, first.DoneAt)
		second, err := store.migrationState(filepath.Join(ConversationsDir, "c2.jsonl"))
		require.NoError(t, err)
		assert.Nil(t, second)

		storage.freeBytes = func(string) (uint64, error) { return 2 * migrationDiskReserve, nil }
		runTestMigration(t, storage)
		assert.Equal(t, []string{"c1/g1", "c2/g2"}, generationKeys(t, store))
	})
}

func TestJSONLRetirement(t *testing.T) {
	convPath := func(storage *Storage, id string) string {
		return filepath.Join(storage.Dir(), ConversationsDir, id+".jsonl")
	}
	retire := func(t *testing.T, storage *Storage) {
		t.Helper()
		for range 2 { // migrate, then retire on the clean start
			_, err := migrateStore(context.Background(), storage)
			require.NoError(t, err)
		}
		retired, err := storage.sql.jsonlRetired()
		require.NoError(t, err)
		require.True(t, retired)
	}
	recordLine := func(t *testing.T, convID, genID, receivedAt, title string) string {
		t.Helper()
		line, err := json.Marshal(generationRecord{
			ReceivedAt:     receivedAt,
			GenerationID:   genID,
			ConversationID: convID,
			Generation:     json.RawMessage(fmt.Sprintf(`{"id":%q,"conversation_title":%q}`, genID, title)),
		})
		require.NoError(t, err)
		return string(line) + "\n"
	}
	titleOf := func(t *testing.T, store *sqlStore, genID string) string {
		t.Helper()
		var row sqlGeneration
		require.NoError(t, store.db.Select("title").Where("gen_id = ?", genID).Take(&row).Error)
		return row.Title
	}
	markRetired := func(t *testing.T, storage *Storage, store *sqlStore) {
		t.Helper()
		storage.sql = store
		require.NoError(t, store.setJSONLRetired())
		storage.setSQLiteOnly()
	}

	t.Run("moves a file with a mid-file unreadable line", func(t *testing.T) {
		storage, store := newMigrationTestStore(t)
		path := convPath(storage, "c1")
		appendRaw(t, path, recordLine(t, "c1", "g1", "2026-08-20T08:00:00Z", "one")+"not-json\n"+recordLine(t, "c1", "g2", "2026-08-20T08:01:00Z", "two"))
		storage.sql = store

		retire(t, storage)
		assert.NoFileExists(t, path)
		assert.FileExists(t, filepath.Join(storage.Dir(), LegacyUnreadableDir, "c1.jsonl"))
		assert.Equal(t, []string{"c1/g1", "c1/g2"}, generationKeys(t, store))
	})

	t.Run("keeps a file changed after the clean check", func(t *testing.T) {
		storage, store := newMigrationTestStore(t)
		appendMigrationRecord(t, storage, "c1", "g1", map[string]any{"id": "g1"})
		storage.sql = store
		runTestMigration(t, storage)
		changed, err := newStoreMigrator(storage).run(context.Background())
		require.NoError(t, err)
		require.False(t, changed)

		path := convPath(storage, "c1")
		appendRaw(t, path, recordLine(t, "c1", "g2", "2026-08-20T08:01:00Z", "two"))
		require.NoError(t, removeLegacyJSONL(context.Background(), storage))
		assert.FileExists(t, path)

		require.NoError(t, store.setJSONLRetired())
		_, err = migrateStore(context.Background(), storage)
		require.NoError(t, err)
		assert.NoFileExists(t, path)
		assert.Equal(t, []string{"c1/g1", "c1/g2"}, generationKeys(t, store))
	})

	t.Run("imports a stray file in a retired store", func(t *testing.T) {
		storage, store := newMigrationTestStore(t)
		markRetired(t, storage, store)
		path := convPath(storage, "c9")
		appendRaw(t, path, recordLine(t, "c9", "g9", "2026-08-20T08:00:00Z", "nine"))
		hub := newEventHub()
		sub := hub.subscribe()
		require.NotNil(t, sub)

		migrateStoreOnStartup(context.Background(), storage, hub)
		assert.Equal(t, []string{"c9/g9"}, generationKeys(t, store))
		assert.NoFileExists(t, path)
		select {
		case <-sub.ch:
		default:
			t.Fatal("the import did not broadcast a change")
		}
		assertNoMeta(t, store, migrationCompleteKey)
	})

	t.Run("a stray copy does not replace a newer generation", func(t *testing.T) {
		for _, tc := range []struct {
			name, stored, stray, want string
		}{
			{"older stray copy", "2026-08-20T10:00:00Z", "2026-08-20T09:00:00Z", "stored"},
			{"fractional second sorts before Z as text", "2026-08-20T08:00:00.5Z", "2026-08-20T08:00:00Z", "stored"},
			{"newer stray copy", "2026-08-20T09:00:00Z", "2026-08-20T10:00:00Z", "stray"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				storage, store := newMigrationTestStore(t)
				markRetired(t, storage, store)
				require.NoError(t, storage.AppendGeneration(generationRecord{
					ReceivedAt: tc.stored, GenerationID: "g1", ConversationID: "c1",
					Generation: json.RawMessage(`{"id":"g1","conversation_title":"stored"}`),
				}))
				appendRaw(t, convPath(storage, "c1"), recordLine(t, "c1", "g1", tc.stray, "stray"))

				_, err := migrateStore(context.Background(), storage)
				require.NoError(t, err)
				assert.Equal(t, tc.want, titleOf(t, store, "g1"))
			})
		}
	})

	t.Run("rows written after retirement survive a stray import", func(t *testing.T) {
		storage, store := newMigrationTestStore(t)
		markRetired(t, storage, store)
		require.NoError(t, storage.AppendGeneration(generationRecord{
			ReceivedAt: "2026-08-20T10:00:00Z", GenerationID: "g2", ConversationID: "c1",
			Generation: json.RawMessage(`{"id":"g2"}`),
		}))
		appendRaw(t, convPath(storage, "c1"), recordLine(t, "c1", "g1", "2026-08-20T09:00:00Z", "one"))

		_, err := migrateStore(context.Background(), storage)
		require.NoError(t, err)
		assert.Equal(t, []string{"c1/g1", "c1/g2"}, generationKeys(t, store))
	})

	t.Run("a stray import lists the conversation after each chunk", func(t *testing.T) {
		storage, store := newMigrationTestStore(t)
		markRetired(t, storage, store)
		appendRaw(t, convPath(storage, "c1"), recordLine(t, "c1", "g1", "2026-08-20T08:00:00Z", "one")+
			recordLine(t, "c1", "g2", "2026-08-20T08:01:00Z", "two"))
		migrator := newStoreMigrator(storage)
		migrator.additive = true
		migrator.rows = 1
		var calls []int
		migrator.sleep = func(ctx context.Context, _ time.Duration) error {
			var conversation sqlConversation
			require.NoError(t, store.db.Where("conv_id = ?", "c1").Take(&conversation).Error)
			calls = append(calls, conversation.Calls)
			return ctx.Err()
		}
		_, err := migrator.run(context.Background())
		require.NoError(t, err)
		assert.Equal(t, []int{1, 2, 2}, calls)
	})

	t.Run("a grown stray file does not duplicate a record without an id", func(t *testing.T) {
		storage, store := newMigrationTestStore(t)
		markRetired(t, storage, store)
		path := convPath(storage, "c1")
		appendRaw(t, path, recordLine(t, "c1", "", "2026-08-20T08:00:00Z", "no id"))
		migrator := newStoreMigrator(storage)
		migrator.additive = true
		migrator.sleep = noMigrationSleep
		_, err := migrator.run(context.Background())
		require.NoError(t, err)

		appendRaw(t, path, recordLine(t, "c1", "g2", "2026-08-20T08:01:00Z", "two"))
		_, err = migrator.run(context.Background())
		require.NoError(t, err)
		assert.Equal(t, []string{"c1/", "c1/g2"}, generationKeys(t, store))
	})
}

func appendRaw(t *testing.T, path, data string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = file.WriteString(data)
	require.NoError(t, err)
	require.NoError(t, file.Close())
}

func generationKeys(t *testing.T, store *sqlStore) []string {
	t.Helper()
	var rows []sqlGeneration
	require.NoError(t, store.db.Select("conv_id, gen_id").Order("conv_id, gen_id").Find(&rows).Error)
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		id := ""
		if row.GenID != nil {
			id = *row.GenID
		}
		keys = append(keys, row.ConvID+"/"+id)
	}
	return keys
}

func assertNoMeta(t *testing.T, store *sqlStore, key string) {
	t.Helper()
	var count int64
	require.NoError(t, store.db.Model(&sqlMeta{}).Where("k = ?", key).Count(&count).Error)
	assert.Zero(t, count, "meta %s", key)
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
