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
	migrationCompleteKey         = "jsonl_migration_complete"
	jsonlRetiredKey              = "jsonl_retired"
	migrationRowsPerTransaction  = 200
	migrationBytesPerTransaction = 16 << 20

	// migrationDiskFactor estimates database bytes per JSONL byte. A 400-file
	// sample with deflated raw measured 0.6; the rest is headroom for the WAL.
	migrationDiskFactor  = 0.8
	migrationDiskReserve = 1 << 30
)

type migrationScanner func(context.Context, string, int64, func(generationRecord, storedGeneration) error) (jsonlScanResult, error)
type migrationSleeper func(context.Context, time.Duration) error

type storeMigrator struct {
	storage   *Storage
	sql       *sqlStore
	scan      migrationScanner
	sleep     migrationSleeper
	now       func() time.Time
	freeBytes func(dir string) (uint64, error)
	rows      int
	bytes     int
	// additive imports into a retired store, where SQLite is the only copy:
	// it never deletes rows and never replaces a newer copy of a generation.
	additive bool
}

func newStoreMigrator(storage *Storage) *storeMigrator {
	freeBytes := storage.freeBytes
	if freeBytes == nil {
		freeBytes = freeDiskBytes
	}
	return &storeMigrator{
		storage:   storage,
		sql:       storage.sql,
		scan:      scanLatestGenerationRecordsUpTo,
		sleep:     sleepForMigration,
		now:       time.Now,
		freeBytes: freeBytes,
		rows:      migrationRowsPerTransaction,
		bytes:     migrationBytesPerTransaction,
	}
}

type diskSpaceError struct {
	dir        string
	need, free uint64
}

func (e *diskSpaceError) Error() string {
	return fmt.Sprintf("SQLite migration needs %d free bytes in %s, %d available", e.need, e.dir, e.free)
}

func (m *storeMigrator) checkFreeSpace(need uint64) error {
	free, err := m.freeBytes(m.storage.Dir())
	if err != nil {
		return fmt.Errorf("check free disk space: %w", err)
	}
	if free < need {
		return &diskSpaceError{dir: m.storage.Dir(), need: need, free: free}
	}
	return nil
}

func migrateStoreOnStartup(ctx context.Context, storage *Storage, hub *eventHub) {
	if !ReceiverSupported() || storage == nil || storage.sql == nil {
		return
	}
	changed, err := migrateStore(ctx, storage)
	if changed {
		hub.broadcast(changeEvent{})
	}
	var spaceErr *diskSpaceError
	switch {
	case err == nil:
	case errors.As(err, &spaceErr):
		if retired, _ := storage.sql.jsonlRetired(); retired {
			storage.logf("local: SQLite migration paused: it needs %d free bytes in %s and %d are available; JSONL written after retirement stays unimported until a later start finds the space",
				spaceErr.need, spaceErr.dir, spaceErr.free)
			break
		}
		storage.logf("local: SQLite migration paused: it needs %d free bytes in %s and %d are available; reads stay on JSONL until a later start finds the space",
			spaceErr.need, spaceErr.dir, spaceErr.free)
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
			// A pre-SQLite daemon sharing the directory, or a downgrade, can
			// write JSONL after retirement.
			migrator := newStoreMigrator(storage)
			migrator.additive = true
			changed, err = migrator.run(ctx)
			if err != nil {
				return err
			}
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
	// A retired store reads SQLite whatever the completion flag says.
	complete := m.additive
	if !m.additive {
		if complete, err = m.sql.migrationComplete(); err != nil {
			return false, err
		}
	}
	var pending []conversationFile
	var remaining uint64
	for _, file := range files {
		needed, err := m.fileNeedsMigration(file)
		if err != nil {
			return false, err
		}
		if needed {
			pending = append(pending, file)
			remaining += uint64(max(file.size, 0))
		}
	}
	if complete && len(pending) == 0 {
		return false, nil
	}
	if !m.additive {
		if err := m.sql.setMigrationComplete(false); err != nil {
			return false, err
		}
	}
	if len(pending) > 0 {
		if err := m.checkFreeSpace(uint64(float64(remaining)*migrationDiskFactor) + migrationDiskReserve); err != nil {
			return false, err
		}
	}

	m.storage.logf("local: checking %d conversation files for SQLite migration", len(files))
	started := time.Now()
	migrated, failed := 0, 0
	for _, file := range pending {
		if err := ctx.Err(); err != nil {
			return migrated > 0, err
		}
		if err := m.checkFreeSpace(migrationDiskReserve); err != nil {
			return migrated > 0, err
		}
		if err := m.migrateFile(ctx, file); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return migrated > 0, ctxErr
			}
			m.storage.logf("local: migrate %s to SQLite: %v", filepath.Base(file.path), err)
			failed++
			continue
		}
		migrated++
	}
	if failed > 0 {
		return migrated > 0, fmt.Errorf("%d of %d conversation files failed and will be retried on the next start", failed, len(pending))
	}
	if m.additive {
		m.storage.logf("local: imported %d JSONL conversation files written after JSONL retirement into SQLite, took %s",
			migrated, time.Since(started).Round(time.Millisecond))
		return migrated > 0, nil
	}
	if err := m.sql.setMigrationComplete(true); err != nil {
		return migrated > 0, err
	}
	m.storage.logf("local: SQLite migration: %d files migrated, %d already current, took %s",
		migrated, len(files)-len(pending), time.Since(started).Round(time.Millisecond))
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
	// Additive mode cannot reset, and upsert cannot deduplicate rows without an
	// id. Conversation files are append-only, so a file at least as large as at
	// its earlier import still starts with the state.Rows rows already in
	// SQLite; among those, only rows with an id are written again.
	idOnlyBelow := 0
	if m.additive && restart && !forceReset && state != nil && info.Size() >= state.Size {
		idOnlyBelow = int(max(state.Rows, 0))
	}
	reset := restart && !m.additive
	chunkSize := max(1, m.rows)
	chunk := make([]sqlGeneration, 0, chunkSize)
	chunkBytes := 0
	rowsSeen := 0
	firstWrite := true
	unreadable := 0
	flush := func(done bool) error {
		if len(chunk) == 0 && !done {
			return nil
		}
		began := time.Now()
		err := m.writeChunk(ctx, file, path, info, chunk, reset && firstWrite, rowsSeen, unreadable, done)
		if err != nil {
			return err
		}
		firstWrite = false
		chunk = chunk[:0]
		chunkBytes = 0
		return m.sleep(ctx, time.Since(began))
	}

	result, err := m.scan(ctx, file.path, info.Size(), func(rec generationRecord, gen storedGeneration) error {
		rowsSeen++
		if rowsSeen <= start {
			return nil
		}
		// File the row under the id from the file name: JSONL detail reads
		// <id>.jsonl, and the reset DELETE in writeChunk matches conv_id = file.id.
		rec.ConversationID = file.id
		row := sqlGenerationFromStored(rec, gen)
		if rowsSeen <= idOnlyBelow && row.GenID == nil {
			return nil
		}
		chunk = append(chunk, row)
		chunkBytes += len(row.Raw) + len(row.Body)
		if len(chunk) == chunkSize || chunkBytes >= m.bytes {
			return flush(false)
		}
		return nil
	})
	m.storage.logSkipped("migrate "+filepath.Base(file.path), result.skipped)
	if err != nil {
		return err
	}
	if start > rowsSeen {
		return errMigrationProgressInvalid
	}
	unreadable = result.unreadable
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
	unreadable int,
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
		if m.additive {
			var err error
			if rows, err = dropOlderSQLGenerations(tx, rows); err != nil {
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
		// Retired reads come from SQLite, so an additive import updates the
		// conversation row after every chunk.
		if done || m.additive {
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
			Unreadable: int64(unreadable),
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
	files, err := storage.conversationFiles()
	if err != nil {
		return err
	}
	var removed, moved, kept int
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		outcome, err := retireLegacyFile(storage, file)
		if err != nil {
			return err
		}
		switch outcome {
		case legacyFileRemoved:
			removed++
		case legacyFileMoved:
			moved++
		default:
			kept++
		}
	}
	if err := os.Remove(filepath.Join(storage.Dir(), legacyStoreFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if removed > 0 {
		storage.logf("local: retired %d migrated JSONL conversation files", removed)
	}
	if moved > 0 {
		storage.logf("local: moved %d JSONL conversation files with unreadable lines to %s", moved, filepath.Join(storage.Dir(), LegacyUnreadableDir))
	}
	if kept > 0 {
		storage.logf("local: kept %d JSONL conversation files that changed after migration; the next start imports them", kept)
	}
	return nil
}

type legacyFileOutcome int

const (
	legacyFileKept legacyFileOutcome = iota
	legacyFileRemoved
	legacyFileMoved
)

func retireLegacyFile(storage *Storage, file conversationFile) (legacyFileOutcome, error) {
	if storage.remigrationRequested(file.id) {
		return legacyFileKept, nil
	}
	name := filepath.Join(ConversationsDir, filepath.Base(file.path))
	state, err := storage.sql.migrationState(name)
	if err != nil || state == nil || state.DoneAt == nil {
		return legacyFileKept, err
	}
	lock := storage.lockFor(file.path)
	lock.Lock()
	defer lock.Unlock()
	// The lock excludes this process only. Stat again just before the removal
	// so a file a pre-SQLite daemon appended to after migration stays. An
	// append between this stat and the remove or rename is not imported.
	info, err := os.Stat(file.path)
	if errors.Is(err, os.ErrNotExist) {
		return legacyFileKept, nil
	}
	if err != nil {
		return legacyFileKept, err
	}
	if info.Size() != state.Size || info.ModTime().UnixNano() != state.MTime {
		return legacyFileKept, nil
	}
	outcome := legacyFileRemoved
	if state.Unreadable > 0 {
		if err := moveToLegacyUnreadable(storage.Dir(), file.path); err != nil {
			return legacyFileKept, err
		}
		outcome = legacyFileMoved
	} else if err := os.Remove(file.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return legacyFileKept, err
	}
	// A file created later at this path is new. If it is at least as large as
	// this one, a stale row would make the additive import skip its ID-less
	// rows up to state.Rows.
	if err := storage.sql.db.Delete(&migratedFile{}, "path = ?", name).Error; err != nil {
		return outcome, err
	}
	return outcome, nil
}

func moveToLegacyUnreadable(dir, path string) error {
	target := filepath.Join(dir, LegacyUnreadableDir)
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	dest := filepath.Join(target, filepath.Base(path))
	if _, err := os.Lstat(dest); err == nil {
		dest = filepath.Join(target, fmt.Sprintf("%s.%d", filepath.Base(path), time.Now().UnixNano()))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(path, dest)
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
