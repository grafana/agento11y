package local

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/grafana/agento11y/go/agento11y"
	"gorm.io/gorm"
)

// ConversationSummary is one row in the viewer's list screen. Numeric
// fields are raw so the client can format them (k/M, ms/s/m) and reuse
// them for tooltips, sort, and the activity histogram.
type ConversationSummary struct {
	ID           string    `json:"id"`
	Title        string    `json:"title,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	LastActivity time.Time `json:"last_activity"`
	Calls        int       `json:"calls"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	TotalTokens  int64     `json:"total_tokens"`
	// TokenBuckets sums the disjoint per-generation buckets across the
	// conversation, for the list's token breakdown tooltip.
	TokenBuckets TokenBuckets `json:"token_buckets"`
	Agents       []string     `json:"agents"`
	Models       []string     `json:"models"`
	// Status is "ok" or "err". "err" means at least one generation in
	// the conversation recorded a call_error.
	Status string `json:"status"`
	// Workspace is the session's working directory (the cwd tag); Branch
	// is the git.branch tag. The viewer groups and filters the list by
	// these. Subagents is the number of subagent steps (generations whose
	// agent_name carries a "parent/child" suffix) — a cheap signal for
	// flagging orchestration-heavy conversations in the list.
	Workspace string `json:"workspace,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Subagents int    `json:"subagents,omitempty"`
}

// GenerationView is one step in the conversation thread.
//
// Messages is the display-order thread for the local viewer. Input and
// Output keep the raw SDK split: user/tool-result messages on input,
// assistant messages on output. They are empty under the default
// metadata_only mode, in which case the viewer should fall back to the
// token counts and tool preview.
type GenerationView struct {
	GenerationID    string    `json:"generation_id"`
	AgentName       string    `json:"agent_name,omitempty"`
	Model           string    `json:"model,omitempty"`
	Provider        string    `json:"provider,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	CompletedAt     time.Time `json:"completed_at"`
	DurationSeconds float64   `json:"duration_seconds"`
	InputTokens     int64     `json:"input_tokens"`
	OutputTokens    int64     `json:"output_tokens"`
	TotalTokens     int64     `json:"total_tokens"`
	// TokenBuckets is this step's disjoint usage split, so the viewer
	// can show where the step's tokens went (cache hit vs fresh input).
	TokenBuckets TokenBuckets        `json:"token_buckets"`
	Messages     []agento11y.Message `json:"messages,omitempty"`
	Input        []agento11y.Message `json:"input,omitempty"`
	Output       []agento11y.Message `json:"output,omitempty"`
	Tools        []string            `json:"tools,omitempty"`
	ToolPreview  string              `json:"tool_preview,omitempty"`
	// Skills is the on-demand skill set this generation loaded. Unlike
	// Tools (derived from message content) it is read from the
	// generation's `skills` field. Empty for the common no-skill turn.
	Skills     []SkillView `json:"skills,omitempty"`
	StopReason string      `json:"stop_reason,omitempty"`
	CallError  string      `json:"call_error,omitempty"`
	// ParentGenerationIDs links this step to the generation(s) that
	// caused it. A cross-agent edge (parent has a different agent_name)
	// marks a subagent launch; same-agent edges chain a single agent's
	// steps. The viewer uses these to build the subagent tree/timeline.
	ParentGenerationIDs []string `json:"parent_generation_ids,omitempty"`
	// ThinkingEnabled reports that the model reasoned on this step. Claude
	// Code transcripts record the flag but not the reasoning text, so the
	// viewer can note "reasoning used" even when no thinking part exists.
	ThinkingEnabled bool `json:"thinking_enabled,omitempty"`
}

// SkillView is one skill a generation loaded, as shown in the viewer.
// Name is the full invoked string, plugin-qualified for plugin skills
// (e.g. "workflow-toolkit:code-review"); the client splits the plugin
// prefix for display.
type SkillView struct {
	Name        string `json:"name"`
	ID          string `json:"id,omitempty"`
	Description string `json:"description,omitempty"`
	Version     string `json:"version,omitempty"`
}

// ConversationDetail is the payload for the detail screen — the
// conversation header plus its chronological generation list.
type ConversationDetail struct {
	ID          string           `json:"id"`
	Title       string           `json:"title,omitempty"`
	Generations []GenerationView `json:"generations"`
}

// ConversationListOptions bounds one conversation-list request.
//
// SQLite applies Since and Limit while querying the returned page. The
// JSONL fallback decodes the legacy store before filtering and limiting it.
type ConversationListOptions struct {
	// Limit caps how many conversations are returned.
	// ≤ 0 means unbounded.
	Limit int
	// Since drops conversations whose last activity predates it. The bound
	// is inclusive, so a conversation whose last activity is exactly Since
	// is kept. Zero means no lower bound.
	Since time.Time
}

// ListConversations produces one ConversationSummary per conversation,
// newest-first by activity with ties broken by conversation id. total counts
// the store before Limit and Since, so a caller holding one page still knows
// whether the store is empty. When sqliteReadsReady reports false, it
// decodes legacy JSONL directly.
func (s *Storage) ListConversations(opts ConversationListOptions) (page []ConversationSummary, total int, err error) {
	ready, err := s.sqliteReadsReady()
	if err != nil {
		return nil, 0, err
	}
	if ready {
		return s.listConversationsSQL(opts)
	}
	files, err := s.conversationFiles()
	if err != nil {
		return nil, 0, err
	}
	out := make([]ConversationSummary, 0, len(files))
	var skipped int
	defer func() { s.logSkipped("list conversations", skipped) }()
	for _, f := range files {
		entry, err := decodeFileSummary(f)
		if err != nil {
			return nil, 0, err
		}
		skipped += entry.skipped
		if entry.ok && (opts.Since.IsZero() || !entry.summary.LastActivity.Before(opts.Since)) {
			out = append(out, entry.summary)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastActivity.Equal(out[j].LastActivity) {
			return out[i].LastActivity.After(out[j].LastActivity)
		}
		return out[i].ID < out[j].ID
	})
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, len(files), nil
}

func (s *Storage) listConversationsSQL(opts ConversationListOptions) ([]ConversationSummary, int, error) {
	var total int64
	if err := s.sql.db.Model(&sqlConversation{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	query := s.sql.db.Model(&sqlConversation{}).Order("activity DESC").Order("conv_id ASC")
	if !opts.Since.IsZero() {
		query = query.Where("activity >= ?", opts.Since)
	}
	if opts.Limit > 0 {
		query = query.Limit(opts.Limit)
	}
	var rows []sqlConversation
	if err := query.Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := make([]ConversationSummary, 0, len(rows))
	for _, row := range rows {
		agents, err := decodeSQLStringList(row.Agents)
		if err != nil {
			return nil, 0, err
		}
		models, err := decodeSQLStringList(row.Models)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, ConversationSummary{
			ID:           row.ConvID,
			Title:        row.Title,
			StartedAt:    row.StartedAt,
			LastActivity: row.Activity,
			Calls:        row.Calls,
			InputTokens:  row.InputTokens,
			OutputTokens: row.OutputTokens,
			TotalTokens:  row.TotalTokens,
			TokenBuckets: TokenBuckets{
				FreshInput: row.FreshInput,
				CacheRead:  row.CacheRead,
				CacheWrite: row.CacheWrite,
				Output:     row.Output,
				Reasoning:  row.Reasoning,
			},
			Agents:    agents,
			Models:    models,
			Status:    row.Status,
			Workspace: row.Workspace,
			Branch:    row.Branch,
			Subagents: row.Subagents,
		})
	}
	return out, int(total), nil
}

func decodeSQLStringList(raw string) ([]string, error) {
	out := []string{}
	if raw == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// conversationFile describes one legacy conversation JSONL file.
// fileNeedsMigration checks its retry marker, size, and modTime against the
// migrated_file state.
type conversationFile struct {
	id      string
	path    string
	size    int64
	modTime time.Time
}

// conversationFiles lists legacy conversation files by id. A missing
// directory yields no files and no error.
func (s *Storage) conversationFiles() ([]conversationFile, error) {
	dir := filepath.Join(s.dir, ConversationsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]conversationFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue // removed between ReadDir and Info
			}
			return nil, err
		}
		out = append(out, conversationFile{
			id:      strings.TrimSuffix(e.Name(), ".jsonl"),
			path:    filepath.Join(dir, e.Name()),
			size:    info.Size(),
			modTime: info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out, nil
}

// fileSummary is the uncached JSONL projection used whenever
// sqliteReadsReady reports false.
type fileSummary struct {
	summary ConversationSummary
	ok      bool
	points  []TokenUsagePoint
	first   time.Time
	last    time.Time
	skipped int
}

func (e *fileSummary) bounds(since time.Time) (first, last time.Time, ok bool) {
	if len(e.points) == 0 {
		return time.Time{}, time.Time{}, false
	}
	if since.IsZero() || !e.first.Before(since) {
		return e.first, e.last, true
	}
	if e.last.Before(since) {
		return time.Time{}, time.Time{}, false
	}
	for _, point := range e.points {
		if point.Timestamp.Before(since) {
			continue
		}
		if !ok || point.Timestamp.Before(first) {
			first = point.Timestamp
		}
		if point.Timestamp.After(last) {
			last = point.Timestamp
		}
		ok = true
	}
	return first, last, ok
}

func decodeFileSummary(file conversationFile) (*fileSummary, error) {
	entry := &fileSummary{}
	agents := map[string]struct{}{}
	models := map[string]struct{}{}
	var summary ConversationSummary
	var hasError, seen bool

	skipped, err := scanLatestSummaryRecords(file.path, func(rec summaryRecord) {
		gen := rec.Generation
		seen = true
		if summary.ID == "" {
			summary.ID = rec.ConversationID
		}
		summary.Calls++
		usage := gen.Usage.toSDK()
		summary.InputTokens += usage.InputTokens
		summary.OutputTokens += usage.OutputTokens
		summary.TotalTokens += totalTokensForView(usage, gen.Model.Provider)
		summary.TokenBuckets = summary.TokenBuckets.plus(disjointTokenUsage(usage, gen.Model.Provider))
		if !gen.StartedAt.IsZero() && (summary.StartedAt.IsZero() || gen.StartedAt.Before(summary.StartedAt)) {
			summary.StartedAt = gen.StartedAt
		}
		if when := recordActivity(gen, rec.ReceivedAt); when.After(summary.LastActivity) {
			summary.LastActivity = when
		}
		if gen.AgentName != "" {
			agents[gen.AgentName] = struct{}{}
		}
		if model := gen.modelName(); model != "" {
			models[model] = struct{}{}
		}
		if summary.Title == "" {
			summary.Title = gen.title()
		}
		if summary.Workspace == "" {
			summary.Workspace = gen.Tags["cwd"]
		}
		if summary.Branch == "" {
			summary.Branch = gen.Tags["git.branch"]
		}
		if strings.Contains(gen.AgentName, "/") {
			summary.Subagents++
		}
		if gen.CallError != "" {
			hasError = true
		}
		if point, ok := tokenUsagePoint(rec); ok {
			entry.points = append(entry.points, point)
			if entry.first.IsZero() || point.Timestamp.Before(entry.first) {
				entry.first = point.Timestamp
			}
			if point.Timestamp.After(entry.last) {
				entry.last = point.Timestamp
			}
		}
	})
	entry.skipped = skipped
	if err != nil {
		return nil, err
	}
	if !seen {
		return entry, nil
	}
	summary.Agents = sortedKeys(agents)
	summary.Models = sortedKeys(models)
	summary.Status = "ok"
	if hasError {
		summary.Status = "err"
	}
	entry.summary = summary
	entry.ok = true
	return entry, nil
}

// ConversationDetail returns the chronological generation list for one
// conversation. Returns (nil, nil) when no generations are recorded for
// the given id, so the handler can answer 404 cleanly.
func (s *Storage) ConversationDetail(id string) (*ConversationDetail, error) {
	if !validConversationID(id) {
		return nil, errors.New("invalid conversation id")
	}
	ready, err := s.sqliteReadsReady()
	if err != nil {
		return nil, err
	}
	if ready {
		return s.conversationDetailSQL(id)
	}
	path := filepath.Join(s.dir, ConversationsDir, id+".jsonl")
	out := &ConversationDetail{ID: id}
	skipped, err := scanLatestGenerationRecords(path, func(_ generationRecord, gen storedGeneration) {
		if out.Title == "" && gen.title() != "" {
			out.Title = gen.title()
		}
		out.Generations = append(out.Generations, generationView(gen))
	})
	s.logSkipped("conversation "+id, skipped)
	if err != nil {
		return nil, err
	}
	if len(out.Generations) == 0 {
		return nil, nil
	}
	sort.SliceStable(out.Generations, func(i, j int) bool {
		return out.Generations[i].StartedAt.Before(out.Generations[j].StartedAt)
	})

	// Each step shows the data of exactly one model call: its own input (the
	// user prompt, or the tool results the harness fed into THIS request)
	// followed by its own output. A tool call's result lands in the NEXT
	// request's input, so it renders on the step that received it — not folded
	// back into the step that issued the call. That keeps every step's content
	// and tokens aligned with the single generation it represents.
	for i := range out.Generations {
		out.Generations[i].Messages = threadMessages(out.Generations[i].Input, out.Generations[i].Output)
	}
	return out, nil
}

func (s *Storage) conversationDetailSQL(id string) (*ConversationDetail, error) {
	var conversation sqlConversation
	if err := s.sql.db.Where("conv_id = ?", id).Take(&conversation).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var rows []sqlGeneration
	if err := s.sql.db.Where("conv_id = ?", id).Order("rowid ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := &ConversationDetail{ID: id, Title: conversation.Title, Generations: make([]GenerationView, 0, len(rows))}
	for _, row := range rows {
		gen, err := storedGenerationFromRaw(row.Raw, row.ConvID)
		if err != nil {
			return nil, err
		}
		out.Generations = append(out.Generations, generationView(gen))
	}
	if len(out.Generations) == 0 {
		return nil, nil
	}
	sort.SliceStable(out.Generations, func(i, j int) bool {
		return out.Generations[i].StartedAt.Before(out.Generations[j].StartedAt)
	})
	for i := range out.Generations {
		out.Generations[i].Messages = threadMessages(out.Generations[i].Input, out.Generations[i].Output)
	}
	return out, nil
}

func generationView(gen storedGeneration) GenerationView {
	usage := gen.Usage.toSDK()
	input := gen.inputMessages()
	output := gen.outputMessages()
	view := GenerationView{
		GenerationID:        gen.ID,
		AgentName:           gen.AgentName,
		Model:               gen.modelName(),
		Provider:            gen.Model.Provider,
		StartedAt:           gen.StartedAt,
		CompletedAt:         gen.CompletedAt,
		InputTokens:         usage.InputTokens,
		OutputTokens:        usage.OutputTokens,
		TotalTokens:         totalTokensForView(usage, gen.Model.Provider),
		TokenBuckets:        disjointTokenUsage(usage, gen.Model.Provider),
		Input:               input,
		Output:              output,
		StopReason:          gen.StopReason,
		CallError:           gen.CallError,
		ParentGenerationIDs: gen.ParentGenerationIDs,
		ThinkingEnabled:     gen.ThinkingEnabled,
	}
	if !gen.StartedAt.IsZero() && !gen.CompletedAt.IsZero() {
		view.DurationSeconds = gen.CompletedAt.Sub(gen.StartedAt).Seconds()
	}
	view.Tools, view.ToolPreview = extractTools(output)
	view.Skills = gen.skillViews()
	return view
}

// TokenBuckets is token usage split into five non-overlapping buckets
// (see disjointTokenUsage). Because they are disjoint, the viewer can
// stack or sum them without double-counting; the chart points, the
// conversation summaries, and the per-step views all share this shape.
type TokenBuckets struct {
	FreshInput int64 `json:"fresh_input"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Output     int64 `json:"output"`
	Reasoning  int64 `json:"reasoning"`
}

func (b TokenBuckets) plus(o TokenBuckets) TokenBuckets {
	return TokenBuckets{
		FreshInput: b.FreshInput + o.FreshInput,
		CacheRead:  b.CacheRead + o.CacheRead,
		CacheWrite: b.CacheWrite + o.CacheWrite,
		Output:     b.Output + o.Output,
		Reasoning:  b.Reasoning + o.Reasoning,
	}
}

// TokenUsagePoint is one generation's disjoint token buckets tagged
// with the model/provider that produced them and the time it ran. The
// viewer re-buckets these by time to draw the token-usage chart. The
// embedded TokenBuckets fields flatten into the JSON object.
type TokenUsagePoint struct {
	Timestamp time.Time `json:"t"`
	Model     string    `json:"model,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	TokenBuckets
}

// TokenUsageOptions bounds one token-metrics request.
type TokenUsageOptions struct {
	// Since drops older generations. SQLite applies it in the query; the
	// JSONL fallback decodes legacy files before applying it. Zero means no
	// lower bound.
	Since time.Time
	// Interval is the bucket width. Zero asks for a derived interval that
	// keeps the response under maxTokenUsageBuckets buckets.
	Interval time.Duration
}

// maxTokenUsageBuckets caps how many buckets a derived interval produces.
// The viewer draws at most CHART_BUCKET_MAX bars, so this is a safety
// bound on the response rather than a display choice.
const maxTokenUsageBuckets = 500

// tokenUsageIntervals is the ladder a derived interval is picked from.
// Every step divides the next, so a client that buckets on one step can
// fold points aggregated on a finer step without splitting a bucket
// across two bars. This list and BUCKET_INTERVALS_MS in app.jsx must stay
// equal; TestBucketLaddersAgree checks that.
var tokenUsageIntervals = []time.Duration{
	10 * time.Second,
	30 * time.Second,
	time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	30 * time.Minute,
	time.Hour,
	2 * time.Hour,
	4 * time.Hour,
	12 * time.Hour,
	24 * time.Hour,
	7 * 24 * time.Hour,
}

// TokenUsagePoints returns token usage aggregated per interval bucket and
// model, oldest-first, plus the interval used. Empty buckets are omitted,
// so the response holds at most one point per model and provider per
// non-empty bucket.
//
// A missing conversations dir yields no points. When sqliteReadsReady
// reports false, legacy JSONL files are decoded directly.
func (s *Storage) TokenUsagePoints(opts TokenUsageOptions) ([]TokenUsagePoint, time.Duration, error) {
	ready, err := s.sqliteReadsReady()
	if err != nil {
		return nil, 0, err
	}
	if ready {
		return s.tokenUsagePointsSQL(opts)
	}
	files, err := s.conversationFiles()
	if err != nil {
		return nil, 0, err
	}
	var skipped int
	defer func() { s.logSkipped("token metrics", skipped) }()

	eligible := make([]*fileSummary, 0, len(files))
	var first, last time.Time
	var pointCount int
	for _, f := range files {
		entry, err := decodeFileSummary(f)
		if err != nil {
			return nil, 0, err
		}
		skipped += entry.skipped
		entryFirst, entryLast, ok := entry.bounds(opts.Since)
		if !ok {
			continue
		}
		if first.IsZero() || entryFirst.Before(first) {
			first = entryFirst
		}
		if entryLast.After(last) {
			last = entryLast
		}
		pointCount += len(entry.points)
		eligible = append(eligible, entry)
	}

	interval := opts.Interval
	if interval <= 0 {
		interval = tokenUsageIntervalFor(last.Sub(first))
	}
	buckets := newTokenUsageBuckets(interval, pointCount)
	for _, entry := range eligible {
		for _, p := range entry.points {
			if !opts.Since.IsZero() && p.Timestamp.Before(opts.Since) {
				continue
			}
			buckets.add(p)
		}
	}
	return buckets.points(), interval, nil
}

type sqlTokenUsageRow struct {
	ReceivedAt  string    `gorm:"column:received_at"`
	StartedAt   time.Time `gorm:"column:started_at"`
	CompletedAt time.Time `gorm:"column:completed_at"`
	Model       string    `gorm:"column:model"`
	Provider    string    `gorm:"column:provider"`
	FreshInput  int64     `gorm:"column:fresh_input"`
	CacheRead   int64     `gorm:"column:cache_read"`
	CacheWrite  int64     `gorm:"column:cache_write"`
	Output      int64     `gorm:"column:output"`
	Reasoning   int64     `gorm:"column:reasoning"`
}

func (s *Storage) tokenUsagePointsSQL(opts TokenUsageOptions) ([]TokenUsagePoint, time.Duration, error) {
	query := s.sql.db.Table("generation").Select(
		"received_at, started_at, completed_at, model, provider, fresh_input, cache_read, cache_write, output, reasoning",
	)
	if !opts.Since.IsZero() {
		query = query.Where("activity >= ?", opts.Since)
	}
	var rows []sqlTokenUsageRow
	if err := query.Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	points := make([]TokenUsagePoint, 0, len(rows))
	var first, last time.Time
	for _, row := range rows {
		buckets := TokenBuckets{
			FreshInput: row.FreshInput,
			CacheRead:  row.CacheRead,
			CacheWrite: row.CacheWrite,
			Output:     row.Output,
			Reasoning:  row.Reasoning,
		}
		if buckets == (TokenBuckets{}) {
			continue
		}
		when := row.StartedAt
		if when.IsZero() {
			when = row.CompletedAt
		}
		if when.IsZero() {
			when, _ = time.Parse(time.RFC3339Nano, row.ReceivedAt)
		}
		if when.IsZero() || !nanosRepresentable(when) || (!opts.Since.IsZero() && when.Before(opts.Since)) {
			continue
		}
		point := TokenUsagePoint{Timestamp: when, Model: row.Model, Provider: row.Provider, TokenBuckets: buckets}
		points = append(points, point)
		if first.IsZero() || when.Before(first) {
			first = when
		}
		if when.After(last) {
			last = when
		}
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = tokenUsageIntervalFor(last.Sub(first))
	}
	buckets := newTokenUsageBuckets(interval, len(points))
	for _, point := range points {
		buckets.add(point)
	}
	return buckets.points(), interval, nil
}

// tokenUsageIntervalFor picks the smallest ladder step that keeps a range
// of the given span under maxTokenUsageBuckets buckets.
func tokenUsageIntervalFor(span time.Duration) time.Duration {
	for _, step := range tokenUsageIntervals {
		if span/step <= maxTokenUsageBuckets {
			return step
		}
	}
	return tokenUsageIntervals[len(tokenUsageIntervals)-1]
}

// tokenUsageBuckets sums points into (bucket start, model, provider)
// groups as they arrive, so a caller can fold a whole store's points in
// without holding them all.
type tokenUsageBuckets struct {
	interval   time.Duration
	indexByKey map[tokenBucketKey]int
	out        []TokenUsagePoint
}

type tokenBucketKey struct {
	start    int64
	model    string
	provider string
}

// newTokenUsageBuckets prepares the fold. points is how many will be added;
// it only sizes the first allocation, so an estimate is fine.
func newTokenUsageBuckets(interval time.Duration, points int) *tokenUsageBuckets {
	return &tokenUsageBuckets{
		interval:   interval,
		indexByKey: map[tokenBucketKey]int{},
		// A bucket holds one point per model and provider, so the output is
		// bounded by the bucket count however many generations came in.
		out: make([]TokenUsagePoint, 0, min(points, 4*maxTokenUsageBuckets)),
	}
}

func (b *tokenUsageBuckets) add(p TokenUsagePoint) {
	start := intervalStart(p.Timestamp, b.interval)
	key := tokenBucketKey{start: start.UnixNano(), model: p.Model, provider: p.Provider}
	if idx, ok := b.indexByKey[key]; ok {
		b.out[idx].TokenBuckets = b.out[idx].TokenBuckets.plus(p.TokenBuckets)
		return
	}
	b.indexByKey[key] = len(b.out)
	b.out = append(b.out, TokenUsagePoint{
		Timestamp:    start,
		Model:        p.Model,
		Provider:     p.Provider,
		TokenBuckets: p.TokenBuckets,
	})
}

// points returns the buckets ordered by timestamp then model then provider,
// so the chart reads them in one pass.
func (b *tokenUsageBuckets) points() []TokenUsagePoint {
	out := b.out
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Timestamp.Equal(out[j].Timestamp) {
			return out[i].Timestamp.Before(out[j].Timestamp)
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Provider < out[j].Provider
	})
	return out
}

// intervalStart floors t onto the interval grid measured from the Unix
// epoch, so a bucket boundary is the same instant for every client that
// divides time the same way. time.Time.Truncate cannot stand in: it
// measures from year 1, which shifts the 7-day step by three days against
// a client flooring from the epoch.
//
// t must be within the range UnixNano covers. tokenUsagePoint drops any
// generation whose timestamp falls outside that range before reaching here.
func intervalStart(t time.Time, interval time.Duration) time.Time {
	if interval <= 0 {
		return t.UTC()
	}
	nanos := t.UnixNano()
	step := int64(interval)
	floored := nanos / step
	if nanos < 0 && nanos%step != 0 {
		floored--
	}
	return time.Unix(0, floored*step).UTC()
}

// nanosRepresentable reports whether t is inside the range UnixNano
// covers, roughly the years 1678 to 2262. Outside it UnixNano wraps, and
// bucketing would move the point to a plausible-looking wrong date instead
// of an obviously wrong one. time.Parse accepts years up to 9999.
func nanosRepresentable(t time.Time) bool {
	return !t.Before(time.Unix(0, math.MinInt64)) && !t.After(time.Unix(0, math.MaxInt64))
}

// tokenUsagePoint builds a TokenUsagePoint from one record. ok is false
// when the generation recorded no tokens, has no usable timestamp, or
// carries a timestamp bucketing cannot place, so the caller can skip it
// rather than plot a zero-height bar at the epoch.
func tokenUsagePoint(rec summaryRecord) (TokenUsagePoint, bool) {
	gen := rec.Generation
	buckets := disjointTokenUsage(gen.Usage.toSDK(), gen.Model.Provider)
	if buckets == (TokenBuckets{}) {
		return TokenUsagePoint{}, false
	}
	when := generationTime(gen, rec.ReceivedAt)
	if when.IsZero() || !nanosRepresentable(when) {
		return TokenUsagePoint{}, false
	}
	return TokenUsagePoint{
		Timestamp:    when,
		Model:        gen.modelName(),
		Provider:     gen.Model.Provider,
		TokenBuckets: buckets,
	}, true
}

// recordActivity is the moment a stored record represents: the later of
// completed_at and started_at, or the receiver's arrival time when the
// record carries neither. The conversation summary and every
// modification-time stamp read it, so the list order and the Since bound
// cannot disagree.
//
// It takes the later of the pair rather than completed_at alone because an
// exporter can report a completed_at that precedes started_at, and because
// generationTime places a token-chart point at started_at. Activity is
// therefore never below the point in the same record, so the chart can skip
// a file by its modification time.
func recordActivity(gen summaryGeneration, receivedAt string) time.Time {
	when := gen.CompletedAt
	if gen.StartedAt.After(when) {
		when = gen.StartedAt
	}
	if !when.IsZero() {
		return when
	}
	when, _ = time.Parse(time.RFC3339Nano, receivedAt)
	return when
}

// generationTime is the wall-clock moment a generation ran, preferring
// started_at, then completed_at, then the receiver's arrival time.
func generationTime(gen summaryGeneration, receivedAt string) time.Time {
	if !gen.StartedAt.IsZero() {
		return gen.StartedAt
	}
	if !gen.CompletedAt.IsZero() {
		return gen.CompletedAt
	}
	when, _ := time.Parse(time.RFC3339Nano, receivedAt)
	return when
}

// disjointTokenUsage splits a generation's usage into five buckets that
// don't overlap, so the viewer can stack them without double-counting.
//
// Usage that carries TokenInputSemanticsInclusive needs no provider name
// on the input axis. The marker says input_tokens already covers every
// input token type, so both cache buckets sit inside input_tokens and
// fresh input is input - cache_read - cache_write for every provider.
// Usage without the marker (legacy records, and exporters that pass
// provider-raw counts through) falls back to the provider-name heuristic
// below.
//
// Providers disagree on how cache and reasoning tokens relate to the
// input/output totals, so both carve-outs are provider-aware:
//
//   - cache_read: Anthropic reports input_tokens as the non-cached input,
//     so cache_read/cache_write are extra on top. OpenAI, Gemini, and
//     codex fold cached tokens into input_tokens, so cache_read is a
//     subset that must be carved back out (see cacheReadInsideInput).
//   - reasoning: OpenAI and codex nest reasoning inside output
//     (completion) tokens, so it's carved out. Gemini reports thoughts as
//     additive (output is just the candidate tokens) and Anthropic
//     doesn't populate it, so for those reasoning stands alone (see
//     reasoningInsideOutput).
//
// No provider the heuristic maps folds cache_write into input, so without
// the inclusive marker cache_write always stands alone.
//
// For well-formed usage the buckets sum back to what the provider
// reported: inclusive-marked usage input + output; provider-raw Anthropic
// input + cache_read + cache_write + output; OpenAI input + output;
// Gemini input + output + reasoning (its total also
// counts tool-use prompt tokens, which the SDK's TokenUsage has no field
// for). When a subset field exceeds its total, the nonNeg clamps keep
// the subset and zero the remainder, so the sum can exceed what was
// reported.
func disjointTokenUsage(u agento11y.TokenUsage, provider string) TokenBuckets {
	b := TokenBuckets{
		FreshInput: nonNeg(u.InputTokens),
		CacheRead:  nonNeg(u.CacheReadInputTokens),
		CacheWrite: nonNeg(u.CacheWriteInputTokens),
		Output:     nonNeg(u.OutputTokens),
		Reasoning:  nonNeg(u.ReasoningTokens),
	}
	switch {
	case u.InputSemantics == agento11y.TokenInputSemanticsInclusive:
		// Both cache buckets are inside input under the OTel contract, so
		// both come back out; the buckets then sum to the reported total.
		b.FreshInput = nonNeg(b.FreshInput - b.CacheRead - b.CacheWrite)
	case cacheReadInsideInput(provider):
		b.FreshInput = nonNeg(b.FreshInput - b.CacheRead)
	}
	if reasoningInsideOutput(provider) {
		b.Output = nonNeg(b.Output - b.Reasoning)
	}
	return b
}

// cacheReadInsideInput reports whether the provider counts cache_read
// tokens within input_tokens (subset semantics). This check is the
// fallback for usage that carries no input-semantics marker; usage that
// carries one never reaches it (see disjointTokenUsage). Anthropic keeps them
// separate; OpenAI and Gemini fold them in, and so does codex, the codex
// agent's fallback provider for model names it can't attribute (its
// usage comes from the Responses API). Unknown providers default to
// "separate" so we never subtract tokens we can't account for and end up
// hiding real input.
//
// pi names its own providers ("openai-codex", "google-antigravity",
// "kimi-coding", "grafana", …) and normalizes their usage in its client
// before either the plugin or the importer sees it: cache_read is always
// disjoint from input, and pi's own total is input + output + cache_read +
// cache_write. Measured over 66,119 assistant turns on the development
// machine, that total holds for every turn and cache_read exceeds input on
// 63,701 of them, which subset semantics cannot produce. So none of pi's
// provider strings belong here, however much they look like the OpenAI or
// Gemini names beside them.
func cacheReadInsideInput(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai", "azure", "azure-openai", "azureopenai", "codex",
		"gemini", "google", "googleai", "google-genai", "vertex", "vertexai", "google-vertex":
		return true
	default:
		return false
	}
}

// reasoningInsideOutput reports whether the provider counts reasoning
// tokens within output_tokens (subset semantics). OpenAI and codex nest
// reasoning inside completion tokens; Gemini reports thoughts as a
// separate additive count and Anthropic doesn't populate reasoning, so
// both keep it standalone. Unknown providers default to "separate" so we
// never subtract reasoning we can't account for and end up hiding real
// output. pi's provider strings are absent for the reason given on
// cacheReadInsideInput.
func reasoningInsideOutput(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai", "azure", "azure-openai", "azureopenai", "codex":
		return true
	default:
		return false
	}
}

func nonNeg(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

// totalTokensForView returns an explicit provider total when present; when
// older records omit it, it falls back to the provider-aware disjoint buckets
// so additive cache fields (notably Anthropic) are not silently dropped.
func totalTokensForView(u agento11y.TokenUsage, provider string) int64 {
	if u.TotalTokens != 0 {
		return u.TotalTokens
	}
	b := disjointTokenUsage(u, provider)
	return b.FreshInput + b.CacheRead + b.CacheWrite + b.Output + b.Reasoning
}

// threadMessages renders one generation's messages in display order: its own
// input — the user prompt, or the tool results the harness fed into THIS
// request — followed by its own output (the assistant's text and tool calls).
// Results are shown on the step that received them (the request after the call
// that produced them), never folded back into the issuing step, so each step
// reflects only the data sent to and produced by that single model call.
func threadMessages(input, output []agento11y.Message) []agento11y.Message {
	if len(input) == 0 && len(output) == 0 {
		return nil
	}
	messages := make([]agento11y.Message, 0, len(input)+len(output))
	messages = append(messages, input...)
	messages = append(messages, output...)
	return messages
}

// scanJSONL walks one per-conversation JSONL file, calling decode on every
// non-empty line and visit on each value decode accepts. A missing file is
// not an error. It returns how many lines decode rejected (a tail truncated
// mid-append is the usual reason), so the caller can report a file the
// viewer is only partly reading.
func scanJSONL[T any](path string, decode func([]byte) (T, bool), visit func(T)) (skipped int, err error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	// JSONL lines can hold full transcripts; bump the buffer well above
	// the default 64 KiB.
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		value, ok := decode(line)
		if !ok {
			skipped++
			continue
		}
		visit(value)
	}
	return skipped, sc.Err()
}

// scanLatestJSONL is scanJSONL keeping only the last line per key, so a
// re-exported generation replaces the earlier copy of itself. Values whose
// key is empty are all kept, in file order.
func scanLatestJSONL[T any](path string, decode func([]byte) (T, bool), keyOf func(T) string, visit func(T)) (int, error) {
	indexByKey := map[string]int{}
	var values []T
	skipped, err := scanJSONL(path, decode, func(value T) {
		key := keyOf(value)
		if key == "" {
			values = append(values, value)
			return
		}
		if idx, ok := indexByKey[key]; ok {
			values[idx] = value
			return
		}
		indexByKey[key] = len(values)
		values = append(values, value)
	})
	if err != nil {
		return skipped, err
	}
	for _, value := range values {
		visit(value)
	}
	return skipped, nil
}

type scannedGenerationRecord struct {
	record generationRecord
	gen    storedGeneration
}

// decodeGenerationRecord decodes one JSONL line into the full stored
// generation, including the input and output message trees. Only the
// detail view and search need those.
//
// A line whose generation the full struct rejects falls back to the summary
// projection, so the detail view and the list accept the same lines: a
// record with an input tree this build cannot read still shows its header,
// tokens and timings rather than disappearing from a conversation the list
// counted.
func decodeGenerationRecord(line []byte) (scannedGenerationRecord, bool) {
	var rec generationRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return scannedGenerationRecord{}, false
	}
	var gen storedGeneration
	if err := json.Unmarshal(rec.Generation, &gen); err != nil {
		var summary summaryGeneration
		if err := json.Unmarshal(rec.Generation, &summary); err != nil {
			return scannedGenerationRecord{}, false
		}
		gen = storedGeneration{summaryGeneration: summary, ConversationID: rec.ConversationID}
	}
	return scannedGenerationRecord{record: rec, gen: gen}, true
}

// scanLatestGenerationRecords walks one per-conversation JSONL file calling
// visit for the newest copy of every decodable record. The detail view and
// search need the full stored generation, message trees included.
func scanLatestGenerationRecords(path string, visit func(generationRecord, storedGeneration)) (int, error) {
	return scanLatestJSONL(path, decodeGenerationRecord, func(r scannedGenerationRecord) string {
		return r.generationID()
	}, func(r scannedGenerationRecord) {
		visit(r.record, r.gen)
	})
}

// scanLatestGenerationRecordsUpTo visits the latest generation records in
// the first size bytes of path without retaining their transcript bodies.
// The first pass records only the last position of each generation id; the
// second pass decodes and visits those positions.
func scanLatestGenerationRecordsUpTo(
	ctx context.Context,
	path string,
	size int64,
	visit func(generationRecord, storedGeneration) error,
) (int, error) {
	if info, err := os.Stat(path); err == nil && !info.Mode().IsRegular() {
		var visitErr error
		skipped, scanErr := scanLatestGenerationRecords(path, func(rec generationRecord, gen storedGeneration) {
			if visitErr == nil {
				visitErr = visit(rec, gen)
			}
		})
		if scanErr != nil {
			return skipped, scanErr
		}
		return skipped, visitErr
	}

	lastPosition := map[string]int{}
	position := 0
	skipped, err := scanJSONLUpTo(ctx, path, size, func(line []byte) error {
		decoded, ok := decodeGenerationRecord(line)
		if !ok {
			return errSkippedJSONLLine
		}
		if key := decoded.generationID(); key != "" {
			lastPosition[key] = position
		}
		position++
		return nil
	})
	if err != nil {
		return skipped, err
	}
	position = 0
	_, err = scanJSONLUpTo(ctx, path, size, func(line []byte) error {
		decoded, ok := decodeGenerationRecord(line)
		if !ok {
			return nil
		}
		key := decoded.generationID()
		current := position
		position++
		if key != "" && lastPosition[key] != current {
			return nil
		}
		return visit(decoded.record, decoded.gen)
	})
	return skipped, err
}

var errSkippedJSONLLine = errors.New("skipped JSONL line")

func scanJSONLUpTo(ctx context.Context, path string, size int64, visit func([]byte) error) (skipped int, err error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, size))
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return skipped, err
		}
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if err := visit(line); errors.Is(err, errSkippedJSONLLine) {
			skipped++
		} else if err != nil {
			return skipped, err
		}
	}
	return skipped, sc.Err()
}

func (r scannedGenerationRecord) generationID() string {
	if id := strings.TrimSpace(r.record.GenerationID); id != "" {
		return id
	}
	return strings.TrimSpace(r.gen.ID)
}

// scanLatestSummaryRecords walks one per-conversation JSONL file decoding
// only the summary projection, no input or output message trees, and
// keeping the last record per generation id.
func scanLatestSummaryRecords(path string, visit func(summaryRecord)) (int, error) {
	return scanLatestJSONL(path, decodeSummaryRecord, func(r summaryRecord) string {
		return r.generationID()
	}, visit)
}

func decodeSummaryRecord(line []byte) (summaryRecord, bool) {
	var rec summaryRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return summaryRecord{}, false
	}
	return rec, true
}

// extractTools walks the assistant's output messages and collects the
// distinct tool names in call order. tool_preview is a short, legible
// snippet of the first call's input: we unwrap common single-field
// shapes (`command`, `query`, `file_path`) and otherwise fall back to
// the raw JSON, truncated.
func extractTools(msgs []agento11y.Message) (names []string, preview string) {
	seen := map[string]struct{}{}
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Kind != agento11y.PartKindToolCall || p.ToolCall == nil {
				continue
			}
			if _, ok := seen[p.ToolCall.Name]; !ok {
				seen[p.ToolCall.Name] = struct{}{}
				names = append(names, p.ToolCall.Name)
			}
			if preview == "" {
				preview = renderToolPreview(p.ToolCall.InputJSON)
			}
		}
	}
	return names, preview
}

const toolPreviewMaxLen = 240

func renderToolPreview(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(input, &m); err == nil {
		for _, key := range []string{"command", "cmd", "query", "prompt", "path", "file_path"} {
			if s, ok := m[key].(string); ok && s != "" {
				return truncate(s, toolPreviewMaxLen)
			}
		}
	}
	return truncate(string(input), toolPreviewMaxLen)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.ValidString(s[:max]) {
		max--
	}
	return s[:max] + "…"
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
