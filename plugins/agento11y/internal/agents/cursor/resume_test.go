package cursor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// Cursor fires sessionStart only when a composer conversation is created, and
// fires sessionEnd when the window or the chat closes. A closed chat can be
// reopened and continued under the same conversation_id without a second
// sessionStart. Every hook payload carries workspace_roots, user_email and
// cursor_version (https://cursor.com/docs/agent/hooks), so the turns after a
// reopen must export the same user, workspace and version as the turns before
// it, and keep the title while the session file is still there.
func TestHookResumedConversationKeepsSessionMetadata(t *testing.T) {
	tests := []struct {
		name     string
		autoTags bool
		// sessionGone deletes the conversation's state between the close and
		// the reopen, as a plugin version that removed it at sessionEnd did.
		sessionGone bool
		wantTitle   string
	}{
		{name: "reopened chat", wantTitle: "list go files"},
		{name: "reopened chat with auto tags", autoTags: true, wantTitle: "list go files"},
		{name: "reopened chat without its session file", sessionGone: true, wantTitle: "now add tests"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			t.Setenv("XDG_STATE_HOME", state)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(state, "config"))
			t.Setenv("HOME", state)
			workspace := t.TempDir()
			writeGitFixture(t, workspace, "git@github.com:grafana/agento11y.git", "feature/resume")

			var (
				mu          sync.Mutex
				generations []map[string]any
			)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Generations []map[string]any `json:"generations"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					http.Error(w, "invalid json", http.StatusBadRequest)
					return
				}
				results := make([]map[string]any, 0, len(request.Generations))
				mu.Lock()
				for _, g := range request.Generations {
					generations = append(generations, g)
					results = append(results, map[string]any{"generation_id": g["id"], "accepted": true})
				}
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
			}))
			defer server.Close()

			envconfig.PinAliasEnvBlank(t)
			t.Setenv("SIGIL_ENDPOINT", server.URL)
			t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
			t.Setenv("SIGIL_AUTH_TOKEN", "token")
			t.Setenv("SIGIL_PROTOCOL", "http")
			t.Setenv("SIGIL_USER_ID", "")
			t.Setenv("SIGIL_TAGS", "")
			t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "full")
			t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv("AGENTO11Y_AUTO_CODING_AGENT_TAGS", "false")
			if tc.autoTags {
				t.Setenv("AGENTO11Y_AUTO_CODING_AGENT_TAGS", "true")
			}

			base := map[string]any{
				"conversation_id": "conv-resume",
				"workspace_roots": []string{workspace},
				"user_email":      "dev@example.com",
				"cursor_version":  "2.0.1",
			}
			var logs bytes.Buffer
			send := func(event map[string]any) {
				t.Helper()
				payload := maps.Clone(base)
				maps.Copy(payload, event)
				raw, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				if err := Hook(context.Background(), bytes.NewReader(raw), io.Discard, log.New(&logs, "", 0)); err != nil {
					t.Fatalf("Hook(%s): %v", event["hook_event_name"], err)
				}
			}
			turn := func(generationID, prompt, at string) {
				t.Helper()
				send(map[string]any{
					"hook_event_name": "beforeSubmitPrompt",
					"generation_id":   generationID,
					"prompt":          prompt,
					"timestamp":       at + ":01Z",
				})
				send(map[string]any{
					"hook_event_name": "afterAgentResponse",
					"generation_id":   generationID,
					"text":            "done",
					"model":           "gpt-5",
					"timestamp":       at + ":02Z",
				})
				send(map[string]any{
					"hook_event_name": "stop",
					"generation_id":   generationID,
					"model":           "gpt-5",
					"status":          "completed",
					"loop_count":      0,
					"timestamp":       at + ":03Z",
				})
			}

			send(map[string]any{
				"hook_event_name":     "sessionStart",
				"session_id":          "conv-resume",
				"is_background_agent": false,
				"composer_mode":       "agent",
				"timestamp":           "2026-10-30T09:00:00Z",
			})
			turn("gen-before-close", "list go files", "2026-10-30T09:00")
			send(map[string]any{
				"hook_event_name":     "sessionEnd",
				"session_id":          "conv-resume",
				"reason":              "window_close",
				"duration_ms":         60000,
				"is_background_agent": false,
				"final_status":        "completed",
				"timestamp":           "2026-10-30T09:01:00Z",
			})
			if tc.sessionGone {
				if err := os.RemoveAll(fragment.ConversationDir("conv-resume")); err != nil {
					t.Fatal(err)
				}
			}
			turn("gen-after-reopen", "now add tests", "2026-10-30T10:00")

			mu.Lock()
			defer mu.Unlock()
			if len(generations) != 2 {
				t.Fatalf("exported %d generations, want 2\nhook log:\n%s", len(generations), logs.String())
			}
			before, after := generations[0], generations[1]
			if before["id"] != "gen-before-close" || after["id"] != "gen-after-reopen" {
				t.Fatalf("exported ids = %v, %v", before["id"], after["id"])
			}

			want := map[string]string{
				"user_id":    "dev@example.com",
				"title":      "list go files",
				"version":    "2.0.1",
				"cwd":        workspace,
				"git.branch": "feature/resume",
			}
			if tc.autoTags {
				want["repo"] = "grafana/agento11y"
				want["user"] = "dev@example.com"
			}
			for label, gen := range map[string]map[string]any{"before close": before, "after reopen": after} {
				if label == "after reopen" {
					want["title"] = tc.wantTitle
				}
				got := exportedSessionFields(gen)
				for key, value := range want {
					if got[key] != value {
						t.Errorf("%s: %s = %q, want %q", label, key, got[key], value)
					}
				}
			}
		})
	}
}

// exportedSessionFields reads the session-scoped values a Cursor generation
// export carries: the user id and title the SDK puts in metadata, the agent
// version, and the tags.
func exportedSessionFields(gen map[string]any) map[string]string {
	out := map[string]string{}
	if metadata, ok := gen["metadata"].(map[string]any); ok {
		out["user_id"], _ = metadata["agento11y.user.id"].(string)
		out["title"], _ = metadata["agento11y.conversation.title"].(string)
	}
	out["version"], _ = gen["agent_version"].(string)
	if tags, ok := gen["tags"].(map[string]any); ok {
		for key, value := range tags {
			out[key], _ = value.(string)
		}
	}
	return out
}

// writeGitFixture creates the two files gitbranch and autotag read: the
// origin remote in the config and the checked out branch in HEAD.
func writeGitFixture(t *testing.T, root, remote, branch string) {
	t.Helper()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	config := "[remote \"origin\"]\n\turl = " + remote + "\n"
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(config), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	head := "ref: refs/heads/" + branch + "\n"
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte(head), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}
}
