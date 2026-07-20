package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/toolevents"
)

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func TestBeforeTool_GuardsDisabledPassesThrough(t *testing.T) {
	t.Setenv("SIGIL_GUARDS_ENABLED", "false")
	var out bytes.Buffer
	BeforeTool(context.Background(), &out, Payload{
		HookEventName: "before_tool",
		SessionID:     "s",
		ToolNameValue: "bash",
	}, discardLogger())
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty (pass-through) when guards are disabled", out.String())
	}
}

func TestBeforeTool_FailClosedDeniesOnMissingCreds(t *testing.T) {
	t.Setenv("SIGIL_GUARDS_ENABLED", "true")
	t.Setenv("SIGIL_GUARDS_FAIL_OPEN", "false")
	t.Setenv("SIGIL_ENDPOINT", "")
	t.Setenv("SIGIL_AUTH_TENANT_ID", "")
	t.Setenv("SIGIL_AUTH_TOKEN", "")

	var out bytes.Buffer
	BeforeTool(context.Background(), &out, Payload{
		HookEventName:  "before_tool",
		SessionID:      "s",
		ToolNameValue:  "bash",
		ToolInputValue: json.RawMessage(`{"command":"ls"}`),
	}, discardLogger())

	var resp struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("stdout is not a JSON deny response: %v (got %q)", err, out.String())
	}
	if resp.Decision != "deny" {
		t.Errorf("decision = %q, want deny (fail-closed with no creds)", resp.Decision)
	}
	if resp.Reason == "" {
		t.Error("deny reason is empty")
	}
}

func TestBeforeTool_FailOpenAllowsOnMissingCreds(t *testing.T) {
	t.Setenv("SIGIL_GUARDS_ENABLED", "true")
	t.Setenv("SIGIL_GUARDS_FAIL_OPEN", "true")
	t.Setenv("SIGIL_ENDPOINT", "")
	t.Setenv("SIGIL_AUTH_TENANT_ID", "")
	t.Setenv("SIGIL_AUTH_TOKEN", "")

	var out bytes.Buffer
	BeforeTool(context.Background(), &out, Payload{
		HookEventName: "before_tool",
		SessionID:     "s",
		ToolNameValue: "bash",
	}, discardLogger())
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty (allow) when failing open", out.String())
	}
}

func TestBeforeToolWriters(t *testing.T) {
	t.Run("deny", func(t *testing.T) {
		var out bytes.Buffer
		writeBeforeToolDeny(&out, "blocked by policy")
		var resp map[string]any
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatalf("invalid deny JSON: %v", err)
		}
		if resp["decision"] != "deny" || resp["reason"] != "blocked by policy" {
			t.Errorf("deny payload = %v", resp)
		}
	})

	t.Run("deny falls back to a default reason", func(t *testing.T) {
		var out bytes.Buffer
		writeBeforeToolDeny(&out, "  ")
		if !strings.Contains(out.String(), "denied by agento11y guard") {
			t.Errorf("missing default reason: %s", out.String())
		}
	})

	t.Run("rewrite", func(t *testing.T) {
		var out bytes.Buffer
		writeBeforeToolRewrite(&out, json.RawMessage(`{"command":"ls -la"}`))
		var resp struct {
			HookSpecificOutput struct {
				ToolInput json.RawMessage `json:"tool_input"`
			} `json:"hook_specific_output"`
		}
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatalf("invalid rewrite JSON: %v", err)
		}
		if string(resp.HookSpecificOutput.ToolInput) != `{"command":"ls -la"}` {
			t.Errorf("tool_input = %s, want the rewritten args", resp.HookSpecificOutput.ToolInput)
		}
	})

	t.Run("rewrite with empty input writes nothing", func(t *testing.T) {
		var out bytes.Buffer
		writeBeforeToolRewrite(&out, nil)
		if out.Len() != 0 {
			t.Errorf("stdout = %q, want empty for an empty rewrite", out.String())
		}
	})
}

func TestAfterTool_RecordsEventWhenOTelConfigured(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")

	AfterTool(Payload{
		HookEventName:   "after_tool",
		SessionID:       "sess-after",
		ToolNameValue:   "bash",
		ToolCallIDValue: "tc-1",
		ToolStatusValue: "failure",
		ToolErrorValue:  json.RawMessage(`"exit 1"`),
		DurationMsValue: ptr(1234.0),
	}, discardLogger())

	events := toolevents.Load("sess-after")
	ev, ok := events["tc-1"]
	if !ok {
		t.Fatalf("no tool event recorded; got %v", events)
	}
	if ev.Status != "failure" || !ev.Failed() {
		t.Errorf("status = %q, want failure", ev.Status)
	}
	if ev.DurationMs != 1234 {
		t.Errorf("duration = %v, want 1234", ev.DurationMs)
	}
	if ev.Error != "exit 1" {
		t.Errorf("error = %q, want exit 1", ev.Error)
	}
}

func TestAfterTool_NoOpWithoutOTel(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	AfterTool(Payload{
		HookEventName:   "after_tool",
		SessionID:       "sess-noop",
		ToolCallIDValue: "tc-1",
		ToolStatusValue: "success",
	}, discardLogger())

	if events := toolevents.Load("sess-noop"); len(events) != 0 {
		t.Errorf("recorded %d events without an OTel exporter, want 0", len(events))
	}
}

func ptr[T any](v T) *T { return &v }
