package local

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
)

// snippetMaxLen caps the snippet length the search endpoint returns. The
// viewer clamps to two lines visually, but bounding the bytes keeps the
// response small even when a generation contains a giant tool result.
const snippetMaxLen = 320

// snippetWindow is how much text on either side of a match the snippet
// preserves before truncation. Picked so a typical match still has enough
// context for "rate limit" to make sense without flooding the row.
const snippetWindow = 80

// Cap scan concurrency at 16 to bound disk pressure.
func searchWorkers() int {
	return min(max(runtime.GOMAXPROCS(0), 1)*2, 16)
}

// SearchHit is one ranked result for conversation search. The fields mirror
// what ConversationSummary exposes so the viewer can reuse the existing
// row idiom (agent/model pills, last_activity, total_tokens, status) and
// adds search-specific extras: a contextual snippet, the per-conversation
// match count, and the first matching generation id so a future detail
// view can deep-link into the matched turn.
type SearchHit struct {
	ID           string       `json:"id"`
	Title        string       `json:"title,omitempty"`
	Agents       []string     `json:"agents"`
	Models       []string     `json:"models"`
	LastActivity time.Time    `json:"last_activity"`
	TotalTokens  int64        `json:"total_tokens"`
	Calls        int          `json:"calls"`
	Status       string       `json:"status"`
	Snippet      string       `json:"snippet,omitempty"`
	MatchCount   int          `json:"match_count"`
	GenerationID string       `json:"generation_id,omitempty"`
	TokenBuckets TokenBuckets `json:"token_buckets"`
}

// SearchConversations runs a case-insensitive full-text search across every
// readable recorded conversation. The match runs over title, agent and
// model names, plus every text/thinking part and every tool-call input
// and tool-result content in every generation. Hits are ranked by total
// match count, with newer-last-activity as a tiebreak.
//
// query is split into whitespace-delimited terms; a hit must contain
// every term at least once (an AND across terms). An empty or whitespace
// query returns no hits without error, matching the endpoint contract.
// limit ≤ 0 means no cap.
func (s *Storage) SearchConversations(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	terms := searchTerms(query)
	if len(terms) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.dir, ConversationsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		paths = append(paths, filepath.Join(dir, entry.Name()))
	}

	var skipped int
	defer func() { s.logSkipped("search", skipped) }()
	slots, skipped, err := scanConversationFiles(ctx, paths, terms, searchConversationFile)
	if err != nil {
		return nil, err
	}

	out := make([]SearchHit, 0, len(slots))
	for _, result := range slots {
		if result.ok {
			out = append(out, result.hit)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].MatchCount != out[j].MatchCount {
			return out[i].MatchCount > out[j].MatchCount
		}
		// Tiebreak by newest last_activity.
		return out[i].LastActivity.After(out[j].LastActivity)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

type searchFileFunc func(string, []string) (SearchHit, bool, int, error)

type searchSlot struct {
	hit SearchHit
	ok  bool
}

func scanConversationFiles(ctx context.Context, paths, terms []string, scan searchFileFunc) ([]searchSlot, int, error) {
	slots := make([]searchSlot, len(paths))
	var skipped atomic.Int64
	var wg sync.WaitGroup
	workerCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()
	var scanErr error
	var scanErrOnce sync.Once
	next := make(chan int)
	wg.Go(func() {
		defer close(next)
		for i := range paths {
			select {
			case next <- i:
			case <-workerCtx.Done():
				return
			}
		}
	})
	for range min(searchWorkers(), max(len(paths), 1)) {
		wg.Go(func() {
			for i := range next {
				if workerCtx.Err() != nil {
					return
				}
				hit, ok, n, err := scan(paths[i], terms)
				skipped.Add(int64(n))
				if errors.Is(err, bufio.ErrTooLong) {
					skipped.Add(1)
					continue
				}
				if err != nil {
					scanErrOnce.Do(func() {
						scanErr = err
						cancelWorkers()
					})
					return
				}
				slots[i] = searchSlot{hit: hit, ok: ok}
			}
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, int(skipped.Load()), err
	}
	if scanErr != nil {
		return nil, int(skipped.Load()), scanErr
	}
	return slots, int(skipped.Load()), nil
}

// searchTerms splits the user query into lower-cased terms. Whitespace is
// the only delimiter; punctuation stays in a term so a literal phrase
// like "panic:" still matches itself. Duplicate terms are dropped so a
// query like "limit limit" doesn't inflate the match count.
func searchTerms(query string) []string {
	terms := strings.Fields(strings.ToLower(strings.TrimSpace(query)))
	if len(terms) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(terms))
	out := make([]string, 0, len(terms))
	for _, t := range terms {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// searchConversationFile scans one conversation JSONL file, aggregating
// the same summary fields ListConversations produces while counting
// matches against the search terms. The first text fragment that
// contains a term is captured for the snippet; the first generation
// containing any term is captured for the matched generation id.
//
// ok is false when the file has no decodable records OR when at least one
// term does not appear anywhere in the conversation (so the result
// satisfies the AND semantics across terms). The third result counts the
// lines no projection could decode.
func searchConversationFile(path string, terms []string) (SearchHit, bool, int, error) {
	hit := SearchHit{}
	agents := map[string]struct{}{}
	models := map[string]struct{}{}
	termHits := make(map[string]int, len(terms))
	var snippet string
	var snippetGenID string
	var seen, hasError bool
	var lastActivity time.Time

	skipped, err := scanLatestGenerationRecords(path, func(r generationRecord, gen storedGeneration) {
		seen = true
		if hit.ID == "" {
			hit.ID = r.ConversationID
		}
		hit.Calls++
		usage := gen.Usage.toSDK()
		hit.TotalTokens += totalTokensForView(usage, gen.Model.Provider)
		hit.TokenBuckets = hit.TokenBuckets.plus(disjointTokenUsage(usage, gen.Model.Provider))

		if gen.AgentName != "" {
			agents[gen.AgentName] = struct{}{}
		}
		if name := gen.modelName(); name != "" {
			models[name] = struct{}{}
		}
		if hit.Title == "" && gen.title() != "" {
			hit.Title = gen.title()
		}
		if gen.CallError != "" {
			hasError = true
		}

		// last_activity tracks the latest known timestamp on any
		// generation, falling back to received_at when started/completed
		// aren't populated — same rule as decodeFileSummary.
		when := gen.CompletedAt
		if when.IsZero() {
			when = gen.StartedAt
		}
		if when.IsZero() {
			when, _ = time.Parse(time.RFC3339Nano, r.ReceivedAt)
		}
		if when.After(lastActivity) {
			lastActivity = when
		}

		// Walk every searchable piece of text in this generation. Each
		// occurrence of a term contributes one match; the first hit per
		// conversation also seeds the snippet and the matched generation
		// id (for deep-linking into the matched turn in a future view).
		visit := func(text string) {
			if text == "" {
				return
			}
			lower := strings.ToLower(text)
			for _, term := range terms {
				if term == "" {
					continue
				}
				count := strings.Count(lower, term)
				if count == 0 {
					continue
				}
				termHits[term] += count
				hit.MatchCount += count
				if snippet == "" {
					snippet = buildSnippet(text, lower, term)
					snippetGenID = gen.ID
				}
			}
		}

		// Always include the cheap fields so a search on the agent or
		// model name surfaces sessions that never wrote matching content.
		visit(gen.AgentName)
		visit(gen.modelName())
		visit(gen.title())

		for _, msg := range gen.inputMessages() {
			visitMessageParts(msg, visit)
		}
		for _, msg := range gen.outputMessages() {
			visitMessageParts(msg, visit)
		}
	})
	if err != nil {
		return SearchHit{}, false, skipped, err
	}
	if !seen {
		return SearchHit{}, false, skipped, nil
	}
	// AND across terms: every term must have contributed at least once.
	for _, term := range terms {
		if termHits[term] == 0 {
			return SearchHit{}, false, skipped, nil
		}
	}
	hit.Agents = sortedKeys(agents)
	hit.Models = sortedKeys(models)
	hit.LastActivity = lastActivity
	hit.Status = "ok"
	if hasError {
		hit.Status = "err"
	}
	hit.Snippet = snippet
	hit.GenerationID = snippetGenID
	return hit, true, skipped, nil
}

// visitMessageParts walks one message's parts and feeds every searchable
// string fragment into visit. Tool-call inputs and tool-result JSON
// bodies are passed through as raw JSON so a search for a key or value
// inside an arguments blob still matches.
func visitMessageParts(msg agento11y.Message, visit func(string)) {
	for _, p := range msg.Parts {
		switch p.Kind {
		case agento11y.PartKindText:
			visit(p.Text)
		case agento11y.PartKindThinking:
			visit(p.Thinking)
		case agento11y.PartKindToolCall:
			if p.ToolCall != nil {
				visit(p.ToolCall.Name)
				if len(p.ToolCall.InputJSON) > 0 {
					visit(string(p.ToolCall.InputJSON))
				}
			}
		case agento11y.PartKindToolResult:
			if p.ToolResult != nil {
				visit(p.ToolResult.Name)
				visit(p.ToolResult.Content)
				if len(p.ToolResult.ContentJSON) > 0 {
					visit(string(p.ToolResult.ContentJSON))
				}
			}
		default:
			// Media parts carry no searchable prose.
		}
	}
}

// buildSnippet returns a short region of text containing the first match
// of term in textLower. The snippet preserves surrounding context up to
// snippetWindow runes on each side and is then UTF-8-safely truncated to
// snippetMaxLen so the JSON payload stays bounded. text carries the
// original casing; textLower must be the lower-cased copy used for the
// match (so indices line up).
func buildSnippet(text, textLower, term string) string {
	idx := strings.Index(textLower, term)
	if idx < 0 {
		return truncate(strings.TrimSpace(text), snippetMaxLen)
	}
	start := max(idx-snippetWindow, 0)
	// Slide start to a rune boundary so we never emit a half rune.
	for start > 0 && (text[start]&0xC0) == 0x80 {
		start--
	}
	end := min(idx+len(term)+snippetWindow, len(text))
	frag := strings.TrimSpace(text[start:end])
	if start > 0 {
		frag = "…" + frag
	}
	return truncate(frag, snippetMaxLen)
}
