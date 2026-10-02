package entry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAgentIntegration drives the real Claude Code and Codex CLIs, headless,
// against a fake model server, then checks what the plugin exported with
// TestGoldenIntegration's capture server, secret guard, and invariants. That
// test replays hook payloads written by hand; here the agent writes them, so
// an agent release that changes a payload, a transcript, or when a hook fires
// fails a run.
//
// Opt-in, since it needs the CLIs. CI installs the versions pinned in
// testdata/agent-clis:
//
//	npm ci --prefix testdata/agent-clis
//	PATH=$PWD/testdata/agent-clis/node_modules/.bin:$PATH AGENT_CLI_TESTS=claude,codex \
//	    go test . -run TestAgentIntegration -count=1 -v
//
// Each run is one prompt, one shell tool call, its result, and a final reply.
// The CLIs get a dummy key, the fake model's URL, and a throwaway HOME, so no
// real model or account is involved.
func TestAgentIntegration(t *testing.T) {
	selected := os.Getenv("AGENT_CLI_TESTS")
	if selected == "" {
		t.Skip("set AGENT_CLI_TESTS=claude,codex to drive the real agent CLIs")
	}
	// A CLI started from inside another agent session inherits that session's
	// variables, and they change the run: CLAUDE_CODE_EAGER_FLUSH alone hides
	// the Stop-before-transcript race. Start every CLI without them.
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		for _, prefix := range []string{"CLAUDE", "ANTHROPIC_", "CODEX_", "OPENAI_"} {
			if strings.HasPrefix(key, prefix) {
				t.Setenv(key, "") // restores the value after the test
				_ = os.Unsetenv(key)
			}
		}
	}

	binDir := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(binDir, "agento11y"), "github.com/grafana/agento11y/plugins/agento11y/cmd/agento11y")
	build.Env = goToolchainEnv
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build agento11y: %v\n%s", err, out)
	}
	// The hooks run `agento11y <agent> hook` from PATH.
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The repository root, from plugins/agento11y/internal/entry.
	repo, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	model := &fakeModel{}
	modelURL := newLoopbackServer(t, model).URL

	// The prompt carries a secret so the guard checks redaction end to end.
	const secret = "glc_abcdefghijklmnopqrstuvwxyz"
	prompt := "Run echo " + fakeToolOutput + " with token " + secret

	const sessionID = "7f0c9a4e-3b1d-4c2e-9f6a-2d8b5e1c7a90"
	for _, cli := range []struct {
		name     string
		sc       scenario
		commands [][]string
	}{
		{
			name: "claude",
			sc: scenario{
				Agent: "claude-code",
				Env: map[string]string{
					"ANTHROPIC_BASE_URL":                       modelURL,
					"ANTHROPIC_API_KEY":                        "test",
					"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
				},
				RawSecrets: []string{secret},
				// One generation for the tool call, one for its result and the reply.
				Invariants: scenarioInvariants{
					ExportCount: 1,
					Generations: 2,
					EveryGeneration: []string{
						`"agent_name":"claude-code"`,
						`"conversation_id":"` + sessionID + `"`,
						`"name":"` + fakeModelName + `"`,
					},
					AnyGeneration: []string{
						"with token [REDACTED:grafana-cloud-token]",
						`"tool_call":{"id":"` + fakeToolCallID + `"`,
						`"content":"` + fakeToolOutput + `"`,
						`"text":"Done."`,
					},
				},
			},
			commands: [][]string{
				{"claude", "-p", prompt, "--plugin-dir", filepath.Join(repo, "plugins", "claude-code"), "--session-id", sessionID, "--allowedTools", "Bash"},
			},
		},
		{
			name: "codex",
			sc: scenario{
				Agent:      "codex",
				Env:        map[string]string{"FAKE_MODEL_KEY": "test"},
				RawSecrets: []string{secret},
				// One generation for the whole turn. Codex picks the session ID, and
				// the tool result is exported as base64 JSON, so neither is matched.
				Invariants: scenarioInvariants{
					ExportCount: 1,
					Generations: 1,
					EveryGeneration: []string{
						`"agent_name":"codex"`,
						`"name":"` + fakeModelName + `"`,
					},
					AnyGeneration: []string{
						"with token [REDACTED:grafana-cloud-token]",
						`"tool_call":{"id":"` + fakeToolCallID + `"`,
						`"tool_call_id":"` + fakeToolCallID + `"`,
						`"text":"Done."`,
					},
				},
			},
			commands: [][]string{
				{"codex", "plugin", "marketplace", "add", repo},
				{"codex", "plugin", "add", "agento11y-codex@agento11y"},
				// Codex's own sandbox is not under test, and it cannot nest inside a
				// sandboxed CI container; the only command the fake model issues is echo.
				{"codex", "exec", "--dangerously-bypass-hook-trust", "--skip-git-repo-check", "-s", "danger-full-access",
					"-c", "model_provider=fake",
					"-c", `model_providers.fake={name="fake",base_url="` + modelURL + `/v1",wire_api="responses",env_key="FAKE_MODEL_KEY"}`,
					"-m", fakeModelName, prompt},
			},
		},
	} {
		t.Run(cli.name, func(t *testing.T) {
			if !strings.Contains(","+selected+",", ","+cli.name+",") {
				t.Skipf("%s not in AGENT_CLI_TESTS", cli.name)
			}
			stateDir, capture := startHookExport(t, cli.sc)
			workDir := t.TempDir()
			model.reset()

			var output []string
			for _, argv := range cli.commands {
				output = append(output, runAgentCommand(t, workDir, argv))
			}
			output = append(output, model.requestLog()...)
			checkExports(t, cli.name, cli.sc, stateDir, output, capture.snapshot())
		})
	}
}

// goToolchainEnv is the environment the test process started with. Package
// variables initialize before TestMain moves HOME, so building the plugin
// with it reuses the real Go module and build caches instead of downloading
// every module into the throwaway HOME.
var goToolchainEnv = os.Environ()

// runAgentCommand runs one CLI command to completion and fails the test on a
// non-zero exit. Its combined output is returned for the failure trail.
func runAgentCommand(t *testing.T, dir string, argv []string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	// The npm launchers start a native child that can outlive the killed
	// launcher and keep the output pipe open; without a delay, a timed-out
	// run would block here until the whole test binary times out.
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	trail := fmt.Sprintf("$ %s\n%s", strings.Join(argv, " "), out)
	if err != nil {
		t.Fatalf("%v\n%s", err, trail)
	}
	return trail
}

const (
	fakeModelName  = "fake-model"
	fakeToolOutput = "hello-from-fake"
	fakeToolCallID = "call_fake"
)

// fakeModel serves the Anthropic Messages and OpenAI Responses streaming
// endpoints. The first request is answered with a shell tool call; a request
// that already carries that call (it echoes the call ID back with the result)
// gets a final reply.
type fakeModel struct {
	mu       sync.Mutex
	requests []string
}

func (m *fakeModel) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = nil
}

func (m *fakeModel) requestLog() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.requests...)
}

func (m *fakeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	called := bytes.Contains(body, []byte(fakeToolCallID))
	m.mu.Lock()
	m.requests = append(m.requests, fmt.Sprintf("model %s %s called=%t body=%.300s", r.Method, r.URL.Path, called, body))
	m.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	switch r.URL.Path {
	case "/v1/messages":
		writeSSE(w, anthropicReply(called)...)
	case "/v1/responses":
		writeSSE(w, responsesReply(called)...)
	default:
		http.NotFound(w, r)
	}
}

func anthropicReply(called bool) []map[string]any {
	id, stop := "msg_tool", "tool_use"
	block := map[string]any{"type": "tool_use", "id": fakeToolCallID, "name": "Bash", "input": map[string]any{}}
	delta := map[string]any{"type": "input_json_delta", "partial_json": `{"command":"echo ` + fakeToolOutput + `"}`}
	if called {
		id, stop = "msg_reply", "end_turn"
		block = map[string]any{"type": "text", "text": ""}
		delta = map[string]any{"type": "text_delta", "text": "Done."}
	}
	return []map[string]any{
		{"type": "message_start", "message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": fakeModelName, "content": []any{},
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 1},
		}},
		{"type": "content_block_start", "index": 0, "content_block": block},
		{"type": "content_block_delta", "index": 0, "delta": delta},
		{"type": "content_block_stop", "index": 0},
		{"type": "message_delta", "delta": map[string]any{"stop_reason": stop}, "usage": map[string]any{"output_tokens": 5}},
		{"type": "message_stop"},
	}
}

func responsesReply(called bool) []map[string]any {
	item := map[string]any{"type": "function_call", "id": "fc_fake", "call_id": fakeToolCallID, "name": "exec_command",
		"arguments": `{"cmd":"echo ` + fakeToolOutput + `"}`}
	if called {
		item = map[string]any{"type": "message", "id": "msg_reply", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": "Done."}}}
	}
	return []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": "resp_fake"}},
		{"type": "response.output_item.done", "output_index": 0, "item": item},
		{"type": "response.completed", "response": map[string]any{"id": "resp_fake", "usage": map[string]any{
			"input_tokens": 10, "input_tokens_details": map[string]any{"cached_tokens": 0},
			"output_tokens": 5, "output_tokens_details": map[string]any{"reasoning_tokens": 0}, "total_tokens": 15,
		}}},
	}
}

func writeSSE(w http.ResponseWriter, events ...map[string]any) {
	for _, event := range events {
		data, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
	}
}
