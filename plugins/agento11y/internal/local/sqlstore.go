package local

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/chatstore"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"
)

const (
	// DatabaseFile is the SQLite database that backs the local viewer.
	DatabaseFile = "conversations.db"
	// RemigrateDir holds one marker per conversation whose JSONL append
	// succeeded after its SQLite mirror write failed.
	RemigrateDir = "remigrate"
)

type sqlConversation struct {
	ConvID       string    `gorm:"column:conv_id;primaryKey;index:idx_conversation_activity,priority:2"`
	Title        string    `gorm:"column:title;not null"`
	Activity     time.Time `gorm:"column:activity;index:idx_conversation_activity,sort:desc,priority:1"`
	StartedAt    time.Time `gorm:"column:started_at"`
	Calls        int       `gorm:"column:calls;not null"`
	InputTokens  int64     `gorm:"column:in_tok;not null"`
	OutputTokens int64     `gorm:"column:out_tok;not null"`
	TotalTokens  int64     `gorm:"column:total_tok;not null"`
	FreshInput   int64     `gorm:"column:fresh_input;not null"`
	CacheRead    int64     `gorm:"column:cache_read;not null"`
	CacheWrite   int64     `gorm:"column:cache_write;not null"`
	Output       int64     `gorm:"column:output;not null"`
	Reasoning    int64     `gorm:"column:reasoning;not null"`
	Agents       string    `gorm:"column:agents;not null"`
	Models       string    `gorm:"column:models;not null"`
	Status       string    `gorm:"column:status;not null"`
	Workspace    string    `gorm:"column:workspace;not null"`
	Branch       string    `gorm:"column:branch;not null"`
	Subagents    int       `gorm:"column:subagents;not null"`
}

func (sqlConversation) TableName() string { return "conversation" }

type sqlGeneration struct {
	RowID        int64     `gorm:"column:rowid;primaryKey;autoIncrement"`
	GenID        *string   `gorm:"column:gen_id;uniqueIndex"`
	ConvID       string    `gorm:"column:conv_id;not null;index:idx_generation_conversation,priority:1"`
	ReceivedAt   string    `gorm:"column:received_at;not null"`
	Activity     time.Time `gorm:"column:activity"`
	StartedAt    time.Time `gorm:"column:started_at;index:idx_generation_conversation,priority:2"`
	CompletedAt  time.Time `gorm:"column:completed_at"`
	Agent        string    `gorm:"column:agent;not null"`
	Model        string    `gorm:"column:model;not null"`
	Provider     string    `gorm:"column:provider;not null"`
	InputTokens  int64     `gorm:"column:in_tok;not null"`
	OutputTokens int64     `gorm:"column:out_tok;not null"`
	TotalTokens  int64     `gorm:"column:total_tok;not null"`
	FreshInput   int64     `gorm:"column:fresh_input;not null"`
	CacheRead    int64     `gorm:"column:cache_read;not null"`
	CacheWrite   int64     `gorm:"column:cache_write;not null"`
	Output       int64     `gorm:"column:output;not null"`
	Reasoning    int64     `gorm:"column:reasoning;not null"`
	CallError    string    `gorm:"column:call_error;not null"`
	Title        string    `gorm:"column:title;not null"`
	Workspace    string    `gorm:"column:workspace;not null"`
	Branch       string    `gorm:"column:branch;not null"`
	IsSubagent   bool      `gorm:"column:is_subagent;not null"`
	Body         string    `gorm:"column:body;not null"`
	Raw          []byte    `gorm:"column:raw;not null"`
}

func (sqlGeneration) TableName() string { return "generation" }

type migratedFile struct {
	Path       string     `gorm:"column:path;primaryKey"`
	Size       int64      `gorm:"column:size;not null"`
	MTime      int64      `gorm:"column:mtime;not null"`
	Rows       int64      `gorm:"column:rows;not null"`
	TargetSize int64      `gorm:"column:target_size;not null"`
	DoneAt     *time.Time `gorm:"column:done_at"`
}

func (migratedFile) TableName() string { return "migrated_file" }

type sqlMeta struct {
	K string `gorm:"column:k;primaryKey"`
	V string `gorm:"column:v;not null"`
}

func (sqlMeta) TableName() string { return "meta" }

type sqlStore struct {
	db      *gorm.DB
	writeMu sync.Mutex
}

type sqliteLogWriter struct {
	logger *log.Logger
}

func (w sqliteLogWriter) Printf(format string, args ...any) {
	w.logger.Printf("local sqlite: "+format, args...)
}

func openSQLStore(dir string, logger *log.Logger) (*sqlStore, error) {
	params := url.Values{}
	params.Add("_pragma", "journal_mode(WAL)")
	params.Add("_pragma", "synchronous(NORMAL)")
	params.Add("_pragma", "busy_timeout(5000)")
	dsn := chatstore.URI(filepath.Join(dir, DatabaseFile)) + "?" + params.Encode()

	var writer gormlogger.Writer = log.New(io.Discard, "", 0)
	if logger != nil {
		writer = sqliteLogWriter{logger: logger}
	}
	gormLog := gormlogger.New(writer, gormlogger.Config{
		SlowThreshold:             200 * time.Millisecond,
		LogLevel:                  gormlogger.Warn,
		IgnoreRecordNotFoundError: true,
		ParameterizedQueries:      true,
	})
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger:      gormLog,
		PrepareStmt: true,
	})
	if err != nil {
		return nil, fmt.Errorf("local sqlite: open %s: %w", filepath.Join(dir, DatabaseFile), err)
	}
	store := &sqlStore{db: db}
	if err := store.migrateSchema(); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

func (s *sqlStore) migrateSchema() error {
	if err := s.db.AutoMigrate(&sqlConversation{}, &sqlGeneration{}, &migratedFile{}, &sqlMeta{}); err != nil {
		return fmt.Errorf("local sqlite: migrate schema: %w", err)
	}
	if err := s.db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS gen_fts USING fts5(
		gen_id UNINDEXED, conv_id UNINDEXED, body,
		content='', contentless_unindexed=1, contentless_delete=1, tokenize='unicode61')`).Error; err != nil {
		return fmt.Errorf("local sqlite: create full-text index: %w", err)
	}
	return nil
}

func (s *sqlStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	db, err := s.db.DB()
	if err != nil {
		return err
	}
	return db.Close()
}

func (s *sqlStore) writeGenerations(recs []generationRecord) error {
	if s == nil || len(recs) == 0 {
		return nil
	}
	rows, err := sqlRowsFromRecords(recs)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.db.Transaction(func(tx *gorm.DB) error {
		conversations, err := upsertSQLGenerations(tx, rows)
		if err != nil {
			return err
		}
		for convID := range conversations {
			if err := recomputeSQLConversation(tx, convID); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *sqlStore) appendGenerations(path string, previousSize, currentSize int64, previousMTime, mtime time.Time, recs []generationRecord) error {
	if s == nil || len(recs) == 0 {
		return nil
	}
	rows, err := sqlRowsFromRecords(recs)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.db.Transaction(func(tx *gorm.DB) error {
		var previous migratedFile
		stateErr := tx.Where("path = ?", path).Take(&previous).Error
		if stateErr != nil && !errors.Is(stateErr, gorm.ErrRecordNotFound) {
			return stateErr
		}
		currentBeforeAppend := errors.Is(stateErr, gorm.ErrRecordNotFound) && previousSize == 0
		if stateErr == nil {
			currentBeforeAppend = previous.DoneAt != nil && previous.Size == previousSize && previous.MTime == previousMTime.UnixNano()
		}
		progress := migratedFile{
			Path:       path,
			Size:       currentSize,
			MTime:      mtime.UnixNano(),
			TargetSize: previousSize,
		}
		if currentBeforeAppend {
			now := time.Now().UTC()
			progress.DoneAt = &now
			progress.Rows = previous.Rows + int64(len(rows))
			progress.TargetSize = currentSize
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "path"}},
			UpdateAll: true,
		}).Create(&progress).Error; err != nil {
			return err
		}
		conversations, err := upsertSQLGenerations(tx, rows)
		if err != nil {
			return err
		}
		for convID := range conversations {
			if err := recomputeSQLConversation(tx, convID); err != nil {
				return err
			}
		}
		return nil
	})
}

func sqlRowsFromRecords(recs []generationRecord) ([]sqlGeneration, error) {
	rows := make([]sqlGeneration, 0, len(recs))
	for _, rec := range recs {
		row, err := sqlGenerationFromRecord(rec)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func upsertSQLGenerations(tx *gorm.DB, rows []sqlGeneration) (map[string]struct{}, error) {
	conversations := map[string]struct{}{}
	for _, row := range rows {
		if row.GenID == nil {
			continue
		}
		var existing sqlGeneration
		if err := tx.Select("conv_id").Where("gen_id = ?", *row.GenID).Take(&existing).Error; err == nil {
			conversations[existing.ConvID] = struct{}{}
		} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
	}
	if err := tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "gen_id"}},
		UpdateAll: true,
	}).CreateInBatches(&rows, 200).Error; err != nil {
		return nil, err
	}
	for i := range rows {
		row := &rows[i]
		if row.GenID != nil {
			if err := tx.Select("rowid").Where("gen_id = ?", *row.GenID).Take(row).Error; err != nil {
				return nil, err
			}
		}
		if err := tx.Exec("DELETE FROM gen_fts WHERE rowid = ?", row.RowID).Error; err != nil {
			return nil, err
		}
		if err := tx.Exec("INSERT INTO gen_fts(rowid, gen_id, conv_id, body) VALUES (?, ?, ?, ?)",
			row.RowID, row.GenID, row.ConvID, row.Body).Error; err != nil {
			return nil, err
		}
		conversations[row.ConvID] = struct{}{}
	}
	return conversations, nil
}

func sqlGenerationFromRecord(rec generationRecord) (sqlGeneration, error) {
	gen, err := storedGenerationFromRaw(rec.Generation, rec.ConversationID)
	if err != nil {
		return sqlGeneration{}, err
	}
	id := strings.TrimSpace(rec.GenerationID)
	if id == "" {
		id = strings.TrimSpace(gen.ID)
	}
	var genID *string
	if id != "" {
		genID = &id
	}
	usage := gen.Usage.toSDK()
	buckets := disjointTokenUsage(usage, gen.Model.Provider)
	return sqlGeneration{
		GenID:        genID,
		ConvID:       rec.ConversationID,
		ReceivedAt:   normalizeSQLTimestamp(rec.ReceivedAt),
		Activity:     normalizeSQLTime(recordActivity(gen.summaryGeneration, rec.ReceivedAt)),
		StartedAt:    normalizeSQLTime(gen.StartedAt),
		CompletedAt:  normalizeSQLTime(gen.CompletedAt),
		Agent:        gen.AgentName,
		Model:        gen.modelName(),
		Provider:     gen.Model.Provider,
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		TotalTokens:  totalTokensForView(usage, gen.Model.Provider),
		FreshInput:   buckets.FreshInput,
		CacheRead:    buckets.CacheRead,
		CacheWrite:   buckets.CacheWrite,
		Output:       buckets.Output,
		Reasoning:    buckets.Reasoning,
		CallError:    gen.CallError,
		Title:        gen.title(),
		Workspace:    gen.Tags["cwd"],
		Branch:       gen.Tags["git.branch"],
		IsSubagent:   strings.Contains(gen.AgentName, "/"),
		Body:         generationSearchBody(gen),
		Raw:          append([]byte(nil), rec.Generation...),
	}, nil
}

func storedGenerationFromRaw(raw []byte, convID string) (storedGeneration, error) {
	var gen storedGeneration
	if err := json.Unmarshal(raw, &gen); err != nil {
		var summary summaryGeneration
		if summaryErr := json.Unmarshal(raw, &summary); summaryErr != nil {
			return storedGeneration{}, fmt.Errorf("local sqlite: decode generation: %w", err)
		}
		gen = storedGeneration{summaryGeneration: summary, ConversationID: convID}
	}
	return gen, nil
}

func generationSearchBody(gen storedGeneration) string {
	var fragments []string
	visitGenerationSearchText(gen, func(text string) {
		if text != "" {
			fragments = append(fragments, text)
		}
	})
	return strings.Join(fragments, "\n")
}

func visitGenerationSearchText(gen storedGeneration, visit func(string)) {
	visit(gen.AgentName)
	visit(gen.modelName())
	visit(gen.title())
	for _, msg := range gen.inputMessages() {
		visitMessageParts(msg, visit)
	}
	for _, msg := range gen.outputMessages() {
		visitMessageParts(msg, visit)
	}
}

func normalizeSQLTime(value time.Time) time.Time {
	if value.IsZero() {
		return value
	}
	return value.UTC()
}

func normalizeSQLTimestamp(value string) string {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return value
	}
	return parsed.UTC().Format(time.RFC3339Nano)
}

func recomputeSQLConversation(tx *gorm.DB, convID string) error {
	var rows []sqlGeneration
	if err := tx.Select(`rowid, gen_id, conv_id, activity, started_at, agent, model,
		in_tok, out_tok, total_tok, fresh_input, cache_read, cache_write, output,
		reasoning, call_error, title, workspace, branch, is_subagent`).
		Where("conv_id = ?", convID).Order("rowid ASC").Find(&rows).Error; err != nil {
		return err
	}
	conversation := buildSQLConversation(convID, rows)
	if conversation == nil {
		return tx.Delete(&sqlConversation{}, "conv_id = ?", convID).Error
	}
	return upsertSQLConversation(tx, conversation)
}

func buildSQLConversation(convID string, rows []sqlGeneration) *sqlConversation {
	if len(rows) == 0 {
		return nil
	}
	agents := map[string]struct{}{}
	models := map[string]struct{}{}
	conversation := &sqlConversation{ConvID: convID, Calls: len(rows), Status: "ok"}
	for _, row := range rows {
		conversation.InputTokens += row.InputTokens
		conversation.OutputTokens += row.OutputTokens
		conversation.TotalTokens += row.TotalTokens
		conversation.FreshInput += row.FreshInput
		conversation.CacheRead += row.CacheRead
		conversation.CacheWrite += row.CacheWrite
		conversation.Output += row.Output
		conversation.Reasoning += row.Reasoning
		if !row.StartedAt.IsZero() && (conversation.StartedAt.IsZero() || row.StartedAt.Before(conversation.StartedAt)) {
			conversation.StartedAt = row.StartedAt
		}
		if row.Activity.After(conversation.Activity) {
			conversation.Activity = row.Activity
		}
		if row.Agent != "" {
			agents[row.Agent] = struct{}{}
		}
		if row.Model != "" {
			models[row.Model] = struct{}{}
		}
		if conversation.Title == "" {
			conversation.Title = row.Title
		}
		if conversation.Workspace == "" {
			conversation.Workspace = row.Workspace
		}
		if conversation.Branch == "" {
			conversation.Branch = row.Branch
		}
		if row.IsSubagent {
			conversation.Subagents++
		}
		if row.CallError != "" {
			conversation.Status = "err"
		}
	}
	conversation.Agents = encodeStringList(sortedKeys(agents))
	conversation.Models = encodeStringList(sortedKeys(models))
	return conversation
}

func upsertSQLConversation(tx *gorm.DB, conversation *sqlConversation) error {
	return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "conv_id"}}, UpdateAll: true}).Create(conversation).Error
}

func encodeStringList(values []string) string {
	sort.Strings(values)
	data, _ := json.Marshal(values)
	return string(data)
}

func (s *Storage) sqliteReadsReady() (bool, error) {
	if s == nil || s.sql == nil {
		return false, nil
	}
	retired, err := s.sql.jsonlRetired()
	if err != nil || retired {
		return retired, err
	}
	complete, err := s.sql.migrationComplete()
	if err != nil || !complete {
		return false, err
	}
	entries, err := os.ReadDir(filepath.Join(s.dir, RemigrateDir))
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

func (s *Storage) markForRemigration(convID string) error {
	if convID == "" {
		return nil
	}
	dir := filepath.Join(s.dir, RemigrateDir)
	return writeFileAtomic(dir, remigrationMarkerName(convID), nil)
}

func remigrationMarkerName(convID string) string {
	return convID + ".retry"
}
