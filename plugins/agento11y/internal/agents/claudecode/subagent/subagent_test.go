package subagent

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/transcript"
)

func TestIsTranscript(t *testing.T) {
	if IsTranscript("/p/sess.jsonl") {
		t.Fatal("session transcript must not match")
	}
	if !IsTranscript("/p/sess/subagents/agent-1.jsonl") {
		t.Fatal("subagent transcript must match")
	}
}

func TestFiles(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "sess-1.jsonl")
	if err := os.WriteFile(sessionPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	subDir := filepath.Join(dir, "sess-1", "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(subDir, "agent-abc.jsonl")
	if err := os.WriteFile(want, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Files(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{want}) {
		t.Fatalf("Files = %v, want [%s]", got, want)
	}

	none, err := Files(filepath.Join(dir, "missing.jsonl"))
	if err != nil || len(none) != 0 {
		t.Fatalf("missing session Files = %v, %v", none, err)
	}
}

func TestAgentIDsFromToolResult(t *testing.T) {
	got := AgentIDsFromToolResult("agentId: abc\nAgent ID: def\nagent_id: `ghi`\n")
	want := []string{"abc", "def", "ghi"}
	if !slices.Equal(got, want) {
		t.Fatalf("AgentIDsFromToolResult = %v, want %v", got, want)
	}
}

func TestCompletedKeysMatchesAgentID(t *testing.T) {
	file := filepath.Join(t.TempDir(), "agent-abc.jsonl")
	lines := []transcript.Line{{
		Type:    "user",
		Message: []byte(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu","content":"agentId: abc\n"}]}`),
	}}
	got := CompletedKeys(lines, []string{file})
	if !got["abc"] {
		t.Fatalf("CompletedKeys = %v, want abc", got)
	}
}

func TestFinalizeRenamesAndLinks(t *testing.T) {
	parents := Parents{
		byAgentID:          map[string]string{"abc": "parent-gen"},
		agentNameByAgentID: map[string]string{"abc": "claude-code/general-purpose"},
	}
	gens := []agento11y.Generation{
		{ID: "g1", AgentName: "claude-code", ConversationID: "other"},
		{ID: "g2", AgentName: "claude-code/general-purpose"},
	}
	Finalize("/p/sess/subagents/agent-abc.jsonl", gens, parents, "sess-1", "claude-code")
	if gens[0].AgentName != "claude-code/general-purpose" {
		t.Fatalf("gens[0].AgentName = %q", gens[0].AgentName)
	}
	if gens[0].ConversationID != "sess-1" || gens[1].ConversationID != "sess-1" {
		t.Fatalf("conversation IDs = %q, %q", gens[0].ConversationID, gens[1].ConversationID)
	}
	if !slices.Equal(gens[0].ParentGenerationIDs, []string{"parent-gen"}) {
		t.Fatalf("gens[0] parents = %v", gens[0].ParentGenerationIDs)
	}
	if !slices.Equal(gens[1].ParentGenerationIDs, []string{"g1"}) {
		t.Fatalf("gens[1] parents = %v", gens[1].ParentGenerationIDs)
	}
}

func TestIDsUniqueSuffixesCollisions(t *testing.T) {
	ids := NewIDs()
	parent := []agento11y.Generation{{ID: "shared"}}
	ids.Observe(parent)
	child := []agento11y.Generation{{ID: "shared"}, {ID: "other"}}
	ids.Unique(child)
	if child[0].ID != "shared#1" {
		t.Fatalf("colliding child ID = %q, want shared#1", child[0].ID)
	}
	if child[1].ID != "other" {
		t.Fatalf("unique child ID = %q, want other", child[1].ID)
	}
}
