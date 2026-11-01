package vibe

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/toolevents"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

func TestHook_PostToolSkillEvidence(t *testing.T) {
	for _, event := range []string{"post_tool", "after_tool"} {
		for _, tc := range []struct {
			name, tool, input, status, want string
		}{
			{name: "success", tool: "skill", input: `{"name":"review"}`, status: "success", want: "review"},
			{name: "failed attempt", tool: "skill", input: `{"name":"review"}`, status: "failure", want: "review"},
			{name: "cancelled attempt", tool: "skill", input: `{"name":"review"}`, status: "cancelled", want: "review"},
			{name: "normalized namespace", tool: "skill", input: `{"name":" team:review "}`, status: "success", want: "team:review"},
			{name: "wrong tool", tool: "read", input: `{"name":"review"}`, status: "success"},
			{name: "wrong case", tool: "Skill", input: `{"name":"review"}`, status: "success"},
			{name: "wrong argument signature", tool: "skill", input: `{"skill":"review"}`, status: "success"},
			{name: "stringified object", tool: "skill", input: `"{\"name\":\"review\"}"`, status: "success"},
			{name: "nonstring name", tool: "skill", input: `{"name":42}`, status: "success"},
			{name: "unsafe name", tool: "skill", input: `{"name":"/private/skills/review/SKILL.md"}`, status: "success"},
		} {
			t.Run(event+"/"+tc.name, func(t *testing.T) {
				envconfig.PinAliasEnvBlank(t)
				t.Setenv("HOME", t.TempDir())
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				// PostTool only persists evidence; it must not contact an exporter.
				t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
				raw, err := json.Marshal(map[string]any{
					"hook_event_name": event, "session_id": "skill-session", "tool_call_id": "call-review",
					"tool_name": tc.tool, "tool_input": json.RawMessage(tc.input), "tool_status": tc.status, "duration_ms": 42.5,
				})
				require.NoError(t, err)
				before := time.Now().UTC()
				var stdout bytes.Buffer
				err = Hook(context.Background(), strings.NewReader(string(raw)), &stdout, log.New(io.Discard, "", 0))
				require.NoError(t, err)
				require.Empty(t, stdout.String(), "post hooks must never signal deny/retry")
				events := toolevents.Load("skill-session")
				require.Len(t, events, 1)
				ev := events["call-review"]
				require.Equal(t, "call-review", ev.ToolCallID)
				require.Equal(t, tc.tool, ev.ToolName)
				require.Equal(t, tc.want, ev.SkillName)
				require.Equal(t, tc.status, ev.Status)
				require.Equal(t, 42.5, ev.DurationMs)
				require.False(t, ev.CompletedAt.Before(before))
				require.False(t, ev.CompletedAt.After(time.Now().UTC()))
			})
		}
	}
}
