package subagent

import (
	"encoding/json"
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

func TestFilesStaysInsideSessionTree(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "sess-1.jsonl")
	if err := os.WriteFile(sessionPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nestedDir := filepath.Join(dir, "sess-1", "subagents", "nested")
	otherDir := filepath.Join(dir, "other-session", "subagents")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(nestedDir, "agent-child.jsonl")
	if err := os.WriteFile(nested, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherDir, "agent-other.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Files(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{nested}) {
		t.Fatalf("Files = %v, want only the nested file under sess-1/subagents", got)
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

func TestDropCoveredSidechain(t *testing.T) {
	files := []string{"/p/sess/subagents/agent-abc.jsonl"}
	lines := []transcript.Line{
		{Type: "user", AgentID: "", IsSidechain: false},
		{Type: "assistant", AgentID: "abc", IsSidechain: true},
		{Type: "assistant", AgentID: "abc", IsSidechain: false},
		{Type: "assistant", AgentID: "other", IsSidechain: true},
	}
	got := DropCoveredSidechain(lines, files, "/p/sess.jsonl")
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (covered sidechain dropped)", len(got))
	}
	self := DropCoveredSidechain(lines, files, files[0])
	if len(self) != 4 {
		t.Fatalf("self file dropped its own turns: len = %d", len(self))
	}
}

func TestBlockingCompletedKeysSkipsBackground(t *testing.T) {
	file := filepath.Join(t.TempDir(), "agent-abc.jsonl")
	lines := []transcript.Line{
		{
			Type:    "assistant",
			Message: []byte(`{"content":[{"type":"tool_use","id":"tu_bg","name":"Agent","input":{"run_in_background":true,"subagent_type":"general-purpose"}}]}`),
		},
		{
			Type:    "user",
			Message: []byte(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_bg","content":"agentId: abc\n"}]}`),
		},
		{
			Type:    "assistant",
			Message: []byte(`{"content":[{"type":"tool_use","id":"tu_block","name":"Agent","input":{"subagent_type":"explore"}}]}`),
		},
		{
			Type:    "user",
			Message: []byte(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_block","content":"agentId: def\n"}]}`),
		},
	}
	got := BlockingCompletedKeys(lines, []string{file, filepath.Join(t.TempDir(), "agent-def.jsonl")})
	if got["abc"] {
		t.Fatal("background agent must not look finished")
	}
	if !got["def"] {
		t.Fatal("blocking agent with a tool result must look finished")
	}
}

func TestBlockingCompletedKeysIgnoresSharedTranscriptDir(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "sess", "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	blocking := filepath.Join(subDir, "agent-done.jsonl")
	running := filepath.Join(subDir, "agent-bg.jsonl")
	if err := os.WriteFile(blocking, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(running, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines := []transcript.Line{
		{
			Type:    "assistant",
			Message: []byte(`{"content":[{"type":"tool_use","id":"tu_done","name":"Agent","input":{"subagent_type":"explore"}}]}`),
		},
		toolResultLine(t, "tu_done", "agentId: done\nTranscript dir: "+subDir+"\n"),
		{
			Type:    "assistant",
			Message: []byte(`{"content":[{"type":"tool_use","id":"tu_bg","name":"Agent","input":{"run_in_background":true,"subagent_type":"general-purpose"}}]}`),
		},
		toolResultLine(t, "tu_bg", "agentId: bg\nTranscript dir: "+subDir+"\n"),
	}
	got := BlockingCompletedKeys(lines, []string{blocking, running})
	if !got["done"] {
		t.Fatal("blocking agent named by agentId must look finished")
	}
	if got["bg"] {
		t.Fatal("shared Transcript dir must not mark the running sibling done")
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

func toolResultLine(t *testing.T, toolUseID, content string) transcript.Line {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"role": "user",
		"content": []map[string]any{{
			"type":        "tool_result",
			"tool_use_id": toolUseID,
			"content":     content,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return transcript.Line{Type: "user", Message: raw}
}
