package entry

import (
	"bytes"
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
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
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	agento11yv1 "github.com/grafana/agento11y/go/proto/agento11y/v1"

	"github.com/grafana/agento11y/go/agento11y"
	claudeuserid "github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/userid"
	codexuserid "github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex/userid"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
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

func setupAgentMetadataFixture(t *testing.T) string {
	t.Helper()
	unsetTestEnv(t, "AGENTO11Y_USER_ID", "SIGIL_USER_ID", "AGENTO11Y_USER_ID_SOURCE", "SIGIL_USER_ID_SOURCE")
	t.Setenv("AGENTO11Y_AUTO_CODING_AGENT_TAGS", "true")
	// Codex requires every AgentIdentityAuthRecord field, even in API-key mode.
	for path, body := range map[string]string{
		filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json"): `{"oauthAccount":{"emailAddress":"` + fixtureEmail + `"}}`,
		filepath.Join(os.Getenv("CODEX_HOME"), "auth.json"):           `{"auth_mode":"apikey","OPENAI_API_KEY":"test","agent_identity":{"agent_runtime_id":"","agent_private_key":"","account_id":"fixture-account","chatgpt_user_id":"fixture-user","email":"` + fixtureEmail + `","plan_type":"free","chatgpt_account_is_fedramp":false}}`,
	} {
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
	for _, name := range []string{"first", "second"} {
		t.Run(name, func(t *testing.T) {
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
			transport, err := envconfig.SelectGenerationTransportWith(envconfig.LookupEnv)
			require.NoError(t, err)
			require.Equal(t, agento11y.GenerationExportProtocolOTel, transport, "shared helper must not hide the default with forced HTTP")
			require.Equal(t, home, os.Getenv("USERPROFILE"))
			require.DirExists(t, os.Getenv("CODEX_HOME"))
			unsetTestEnv(t, "AGENTO11Y_USER_ID", "SIGIL_USER_ID")
			require.Empty(t, claudeuserid.Resolve())
			require.Empty(t, codexuserid.Resolve())
			dir := setupAgentMetadataFixture(t)
			checkAgentAccounts(t)
			require.Equal(t, fixtureRepo, gitbranch.Repo(dir))
			require.Equal(t, fixtureBranch, gitbranch.Resolve(dir))
		})
	}
}

func checkAgentAccounts(t *testing.T) {
	t.Helper()
	require.Equal(t, fixtureEmail, claudeuserid.Resolve())
	require.Equal(t, fixtureEmail, codexuserid.Resolve())
}

// wantUsage is one generation's expected usage. input includes both cache
// buckets, so total is input plus output.
type wantUsage struct {
	input, output, cacheRead, cacheWrite, reasoning int64
}

// buckets returns the nonzero token-usage metric observations by token type.
func (u wantUsage) buckets() map[string]int64 {
	out := map[string]int64{}
	for name, value := range map[string]int64{"input": u.input, "output": u.output, "cache_read": u.cacheRead, "cache_write": u.cacheWrite, "reasoning": u.reasoning} {
		if value != 0 {
			out[name] = value
		}
	}
	return out
}

// wantGeneration is one exported generation. prompt and text are checked only
// in full capture.
type wantGeneration struct {
	prompt, text   string
	calls, results []shellCall
	usage          wantUsage
}

// agentExpectation is everything one CLI case must export. An empty
// generations list means exactly zero.
type agentExpectation struct {
	agent, mode string
	// conversation is the expected conversation ID; empty accepts any one ID.
	conversation string
	generations  []wantGeneration
	// forbidden must not leave the machine in metadata-only capture.
	forbidden []string
	// secrets must not leave the machine in any capture mode.
	secrets []string
}

func (e agentExpectation) provider() string { return agentProvider(e.agent) }

func agentProvider(agent string) string {
	if agent == "claude-code" {
		return "anthropic"
	}
	return "openai"
}

// cliAgent maps an AGENT_CLI_TESTS name to its exported agent name.
func cliAgent(name string) string {
	if name == "claude" {
		return "claude-code"
	}
	return name
}

func (e agentExpectation) full() bool { return e.mode == "full" }

// exportsResults reports whether tool results are exported. Codex metadata
// capture drops whole tool-result messages.
func (e agentExpectation) exportsResults() bool { return e.full() || e.agent == "claude-code" }

// reportsFailure reports whether a failed command marks its result and tool
// span as errors. Codex's PostToolUse hook carries no exit status.
func (e agentExpectation) reportsFailure() bool { return e.agent == "claude-code" }

// expectAgent derives the generations a script produces. Claude exports one
// generation per successful model response, with input = wire input + both
// cache buckets. Codex exports one per turn that has a successful response
// and adds up that turn's usage.
func expectAgent(agent, mode string, turns []modelTurn) agentExpectation {
	want := agentExpectation{agent: agent, mode: mode}
	for _, turn := range turns {
		want.forbidden = append(want.forbidden, turn.prompt)
		for word := range strings.FieldsSeq(turn.prompt) {
			if strings.HasSuffix(word, "-prompt-marker") {
				want.forbidden = append(want.forbidden, word)
			}
		}
		want.forbidden = append(want.forbidden, turn.markers...)
		var answered []modelResponse
		for _, response := range turn.responses {
			if response.status == 0 {
				answered = append(answered, response)
			}
		}
		if len(answered) == 0 {
			continue
		}
		if agent == "codex" {
			g := wantGeneration{prompt: turn.wantPrompt(), text: answered[len(answered)-1].wantText()}
			for _, response := range answered {
				g.calls = append(g.calls, response.calls...)
				u := response.usage
				g.usage.input += u.input
				g.usage.output += u.output
				g.usage.cacheRead += u.cacheRead
				g.usage.reasoning += u.reasoning
			}
			g.results = g.calls
			want.generations = append(want.generations, g)
		} else {
			for i, response := range answered {
				u := response.usage
				g := wantGeneration{calls: response.calls, text: response.wantText(), usage: wantUsage{
					input: u.input + u.cacheRead + u.cacheWrite, output: u.output, cacheRead: u.cacheRead, cacheWrite: u.cacheWrite,
				}}
				if i == 0 {
					g.prompt = turn.wantPrompt()
				} else {
					g.results = answered[i-1].calls
				}
				want.generations = append(want.generations, g)
			}
		}
		for _, response := range answered {
			want.forbidden = append(want.forbidden, response.text)
			for _, call := range response.calls {
				want.forbidden = append(want.forbidden, call.marker, call.output)
			}
		}
	}
	want.forbidden = slices.DeleteFunc(want.forbidden, func(s string) bool { return s == "" })
	return want
}

// validateGenerationExports decodes the raw generation exports, checks them
// against want, and returns them in the order of want.generations.
func validateGenerationExports(captured []capturedRequest, want agentExpectation) ([]*agento11yv1.Generation, error) {
	if len(want.generations) == 0 && len(captured) != 0 {
		return nil, fmt.Errorf("got %d generation export requests, want 0", len(captured))
	}
	var generations []*agento11yv1.Generation
	seen := map[string]bool{}
	conversation := want.conversation
	for _, request := range captured {
		if request.path != "/api/v1/generations:export" {
			return nil, fmt.Errorf("unexpected export path %q", request.path)
		}
		for _, secret := range want.secrets {
			if secretLeaked(request.body, secret) {
				return nil, fmt.Errorf("leaked secret %q", secret)
			}
		}
		if !want.full() {
			for _, forbidden := range want.forbidden {
				if secretLeaked(request.body, forbidden) {
					return nil, fmt.Errorf("leaked ordinary content %q", forbidden)
				}
			}
		}
		var body agento11yv1.ExportGenerationsRequest
		if err := protojson.Unmarshal(request.body, &body); err != nil {
			return nil, fmt.Errorf("decode generation export: %w", err)
		}
		for _, g := range body.Generations {
			if g.Id == "" || seen[g.Id] || g.ConversationId == "" {
				return nil, fmt.Errorf("missing or duplicate generation/conversation ID")
			}
			seen[g.Id] = true
			conversation = cmp.Or(conversation, g.ConversationId)
			if g.ConversationId != conversation {
				return nil, fmt.Errorf("conversation ID %q, want %q", g.ConversationId, conversation)
			}
			if err := checkGenerationCommon(g, want); err != nil {
				return nil, fmt.Errorf("generation %s: %w", g.Id, err)
			}
			generations = append(generations, g)
		}
	}
	if len(generations) != len(want.generations) {
		return nil, fmt.Errorf("got %d generations, want %d", len(generations), len(want.generations))
	}
	calls := map[string]string{}
	for _, g := range generations {
		for _, message := range g.Output {
			for _, part := range message.Parts {
				if call := part.GetToolCall(); call != nil {
					if other, ok := calls[call.Id]; ok {
						return nil, fmt.Errorf("tool call %q in generations %s and %s", call.Id, other, g.Id)
					}
					calls[call.Id] = g.Id
				}
			}
		}
	}
	order, err := matchGenerations(want.generations, generations, func(w wantGeneration, g *agento11yv1.Generation) error {
		return checkGeneration(w, g, want)
	})
	if err != nil {
		return nil, err
	}
	ordered := make([]*agento11yv1.Generation, len(order))
	for i, j := range order {
		ordered[i] = generations[j]
	}
	return ordered, nil
}

// matchGenerations pairs each want with a distinct generation that passes
// check, independent of export order. It returns the generation index for
// each want.
func matchGenerations(wants []wantGeneration, generations []*agento11yv1.Generation, check func(wantGeneration, *agento11yv1.Generation) error) ([]int, error) {
	fits := make([][]bool, len(wants))
	reasons := make([][]error, len(wants))
	for i, w := range wants {
		fits[i] = make([]bool, len(generations))
		for j, g := range generations {
			if err := check(w, g); err != nil {
				reasons[i] = append(reasons[i], fmt.Errorf("generation %s: %w", g.Id, err))
			} else {
				fits[i][j] = true
			}
		}
	}
	owner := make([]int, len(generations))
	for j := range owner {
		owner[j] = -1
	}
	var assign func(int, []bool) bool
	assign = func(i int, visited []bool) bool {
		for j := range generations {
			if !fits[i][j] || visited[j] {
				continue
			}
			visited[j] = true
			if owner[j] < 0 || assign(owner[j], visited) {
				owner[j] = i
				return true
			}
		}
		return false
	}
	for i := range wants {
		if !assign(i, make([]bool, len(generations))) {
			return nil, fmt.Errorf("no generation matches want[%d]: %w", i, errors.Join(reasons[i]...))
		}
	}
	order := make([]int, len(wants))
	for j, i := range owner {
		order[i] = j
	}
	return order, nil
}

func checkGenerationCommon(g *agento11yv1.Generation, want agentExpectation) error {
	if g.AgentName != want.agent || g.GetModel().GetName() != fakeModelName || g.GetModel().GetProvider() != want.provider() {
		return fmt.Errorf("incorrect agent/model/provider")
	}
	if g.StartedAt == nil || g.CompletedAt == nil || g.StartedAt.CheckValid() != nil || g.CompletedAt.CheckValid() != nil || g.CompletedAt.AsTime().Before(g.StartedAt.AsTime()) {
		return fmt.Errorf("invalid timestamps")
	}
	if !validHexID(g.TraceId, 32) || !validHexID(g.SpanId, 16) {
		return fmt.Errorf("invalid trace/span ID %q/%q", g.TraceId, g.SpanId)
	}
	metadata := g.GetMetadata().GetFields()
	if metadata["agento11y.user.id"].GetStringValue() != fixtureEmail {
		return fmt.Errorf("incorrect or missing identity")
	}
	if got := metadata["agento11y.sdk.content_capture_mode"].GetStringValue(); got != want.mode {
		return fmt.Errorf("incorrect capture mode %q, want %q", got, want.mode)
	}
	for key, value := range map[string]string{"user": fixtureEmail, "repo": fixtureRepo, "git.branch": fixtureBranch} {
		if g.Tags[key] != value {
			return fmt.Errorf("incorrect tag %s: got %q, want %q", key, g.Tags[key], value)
		}
	}
	if !want.full() && (g.SystemPrompt != "" || len(g.RawArtifacts) != 0 || metadata["agento11y.conversation.title"].GetStringValue() != "") {
		return fmt.Errorf("leaked prompt/artifact/title content")
	}
	return nil
}

func validHexID(id string, length int) bool {
	raw, err := hex.DecodeString(id)
	return len(id) == length && err == nil && slices.ContainsFunc(raw, func(b byte) bool { return b != 0 })
}

// checkGeneration compares one generation with one want: usage, tool
// definitions, message roles, every call and result by ID, result status,
// and the capture mode's content policy.
func checkGeneration(w wantGeneration, g *agento11yv1.Generation, want agentExpectation) error {
	u := g.GetUsage()
	if w.usage == (wantUsage{}) {
		if u != nil && (u.InputTokens != 0 || u.OutputTokens != 0 || u.TotalTokens != 0 || u.CacheReadInputTokens != 0 || u.CacheWriteInputTokens != 0 || u.ReasoningTokens != 0 || u.InputSemantics != 0) {
			return fmt.Errorf("usage %v, want none", u)
		}
	} else if u == nil || u.InputTokens != w.usage.input || u.OutputTokens != w.usage.output || u.TotalTokens != w.usage.input+w.usage.output ||
		u.CacheReadInputTokens != w.usage.cacheRead || u.CacheWriteInputTokens != w.usage.cacheWrite || u.ReasoningTokens != w.usage.reasoning ||
		u.InputSemantics != agento11yv1.TokenInputSemantics_TOKEN_INPUT_SEMANTICS_INCLUSIVE {
		return fmt.Errorf("usage %v, want %+v inclusive", u, w.usage)
	}

	var definitions []string
	for _, definition := range g.Tools {
		definitions = append(definitions, definition.Name)
		if !want.full() && (definition.Description != "" || len(definition.InputSchemaJson) != 0) {
			return fmt.Errorf("leaked tool definition schema")
		}
	}
	var wantDefinitions []string
	if len(w.calls) > 0 {
		wantDefinitions = []string{"Bash"}
	}
	if !slices.Equal(definitions, wantDefinitions) {
		return fmt.Errorf("tool definitions %v, want %v", definitions, wantDefinitions)
	}

	byID := map[string]shellCall{}
	for _, call := range slices.Concat(w.calls, w.results) {
		byID[call.id] = call
	}
	var calls, results, prompts, texts []string
	for _, message := range g.Input {
		for _, part := range message.Parts {
			switch payload := part.Payload.(type) {
			case *agento11yv1.Part_ToolResult:
				if message.Role != agento11yv1.MessageRole_MESSAGE_ROLE_TOOL {
					return fmt.Errorf("tool result in %s message", message.Role)
				}
				result := payload.ToolResult
				results = append(results, result.ToolCallId)
				call, ok := byID[result.ToolCallId]
				if !ok {
					continue
				}
				if (!call.failed || want.reportsFailure()) && result.IsError != call.failed {
					return fmt.Errorf("tool result %q is_error = %t, want %t", call.id, result.IsError, call.failed)
				}
				if err := checkToolContent("tool result "+call.id, result.Content, result.ContentJson, call.wantResult(), want.full()); err != nil {
					return err
				}
			case *agento11yv1.Part_Text:
				if message.Role != agento11yv1.MessageRole_MESSAGE_ROLE_USER {
					return fmt.Errorf("input text in %s message", message.Role)
				}
				prompts = append(prompts, payload.Text)
			default:
				return fmt.Errorf("unexpected input part %T", payload)
			}
		}
	}
	for _, message := range g.Output {
		if message.Role != agento11yv1.MessageRole_MESSAGE_ROLE_ASSISTANT {
			return fmt.Errorf("output %s message", message.Role)
		}
		for _, part := range message.Parts {
			switch payload := part.Payload.(type) {
			case *agento11yv1.Part_ToolCall:
				call := payload.ToolCall
				calls = append(calls, call.Id)
				if call.Name != "Bash" {
					return fmt.Errorf("tool call %q name %q, want Bash", call.Id, call.Name)
				}
				if wantCall, ok := byID[call.Id]; ok {
					if err := checkToolContent("tool call "+call.Id, "", call.InputJson, wantCall.wantInput(), want.full()); err != nil {
						return err
					}
				}
			case *agento11yv1.Part_Text:
				texts = append(texts, payload.Text)
			case *agento11yv1.Part_Thinking:
				if !want.full() && payload.Thinking != "" {
					return fmt.Errorf("leaked message content")
				}
			default:
				return fmt.Errorf("unexpected output part %T", payload)
			}
		}
	}
	wantResults := w.results
	if !want.exportsResults() {
		wantResults = nil
	}
	if err := sameIDs("tool calls", calls, w.calls); err != nil {
		return err
	}
	if err := sameIDs("tool results", results, wantResults); err != nil {
		return err
	}
	if err := checkProse("prompt", prompts, w.prompt, want.full()); err != nil {
		return err
	}
	return checkProse("reply", texts, w.text, want.full())
}

func sameIDs(kind string, got []string, want []shellCall) error {
	wantIDs := make([]string, len(want))
	for i, call := range want {
		wantIDs[i] = call.id
	}
	got = slices.Clone(got)
	slices.Sort(got)
	slices.Sort(wantIDs)
	if !slices.Equal(got, wantIDs) {
		return fmt.Errorf("%s %v, want %v", kind, got, wantIDs)
	}
	return nil
}

// checkToolContent requires marker in full capture and no content otherwise.
// encoded must be valid JSON when present.
func checkToolContent(what, text string, encoded []byte, marker string, full bool) error {
	if !full {
		if text != "" || len(encoded) != 0 {
			return fmt.Errorf("%s leaked tool content", what)
		}
		return nil
	}
	if len(encoded) != 0 && !json.Valid(encoded) {
		return fmt.Errorf("%s has invalid JSON content", what)
	}
	if !strings.Contains(text+string(encoded), marker) {
		return fmt.Errorf("%s content %q%s lacks %q", what, text, encoded, marker)
	}
	return nil
}

// checkProse requires, in full capture, some nonempty text containing marker,
// or no nonempty text when marker is empty. Other modes allow only empty texts.
func checkProse(what string, texts []string, marker string, full bool) error {
	texts = slices.DeleteFunc(slices.Clone(texts), func(s string) bool { return s == "" })
	if !full {
		if len(texts) != 0 {
			return fmt.Errorf("leaked message content in %s", what)
		}
		return nil
	}
	if marker == "" {
		if len(texts) != 0 {
			return fmt.Errorf("%s %q, want none", what, texts)
		}
		return nil
	}
	if !slices.ContainsFunc(texts, func(s string) bool { return strings.Contains(s, marker) }) {
		return fmt.Errorf("%s %q lacks %q", what, texts, marker)
	}
	return nil
}

// generationExportFixture builds the export the adapters produce for want.
func generationExportFixture(want agentExpectation) *agento11yv1.ExportGenerationsRequest {
	text := func(role agento11yv1.MessageRole, s string) *agento11yv1.Message {
		return &agento11yv1.Message{Role: role, Parts: []*agento11yv1.Part{{Payload: &agento11yv1.Part_Text{Text: s}}}}
	}
	r := &agento11yv1.ExportGenerationsRequest{}
	for i, w := range want.generations {
		metadata, _ := structpb.NewStruct(map[string]any{"agento11y.user.id": fixtureEmail, "agento11y.sdk.content_capture_mode": want.mode})
		g := &agento11yv1.Generation{
			Id: fmt.Sprintf("gen-%d", i), ConversationId: cmp.Or(want.conversation, "fixture-session"), AgentName: want.agent,
			Model:   &agento11yv1.ModelRef{Provider: want.provider(), Name: fakeModelName},
			TraceId: fmt.Sprintf("%032x", 1), SpanId: fmt.Sprintf("%016x", i+1),
			StartedAt: timestamppb.New(time.Unix(100, 0)), CompletedAt: timestamppb.New(time.Unix(101, 0)),
			Metadata: metadata, Tags: map[string]string{"user": fixtureEmail, "repo": fixtureRepo, "git.branch": fixtureBranch},
		}
		if u := w.usage; u != (wantUsage{}) {
			g.Usage = &agento11yv1.TokenUsage{
				InputTokens: u.input, OutputTokens: u.output, TotalTokens: u.input + u.output,
				CacheReadInputTokens: u.cacheRead, CacheWriteInputTokens: u.cacheWrite, ReasoningTokens: u.reasoning,
				InputSemantics: agento11yv1.TokenInputSemantics_TOKEN_INPUT_SEMANTICS_INCLUSIVE,
			}
		}
		if len(w.calls) > 0 {
			g.Tools = []*agento11yv1.ToolDefinition{{Name: "Bash"}}
		}
		if want.full() && w.prompt != "" {
			g.Input = append(g.Input, text(agento11yv1.MessageRole_MESSAGE_ROLE_USER, w.prompt))
		}
		if results := w.results; len(results) > 0 && want.exportsResults() {
			message := &agento11yv1.Message{Role: agento11yv1.MessageRole_MESSAGE_ROLE_TOOL}
			for _, call := range results {
				result := &agento11yv1.ToolResult{ToolCallId: call.id, IsError: call.failed && want.reportsFailure()}
				if want.full() && want.agent == "codex" {
					result.ContentJson, _ = json.Marshal(map[string]string{"output": call.wantResult()})
				} else if want.full() {
					result.Content = call.wantResult()
				}
				message.Parts = append(message.Parts, &agento11yv1.Part{Payload: &agento11yv1.Part_ToolResult{ToolResult: result}})
			}
			g.Input = append(g.Input, message)
		}
		output := &agento11yv1.Message{Role: agento11yv1.MessageRole_MESSAGE_ROLE_ASSISTANT}
		for _, call := range w.calls {
			toolCall := &agento11yv1.ToolCall{Id: call.id, Name: "Bash"}
			if want.full() {
				toolCall.InputJson, _ = json.Marshal(map[string]string{"command": call.wantInput()})
			}
			output.Parts = append(output.Parts, &agento11yv1.Part{Payload: &agento11yv1.Part_ToolCall{ToolCall: toolCall}})
		}
		if want.full() && w.text != "" {
			output.Parts = append(output.Parts, text(agento11yv1.MessageRole_MESSAGE_ROLE_ASSISTANT, w.text).Parts...)
		}
		if len(output.Parts) > 0 {
			g.Output = []*agento11yv1.Message{output}
		}
		r.Generations = append(r.Generations, g)
	}
	return r
}

func fixtureExpectations() map[string]agentExpectation {
	out := map[string]agentExpectation{}
	for _, agent := range []string{"claude-code", "codex"} {
		provider := agentProvider(agent)
		for _, name := range agentCaseNames {
			for _, mode := range []string{"full", "metadata_only"} {
				want := expectAgent(agent, mode, agentCaseTurns(name, provider))
				want.secrets = agentCaseSecrets
				out[agent+"/"+mode+"/"+name] = want
			}
		}
	}
	return out
}

func TestValidateGenerationExports(t *testing.T) {
	expectations := fixtureExpectations()
	names := slices.Sorted(maps.Keys(expectations))
	for _, name := range names {
		t.Run("valid/"+name, func(t *testing.T) {
			want := expectations[name]
			fixture := generationExportFixture(want)
			// Export order and batching must not matter.
			slices.Reverse(fixture.Generations)
			var captured []capturedRequest
			for _, g := range fixture.Generations {
				body, err := protojson.Marshal(&agento11yv1.ExportGenerationsRequest{Generations: []*agento11yv1.Generation{g}})
				require.NoError(t, err)
				captured = append(captured, capturedRequest{path: "/api/v1/generations:export", body: body})
			}
			got, err := validateGenerationExports(captured, want)
			require.NoError(t, err)
			require.Len(t, got, len(want.generations))
			for i, g := range got {
				require.Equal(t, fmt.Sprintf("gen-%d", i), g.Id)
			}
		})
	}

	first := func(r *agento11yv1.ExportGenerationsRequest) *agento11yv1.Generation { return r.Generations[0] }
	resultOf := func(r *agento11yv1.ExportGenerationsRequest, i int) *agento11yv1.ToolResult {
		for _, message := range r.Generations[i].Input {
			for _, part := range message.Parts {
				if result := part.GetToolResult(); result != nil {
					return result
				}
			}
		}
		panic("fixture has no tool result")
	}
	callOf := func(r *agento11yv1.ExportGenerationsRequest, i int) *agento11yv1.ToolCall {
		return r.Generations[i].Output[0].Parts[0].GetToolCall()
	}
	// The generation holding the tool results: Claude's second, Codex's only.
	resultGeneration := func(agent string) int {
		if agent == "claude-code" {
			return 1
		}
		return 0
	}
	for _, tc := range []struct {
		name    string
		cases   []string // agent/mode/case keys; empty means both agents' metadata baseline
		mutate  func(*agento11yv1.ExportGenerationsRequest, agentExpectation)
		raw     func([]byte) []byte
		wantErr string
	}{
		{name: "missing identity", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			delete(first(r).Metadata.Fields, "agento11y.user.id")
		}, wantErr: "identity"},
		{name: "wrong identity", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			first(r).Metadata.Fields["agento11y.user.id"] = structpb.NewStringValue("wrong")
		}, wantErr: "identity"},
		{name: "wrong repository", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) { first(r).Tags["repo"] = "wrong" }, wantErr: "tag repo"},
		{name: "missing branch", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) { delete(first(r).Tags, "git.branch") }, wantErr: "tag git.branch"},
		{name: "missing user tag", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) { delete(first(r).Tags, "user") }, wantErr: "tag user"},
		{name: "missing usage", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) { first(r).Usage = nil }, wantErr: "usage"},
		{name: "wrong usage", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) { first(r).Usage.InputTokens++ }, wantErr: "usage"},
		{name: "wrong total", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) { first(r).Usage.TotalTokens++ }, wantErr: "usage"},
		{name: "exclusive semantics", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			first(r).Usage.InputSemantics = agento11yv1.TokenInputSemantics_TOKEN_INPUT_SEMANTICS_UNSPECIFIED
		}, wantErr: "inclusive"},
		{name: "wrong cache read", cases: []string{"claude-code/metadata_only/nonzero_usage", "codex/metadata_only/nonzero_usage"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			first(r).Usage.CacheReadInputTokens--
		}, wantErr: "usage"},
		{name: "wrong cache write", cases: []string{"claude-code/metadata_only/nonzero_usage"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			first(r).Usage.CacheWriteInputTokens = 0
		}, wantErr: "usage"},
		{name: "wrong reasoning", cases: []string{"codex/metadata_only/nonzero_usage", "claude-code/metadata_only/nonzero_usage"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			first(r).Usage.ReasoningTokens++
		}, wantErr: "usage"},
		{name: "last response usage instead of turn usage", cases: []string{"codex/metadata_only/resume"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			u := r.Generations[1].Usage
			u.InputTokens, u.CacheReadInputTokens, u.OutputTokens, u.ReasoningTokens, u.TotalTokens = 31, 9, 29, 8, 60
		}, wantErr: "usage"},
		{name: "session usage instead of turn usage", cases: []string{"codex/metadata_only/resume"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			u := r.Generations[1].Usage
			u.InputTokens, u.CacheReadInputTokens, u.OutputTokens, u.ReasoningTokens, u.TotalTokens = 86, 22, 64, 16, 150
		}, wantErr: "usage"},
		{name: "reversed timestamps", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			first(r).CompletedAt = timestamppb.New(time.Unix(1, 0))
		}, wantErr: "timestamps"},
		{name: "missing timestamps", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) { first(r).StartedAt = nil }, wantErr: "timestamps"},
		{name: "missing ID", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) { first(r).Id = "" }, wantErr: "ID"},
		{name: "duplicate ID", cases: []string{"claude-code/metadata_only/baseline"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			r.Generations[1].Id = r.Generations[0].Id
		}, wantErr: "duplicate"},
		{name: "missing span ID", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) { first(r).SpanId = "" }, wantErr: "trace/span ID"},
		{name: "zero trace ID", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			first(r).TraceId = strings.Repeat("0", 32)
		}, wantErr: "trace/span ID"},
		{name: "changed conversation", cases: []string{"claude-code/metadata_only/resume", "codex/metadata_only/resume"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			r.Generations[len(r.Generations)-1].ConversationId = "another-session"
		}, wantErr: "conversation ID"},
		{name: "wrong provider", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) { first(r).Model.Provider = "wrong" }, wantErr: "provider"},
		{name: "full capture", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			first(r).Metadata.Fields["agento11y.sdk.content_capture_mode"] = structpb.NewStringValue("full")
		}, wantErr: "capture mode"},
		{name: "metadata capture in full mode", cases: []string{"claude-code/full/baseline", "codex/full/baseline"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			first(r).Metadata.Fields["agento11y.sdk.content_capture_mode"] = structpb.NewStringValue("metadata_only")
		}, wantErr: "capture mode"},
		{name: "prompt", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			first(r).SystemPrompt = "ordinary prompt"
		}, wantErr: "prompt/artifact/title"},
		{name: "title", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			first(r).Metadata.Fields["agento11y.conversation.title"] = structpb.NewStringValue("ordinary title")
		}, wantErr: "prompt/artifact/title"},
		{name: "artifact", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			first(r).RawArtifacts = []*agento11yv1.Artifact{{Payload: []byte("ordinary artifact")}}
		}, wantErr: "prompt/artifact/title"},
		{name: "text", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			first(r).Input = []*agento11yv1.Message{{Role: agento11yv1.MessageRole_MESSAGE_ROLE_USER, Parts: []*agento11yv1.Part{{Payload: &agento11yv1.Part_Text{Text: "ordinary text"}}}}}
		}, wantErr: "message content"},
		{name: "thinking", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			first(r).Output[0].Parts = append(first(r).Output[0].Parts, &agento11yv1.Part{Payload: &agento11yv1.Part_Thinking{Thinking: "ordinary thought"}})
		}, wantErr: "message content"},
		{name: "model tool definition name", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			first(r).Tools[0].Name = "exec_command"
		}, wantErr: "tool definitions"},
		{name: "missing tool definition", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) { first(r).Tools = nil }, wantErr: "tool definitions"},
		{name: "advertised tool catalog", cases: []string{"claude-code/full/text_only", "codex/metadata_only/text_only"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			first(r).Tools = []*agento11yv1.ToolDefinition{{Name: "Bash"}}
		}, wantErr: "tool definitions"},
		{name: "model tool call name", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) { callOf(r, 0).Name = "exec_command" }, wantErr: "tool call"},
		{name: "schema", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			first(r).Tools[0].InputSchemaJson = []byte(`{"description":"ordinary schema"}`)
		}, wantErr: "schema"},
		{name: "tool input", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			callOf(r, 0).InputJson = []byte(`{"command":"ordinary input"}`)
		}, wantErr: "tool content"},
		{name: "tool ID", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) { callOf(r, 0).Id = "wrong" }, wantErr: "tool calls"},
		{name: "tool call in input role", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			first(r).Output[0].Role = agento11yv1.MessageRole_MESSAGE_ROLE_USER
		}, wantErr: "output MESSAGE_ROLE_USER message"},
		{name: "tool result role", cases: []string{"claude-code/metadata_only/baseline", "codex/full/baseline"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, want agentExpectation) {
			r.Generations[resultGeneration(want.agent)].Input[len(r.Generations[resultGeneration(want.agent)].Input)-1].Role = agento11yv1.MessageRole_MESSAGE_ROLE_USER
		}, wantErr: "tool result in MESSAGE_ROLE_USER"},
		{name: "dropped result", cases: []string{"claude-code/full/multi_tool", "codex/full/multi_tool", "claude-code/metadata_only/multi_tool"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, want agentExpectation) {
			message := r.Generations[resultGeneration(want.agent)].Input
			parts := message[len(message)-1].Parts
			message[len(message)-1].Parts = parts[1:]
		}, wantErr: "tool results"},
		{name: "unrelated result", cases: []string{"claude-code/full/multi_tool", "codex/full/multi_tool"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, want agentExpectation) {
			resultOf(r, resultGeneration(want.agent)).ToolCallId = "call_unrelated"
		}, wantErr: "tool results"},
		{name: "Codex metadata result", cases: []string{"codex/metadata_only/baseline"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			first(r).Input = []*agento11yv1.Message{{Role: agento11yv1.MessageRole_MESSAGE_ROLE_TOOL, Parts: []*agento11yv1.Part{{Payload: &agento11yv1.Part_ToolResult{ToolResult: &agento11yv1.ToolResult{ToolCallId: fakeToolCallID}}}}}}
		}, wantErr: "tool results"},
		{name: "duplicate call across generations", cases: []string{"claude-code/metadata_only/multi_tool"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			callOf(r, 1).Id = callOf(r, 0).Id
		}, wantErr: "in generations"},
		{name: "resumed generation repeats earlier call", cases: []string{"codex/metadata_only/resume"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			r.Generations[1].Output[0].Parts = append(r.Generations[1].Output[0].Parts, r.Generations[0].Output[0].Parts...)
		}, wantErr: "in generations"},
		{name: "failed result without error", cases: []string{"claude-code/full/failed_command", "claude-code/metadata_only/failed_command"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			resultOf(r, 1).IsError = false
		}, wantErr: "is_error = false, want true"},
		{name: "successful result with error", cases: []string{"claude-code/full/baseline", "codex/full/baseline", "codex/full/failed_command"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, want agentExpectation) {
			for _, message := range r.Generations[resultGeneration(want.agent)].Input {
				for _, part := range message.Parts {
					if result := part.GetToolResult(); result != nil && !strings.Contains(result.ToolCallId, "fail") {
						result.IsError = true
					}
				}
			}
		}, wantErr: "is_error = true, want false"},
		{name: "missing result content", cases: []string{"claude-code/full/baseline", "codex/full/baseline"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, want agentExpectation) {
			result := resultOf(r, resultGeneration(want.agent))
			result.Content, result.ContentJson = "", nil
		}, wantErr: "lacks"},
		{name: "invalid result JSON", cases: []string{"codex/full/baseline"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			resultOf(r, 0).ContentJson = []byte(fakeToolOutput)
		}, wantErr: "invalid JSON"},
		{name: "null result JSON", cases: []string{"codex/full/baseline"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			resultOf(r, 0).ContentJson = []byte("null")
		}, wantErr: "lacks"},
		{name: "missing reply", cases: []string{"claude-code/full/text_only", "codex/full/text_only"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			first(r).Output = nil
		}, wantErr: "reply"},
		{name: "missing prompt", cases: []string{"claude-code/full/text_only", "codex/full/text_only"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			first(r).Input = nil
		}, wantErr: "prompt"},
		{name: "missing redaction marker", cases: []string{"claude-code/full/secrets", "codex/full/secrets"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			callOf(r, 0).InputJson = []byte(`{"command":"secret-argument-marker"}`)
		}, wantErr: "lacks"},
		{name: "missing result redaction marker", cases: []string{"claude-code/full/secrets", "codex/full/secrets"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, want agentExpectation) {
			result := resultOf(r, resultGeneration(want.agent))
			result.Content, result.ContentJson = "secret-output-marker", nil
		}, wantErr: "lacks"},
		{name: "raw argument secret", cases: []string{"claude-code/full/secrets", "codex/metadata_only/secrets"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			first(r).Tags["custom"] = secretArgumentToken
		}, wantErr: "leaked secret"},
		{name: "base64 output secret", cases: []string{"claude-code/full/secrets", "codex/full/secrets"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			callOf(r, 0).InputJson = []byte(`{"command":"secret-argument-marker [REDACTED:grafana-cloud-token] ` + secretOutputToken + `"}`)
		}, wantErr: "leaked secret"},
		{name: "escaped reply secret", cases: []string{"claude-code/full/secrets"}, raw: func(body []byte) []byte {
			return bytes.Replace(body, []byte("secret-reply-marker"), []byte(`secret-reply-marker\n`+secretReplyToken), 1)
		}, wantErr: "leaked secret"},
		{name: "metadata leak", mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			first(r).Metadata.Fields["custom"] = structpb.NewStringValue(fakeToolOutput)
		}, wantErr: "ordinary content"},
		{name: "cut prompt leak", cases: []string{"claude-code/metadata_only/multi_tool", "codex/metadata_only/text_only"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, want agentExpectation) {
			words := strings.Fields(want.forbidden[0])
			first(r).Tags["custom"] = strings.Join(words[:len(words)-1], " ")
		}, wantErr: "ordinary content"},
		{name: "base64 metadata leak", cases: []string{"claude-code/metadata_only/secrets", "codex/metadata_only/secrets"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			callOf(r, 0).InputJson = []byte(`{"command":"secret-argument-marker"}`)
		}, wantErr: "ordinary content"},
		{name: "missing generation", cases: []string{"claude-code/metadata_only/multi_tool", "codex/metadata_only/resume"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			r.Generations = r.Generations[1:]
		}, wantErr: "generations, want"},
		{name: "extra generation", cases: []string{"codex/metadata_only/text_only"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			extra := proto.Clone(first(r)).(*agento11yv1.Generation)
			extra.Id = "gen-extra"
			r.Generations = append(r.Generations, extra)
		}, wantErr: "2 generations, want 1"},
		{name: "export for terminal failure", cases: []string{"claude-code/metadata_only/model_error_400", "codex/full/model_error_400"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			r.Generations = generationExportFixture(fixtureExpectations()["codex/full/baseline"]).Generations
		}, wantErr: "export requests, want 0"},
		{name: "error response usage", cases: []string{"claude-code/metadata_only/model_error_503", "codex/metadata_only/model_error_503"}, mutate: func(r *agento11yv1.ExportGenerationsRequest, _ agentExpectation) {
			first(r).Usage.OutputTokens++
			first(r).Usage.TotalTokens++
		}, wantErr: "usage"},
	} {
		cases := tc.cases
		if cases == nil {
			cases = []string{"claude-code/metadata_only/baseline", "codex/metadata_only/baseline"}
		}
		for _, key := range cases {
			t.Run(tc.name+"/"+key, func(t *testing.T) {
				want, ok := expectations[key]
				require.True(t, ok, key)
				r := generationExportFixture(want)
				if tc.mutate != nil {
					tc.mutate(r, want)
				}
				body, err := protojson.Marshal(r)
				require.NoError(t, err)
				if tc.raw != nil {
					body = tc.raw(body)
				}
				_, err = validateGenerationExports([]capturedRequest{{path: "/api/v1/generations:export", body: body}}, want)
				require.ErrorContains(t, err, tc.wantErr)
			})
		}
	}
}

func TestExpectAgent(t *testing.T) {
	type shape struct {
		calls, results []string
		usage          wantUsage
	}
	ids := func(calls []shellCall) []string {
		out := []string{}
		for _, call := range calls {
			out = append(out, call.id)
		}
		return out
	}
	for _, tc := range []struct {
		agent, name string
		want        []shape
	}{
		{agent: "claude-code", name: "text_only", want: []shape{{calls: []string{}, results: []string{}, usage: wantUsage{input: 4, output: 3}}}},
		{agent: "codex", name: "text_only", want: []shape{{calls: []string{}, results: []string{}, usage: wantUsage{input: 4, output: 3}}}},
		{agent: "claude-code", name: "multi_tool", want: []shape{
			{calls: []string{"call_multi_a", "call_multi_b"}, results: []string{}, usage: wantUsage{input: 5, output: 2}},
			{calls: []string{"call_multi_c"}, results: []string{"call_multi_a", "call_multi_b"}, usage: wantUsage{input: 6, output: 2}},
			{calls: []string{}, results: []string{"call_multi_c"}, usage: wantUsage{input: 7, output: 3}},
		}},
		{agent: "codex", name: "multi_tool", want: []shape{{
			calls: []string{"call_multi_a", "call_multi_b", "call_multi_c"}, results: []string{"call_multi_a", "call_multi_b", "call_multi_c"}, usage: wantUsage{input: 18, output: 7},
		}}},
		{agent: "claude-code", name: "nonzero_usage", want: []shape{
			{calls: []string{"call_usage"}, results: []string{}, usage: wantUsage{input: 16, output: 5, cacheRead: 3, cacheWrite: 2}},
			{calls: []string{}, results: []string{"call_usage"}, usage: wantUsage{input: 28, output: 9, cacheRead: 7, cacheWrite: 4}},
		}},
		{agent: "codex", name: "nonzero_usage", want: []shape{{calls: []string{"call_usage"}, results: []string{"call_usage"}, usage: wantUsage{input: 32, output: 18, cacheRead: 7, reasoning: 4}}}},
		// The resumed Codex generation has the second turn's usage: 54/46/100, not 31/29/60 or the 86/64/150 session total.
		{agent: "codex", name: "resume", want: []shape{
			{calls: []string{"call_usage"}, results: []string{"call_usage"}, usage: wantUsage{input: 32, output: 18, cacheRead: 7, reasoning: 4}},
			{calls: []string{"call_resume"}, results: []string{"call_resume"}, usage: wantUsage{input: 54, output: 46, cacheRead: 15, reasoning: 12}},
		}},
		{agent: "claude-code", name: "resume", want: []shape{
			{calls: []string{"call_usage"}, results: []string{}, usage: wantUsage{input: 16, output: 5, cacheRead: 3, cacheWrite: 2}},
			{calls: []string{}, results: []string{"call_usage"}, usage: wantUsage{input: 28, output: 9, cacheRead: 7, cacheWrite: 4}},
			{calls: []string{"call_resume"}, results: []string{}, usage: wantUsage{input: 27, output: 6, cacheRead: 5, cacheWrite: 3}},
			{calls: []string{}, results: []string{"call_resume"}, usage: wantUsage{input: 35, output: 8, cacheRead: 11, cacheWrite: 1}},
		}},
		{agent: "claude-code", name: "model_error_503", want: []shape{{calls: []string{}, results: []string{}, usage: wantUsage{input: 4, output: 3}}}},
		{agent: "codex", name: "model_error_503", want: []shape{{calls: []string{}, results: []string{}, usage: wantUsage{input: 4, output: 3}}}},
		{agent: "claude-code", name: "model_error_400"},
		{agent: "codex", name: "model_error_400"},
	} {
		t.Run(tc.agent+"/"+tc.name, func(t *testing.T) {
			want := expectAgent(tc.agent, "full", agentCaseTurns(tc.name, agentProvider(tc.agent)))
			var got []shape
			for _, g := range want.generations {
				got = append(got, shape{calls: ids(g.calls), results: ids(g.results), usage: g.usage})
			}
			require.Equal(t, tc.want, got)
		})
	}
	t.Run("codex resume session total", func(t *testing.T) {
		var total wantUsage
		for _, g := range expectAgent("codex", "full", agentCaseTurns("resume", "openai")).generations {
			total.input += g.usage.input
			total.output += g.usage.output
			total.cacheRead += g.usage.cacheRead
			total.reasoning += g.usage.reasoning
		}
		require.Equal(t, wantUsage{input: 86, output: 64, cacheRead: 22, reasoning: 16}, total)
		require.EqualValues(t, 150, total.input+total.output)
	})
}

func TestRunAgentProcess(t *testing.T) {
	helper := func(role string, args ...string) []string {
		return append([]string{os.Args[0], "-test.run=^TestAgentCommandProcess$", "--", role}, args...)
	}
	for _, tc := range []struct {
		name       string
		argv       []string
		cancel     bool
		exitCode   int
		stdout     string
		stderr     string
		okFailures []string
		badChecks  []string
	}{
		{name: "success", argv: helper("streams"), stdout: "helper-stdout\n", stderr: "helper-stderr\n",
			okFailures: []string{""}, badChecks: []string{"helper-stdout"}},
		{name: "expected failure", argv: helper("failure"), exitCode: 7, stderr: "helper-failure-marker\n",
			okFailures: []string{"helper-failure-marker"}, badChecks: []string{"", "other-marker"}},
		{name: "start failure", argv: []string{filepath.Join(t.TempDir(), "missing-agent")}, exitCode: -1,
			badChecks: []string{"", "missing-agent"}},
		{name: "cancel descendants", argv: helper("launcher"), cancel: true, exitCode: -1,
			badChecks: []string{"", "helper"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			argv := tc.argv
			connections := make(chan net.Conn, 1)
			if tc.cancel {
				listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
				require.NoError(t, err)
				t.Cleanup(func() { _ = listener.Close() })
				require.NoError(t, listener.SetDeadline(time.Now().Add(10*time.Second)))
				argv = append(slices.Clone(argv), listener.Addr().String())
				go func() {
					connection, err := listener.Accept()
					if err == nil {
						connections <- connection
					}
					close(connections)
					cancel()
				}()
			}
			outcome := runAgentProcess(ctx, t.TempDir(), argv)
			require.Equal(t, tc.exitCode, outcome.exitCode, outcome.trail())
			if tc.exitCode >= 0 {
				require.Equal(t, tc.stdout, outcome.stdout)
				require.Equal(t, tc.stderr, outcome.stderr)
			} else {
				require.Error(t, outcome.err)
			}
			for _, failure := range tc.okFailures {
				require.NoError(t, outcome.check(failure))
			}
			for _, failure := range tc.badChecks {
				require.Error(t, outcome.check(failure), "check(%q) must fail", failure)
			}
			if tc.cancel {
				require.ErrorIs(t, outcome.err, context.Canceled)
				connection, ok := <-connections
				require.True(t, ok, "descendant never connected")
				t.Cleanup(func() { _ = connection.Close() })
				require.NoError(t, connection.SetReadDeadline(time.Now().Add(5*time.Second)))
				_, err := connection.Read(make([]byte, 1))
				require.Error(t, err, "descendant connection must close after cancellation")
				require.False(t, os.IsTimeout(err), "descendant connection remained open after cancellation")
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
	case "streams":
		fmt.Fprintln(os.Stdout, "helper-stdout")
		fmt.Fprintln(os.Stderr, "helper-stderr")
		os.Exit(0)
	case "failure":
		fmt.Fprintln(os.Stderr, "helper-failure-marker")
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

// registerAgentDiagnostics logs the plugin debug log and trail() when the
// test fails. trail is read at cleanup, so output appended after a command
// fails is still shown.
func registerAgentDiagnostics(t *testing.T, stateDir string, trail func() []string) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		logPath := filepath.Join(stateDir, "agento11y", "logs", "agento11y.log")
		if data, err := os.ReadFile(logPath); err == nil {
			t.Logf("agento11y debug log (%s):\n%s", logPath, data)
		}
		for _, line := range trail() {
			t.Log(line)
		}
	})
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
		if mode == "command failure" {
			var trail []string
			registerAgentDiagnostics(t, stateDir, func() []string { return trail })
			outcome := runAgentProcess(t.Context(), t.TempDir(), []string{os.Args[0], "-test.run=^TestAgentCommandProcess$", "--", "failure"})
			trail = append(trail, outcome.trail(), "synthetic model request")
			require.NoError(t, outcome.check(""))
			return
		}
		registerExportDiagnostics(t, stateDir, []string{"synthetic CLI output", "synthetic model request"})
		if mode == "raw validation" || mode == "missing debug log" {
			_, err := validateGenerationExports(nil, expectAgent("claude-code", "metadata_only", agentCaseTurns("baseline", "anthropic")))
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
		{name: "raw validation", wantErr: "got 0 generations, want 2", wantLog: true},
		{name: "missing debug log", wantErr: "got 0 generations, want 2"},
		{name: "invariant validation", wantErr: "export request count = 0, want 1", wantLog: true},
		{name: "command failure", wantErr: "helper-failure-marker", wantLog: true},
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
				if trail == "synthetic CLI output" && tc.name == "command failure" {
					trail = "[exit 7"
				}
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

var agentCLINames = []string{"claude", "codex"}

// parseAgentSelection parses AGENT_CLI_TESTS. An unknown name fails, so a
// typo cannot pass by skipping every agent; an empty value selects none.
func parseAgentSelection(value string) (map[string]bool, error) {
	selected := map[string]bool{}
	if strings.TrimSpace(value) == "" {
		return selected, nil
	}
	for name := range strings.SplitSeq(value, ",") {
		name = strings.TrimSpace(name)
		if !slices.Contains(agentCLINames, name) {
			return nil, fmt.Errorf("AGENT_CLI_TESTS: unknown agent %q, want a comma-separated subset of %v", name, agentCLINames)
		}
		selected[name] = true
	}
	return selected, nil
}

func TestParseAgentSelection(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    []string
		wantErr string
	}{
		{value: ""},
		{value: "  "},
		{value: "claude", want: []string{"claude"}},
		{value: "claude, codex", want: []string{"claude", "codex"}},
		{value: "cluade", wantErr: `unknown agent "cluade"`},
		{value: "claude,", wantErr: `unknown agent ""`},
		{value: "claude,opencode", wantErr: `unknown agent "opencode"`},
	} {
		t.Run(tc.value, func(t *testing.T) {
			got, err := parseAgentSelection(tc.value)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.ElementsMatch(t, tc.want, slices.Collect(maps.Keys(got)))
		})
	}
}
