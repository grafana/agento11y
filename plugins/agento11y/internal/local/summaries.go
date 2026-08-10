package local

import (
	"context"
	"strings"
	"sync"
	"time"
)

// fileSummary holds one conversation file's two decoded projections, the
// list's conversation summary and the token chart's usage points, against
// the size and modification time they were decoded at.
//
// Conversation files are append-only: appendJSONL opens with O_APPEND and
// never O_TRUNC, and nothing in the package rewrites or compacts them. So a
// file whose size and modification time both still match what an entry
// recorded holds the bytes that entry was decoded from, and can be answered
// without reopening the file.
type fileSummary struct {
	size    int64
	modTime time.Time

	// summary is the list projection. ok is false when the file held no
	// decodable record, and the list skips such a file.
	summary ConversationSummary
	ok      bool

	// points is the token-chart projection: one point per generation that
	// recorded usage, in file order. first and last are the oldest and the
	// newest point timestamp, so a reader can place the whole file against
	// a requested range without walking the points.
	points []TokenUsagePoint
	first  time.Time
	last   time.Time

	// skipped counts the lines no projection could decode. A reader adds it
	// on every hit, so the count it reports describes the store rather than
	// the state of the cache.
	skipped int
}

// bounds returns the oldest and newest point timestamp at or after since,
// and whether the entry holds any such point. A file whose oldest point is
// already at or after the bound answers from first and last without walking
// the points, which is every file except the one the bound cuts through.
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
	for _, p := range e.points {
		if p.Timestamp.Before(since) {
			continue
		}
		if !ok || p.Timestamp.Before(first) {
			first = p.Timestamp
		}
		if p.Timestamp.After(last) {
			last = p.Timestamp
		}
		ok = true
	}
	return first, last, ok
}

// summaryCache holds one fileSummary per conversation file path. An entry
// is immutable once stored and is replaced whole, so a caller holding one
// keeps reading a consistent snapshot while another goroutine refreshes the
// same path. The mutex guards the maps alone: decoding runs outside it, so
// a large file cannot block a request that needs a different one.
type summaryCache struct {
	// decode reads one file into an entry. Tests replace it to observe
	// which files a read decoded; a nil value uses decodeFileSummary.
	decode func(conversationFile) (*fileSummary, error)

	mu      sync.Mutex
	entries map[string]*fileSummary
	// inflight holds the decode running for a path, so a second caller
	// waits for it rather than repeating it.
	inflight map[string]*summaryDecode
}

// summaryDecode is one decode in progress. entry and err are written
// before done is closed and read only after it, so a waiter needs no lock
// to read them.
type summaryDecode struct {
	done  chan struct{}
	entry *fileSummary
	err   error
}

// matches reports whether the entry was decoded from the bytes f names.
func (e *fileSummary) matches(f conversationFile) bool {
	return e != nil && e.size == f.size && e.modTime.Equal(f.modTime)
}

// get returns the entry for f, decoding the file when no entry exists or
// either validator differs.
//
// One decode per path runs at a time. The background warm walks the store
// in the same newest-first order a request does, so without this a request
// landing partway through a warm would catch up to the file the warm is on
// and decode every remaining file a second time.
func (c *summaryCache) get(f conversationFile) (*fileSummary, error) {
	for {
		entry, flight, owned := c.claim(f)
		if entry != nil {
			return entry, nil
		}
		if !owned {
			<-flight.done
			if flight.err != nil {
				return nil, flight.err
			}
			if flight.entry.matches(f) {
				return flight.entry, nil
			}
			// The decode read a different version of the file than this
			// caller stated. Take the next turn rather than answer from it.
			continue
		}
		return c.fill(f, flight)
	}
}

// claim resolves one attempt at f under the lock. It returns a valid
// entry, or the decode already running for the path, or a decode this
// caller owns and must run.
func (c *summaryCache) claim(f conversationFile) (entry *fileSummary, flight *summaryDecode, owned bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if held := c.entries[f.path]; held.matches(f) {
		return held, nil, false
	}
	if running, ok := c.inflight[f.path]; ok {
		return nil, running, false
	}
	flight = &summaryDecode{done: make(chan struct{})}
	if c.inflight == nil {
		c.inflight = map[string]*summaryDecode{}
	}
	c.inflight[f.path] = flight
	return nil, flight, true
}

// fill runs the decode this caller claimed, stores it, and releases the
// callers waiting on it. A failed decode is not cached, so the next read
// retries the file and reports the failure itself.
func (c *summaryCache) fill(f conversationFile, flight *summaryDecode) (*fileSummary, error) {
	decode := c.decode
	if decode == nil {
		decode = decodeFileSummary
	}
	// The entry records the size and modification time observed before the
	// decode, so a file appended to while it is being read is decoded again
	// on the next request instead of staying stale.
	entry, err := decode(f)

	c.mu.Lock()
	if err == nil {
		if c.entries == nil {
			c.entries = map[string]*fileSummary{}
		}
		c.entries[f.path] = entry
	}
	delete(c.inflight, f.path)
	c.mu.Unlock()

	flight.entry, flight.err = entry, err
	close(flight.done)
	if err != nil {
		return nil, err
	}
	return entry, nil
}

// invalidate drops the entry for path. The stat validation would catch the
// change on its own; dropping the entry on write removes the dependence on
// how finely the filesystem records modification times.
func (c *summaryCache) invalidate(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, path)
}

// prune drops entries whose file is gone, using the paths the walk already
// found. Building the live set costs a map the size of the store, so it is
// skipped while the cache holds no more paths than the walk did.
//
// That test is a heuristic, not an invariant. Both readers can end their
// walk early, so an entry for a deleted file can survive until a walk sees
// more files than the cache holds. No read serves it, because a deleted
// path never comes back from a walk. It costs memory until then.
func (c *summaryCache) prune(files []conversationFile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) <= len(files) {
		return
	}
	live := make(map[string]struct{}, len(files))
	for _, f := range files {
		live[f.path] = struct{}{}
	}
	for path := range c.entries {
		if _, ok := live[path]; !ok {
			delete(c.entries, path)
		}
	}
}

// decodeFileSummary reads one per-conversation JSONL file and fills both
// projections in a single pass, so a list request warms what the token
// chart reads and the other way round. It decodes the summary projection
// only, so a conversation holding megabytes of messages costs the line scan
// and nothing more.
func decodeFileSummary(f conversationFile) (*fileSummary, error) {
	entry := &fileSummary{size: f.size, modTime: f.modTime}
	agents := map[string]struct{}{}
	models := map[string]struct{}{}
	var sum ConversationSummary
	var hasError, seen bool

	skipped, err := scanLatestSummaryRecords(f.path, func(rec summaryRecord) {
		r, gen := rec, rec.Generation
		seen = true
		if sum.ID == "" {
			sum.ID = r.ConversationID
		}
		sum.Calls++
		usage := gen.Usage.toSDK()
		sum.InputTokens += usage.InputTokens
		sum.OutputTokens += usage.OutputTokens
		sum.TotalTokens += totalTokensForView(usage, gen.Model.Provider)
		sum.TokenBuckets = sum.TokenBuckets.plus(disjointTokenUsage(usage, gen.Model.Provider))

		if !gen.StartedAt.IsZero() && (sum.StartedAt.IsZero() || gen.StartedAt.Before(sum.StartedAt)) {
			sum.StartedAt = gen.StartedAt
		}
		// last_activity tracks the latest known timestamp on any
		// generation, falling back to received_at when started/completed
		// aren't populated so freshly-arrived records still bubble up.
		when := gen.CompletedAt
		if when.IsZero() {
			when = gen.StartedAt
		}
		if when.IsZero() {
			when, _ = time.Parse(time.RFC3339Nano, r.ReceivedAt)
		}
		if when.After(sum.LastActivity) {
			sum.LastActivity = when
		}

		if gen.AgentName != "" {
			agents[gen.AgentName] = struct{}{}
		}
		if name := gen.modelName(); name != "" {
			models[name] = struct{}{}
		}
		if sum.Title == "" && gen.title() != "" {
			sum.Title = gen.title()
		}
		if gen.CallError != "" {
			hasError = true
		}
		// cwd/branch are per-session and identical across a conversation's
		// generations; take the first non-empty. A generation whose
		// agent_name carries a subagent suffix ("claude-code/general-purpose")
		// is one step of a spawned subagent.
		if sum.Workspace == "" {
			sum.Workspace = gen.Tags["cwd"]
		}
		if sum.Branch == "" {
			sum.Branch = gen.Tags["git.branch"]
		}
		if strings.Contains(gen.AgentName, "/") {
			sum.Subagents++
		}

		if p, ok := tokenUsagePoint(rec); ok {
			entry.points = append(entry.points, p)
			if entry.first.IsZero() || p.Timestamp.Before(entry.first) {
				entry.first = p.Timestamp
			}
			if p.Timestamp.After(entry.last) {
				entry.last = p.Timestamp
			}
		}
	})
	entry.skipped = skipped
	if err != nil {
		return nil, err
	}
	if !seen {
		return entry, nil // empty or all-invalid file
	}
	sum.Agents = sortedKeys(agents)
	sum.Models = sortedKeys(models)
	sum.Status = "ok"
	if hasError {
		sum.Status = "err"
	}
	entry.summary = sum
	entry.ok = true
	return entry, nil
}

// warmSummaries decodes every conversation file into the cache, newest
// first, so the page the viewer opens on is ready before the older store
// is. It runs in the background and never on a request path. A request that
// reaches a file the warm is decoding waits for that one decode, see get.
//
// Warming is best effort. A file that fails to decode is left uncached for
// a later request to retry and report.
func (s *Storage) warmSummaries(ctx context.Context) {
	files, err := s.conversationFiles()
	if err != nil {
		return
	}
	for _, f := range files {
		if ctx.Err() != nil {
			return
		}
		_, _ = s.summaries.get(f)
	}
}
