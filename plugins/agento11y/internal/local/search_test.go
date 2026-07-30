package local

import (
	"encoding/json"
	"strings"
	"testing"

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

	t.Run("ranks by total match count then newest", func(t *testing.T) {
		hits, err := s.SearchConversations("rate limit", 0)
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
		hits, err := s.SearchConversations("limit", 0)
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
		hits, err := s.SearchConversations("RATE", 0)
		require.NoError(t, err)
		assert.NotEmpty(t, hits)
	})

	t.Run("matches inside tool-call input JSON", func(t *testing.T) {
		hits, err := s.SearchConversations("curl", 0)
		require.NoError(t, err)
		require.Len(t, hits, 1)
		assert.Equal(t, "conv-A", hits[0].ID)
	})

	t.Run("no match returns empty hits, not error", func(t *testing.T) {
		hits, err := s.SearchConversations("absolutely-not-in-any-conversation-xyz", 0)
		require.NoError(t, err)
		assert.Empty(t, hits)
	})

	t.Run("empty query returns no hits and no error", func(t *testing.T) {
		hits, err := s.SearchConversations("", 0)
		require.NoError(t, err)
		assert.Empty(t, hits)

		hits, err = s.SearchConversations("    ", 0)
		require.NoError(t, err)
		assert.Empty(t, hits)
	})

	t.Run("limit caps the result count", func(t *testing.T) {
		hits, err := s.SearchConversations("rate", 1)
		require.NoError(t, err)
		assert.Len(t, hits, 1)
	})

	t.Run("hit carries the summary fields the UI needs", func(t *testing.T) {
		hits, err := s.SearchConversations("rate", 0)
		require.NoError(t, err)
		require.NotEmpty(t, hits)
		hit := hits[0]
		assert.False(t, hit.LastActivity.IsZero())
		assert.NotEmpty(t, hit.Agents)
		assert.NotEmpty(t, hit.Models)
		assert.Greater(t, hit.Calls, 0)
		assert.Equal(t, "ok", hit.Status)
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

		hits, err := s.SearchConversations("quotacrunch", 0)
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

		hits, err := s.SearchConversations("quotacrunch", 0)
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
