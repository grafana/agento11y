package local

import (
	"math"
	"slices"
	"sort"
	"strings"
	"time"
)

type ToolAnalyticsOptions struct {
	Since     time.Time
	Before    time.Time
	Workspace *string
	Interval  time.Duration
}

// SkillsToolsMetricsResponse is deliberately ready for a future sibling
// skills section. This release only serializes tools.
type SkillsToolsMetricsResponse struct {
	Tools ToolAnalytics `json:"tools"`
}

type ToolAnalytics struct {
	Totals          ToolAnalyticsTotals   `json:"totals"`
	Rows            []ToolAnalyticsRow    `json:"rows"`
	Buckets         []ToolAnalyticsBucket `json:"buckets"`
	Workspaces      []ToolWorkspaceFacet  `json:"workspaces"`
	IntervalSeconds int64                 `json:"interval_seconds"`
	Coverage        ToolAnalyticsCoverage `json:"coverage"`
}

type ToolAnalyticsTotals struct {
	Calls           int `json:"calls"`
	Failures        int `json:"failures"`
	Tools           int `json:"tools"`
	Sessions        int `json:"sessions"`
	DurationSamples int `json:"duration_samples"`
}

type ToolAnalyticsCoverage struct {
	GenerationCalls int `json:"generation_calls"`
	ProjectedSpans  int `json:"projected_spans"`
	MatchedCalls    int `json:"matched_calls"`
}

type ToolAnalyticsRow struct {
	Name               string   `json:"name"`
	Calls              int      `json:"calls"`
	Failures           int      `json:"failures"`
	Sessions           int      `json:"sessions"`
	DurationSamples    int      `json:"duration_samples"`
	P50DurationSeconds *float64 `json:"p50_duration_seconds,omitempty"`
	P95DurationSeconds *float64 `json:"p95_duration_seconds,omitempty"`
}

type ToolAnalyticsBucket struct {
	Timestamp time.Time `json:"t"`
	Name      string    `json:"name"`
	Calls     int       `json:"calls"`
	Failures  int       `json:"failures"`
}

type ToolWorkspaceFacet struct {
	Path     string `json:"path"`
	Calls    int    `json:"calls"`
	Sessions int    `json:"sessions"`
}

type toolObservation struct {
	ConversationID string
	Workspace      string
	HasSession     bool
	HasGeneration  bool
	HasSpan        bool
	CallID         string
	Name           string
	Timestamp      time.Time
	Failed         bool
	Duration       *time.Duration
}

type conversationToolSource struct {
	id        string
	workspace string
	summary   *fileSummary
	spans     []toolSpanRecord
}

// ToolAnalytics reconciles every source observation before applying the
// selected range and workspace. No result limit participates in this query.
func (s *Storage) ToolAnalytics(opts ToolAnalyticsOptions) (ToolAnalytics, error) {
	observations, err := s.toolObservations()
	if err != nil {
		return ToolAnalytics{}, err
	}

	facets := aggregateToolWorkspaceFacets(observations, opts.Since, opts.Before)
	selected := make([]toolObservation, 0, len(observations))
	var first, last time.Time
	for _, observation := range observations {
		if !nanosRepresentable(observation.Timestamp) ||
			!inPeriod(observation.Timestamp, opts.Since, opts.Before) ||
			!workspaceMatches(observation.Workspace, opts.Workspace) || observation.Name == "" {
			continue
		}
		selected = append(selected, observation)
		if first.IsZero() || observation.Timestamp.Before(first) {
			first = observation.Timestamp
		}
		if observation.Timestamp.After(last) {
			last = observation.Timestamp
		}
	}

	interval := opts.Interval
	if interval <= 0 {
		interval = tokenUsageIntervalFor(last.Sub(first))
	}
	analytics := aggregateToolAnalytics(selected, interval)
	analytics.Workspaces = facets
	analytics.IntervalSeconds = int64(interval.Seconds())
	return analytics, nil
}

// toolObservations returns one observation per reconciled generation call or
// span. A readable generation summary supplies session/workspace identity; a
// sidecar without one still contributes to analytics but cannot create a
// Sessions row.
func (s *Storage) toolObservations() ([]toolObservation, error) {
	conversationFiles, err := s.conversationFiles()
	if err != nil {
		return nil, err
	}
	spanFiles, err := s.toolSpanFiles()
	if err != nil {
		return nil, err
	}

	sources := make(map[string]*conversationToolSource, len(conversationFiles)+len(spanFiles))
	order := make([]string, 0, len(conversationFiles)+len(spanFiles))
	claim := func(id string) *conversationToolSource {
		if source := sources[id]; source != nil {
			return source
		}
		source := &conversationToolSource{id: id}
		sources[id] = source
		order = append(order, id)
		return source
	}

	var skippedGenerations, skippedSpans int
	defer func() {
		s.logSkipped("skills-tools generation metrics", skippedGenerations)
		s.logSkipped("skills-tools span metrics", skippedSpans)
	}()
	for _, file := range conversationFiles {
		entry, err := s.summaries.get(file)
		if err != nil {
			return nil, err
		}
		skippedGenerations += entry.skipped
		if !entry.ok {
			continue
		}
		source := claim(file.id)
		source.summary = entry
		source.workspace = entry.summary.Workspace
	}
	// Dedupe the combined persisted stream before assigning spans to
	// conversations. DeliveryOrder makes the final delivery win even when its
	// conversation attribute changed; file order is the legacy tie-break.
	persistedSpans := make([]toolSpanRecord, 0)
	for _, file := range spanFiles {
		entry, err := s.toolSpans.get(file)
		if err != nil {
			return nil, err
		}
		skippedSpans += entry.skipped
		for _, span := range entry.spans {
			if span.ConversationID == file.id {
				persistedSpans = append(persistedSpans, span)
			}
		}
	}
	for _, span := range dedupeToolSpans(persistedSpans) {
		source := claim(span.ConversationID)
		source.spans = append(source.spans, span)
	}
	s.summaries.prune(conversationFiles)

	out := make([]toolObservation, 0)
	for _, id := range order {
		out = append(out, reconcileToolSource(sources[id])...)
	}
	return out, nil
}

// reconcileToolSource pairs duplicate call IDs FIFO. Only nonblank IDs can
// match. Every unpaired generation occurrence and every distinct unpaired
// span remains an observation.
func reconcileToolSource(source *conversationToolSource) []toolObservation {
	spanQueues := map[string][]int{}
	matchedSpans := make([]bool, len(source.spans))
	for index, span := range source.spans {
		if id := strings.TrimSpace(span.ToolCallID); id != "" {
			spanQueues[id] = append(spanQueues[id], index)
		}
	}

	occurrences := []toolOccurrence(nil)
	if source.summary != nil {
		occurrences = source.summary.toolOccurrences
	}
	out := make([]toolObservation, 0, len(occurrences)+len(source.spans))
	for _, occurrence := range occurrences {
		observation := toolObservation{
			ConversationID: source.id,
			Workspace:      source.workspace,
			HasSession:     true,
			HasGeneration:  true,
			CallID:         strings.TrimSpace(occurrence.CallID),
			Name:           strings.TrimSpace(occurrence.Name),
			Timestamp:      occurrence.Timestamp,
			Failed:         occurrence.Failed,
		}
		if queue := spanQueues[observation.CallID]; observation.CallID != "" && len(queue) > 0 {
			index := queue[0]
			spanQueues[observation.CallID] = queue[1:]
			matchedSpans[index] = true
			mergeToolSpan(&observation, source.spans[index])
		}
		out = append(out, observation)
	}
	for index, span := range source.spans {
		if matchedSpans[index] {
			continue
		}
		observation := toolObservation{
			ConversationID: source.id,
			Workspace:      source.workspace,
			HasSession:     source.summary != nil,
			CallID:         strings.TrimSpace(span.ToolCallID),
		}
		mergeToolSpan(&observation, span)
		out = append(out, observation)
	}
	return out
}

func mergeToolSpan(observation *toolObservation, span toolSpanRecord) {
	observation.HasSpan = true
	if name := strings.TrimSpace(span.ToolName); observation.Name == "" {
		observation.Name = name
	}
	if !span.StartedAt.IsZero() {
		observation.Timestamp = span.StartedAt
	}
	observation.Failed = observation.Failed || span.Failed
	if !span.StartedAt.IsZero() && !span.CompletedAt.IsZero() && !span.CompletedAt.Before(span.StartedAt) {
		duration := span.CompletedAt.Sub(span.StartedAt)
		observation.Duration = &duration
	}
}

func aggregateToolAnalytics(observations []toolObservation, interval time.Duration) ToolAnalytics {
	type rowAccumulator struct {
		row       ToolAnalyticsRow
		durations []time.Duration
		sessions  map[string]struct{}
	}
	type bucketKey struct {
		start int64
		name  string
	}
	rows := map[string]*rowAccumulator{}
	buckets := map[bucketKey]*ToolAnalyticsBucket{}
	sessions := map[string]struct{}{}
	var coverage ToolAnalyticsCoverage
	totalCalls := 0
	for _, observation := range observations {
		if !nanosRepresentable(observation.Timestamp) || observation.Timestamp.IsZero() {
			continue
		}
		totalCalls++
		if observation.HasGeneration {
			coverage.GenerationCalls++
		}
		if observation.HasSpan {
			coverage.ProjectedSpans++
		}
		if observation.HasGeneration && observation.HasSpan {
			coverage.MatchedCalls++
		}
		row := rows[observation.Name]
		if row == nil {
			row = &rowAccumulator{row: ToolAnalyticsRow{Name: observation.Name}, sessions: map[string]struct{}{}}
			rows[observation.Name] = row
		}
		row.row.Calls++
		if observation.Failed {
			row.row.Failures++
		}
		if observation.Duration != nil {
			row.durations = append(row.durations, *observation.Duration)
		}
		if observation.HasSession {
			row.sessions[observation.ConversationID] = struct{}{}
			sessions[observation.ConversationID] = struct{}{}
		}

		start := intervalStart(observation.Timestamp, interval)
		key := bucketKey{start: start.UnixNano(), name: observation.Name}
		bucket := buckets[key]
		if bucket == nil {
			bucket = &ToolAnalyticsBucket{Timestamp: start, Name: observation.Name}
			buckets[key] = bucket
		}
		bucket.Calls++
		if observation.Failed {
			bucket.Failures++
		}
	}

	analytics := ToolAnalytics{
		Rows:       make([]ToolAnalyticsRow, 0, len(rows)),
		Buckets:    make([]ToolAnalyticsBucket, 0, len(buckets)),
		Workspaces: []ToolWorkspaceFacet{},
	}
	analytics.Coverage = coverage
	analytics.Totals.Calls = totalCalls
	analytics.Totals.Tools = len(rows)
	analytics.Totals.Sessions = len(sessions)
	for _, accumulator := range rows {
		accumulator.row.Sessions = len(accumulator.sessions)
		accumulator.row.DurationSamples = len(accumulator.durations)
		analytics.Totals.Failures += accumulator.row.Failures
		analytics.Totals.DurationSamples += accumulator.row.DurationSamples
		if len(accumulator.durations) > 0 {
			slices.Sort(accumulator.durations)
			p50 := nearestRankDuration(accumulator.durations, 0.50).Seconds()
			p95 := nearestRankDuration(accumulator.durations, 0.95).Seconds()
			accumulator.row.P50DurationSeconds = &p50
			accumulator.row.P95DurationSeconds = &p95
		}
		analytics.Rows = append(analytics.Rows, accumulator.row)
	}
	sort.Slice(analytics.Rows, func(i, j int) bool {
		if analytics.Rows[i].Calls != analytics.Rows[j].Calls {
			return analytics.Rows[i].Calls > analytics.Rows[j].Calls
		}
		return analytics.Rows[i].Name < analytics.Rows[j].Name
	})
	for _, bucket := range buckets {
		analytics.Buckets = append(analytics.Buckets, *bucket)
	}
	sort.Slice(analytics.Buckets, func(i, j int) bool {
		if !analytics.Buckets[i].Timestamp.Equal(analytics.Buckets[j].Timestamp) {
			return analytics.Buckets[i].Timestamp.Before(analytics.Buckets[j].Timestamp)
		}
		return analytics.Buckets[i].Name < analytics.Buckets[j].Name
	})
	return analytics
}

func nearestRankDuration(sorted []time.Duration, percentile float64) time.Duration {
	rank := max(1, int(math.Ceil(percentile*float64(len(sorted)))))
	return sorted[rank-1]
}

func aggregateToolWorkspaceFacets(observations []toolObservation, since, before time.Time) []ToolWorkspaceFacet {
	type accumulator struct {
		facet    ToolWorkspaceFacet
		sessions map[string]struct{}
	}
	byPath := map[string]*accumulator{}
	for _, observation := range observations {
		if !nanosRepresentable(observation.Timestamp) ||
			!inPeriod(observation.Timestamp, since, before) || observation.Name == "" {
			continue
		}
		entry := byPath[observation.Workspace]
		if entry == nil {
			entry = &accumulator{facet: ToolWorkspaceFacet{Path: observation.Workspace}, sessions: map[string]struct{}{}}
			byPath[observation.Workspace] = entry
		}
		entry.facet.Calls++
		if observation.HasSession {
			entry.sessions[observation.ConversationID] = struct{}{}
		}
	}
	out := make([]ToolWorkspaceFacet, 0, len(byPath))
	for _, entry := range byPath {
		entry.facet.Sessions = len(entry.sessions)
		out = append(out, entry.facet)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sessions != out[j].Sessions {
			return out[i].Sessions > out[j].Sessions
		}
		if out[i].Calls != out[j].Calls {
			return out[i].Calls > out[j].Calls
		}
		return out[i].Path < out[j].Path
	})
	return out
}
