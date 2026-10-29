package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/subagent"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

const liveReviewSession = "live-subagent-review"

func liveReviewEnv(t *testing.T) (string, *[][]exportedGeneration) {
	t.Helper()
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	previous := transcriptSettleWindow
	transcriptSettleWindow = 0
	t.Cleanup(func() { transcriptSettleWindow = previous })
	url, exports := startGenerationExportServer(t)
	setHookExportEnv(t, url)
	return filepath.Join(t.TempDir(), liveReviewSession+".jsonl"), exports
}

func liveReviewPut(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func liveReviewHook(t *testing.T, path, event string) {
	t.Helper()
	runHookForTest(t, hookInput{HookEventName: event, SessionID: liveReviewSession, TranscriptPath: path})
}

func liveReviewSpawn(request, id string, background bool) string {
	return buildHookAssistantLineAt(liveReviewSession, request, "tool_use", hookFixtureTimestamp, []any{map[string]any{"type": "tool_use", "id": id, "name": "Agent", "input": map[string]any{"subagent_type": "Explorer", "run_in_background": background}}}, 5) + "\n"
}

func liveReviewChildren(exports [][]exportedGeneration) []exportedGeneration {
	var children []exportedGeneration
	for _, batch := range exports {
		children = append(children, subagentExports(batch)...)
	}
	return children
}

func TestLiveSubagentIncompleteTail(t *testing.T) {
	for _, tc := range []struct {
		name       string
		delayed    bool
		background bool
		event      string
	}{
		{name: "terminated foreground", event: "Stop"},
		{name: "delayed foreground", delayed: true, event: "Stop"},
		{name: "delayed background", delayed: true, background: true, event: "Stop"},
		{name: "delayed session end", delayed: true, event: "SessionEnd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, exports := liveReviewEnv(t)
			recorder := installGlobalSpanRecorder(t)
			child := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents", "agent-child.jsonl")
			prefix := buildHookSidechainAssistantLineID(liveReviewSession, "child-request", "child", "explorer", "same-message", "", []any{map[string]any{"type": "tool_use", "id": "skill-call", "name": "Skill", "input": map[string]any{"skill": "review"}}}, 3) + "\n" + buildHookToolResultJSONL(liveReviewSession, "skill-call", "loaded") + "\n"
			last := buildHookSidechainAssistantLineID(liveReviewSession, "child-request", "child", "explorer", "same-message", "", []any{map[string]any{"type": "text", "text": "all done"}}, 660)
			text := prefix + last
			if !tc.delayed {
				text += "\n"
			}
			liveReviewPut(t, child, text)
			liveReviewPut(t, path, liveReviewSpawn("spawn", "spawn-call", tc.background)+buildHookToolResultJSONL(liveReviewSession, "spawn-call", "agentId: child")+"\n"+buildHookAssistantJSONL(liveReviewSession, "parent-close", "end_turn", "done", 5)+"\n")
			liveReviewHook(t, path, tc.event)
			if tc.delayed {
				if children := liveReviewChildren(*exports); len(children) != 0 {
					t.Fatalf("incomplete tail exported a prefix: %+v", children)
				}
				st := state.Load(liveReviewSession)
				if st.SubagentOffsets["child"] != 0 || st.Tools.Offsets[toolPath(child)] != int64(len(prefix)) {
					t.Fatalf("generation/observation cursors lost incomplete tail: %+v", st)
				}
				if st.Tools.CompletedChildren["child"] != (!tc.background && tc.event == "Stop") {
					t.Fatalf("wrong persisted child completion: %+v", st.Tools.CompletedChildren)
				}
				liveReviewHook(t, path, "Stop")
				if children := liveReviewChildren(*exports); len(children) != 0 {
					t.Fatalf("replay exported a prefix: %+v", children)
				}
				liveReviewPut(t, child, text+"\n")
			}
			liveReviewHook(t, path, "Stop")
			if !tc.background && tc.event == "Stop" {
				if children := liveReviewChildren(*exports); len(children) != 1 {
					t.Fatalf("completed foreground child waits for SessionEnd: %+v", children)
				}
				tools := spansByName(recorder.Ended(), "execute_tool")
				skills := 0
				for _, tool := range tools {
					if spanAttr(tool, "gen_ai.skill.name") == "review" {
						skills++
					}
				}
				if skills != 1 {
					t.Fatalf("completed foreground Skill observations=%d, want 1", skills)
				}
			}
			if completed := state.Load(liveReviewSession).Tools.CompletedChildren; len(completed) != 0 {
				t.Fatalf("consumed child completion retained: %+v", completed)
			}
			liveReviewHook(t, path, "SessionEnd")
			liveReviewHook(t, path, "Stop")
			children := liveReviewChildren(*exports)
			if len(children) != 1 {
				t.Fatalf("child generations=%d, want exactly one: %+v", len(children), children)
			}
			if _, out := children[0].usageTokens(); out != 660 {
				t.Fatalf("first-write output tokens=%d, want 660", out)
			}
			if got := state.Load(liveReviewSession).SubagentOffsets["child"]; got != int64(len(prefix+last+"\n")) {
				t.Fatalf("final cursor=%d, want EOF", got)
			}
		})
	}
}

func TestLiveSubagentDirectoryParent(t *testing.T) {
	for _, filename := range []string{"child.jsonl", "agent-child.jsonl"} {
		for _, scope := range []string{"parent", "sidechain", "nested", "delayed child"} {
			t.Run(filename+"/"+scope, func(t *testing.T) {
				path, exports := liveReviewEnv(t)
				root := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents")
				child := filepath.Join(root, filename)
				if scope == "sidechain" {
					child = filepath.Join(root, "target", filename)
				}
				spawnPath := path
				if scope == "nested" {
					spawnPath = filepath.Join(root, "agent-outer.jsonl")
					child = filepath.Join(root, "agent-outer", "subagents", filename)
				}
				childText := buildHookAssistantJSONL(liveReviewSession, "child-request", "end_turn", "child done", 17) + "\n"
				if scope == "delayed child" {
					childText = strings.Replace(childText, `"stop_reason":"end_turn"`, `"stop_reason":null`, 1)
				}
				liveReviewPut(t, child, childText)
				spawn := liveReviewSpawn("target-spawn", "same-call", scope == "delayed child") + buildHookToolResultJSONL(liveReviewSession, "same-call", "Transcript dir: "+filepath.Dir(child)) + "\n"
				if scope == "sidechain" {
					spawn = strings.ReplaceAll(spawn, `"sessionId":`, `"isSidechain":true,"agentId":"inline","sessionId":`)
				}
				if scope == "sidechain" || scope == "nested" {
					other := filepath.Join(root, "other", "agent-other.jsonl")
					liveReviewPut(t, other, buildHookAssistantJSONL(liveReviewSession, "other-child", "end_turn", "other done", 5)+"\n")
					liveReviewPut(t, path, liveReviewSpawn("other-spawn", "same-call", false)+buildHookToolResultJSONL(liveReviewSession, "same-call", "Transcript dir: "+filepath.Dir(other))+"\n")
					if spawnPath == path {
						data, err := os.ReadFile(path)
						if err != nil {
							t.Fatal(err)
						}
						spawn = string(data) + spawn
					}
				}
				liveReviewPut(t, spawnPath, spawn+buildHookAssistantJSONL(liveReviewSession, "close", "end_turn", "done", 5)+"\n")
				liveReviewHook(t, path, "Stop")
				st := state.Load(liveReviewSession)
				wantParent := st.SubagentParents[subagent.FileKey(child)]
				if wantParent == "" {
					t.Fatal("directory result did not remember a scoped parent")
				}
				if scope == "sidechain" || scope == "nested" {
					if wantParent == st.SubagentParents["other"] {
						t.Fatal("duplicate tool IDs merged spawning scopes")
					}
				}
				if scope == "delayed child" {
					if children := liveReviewChildren(*exports); len(children) != 0 {
						t.Fatalf("background child finalized early: %+v", children)
					}
					liveReviewHook(t, path, "SessionEnd")
				}
				liveReviewHook(t, path, "Stop")
				found := 0
				for _, gen := range liveReviewChildren(*exports) {
					_, out := gen.usageTokens()
					if out == 17 && len(gen.ParentGenerationIDs) == 1 && gen.ParentGenerationIDs[0] == wantParent {
						found++
					}
				}
				if found != 1 {
					t.Fatalf("children linked to scoped parent %q=%d, want one: %+v", wantParent, found, liveReviewChildren(*exports))
				}
			})
		}
	}
}
