package entry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	agento11yv1 "github.com/grafana/agento11y/go/proto/agento11y/v1"

	claudeuserid "github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/userid"
	codexuserid "github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex/userid"
	"github.com/grafana/agento11y/plugins/agento11y/internal/gitbranch"
)

const (
	fixtureEmail  = "agent@example.test"
	fixtureRepo   = "example/test-project"
	fixtureBranch = "integration-test"
)

func unsetTestEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		t.Setenv(key, "")
		require.NoError(t, os.Unsetenv(key))
	}
}

func clearAgentEnvironment(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		for _, prefix := range []string{"CLAUDE", "ANTHROPIC_", "CODEX_", "OPENAI_", "OTEL_", "AGENTO11Y_", "SIGIL_", "GIT_"} {
			if strings.HasPrefix(key, prefix) {
				unsetTestEnv(t, key)
				break
			}
		}
	}
}

// setupAgentMetadataFixture turns on automatic tags and returns a Git checkout
// to run in. With identity, both agents' account files carry fixtureEmail;
// without, Codex has the API-key login that codex login --with-api-key writes
// and Claude Code has no account file.
func setupAgentMetadataFixture(t *testing.T, identity bool) string {
	t.Helper()
	unsetTestEnv(t, "AGENTO11Y_USER_ID", "SIGIL_USER_ID", "AGENTO11Y_USER_ID_SOURCE", "SIGIL_USER_ID_SOURCE")
	t.Setenv("AGENTO11Y_AUTO_CODING_AGENT_TAGS", "true")
	// Codex requires every AgentIdentityAuthRecord field, even in API-key mode.
	accounts := map[string]string{
		filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json"): `{"oauthAccount":{"emailAddress":"` + fixtureEmail + `"}}`,
		filepath.Join(os.Getenv("CODEX_HOME"), "auth.json"):           `{"auth_mode":"apikey","OPENAI_API_KEY":"test","agent_identity":{"agent_runtime_id":"","agent_private_key":"","account_id":"fixture-account","chatgpt_user_id":"fixture-user","email":"` + fixtureEmail + `","plan_type":"free","chatgpt_account_is_fedramp":false}}`,
	}
	if !identity {
		accounts = map[string]string{filepath.Join(os.Getenv("CODEX_HOME"), "auth.json"): `{"auth_mode":"apikey","OPENAI_API_KEY":"test"}`}
	}
	for path, body := range accounts {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	}
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-b", fixtureBranch}, {"remote", "add", "origin", "https://github.com/" + fixtureRepo + ".git"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	return dir
}

func TestAgentScenarioIsolation(t *testing.T) {
	for _, tc := range []struct{ name, user string }{{"first", fixtureEmail}, {"second", fixtureEmail}, {"no identity", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			for _, key := range []string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_RESOURCE_ATTRIBUTES", "OTEL_TRACES_SAMPLER", "AGENTO11Y_OTEL_AUTH_TOKEN", "SIGIL_OTEL_AUTH_TOKEN", "GIT_DIR"} {
				t.Setenv(key, "inherited")
			}
			clearAgentEnvironment(t)
			for _, key := range []string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_RESOURCE_ATTRIBUTES", "OTEL_TRACES_SAMPLER", "AGENTO11Y_OTEL_AUTH_TOKEN", "SIGIL_OTEL_AUTH_TOKEN", "GIT_DIR"} {
				_, exists := os.LookupEnv(key)
				require.False(t, exists, key)
			}
			t.Setenv("USERPROFILE", t.TempDir())
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			t.Setenv("CODEX_HOME", t.TempDir())
			home, _ := startHookExport(t, scenario{})
			require.Equal(t, home, os.Getenv("USERPROFILE"))
			require.DirExists(t, os.Getenv("CODEX_HOME"))
			unsetTestEnv(t, "AGENTO11Y_USER_ID", "SIGIL_USER_ID")
			require.Empty(t, claudeuserid.Resolve())
			require.Empty(t, codexuserid.Resolve())
			dir := setupAgentMetadataFixture(t, tc.user != "")
			checkAgentAccounts(t, tc.user)
			require.Equal(t, fixtureRepo, gitbranch.Repo(dir))
			require.Equal(t, fixtureBranch, gitbranch.Resolve(dir))
		})
	}
}

func checkAgentAccounts(t *testing.T, user string) {
	t.Helper()
	require.Equal(t, user, claudeuserid.Resolve())
	require.Equal(t, user, codexuserid.Resolve())
}

// validateMetadataExports checks the generation exports of a metadata-only
// run and returns their conversation ID and generation IDs. An empty user
// means no identity resolved, so neither the user ID nor the user tag may be
// set in any form.
func validateMetadataExports(captured []capturedRequest, agent, user string) (string, map[string]bool, error) {
	// Codex reports exec_command as Bash in hook payloads.
	wantCount, provider, tool := 1, "openai", "Bash"
	if agent == "claude-code" {
		wantCount, provider, tool = 2, "anthropic", "Bash"
	}
	seen := map[string]bool{}
	conversation := ""
	calls, results := 0, 0
	for _, request := range captured {
		if request.path != "/api/v1/generations:export" {
			return "", nil, fmt.Errorf("unexpected export path %q", request.path)
		}
		var body agento11yv1.ExportGenerationsRequest
		if err := protojson.Unmarshal(request.body, &body); err != nil {
			return "", nil, fmt.Errorf("decode generation export: %w", err)
		}
		for _, g := range body.Generations {
			if g.Id == "" || seen[g.Id] || g.ConversationId == "" {
				return "", nil, fmt.Errorf("missing or duplicate generation/conversation ID")
			}
			seen[g.Id] = true
			if conversation != "" && conversation != g.ConversationId {
				return "", nil, fmt.Errorf("inconsistent conversation ID")
			}
			conversation = g.ConversationId
			if g.AgentName != agent || g.GetModel().GetName() != fakeModelName || g.GetModel().GetProvider() != provider {
				return "", nil, fmt.Errorf("incorrect agent/model/provider")
			}
			if g.StartedAt == nil || g.CompletedAt == nil || g.StartedAt.CheckValid() != nil || g.CompletedAt.CheckValid() != nil || g.CompletedAt.AsTime().Before(g.StartedAt.AsTime()) {
				return "", nil, fmt.Errorf("invalid timestamps")
			}
			input, output := int64(10), int64(5)
			if agent == "codex" {
				input, output = 20, 10 // Two fake Responses requests form one Codex turn.
			}
			if g.Usage == nil || g.Usage.InputTokens != input || g.Usage.OutputTokens != output || g.Usage.TotalTokens != input+output || g.Usage.CacheReadInputTokens != 0 || g.Usage.CacheWriteInputTokens != 0 || g.Usage.ReasoningTokens != 0 {
				return "", nil, fmt.Errorf("incorrect or missing usage: %v", g.Usage)
			}
			metadata := g.GetMetadata().GetFields()
			if id, ok := metadata["agento11y.user.id"]; ok != (user != "") || id.GetStringValue() != user {
				return "", nil, fmt.Errorf("incorrect or missing identity")
			}
			if metadata["agento11y.sdk.content_capture_mode"].GetStringValue() != "metadata_only" {
				return "", nil, fmt.Errorf("incorrect capture mode")
			}
			for key, want := range map[string]string{"user": user, "repo": fixtureRepo, "git.branch": fixtureBranch} {
				if got, ok := g.Tags[key]; ok != (want != "") || got != want {
					return "", nil, fmt.Errorf("incorrect tag %s: got %q, want %q", key, g.Tags[key], want)
				}
			}
			if g.SystemPrompt != "" || len(g.RawArtifacts) != 0 || metadata["agento11y.conversation.title"].GetStringValue() != "" {
				return "", nil, fmt.Errorf("leaked prompt/artifact/title content")
			}
			for _, definition := range g.Tools {
				if definition.Name != tool || definition.Description != "" || len(definition.InputSchemaJson) != 0 {
					return "", nil, fmt.Errorf("incorrect tool definition or leaked schema")
				}
			}
			for _, message := range slices.Concat(g.Input, g.Output) {
				for _, part := range message.Parts {
					switch payload := part.Payload.(type) {
					case *agento11yv1.Part_ToolCall:
						call := payload.ToolCall
						if call.Id != fakeToolCallID || call.Name != tool || len(call.InputJson) != 0 {
							return "", nil, fmt.Errorf("incorrect tool call or leaked tool input")
						}
						calls++
					case *agento11yv1.Part_ToolResult:
						result := payload.ToolResult
						if agent != "claude-code" || result.ToolCallId != fakeToolCallID || result.IsError || result.Content != "" || len(result.ContentJson) != 0 {
							return "", nil, fmt.Errorf("incorrect tool result or leaked tool output")
						}
						results++
					default:
						if part.GetText() != "" || part.GetThinking() != "" || part.GetMedia() != nil {
							return "", nil, fmt.Errorf("leaked message content")
						}
					}
				}
			}
		}
		for _, forbidden := range []string{"Run echo", fakeToolOutput, "Done.", "glc_abcdefghijklmnopqrstuvwxyz"} {
			if secretLeaked(request.body, forbidden) {
				return "", nil, fmt.Errorf("leaked ordinary content %q", forbidden)
			}
		}
	}
	if len(seen) != wantCount || calls != 1 || (agent == "claude-code" && results != 1) {
		return "", nil, fmt.Errorf("incorrect generation/tool structure: generations=%d calls=%d results=%d", len(seen), calls, results)
	}
	return conversation, seen, nil
}

func TestValidateMetadataExports(t *testing.T) {
	for _, agent := range []string{"claude-code", "codex"} {
		for _, tc := range []struct {
			name       string
			noIdentity bool
			mutate     func(*agento11yv1.ExportGenerationsRequest)
			wantErr    string
		}{
			{name: "valid"},
			{name: "no identity", noIdentity: true, mutate: func(r *agento11yv1.ExportGenerationsRequest) {
				for _, g := range r.Generations {
					delete(g.Metadata.Fields, "agento11y.user.id")
					delete(g.Tags, "user")
				}
			}},
			{name: "user tag without identity", noIdentity: true, mutate: func(r *agento11yv1.ExportGenerationsRequest) {
				for _, g := range r.Generations {
					delete(g.Metadata.Fields, "agento11y.user.id")
					g.Tags["user"] = "os-login"
				}
			}, wantErr: "tag user"},
			{name: "user ID without identity", noIdentity: true, mutate: func(r *agento11yv1.ExportGenerationsRequest) {
				for _, g := range r.Generations {
					delete(g.Tags, "user")
				}
			}, wantErr: "identity"},
			{name: "missing identity", mutate: func(r *agento11yv1.ExportGenerationsRequest) {
				delete(r.Generations[0].Metadata.Fields, "agento11y.user.id")
			}, wantErr: "identity"},
			{name: "wrong identity", mutate: func(r *agento11yv1.ExportGenerationsRequest) {
				r.Generations[0].Metadata.Fields["agento11y.user.id"] = structpb.NewStringValue("wrong")
			}, wantErr: "identity"},
			{name: "wrong repository", mutate: func(r *agento11yv1.ExportGenerationsRequest) { r.Generations[0].Tags["repo"] = "wrong" }, wantErr: "tag repo"},
			{name: "missing branch", mutate: func(r *agento11yv1.ExportGenerationsRequest) { delete(r.Generations[0].Tags, "git.branch") }, wantErr: "tag git.branch"},
			{name: "missing user tag", mutate: func(r *agento11yv1.ExportGenerationsRequest) { delete(r.Generations[0].Tags, "user") }, wantErr: "tag user"},
			{name: "missing usage", mutate: func(r *agento11yv1.ExportGenerationsRequest) { r.Generations[0].Usage = nil }, wantErr: "usage"},
			{name: "wrong usage", mutate: func(r *agento11yv1.ExportGenerationsRequest) { r.Generations[0].Usage.InputTokens++ }, wantErr: "usage"},
			{name: "reversed timestamps", mutate: func(r *agento11yv1.ExportGenerationsRequest) {
				r.Generations[0].CompletedAt = timestamppb.New(time.Unix(1, 0))
			}, wantErr: "timestamps"},
			{name: "missing timestamps", mutate: func(r *agento11yv1.ExportGenerationsRequest) { r.Generations[0].StartedAt = nil }, wantErr: "timestamps"},
			{name: "missing ID", mutate: func(r *agento11yv1.ExportGenerationsRequest) { r.Generations[0].Id = "" }, wantErr: "ID"},
			{name: "wrong provider", mutate: func(r *agento11yv1.ExportGenerationsRequest) { r.Generations[0].Model.Provider = "wrong" }, wantErr: "provider"},
			{name: "full capture", mutate: func(r *agento11yv1.ExportGenerationsRequest) {
				r.Generations[0].Metadata.Fields["agento11y.sdk.content_capture_mode"] = structpb.NewStringValue("full")
			}, wantErr: "capture mode"},
			{name: "prompt", mutate: func(r *agento11yv1.ExportGenerationsRequest) { r.Generations[0].SystemPrompt = "ordinary prompt" }, wantErr: "prompt/artifact/title"},
			{name: "title", mutate: func(r *agento11yv1.ExportGenerationsRequest) {
				r.Generations[0].Metadata.Fields["agento11y.conversation.title"] = structpb.NewStringValue("ordinary title")
			}, wantErr: "prompt/artifact/title"},
			{name: "artifact", mutate: func(r *agento11yv1.ExportGenerationsRequest) {
				r.Generations[0].RawArtifacts = []*agento11yv1.Artifact{{Payload: []byte("ordinary artifact")}}
			}, wantErr: "prompt/artifact/title"},
			{name: "text", mutate: func(r *agento11yv1.ExportGenerationsRequest) {
				r.Generations[0].Input = []*agento11yv1.Message{{Parts: []*agento11yv1.Part{{Payload: &agento11yv1.Part_Text{Text: "ordinary text"}}}}}
			}, wantErr: "message content"},
			{name: "thinking", mutate: func(r *agento11yv1.ExportGenerationsRequest) {
				r.Generations[0].Input = []*agento11yv1.Message{{Parts: []*agento11yv1.Part{{Payload: &agento11yv1.Part_Thinking{Thinking: "ordinary thought"}}}}}
			}, wantErr: "message content"},
			{name: "model tool definition name", mutate: func(r *agento11yv1.ExportGenerationsRequest) {
				r.Generations[0].Tools[0].Name = "exec_command"
			}, wantErr: "tool definition"},
			{name: "model tool call name", mutate: func(r *agento11yv1.ExportGenerationsRequest) {
				r.Generations[0].Output[0].Parts[0].GetToolCall().Name = "exec_command"
			}, wantErr: "tool call"},
			{name: "schema", mutate: func(r *agento11yv1.ExportGenerationsRequest) {
				r.Generations[0].Tools[0].InputSchemaJson = []byte(`{"description":"ordinary schema"}`)
			}, wantErr: "schema"},
			{name: "tool input", mutate: func(r *agento11yv1.ExportGenerationsRequest) {
				r.Generations[0].Output[0].Parts[0].GetToolCall().InputJson = []byte(`{"command":"ordinary input"}`)
			}, wantErr: "tool input"},
			{name: "tool ID", mutate: func(r *agento11yv1.ExportGenerationsRequest) {
				r.Generations[0].Output[0].Parts[0].GetToolCall().Id = "wrong"
			}, wantErr: "tool call"},
			{name: "metadata leak", mutate: func(r *agento11yv1.ExportGenerationsRequest) {
				r.Generations[0].Metadata.Fields["custom"] = structpb.NewStringValue(fakeToolOutput)
			}, wantErr: "ordinary content"},
		} {
			t.Run(agent+"/"+tc.name, func(t *testing.T) {
				r := metadataExportFixture(t, agent)
				if tc.mutate != nil {
					tc.mutate(r)
				}
				body, err := protojson.Marshal(r)
				require.NoError(t, err)
				user := fixtureEmail
				if tc.noIdentity {
					user = ""
				}
				conversation, _, err := validateMetadataExports([]capturedRequest{{path: "/api/v1/generations:export", body: body}}, agent, user)
				if tc.wantErr == "" {
					require.NoError(t, err)
					require.Equal(t, "fixture-session", conversation)
				} else {
					require.ErrorContains(t, err, tc.wantErr)
				}
			})
		}
	}
}

func metadataExportFixture(t *testing.T, agent string) *agento11yv1.ExportGenerationsRequest {
	t.Helper()
	count, provider, tool := 1, "openai", "Bash"
	input, output := int64(20), int64(10)
	if agent == "claude-code" {
		count, provider, tool, input, output = 2, "anthropic", "Bash", 10, 5
	}
	r := &agento11yv1.ExportGenerationsRequest{}
	for i := range count {
		metadata, err := structpb.NewStruct(map[string]any{"agento11y.user.id": fixtureEmail, "agento11y.sdk.content_capture_mode": "metadata_only"})
		require.NoError(t, err)
		r.Generations = append(r.Generations, &agento11yv1.Generation{
			Id: fmt.Sprintf("gen-%d", i), ConversationId: "fixture-session", AgentName: agent,
			Model:     &agento11yv1.ModelRef{Provider: provider, Name: fakeModelName},
			StartedAt: timestamppb.New(time.Unix(100, 0)), CompletedAt: timestamppb.New(time.Unix(101, 0)),
			Usage:    &agento11yv1.TokenUsage{InputTokens: input, OutputTokens: output, TotalTokens: input + output},
			Metadata: metadata, Tags: map[string]string{"user": fixtureEmail, "repo": fixtureRepo, "git.branch": fixtureBranch},
		})
	}
	r.Generations[0].Tools = []*agento11yv1.ToolDefinition{{Name: tool}}
	r.Generations[0].Output = []*agento11yv1.Message{{Parts: []*agento11yv1.Part{{Payload: &agento11yv1.Part_ToolCall{ToolCall: &agento11yv1.ToolCall{Id: fakeToolCallID, Name: tool}}}}}}
	if agent == "claude-code" {
		r.Generations[1].Input = []*agento11yv1.Message{{Parts: []*agento11yv1.Part{{Payload: &agento11yv1.Part_ToolResult{ToolResult: &agento11yv1.ToolResult{ToolCallId: fakeToolCallID}}}}}}
	}
	return r
}

func TestValidateCodexToolResult(t *testing.T) {
	for _, tc := range []struct {
		name    string
		callID  string
		content any
		isError bool
		wantErr string
	}{
		{name: "object result", callID: fakeToolCallID, content: []byte(`{"output":"hello-from-fake\n"}`)},
		{name: "string result", callID: fakeToolCallID, content: []byte(`"hello-from-fake\n"`)},
		{name: "missing content", callID: fakeToolCallID, wantErr: "missing or invalid content_json"},
		{name: "invalid content", callID: fakeToolCallID, content: []byte(fakeToolOutput), wantErr: "missing or invalid content_json"},
		{name: "invalid base64", callID: fakeToolCallID, content: "%", wantErr: "decode Codex generation"},
		{name: "wrong output", callID: fakeToolCallID, content: []byte(`{"output":"wrong"}`), wantErr: "want \"hello-from-fake\""},
		{name: "null content", callID: fakeToolCallID, content: []byte(`null`), wantErr: "want \"hello-from-fake\""},
		{name: "failed result", callID: fakeToolCallID, content: []byte(`"hello-from-fake"`), isError: true, wantErr: "reports an error"},
		{name: "unrelated result", callID: "another-call", content: []byte(`"hello-from-fake"`), wantErr: "not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			generation, err := json.Marshal(map[string]any{
				"input": []any{map[string]any{"parts": []any{
					map[string]any{"text": fakeToolOutput},
					map[string]any{"tool_result": map[string]any{
						"tool_call_id": tc.callID,
						"content_json": tc.content,
						"is_error":     tc.isError,
					}},
				}}},
				"output": []any{map[string]any{"parts": []any{
					map[string]any{"tool_call": map[string]any{
						"id": fakeToolCallID, "input_json": []byte(`{"cmd":"echo hello-from-fake"}`),
					}},
				}}},
			})
			require.NoError(t, err)
			err = validateCodexToolResult([]goldenExport{{Generations: []json.RawMessage{generation}}})
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestNewAgentCommand(t *testing.T) {
	for _, tc := range []struct {
		name     string
		role     string
		exitCode int
		cancel   bool
	}{
		{name: "success", role: "success"},
		{name: "failure", role: "failure", exitCode: 7},
		{name: "cancel descendants", role: "launcher", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			argv := []string{os.Args[0], "-test.run=^TestAgentCommandProcess$", "--", tc.role}
			var listener *net.TCPListener
			if tc.cancel {
				var err error
				listener, err = net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
				require.NoError(t, err)
				t.Cleanup(func() { _ = listener.Close() })
				require.NoError(t, listener.SetDeadline(time.Now().Add(10*time.Second)))
				argv = append(argv, listener.Addr().String())
			}
			cmd := newAgentCommand(ctx, t.TempDir(), argv)
			cmd.WaitDelay = 100 * time.Millisecond
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			require.NoError(t, cmd.Start())
			t.Cleanup(func() {
				cancel()
				if cmd.ProcessState == nil {
					_ = cmd.Wait()
				}
			})
			var connection net.Conn
			if tc.cancel {
				var err error
				connection, err = listener.Accept()
				require.NoError(t, err)
				t.Cleanup(func() { _ = connection.Close() })
				cancel()
			}
			err := cmd.Wait()
			switch {
			case tc.cancel:
				require.Error(t, err, "output: %s", output.String())
				require.ErrorIs(t, ctx.Err(), context.Canceled)
				require.NoError(t, connection.SetReadDeadline(time.Now().Add(5*time.Second)))
				_, err = connection.Read(make([]byte, 1))
				require.Error(t, err, "descendant connection must close after cancellation")
				require.False(t, os.IsTimeout(err), "descendant connection remained open after cancellation")
			case tc.exitCode != 0:
				var exitErr *exec.ExitError
				require.ErrorAs(t, err, &exitErr, "output: %s", output.String())
				require.Equal(t, tc.exitCode, exitErr.ExitCode())
			default:
				require.NoError(t, err, "output: %s", output.String())
			}
		})
	}
}

func TestAgentCommandProcess(t *testing.T) {
	index := slices.Index(os.Args, "--")
	if index < 0 {
		return
	}
	args := os.Args[index+1:]
	switch args[0] {
	case "success":
		os.Exit(0)
	case "failure":
		os.Exit(7)
	case "launcher":
		cmd := exec.Command(os.Args[0], "-test.run=^TestAgentCommandProcess$", "--", "child", args[1])
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		require.NoError(t, cmd.Run())
	case "child":
		connection, err := net.DialTimeout("tcp4", args[1], 5*time.Second)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, connection)
		_ = connection.Close()
	default:
		t.Fatalf("unknown helper role %q", args[0])
	}
}

func TestExportDiagnostics(t *testing.T) {
	if index := slices.Index(os.Args, "--export-diagnostics"); index >= 0 {
		mode := os.Args[index+1]
		stateDir := t.TempDir()
		if mode != "missing debug log" {
			logPath := filepath.Join(stateDir, "agento11y", "logs", "agento11y.log")
			require.NoError(t, os.MkdirAll(filepath.Dir(logPath), 0o700))
			require.NoError(t, os.WriteFile(logPath, []byte("synthetic plugin debug trail"), 0o600))
		}
		registerExportDiagnostics(t, stateDir, []string{"synthetic CLI output", "synthetic model request"})
		if mode == "raw validation" || mode == "missing debug log" {
			_, _, err := validateMetadataExports(nil, "claude-code", fixtureEmail)
			require.NoError(t, err)
		}
		sc := scenario{}
		if mode == "invariant validation" {
			sc.Invariants.ExportCount = 1
		}
		checkExports(t, "diagnostics", sc, nil)
		return
	}
	for _, tc := range []struct {
		name    string
		wantErr string
		wantLog bool
	}{
		{name: "raw validation", wantErr: "incorrect generation/tool structure", wantLog: true},
		{name: "missing debug log", wantErr: "incorrect generation/tool structure"},
		{name: "invariant validation", wantErr: "export request count = 0, want 1", wantLog: true},
		{name: "success"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExportDiagnostics$", "-test.v", "--", "--export-diagnostics", tc.name)
			out, err := cmd.CombinedOutput()
			if tc.wantErr == "" {
				require.NoError(t, err, "%s", out)
			} else {
				var exitErr *exec.ExitError
				require.ErrorAs(t, err, &exitErr, "%s", out)
				require.Equal(t, 1, exitErr.ExitCode(), "%s", out)
				require.Contains(t, string(out), tc.wantErr)
			}
			for _, trail := range []string{"synthetic CLI output", "synthetic model request", "synthetic plugin debug trail", "agento11y debug log ("} {
				want := tc.wantErr != ""
				if strings.Contains(trail, "debug") {
					want = tc.wantLog
				}
				if want {
					require.Contains(t, string(out), trail)
				} else {
					require.NotContains(t, string(out), trail)
				}
			}
		})
	}
}

func TestLoopbackServerCleanup(t *testing.T) {
	var server *httptest.Server
	t.Cleanup(func() {
		if server != nil {
			server.Close()
		}
	})
	t.Run("server", func(t *testing.T) {
		server = newLoopbackServer(t, http.NotFoundHandler())
	})
	if server == nil {
		t.Skip("loopback listen unavailable")
	}
	connection, err := net.DialTimeout("tcp4", server.Listener.Addr().String(), time.Second)
	if err == nil {
		_ = connection.Close()
		t.Fatal("loopback listener survived its test cleanup")
	}
}
