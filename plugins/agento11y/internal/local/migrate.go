package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	migrationCompleteKey        = "jsonl_migration_complete"
	jsonlRetiredKey             = "jsonl_retired"
	migrationRowsPerTransaction = 200
)

type migrationScanner func(context.Context, string, int64, func(generationRecord, storedGeneration) error) (int, error)
type migrationSleeper func(context.Context, time.Duration) error

type storeMigrator struct {
	storage *Storage
	sql     *sqlStore
	scan    migrationScanner
	sleep   migrationSleeper
	now     func() time.Time
	rows    int
}

func newStoreMigrator(storage *Storage) *storeMigrator {
	return &storeMigrator{
		storage: storage,
		sql:     storage.sql,
		scan:    scanLatestGenerationRecordsUpTo,
		sleep:   sleepForMigration,
		now:     time.Now,
		rows:    migrationRowsPerTransaction,
	}
}

// migrateStoreOnStartup migrates legacy files without delaying daemon
// readiness. Completion tells open viewers to refetch through SSE. The JSONL
// rollback copy survives until a later start reports no migration changes.
func migrateStoreOnStartup(ctx context.Context, storage *Storage, hub *eventHub) {
	if !ReceiverSupported() || storage == nil || storage.sql == nil {
		return
	}
	changed, err := migrateStore(ctx, storage)
	if changed && err == nil {
		hub.broadcast(changeEvent{})
	}
	switch {
	case err == nil:
	case errors.Is(err, errLockBusy):
		storage.logf("local: another daemon holds %s, leaving SQLite migration to it", MigrationLockFile)
	case errors.Is(err, context.Canceled):
		storage.logf("local: SQLite migration stopped at shutdown and will resume on the next start")
	default:
		storage.logf("local: migrate %s to SQLite: %v", ConversationsDir, err)
	}
}

func migrateStore(ctx context.Context, storage *Storage) (bool, error) {
	if storage == nil || storage.sql == nil {
		return false, errors.New("local sqlite migration: store is not open")
	}
	var changed bool
	err := withMigrationLock(storage.Dir(), func() error {
		retired, err := storage.sql.jsonlRetired()
		if err != nil {
			return err
		}
		if retired {
			storage.setSQLiteOnly()
			return removeLegacyJSONL(ctx, storage)
		}
		changed, err = newStoreMigrator(storage).run(ctx)
		if err != nil || changed {
			return err
		}
		changed, err = retireJSONL(ctx, storage)
		return err
	})
	return changed, err
}

func (m *storeMigrator) run(ctx context.Context) (bool, error) {
	files, err := m.storage.conversationFiles()
	if err != nil {
		return false, err
	}
	complete, err := m.sql.migrationComplete()
	if err != nil {
		return false, err
	}
	needsMigration := !complete
	if !needsMigration {
		for _, file := range files {
			if needed, err := m.fileNeedsMigration(file); err != nil {
				return false, err
			} else if needed {
				needsMigration = true
				break
			}
		}
	}
	if !needsMigration {
		return false, nil
	}
	if err := m.sql.setMigrationComplete(false); err != nil {
		return false, err
	}

	m.storage.logf("local: checking %d conversation files for SQLite migration", len(files))
	started := time.Now()
	migrated := 0
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return migrated > 0, err
		}
		needed, err := m.fileNeedsMigration(file)
		if err != nil {
			return migrated > 0, err
		}
		if !needed {
			continue
		}
		if err := m.migrateFile(ctx, file); err != nil {
			return migrated > 0, err
		}
		migrated++
	}
	if err := m.sql.setMigrationComplete(true); err != nil {
		return migrated > 0, err
	}
	m.storage.logf("local: SQLite migration: %d files migrated, %d already current, took %s",
		migrated, len(files)-migrated, time.Since(started).Round(time.Millisecond))
	return true, nil
}

func (m *storeMigrator) fileNeedsMigration(file conversationFile) (bool, error) {
	if m.storage.remigrationRequested(file.id) {
		return true, nil
	}
	state, err := m.sql.migrationState(filepath.Join(ConversationsDir, filepath.Base(file.path)))
	if err != nil {
		return false, err
	}
	if state == nil || state.DoneAt == nil {
		return true, nil
	}
	return state.Size != file.size || state.MTime != file.modTime.UnixNano(), nil
}

var (
	errMigrationFileChanged     = errors.New("conversation file changed during migration")
	errMigrationProgressInvalid = errors.New("migration progress exceeds file rows")
)

func (m *storeMigrator) migrateFile(ctx context.Context, file conversationFile) error {
	forceReset := false
	for {
		err := m.migrateFileSnapshot(ctx, file, forceReset)
		switch {
		case errors.Is(err, errMigrationProgressInvalid):
			forceReset = true
		case errors.Is(err, errMigrationFileChanged):
			forceReset = false
		default:
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

func (m *storeMigrator) migrateFileSnapshot(ctx context.Context, file conversationFile, forceReset bool) error {
	lock := m.storage.lockFor(file.path)
	lock.Lock()
	info, err := os.Stat(file.path)
	lock.Unlock()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	path := filepath.Join(ConversationsDir, filepath.Base(file.path))
	state, err := m.sql.migrationState(path)
	if err != nil {
		return err
	}
	marker := m.storage.remigrationRequested(file.id)
	if state != nil && state.DoneAt != nil && !marker && state.Size == info.Size() && state.MTime == info.ModTime().UnixNano() {
		return nil
	}

	restart := forceReset || marker || state == nil || state.DoneAt != nil || state.Size != info.Size() || state.TargetSize != info.Size()
	start := 0
	if !restart {
		start = int(state.Rows)
		if start < 0 {
			start = 0
			restart = true
		}
	}
	chunkSize := max(1, m.rows)
	chunk := make([]sqlGeneration, 0, chunkSize)
	rowsSeen := 0
	firstWrite := true
	flush := func(done bool) error {
		if len(chunk) == 0 && !done {
			return nil
		}
		began := time.Now()
		err := m.writeChunk(ctx, file, path, info, chunk, restart && firstWrite, rowsSeen, done)
		if err != nil {
			return err
		}
		firstWrite = false
		chunk = chunk[:0]
		return m.sleep(ctx, time.Since(began))
	}

	skipped, err := m.scan(ctx, file.path, info.Size(), func(rec generationRecord, _ storedGeneration) error {
		row, err := sqlGenerationFromRecord(rec)
		if err != nil {
			return err
		}
		rowsSeen++
		if rowsSeen <= start {
			return nil
		}
		chunk = append(chunk, row)
		if len(chunk) == chunkSize {
			return flush(false)
		}
		return nil
	})
	m.storage.logSkipped("migrate "+filepath.Base(file.path), skipped)
	if err != nil {
		return err
	}
	if start > rowsSeen {
		return errMigrationProgressInvalid
	}
	if err := flush(true); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(m.storage.Dir(), RemigrateDir, remigrationMarkerName(file.id))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove remigration marker for %s: %w", file.id, err)
	}
	return nil
}

func (m *storeMigrator) writeChunk(
	ctx context.Context,
	file conversationFile,
	path string,
	info os.FileInfo,
	rows []sqlGeneration,
	reset bool,
	rowsDone int,
	done bool,
) error {
	lock := m.storage.lockFor(file.path)
	lock.Lock()
	defer lock.Unlock()
	current, err := os.Stat(file.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errMigrationFileChanged
		}
		return err
	}
	if current.Size() != info.Size() || current.ModTime().UnixNano() != info.ModTime().UnixNano() {
		return errMigrationFileChanged
	}

	m.sql.writeMu.Lock()
	defer m.sql.writeMu.Unlock()
	return m.sql.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		convID := file.id
		if reset {
			if err := tx.Exec("DELETE FROM gen_fts WHERE rowid IN (SELECT rowid FROM generation WHERE conv_id = ?)", convID).Error; err != nil {
				return err
			}
			if err := tx.Delete(&sqlGeneration{}, "conv_id = ?", convID).Error; err != nil {
				return err
			}
			if err := tx.Delete(&sqlConversation{}, "conv_id = ?", convID).Error; err != nil {
				return err
			}
		}
		if len(rows) > 0 {
			conversations, err := upsertSQLGenerations(tx, rows)
			if err != nil {
				return err
			}
			for changedConvID := range conversations {
				if changedConvID == convID {
					continue
				}
				if err := recomputeSQLConversation(tx, changedConvID); err != nil {
					return err
				}
			}
		}
		if done {
			if err := recomputeSQLConversation(tx, convID); err != nil {
				return err
			}
		}

		var doneAt *time.Time
		if done {
			now := m.now().UTC()
			doneAt = &now
		}
		progress := migratedFile{
			Path:       path,
			Size:       info.Size(),
			MTime:      info.ModTime().UnixNano(),
			Rows:       int64(rowsDone),
			TargetSize: info.Size(),
			DoneAt:     doneAt,
		}
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "path"}},
			UpdateAll: true,
		}).Create(&progress).Error
	})
}

func (s *sqlStore) migrationState(path string) (*migratedFile, error) {
	var state migratedFile
	err := s.db.Where("path = ?", path).Take(&state).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &state, nil
}

func (s *sqlStore) migrationComplete() (bool, error) {
	var meta sqlMeta
	err := s.db.Where("k = ?", migrationCompleteKey).Take(&meta).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return meta.V == "1", nil
}

func (s *sqlStore) setMigrationComplete(complete bool) error {
	if !complete {
		return s.db.Delete(&sqlMeta{}, "k = ?", migrationCompleteKey).Error
	}
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "k"}},
		UpdateAll: true,
	}).Create(&sqlMeta{K: migrationCompleteKey, V: "1"}).Error
}

func (s *sqlStore) jsonlRetired() (bool, error) {
	var meta sqlMeta
	err := s.db.Where("k = ?", jsonlRetiredKey).Take(&meta).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return meta.V == "1", nil
}

func (s *sqlStore) setJSONLRetired() error {
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "k"}},
		UpdateAll: true,
	}).Create(&sqlMeta{K: jsonlRetiredKey, V: "1"}).Error
}

func (s *Storage) setSQLiteOnly() {
	s.modeMu.Lock()
	s.sqlOnly = true
	s.modeMu.Unlock()
}

func retireJSONL(ctx context.Context, storage *Storage) (bool, error) {
	var changed bool
	err := withRetirementWriteLock(storage.Dir(), func() error {
		var err error
		changed, err = retireJSONLExclusive(ctx, storage)
		return err
	})
	return changed, err
}

func retireJSONLExclusive(ctx context.Context, storage *Storage) (bool, error) {
	storage.modeMu.Lock()
	defer storage.modeMu.Unlock()

	// Recheck while dual-writes are blocked in every daemon. Otherwise a failed
	// mirror can add a recovery marker after validation, and cleanup can delete
	// its JSONL copy.
	changed, err := newStoreMigrator(storage).run(ctx)
	if err != nil || changed {
		return changed, err
	}
	if err := storage.sql.setJSONLRetired(); err != nil {
		return false, err
	}
	storage.sqlOnly = true
	return false, removeLegacyJSONLLocked(ctx, storage)
}

func removeLegacyJSONL(ctx context.Context, storage *Storage) error {
	storage.modeMu.Lock()
	defer storage.modeMu.Unlock()
	return removeLegacyJSONLLocked(ctx, storage)
}

func removeLegacyJSONLLocked(ctx context.Context, storage *Storage) error {
	entries, err := os.ReadDir(filepath.Join(storage.Dir(), ConversationsDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	removed := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		if err := os.Remove(filepath.Join(storage.Dir(), ConversationsDir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		removed++
	}
	if err := os.Remove(filepath.Join(storage.Dir(), "store.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if removed > 0 {
		storage.logf("local: retired %d migrated JSONL conversation files", removed)
	}
	return nil
}

func (s *Storage) remigrationRequested(convID string) bool {
	_, err := os.Stat(filepath.Join(s.dir, RemigrateDir, remigrationMarkerName(convID)))
	return err == nil
}

func sleepForMigration(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
