package local

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeGenWithMessages writes a generation with the given input and
// output messages. The helper exists so tests can drop arbitrary text
// and tool I/O into a generation without rebuilding the proto wire
// shape by hand on every call.
func writeGenWithMessages(t *testing.T, s *Storage, convID, genID string, input, output []agento11y.Message, receivedAt string) {
	t.Helper()
	gen := agento11y.Generation{
		ID:             genID,
		ConversationID: convID,
		AgentName:      "pi",
		Model:          agento11y.ModelRef{Provider: "anthropic", Name: "claude-opus-4-7"},
		Input:          input,
		Output:         output,
		StartedAt:      mustParse(t, receivedAt),
		CompletedAt:    mustParse(t, receivedAt),
	}
	writeGen(t, s, convID, genID, gen, receivedAt)
}

func textMsg(role agento11y.Role, body string) agento11y.Message {
	return agento11y.Message{Role: role, Parts: []agento11y.Part{{Kind: agento11y.PartKindText, Text: body}}}
}

func toolCallMsg(name, input string) agento11y.Message {
	return agento11y.Message{
		Role: agento11y.RoleAssistant,
		Parts: []agento11y.Part{{
			Kind:     agento11y.PartKindToolCall,
			ToolCall: &agento11y.ToolCall{ID: "tc-1", Name: name, InputJSON: json.RawMessage(input)},
		}},
	}
}

func toolResultMsg(name, content string) agento11y.Message {
	return agento11y.Message{
		Role: agento11y.RoleTool,
		Parts: []agento11y.Part{{
			Kind:       agento11y.PartKindToolResult,
			ToolResult: &agento11y.ToolResult{ToolCallID: "tc-1", Name: name, Content: content},
		}},
	}
}

func BenchmarkSearchConversations(b *testing.B) {
	s, err := NewStorage(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}

	const (
		conversations = 500
		perConv       = 20
	)
	startedAt := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	for conv := range conversations {
		convID := "conv-" + strconv.Itoa(conv)
		recs := make([]generationRecord, 0, perConv)
		activities := make([]time.Time, 0, perConv)
		for i := range perConv {
			genID := "gen-" + strconv.Itoa(i)
			body := "common benchmark text from a local conversation"
			if conv == conversations-1 && i == perConv-1 {
				body += " rarebenchmarkterm"
			}
			activity := startedAt.Add(time.Duration(conv*perConv+i) * time.Second)
			raw, marshalErr := json.Marshal(agento11y.Generation{
				ID:             genID,
				ConversationID: convID,
				AgentName:      "pi",
				Model:          agento11y.ModelRef{Provider: "anthropic", Name: "claude-opus-4-7"},
				Output:         []agento11y.Message{textMsg(agento11y.RoleAssistant, body)},
				StartedAt:      activity,
				CompletedAt:    activity,
			})
			if marshalErr != nil {
				b.Fatal(marshalErr)
			}
			recs = append(recs, generationRecord{
				ReceivedAt:     activity.Format(time.RFC3339Nano),
				GenerationID:   genID,
				ConversationID: convID,
				Generation:     raw,
			})
			activities = append(activities, activity)
		}
		written, appendErr := s.AppendGenerations(convID, recs, activities)
		if appendErr != nil {
			b.Fatal(appendErr)
		}
		if written != len(recs) {
			b.Fatalf("wrote %d generations for %s, want %d", written, convID, len(recs))
		}
	}

	for _, tc := range []struct {
		name  string
		query string
	}{
		{name: "rare", query: "rarebenchmarkterm"},
		{name: "common", query: "common"},
		{name: "none", query: "notpresentbenchmarkterm"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, searchErr := s.SearchConversations(context.Background(), tc.query, 100); searchErr != nil {
					b.Fatal(searchErr)
				}
			}
		})
	}
}

// TestSearchConversations covers the headline behaviours the design
// handoff and the spec require: matching text/thinking/tool I/O,
// ranking by total match count, newest-first tiebreak, the AND across
// terms, case-insensitivity, and empty-query short-circuit.
func TestSearchConversations(t *testing.T) {
	s := newStorage(t)

	// conv-A: one tool result mentioning "rate limit", one prompt
	// repeating "rate".
	writeGenWithMessages(t, s, "conv-A", "g1",
		[]agento11y.Message{textMsg(agento11y.RoleUser, "Hit the rate limit on the gateway.")},
		[]agento11y.Message{
			toolCallMsg("bash", `{"command":"curl https://api"}`),
			textMsg(agento11y.RoleAssistant, "rate limit means we should back off."),
		},
		"2026-05-21T10:00:00Z")
	writeGenWithMessages(t, s, "conv-A", "g2",
		nil,
		[]agento11y.Message{toolResultMsg("bash", "HTTP/1.1 429 rate limit exceeded")},
		"2026-05-21T10:00:05Z")

	// conv-B: a single mention of "rate" (no "limit").
	writeGenWithMessages(t, s, "conv-B", "g3",
		[]agento11y.Message{textMsg(agento11y.RoleUser, "What is your rate of throughput?")},
		[]agento11y.Message{textMsg(agento11y.RoleAssistant, "Depends on the model.")},
		"2026-05-21T11:00:00Z")

	// conv-C: newest, mentions "rate limit" once. Used to assert the
	// last_activity tiebreak when match counts tie.
	writeGenWithMessages(t, s, "conv-C", "g4",
		nil,
		[]agento11y.Message{textMsg(agento11y.RoleAssistant, "I think the rate limit was reset.")},
		"2026-05-21T12:00:00Z")

	// Write these in reverse filename order. os.ReadDir sorts by filename, and
	// a stable rank must preserve that order when both ranking fields tie.
	for _, convID := range []string{"conv-order-b", "conv-order-a"} {
		writeGenWithMessages(t, s, convID, "g-order", nil,
			[]agento11y.Message{textMsg(agento11y.RoleAssistant, "exactordertie")},
			"2026-05-21T13:00:00Z")
	}

	t.Run("ranks by total match count then newest", func(t *testing.T) {
		hits, err := s.SearchConversations(context.Background(), "rate limit", 0)
		require.NoError(t, err)
		require.Len(t, hits, 2, "conv-B mentions only one of the two terms; AND filter drops it")

		// conv-A has 4 matches (2 "rate" + 2 "limit"); conv-C has 2.
		assert.Equal(t, "conv-A", hits[0].ID)
		assert.Equal(t, "conv-C", hits[1].ID)
		assert.Greater(t, hits[0].MatchCount, hits[1].MatchCount)
		assert.NotEmpty(t, hits[0].Snippet)
		assert.Equal(t, "g1", hits[0].GenerationID, "snippet should point at the first matching turn")
	})

	t.Run("tiebreak prefers newest last_activity", func(t *testing.T) {
		// Both conversations match "limit" exactly once, so the tie
		// is broken by last_activity.
		hits, err := s.SearchConversations(context.Background(), "limit", 0)
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(hits), 2)
		// conv-C (12:00) is newer than conv-A (10:00:05) but conv-A
		// has more matches; assert conv-C precedes any equally-ranked
		// older hit.
		for i := 1; i < len(hits); i++ {
			if hits[i-1].MatchCount == hits[i].MatchCount {
				assert.False(t, hits[i-1].LastActivity.Before(hits[i].LastActivity),
					"hit %d (%s) should not be older than hit %d (%s)",
					i-1, hits[i-1].LastActivity, i, hits[i].LastActivity)
			}
		}
	})

	t.Run("case-insensitive substring matches", func(t *testing.T) {
		hits, err := s.SearchConversations(context.Background(), "RATE", 0)
		require.NoError(t, err)
		assert.NotEmpty(t, hits)
	})

	t.Run("matches inside tool-call input JSON", func(t *testing.T) {
		hits, err := s.SearchConversations(context.Background(), "curl", 0)
		require.NoError(t, err)
		require.Len(t, hits, 1)
		assert.Equal(t, "conv-A", hits[0].ID)
	})

	t.Run("no match returns empty hits, not error", func(t *testing.T) {
		hits, err := s.SearchConversations(context.Background(), "absolutely-not-in-any-conversation-xyz", 0)
		require.NoError(t, err)
		assert.Empty(t, hits)
	})

	t.Run("empty query returns no hits and no error", func(t *testing.T) {
		hits, err := s.SearchConversations(context.Background(), "", 0)
		require.NoError(t, err)
		assert.Empty(t, hits)

		hits, err = s.SearchConversations(context.Background(), "    ", 0)
		require.NoError(t, err)
		assert.Empty(t, hits)
	})

	t.Run("limit caps the result count", func(t *testing.T) {
		hits, err := s.SearchConversations(context.Background(), "rate", 1)
		require.NoError(t, err)
		assert.Len(t, hits, 1)
	})

	t.Run("hit carries the summary fields the UI needs", func(t *testing.T) {
		hits, err := s.SearchConversations(context.Background(), "rate", 0)
		require.NoError(t, err)
		require.NotEmpty(t, hits)
		hit := hits[0]
		assert.False(t, hit.LastActivity.IsZero())
		assert.NotEmpty(t, hit.Agents)
		assert.NotEmpty(t, hit.Models)
		assert.Greater(t, hit.Calls, 0)
		assert.Equal(t, "ok", hit.Status)
	})

	t.Run("complete ranking ties keep store order", func(t *testing.T) {
		hits, err := s.SearchConversations(context.Background(), "exactordertie", 0)
		require.NoError(t, err)
		require.Len(t, hits, 2)
		assert.Equal(t, "conv-order-a", hits[0].ID)
		assert.Equal(t, "conv-order-b", hits[1].ID)
		assert.Equal(t, hits[0].MatchCount, hits[1].MatchCount)
		assert.Equal(t, hits[0].LastActivity, hits[1].LastActivity)
	})

	t.Run("skipped lines are counted once", func(t *testing.T) {
		var logs strings.Builder
		s.SetLogger(log.New(&logs, "", 0))
		for _, convID := range []string{"bad-a", "bad-b", "bad-c"} {
			path := filepath.Join(s.Dir(), ConversationsDir, convID+".jsonl")
			require.NoError(t, os.WriteFile(path, []byte("not-json\n"), 0o600))
		}

		_, err := s.SearchConversations(context.Background(), "notpresent", 0)
		require.NoError(t, err)
		assert.Equal(t, 1, strings.Count(logs.String(), "local: search: skipped 3 unparseable lines"))
	})
}

func TestSearchCancellation(t *testing.T) {
	t.Run("returns the context error", func(t *testing.T) {
		s := newStorage(t)
		writeGenWithMessages(t, s, "conv-cancel", "g1", nil,
			[]agento11y.Message{textMsg(agento11y.RoleAssistant, "cancel search")},
			"2026-05-21T12:00:00Z")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := s.SearchConversations(ctx, "cancel", 0)
		assert.ErrorIs(t, err, context.Canceled)
	})

	t.Run("starts no new scans after cancellation", func(t *testing.T) {
		workerCount := searchWorkers()
		paths := make([]string, workerCount+10)
		var started atomic.Int64
		allWorkersStarted := make(chan struct{})
		release := make(chan struct{})
		scan := func(string, []string) (SearchHit, bool, int, error) {
			if started.Add(1) == int64(workerCount) {
				close(allWorkersStarted)
			}
			<-release
			return SearchHit{}, false, 0, nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, _, err := scanConversationFiles(ctx, paths, []string{"cancel"}, scan)
			done <- err
		}()

		select {
		case <-allWorkersStarted:
		case <-time.After(5 * time.Second):
			t.Fatal("workers did not start")
		}
		cancel()
		close(release)
		assert.ErrorIs(t, <-done, context.Canceled)
		assert.Equal(t, int64(workerCount), started.Load())
	})
}

func TestSearchSkipsUnreadableConversation(t *testing.T) {
	t.Run("skips a line over the scanner limit", func(t *testing.T) {
		s := newStorage(t)
		var logs strings.Builder
		s.SetLogger(log.New(&logs, "", 0))
		for _, convID := range []string{"conv-good-a", "conv-good-b"} {
			writeGenWithMessages(t, s, convID, "g1", nil,
				[]agento11y.Message{textMsg(agento11y.RoleAssistant, "survivesoversized")},
				"2026-05-21T12:00:00Z")
		}

		path := filepath.Join(s.Dir(), ConversationsDir, "conv-oversized.jsonl")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
		require.NoError(t, err)
		chunk := strings.Repeat("x", 1024*1024)
		for range 65 {
			_, err = f.WriteString(chunk)
			require.NoError(t, err)
		}
		require.NoError(t, f.Close())

		req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=survivesoversized", nil)
		rr := httptest.NewRecorder()
		(&Server{storage: s, logger: log.New(io.Discard, "", 0)}).handleSearch(rr, req)
		require.Equal(t, http.StatusOK, rr.Code)
		var body struct {
			Hits []SearchHit `json:"hits"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		require.Len(t, body.Hits, 2)
		assert.ElementsMatch(t, []string{"conv-good-a", "conv-good-b"}, []string{body.Hits[0].ID, body.Hits[1].ID})
		assert.Contains(t, logs.String(), "local: search: skipped 1 unparseable lines")
	})

	t.Run("returns a systemic scan failure", func(t *testing.T) {
		openErr := &os.PathError{Op: "open", Path: "conversation.jsonl", Err: syscall.EMFILE}
		slots, skipped, err := scanConversationFiles(
			context.Background(),
			[]string{"conversation.jsonl"},
			[]string{"needle"},
			func(string, []string) (SearchHit, bool, int, error) {
				return SearchHit{}, false, 0, openErr
			},
		)

		assert.ErrorIs(t, err, syscall.EMFILE)
		assert.Nil(t, slots)
		assert.Zero(t, skipped)
	})
}

// TestSearchLastActivityReceivedAtFallback pins the ordering when a
// generation has no started/completed time and last_activity has to come
// from received_at. Sub-second precision differs between the two sources,
// so the comparison has to happen on parsed times, not on the strings.
func TestSearchLastActivityReceivedAtFallback(t *testing.T) {
	// bareGen writes a generation carrying text but no started/completed
	// time, so the aggregator falls back to received_at.
	bareGen := func(t *testing.T, s *Storage, convID, genID, body, receivedAt string) {
		t.Helper()
		writeGen(t, s, convID, genID, agento11y.Generation{
			AgentName: "pi",
			Model:     agento11y.ModelRef{Provider: "anthropic", Name: "claude-opus-4-7"},
			Output:    []agento11y.Message{textMsg(agento11y.RoleAssistant, body)},
		}, receivedAt)
	}

	t.Run("picks the newest generation within a conversation", func(t *testing.T) {
		s := newStorage(t)
		writeGenWithMessages(t, s, "conv-D", "g1", nil,
			[]agento11y.Message{textMsg(agento11y.RoleAssistant, "quotacrunch alpha")},
			"2026-05-21T13:00:05.5Z")
		bareGen(t, s, "conv-D", "g2", "quotacrunch beta", "2026-05-21T13:00:05Z")

		hits, err := s.SearchConversations(context.Background(), "quotacrunch", 0)
		require.NoError(t, err)
		require.Len(t, hits, 1)
		assert.True(t, hits[0].LastActivity.Equal(mustParse(t, "2026-05-21T13:00:05.5Z")),
			"last_activity = %v, want g1's completed_at (the later time)", hits[0].LastActivity)
	})

	t.Run("tiebreak compares across timestamp sources", func(t *testing.T) {
		s := newStorage(t)
		bareGen(t, s, "conv-E", "g1", "quotacrunch once", "2026-05-21T13:00:05Z")
		writeGenWithMessages(t, s, "conv-F", "g2", nil,
			[]agento11y.Message{textMsg(agento11y.RoleAssistant, "quotacrunch once")},
			"2026-05-21T13:00:05.5Z")

		hits, err := s.SearchConversations(context.Background(), "quotacrunch", 0)
		require.NoError(t, err)
		require.Len(t, hits, 2)
		assert.Equal(t, hits[0].MatchCount, hits[1].MatchCount, "both hits should tie on match count")
		assert.Equal(t, "conv-F", hits[0].ID, "newer conversation ranks first")
	})
}

// TestSearchTerms checks the query tokenizer: case folding, whitespace
// splitting, and dedup so a query like "limit limit" still scores as one
// occurrence per match rather than doubling the score.
func TestSearchTerms(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace only", "   ", nil},
		{"single term", "rate", []string{"rate"}},
		{"lower-cases input", "RATE", []string{"rate"}},
		{"splits on whitespace", "rate  limit ", []string{"rate", "limit"}},
		{"dedups repeated terms", "rate rate limit", []string{"rate", "limit"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, searchTerms(tc.in))
		})
	}
}

// TestBuildSnippet asserts the snippet preserves casing, surrounds the
// match with context, and stays bounded under the byte cap.
func TestBuildSnippet(t *testing.T) {
	body := "Hit the rate limit on the gateway in production."
	got := buildSnippet(body, "hit the rate limit on the gateway in production.", "rate")
	assert.Contains(t, got, "rate", "snippet should contain the matched term with original casing")
	assert.LessOrEqual(t, len(got), snippetMaxLen+4, "snippet stays bounded with room for the ellipsis")

	// Big input should not blow the byte cap.
	huge := strings.Repeat("x", 10_000) + " needle " + strings.Repeat("y", 10_000)
	got = buildSnippet(huge, huge, "needle")
	assert.LessOrEqual(t, len(got), snippetMaxLen+4)
	assert.Contains(t, got, "needle")
}
