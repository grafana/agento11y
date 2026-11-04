package entry

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	fakeModelName     = "gpt-fake-model"
	failedCommandExit = 7
)

// providerUsage is what one fake response reports. Anthropic input excludes
// both cache buckets; Responses input already includes cached tokens.
type providerUsage struct {
	input, cacheRead, cacheWrite, output, reasoning int64
}

type shellCall struct {
	id      string
	command string
	// marker appears in the command, and in its output unless exportedResult is set.
	marker string
	// output must appear in the result the CLI returns to the model.
	output string
	failed bool
	// exportedInput and exportedResult must appear in full-capture exports.
	// Empty means marker.
	exportedInput, exportedResult string
}

func echoCall(id, marker string) shellCall {
	return shellCall{id: id, command: "echo " + marker, marker: marker, output: marker}
}

func failingCall(id, marker string) shellCall {
	return shellCall{id: id, command: fmt.Sprintf("echo %s; exit %d", marker, failedCommandExit), marker: marker, output: marker, failed: true}
}

func (c shellCall) wantInput() string  { return cmp.Or(c.exportedInput, c.marker) }
func (c shellCall) wantResult() string { return cmp.Or(c.exportedResult, c.marker) }

// modelResponse is one scripted answer. A nonzero status answers with a
// provider-shaped JSON error before any stream starts.
type modelResponse struct {
	status       int
	message      string
	calls        []shellCall
	text         string
	exportedText string
	usage        providerUsage
}

func (r modelResponse) wantText() string { return cmp.Or(r.exportedText, r.text) }

// modelTurn is one CLI invocation: a prompt and the answers it gets, in order.
type modelTurn struct {
	prompt, exportedPrompt string
	responses              []modelResponse
	// markers are safe content markers that metadata-only capture must drop,
	// beyond the prompt, its *-prompt-marker word, replies, and tool output.
	markers []string
}

func (t modelTurn) wantPrompt() string { return cmp.Or(t.exportedPrompt, t.prompt) }

// scriptedModel serves one provider's streaming endpoint from a fixed script.
// Before it answers, it checks that the request carries every earlier tool
// result plus exactly the results of the previous response, in any order.
// Anything else is recorded as an error and answered with HTTP 400.
type scriptedModel struct {
	provider string
	turns    []modelTurn

	mu       sync.Mutex
	turn     int
	step     int
	answered map[string]bool
	requests int
	log      []string
	errs     []error
}

func newScriptedModel(provider string, turns []modelTurn) *scriptedModel {
	return &scriptedModel{provider: provider, turns: turns, answered: map[string]bool{}}
}

func (m *scriptedModel) path() string {
	if m.provider == "anthropic" {
		return "/v1/messages"
	}
	return "/v1/responses"
}

func (m *scriptedModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.Method != http.MethodPost || r.URL.Path != m.path() {
		m.log = append(m.log, fmt.Sprintf("model %s %s: not served", r.Method, r.URL.Path))
		http.NotFound(w, r)
		return
	}
	m.requests++
	m.log = append(m.log, fmt.Sprintf("model request %d (turn %d, step %d): %.400s", m.requests, m.turn, m.step, body))
	response, err := m.next(body)
	if err != nil {
		m.errs = append(m.errs, fmt.Errorf("model request %d: %w", m.requests, err))
		writeProviderError(w, m.provider, http.StatusBadRequest, "unexpected request: "+err.Error())
		return
	}
	id := fmt.Sprintf("fake_%d", m.requests)
	if response.status != 0 {
		writeProviderError(w, m.provider, response.status, response.message)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	if m.provider == "anthropic" {
		// Claude groups a response's transcript fragments by request ID.
		w.Header().Set("request-id", "req_"+id)
		writeSSE(w, anthropicEvents(response, "msg_"+id)...)
		return
	}
	writeSSE(w, responsesEvents(response, "resp_"+id)...)
}

func (m *scriptedModel) next(body []byte) (modelResponse, error) {
	if m.turn >= len(m.turns) {
		return modelResponse{}, errors.New("request after the script ended")
	}
	request, err := parseProviderRequest(m.provider, body)
	if err != nil {
		return modelResponse{}, err
	}
	if !request.stream {
		return modelResponse{}, errors.New("request is not streaming")
	}
	turn := m.turns[m.turn]
	var previous *modelResponse
	for i := m.step - 1; i >= 0; i-- {
		if turn.responses[i].status == 0 {
			previous = &turn.responses[i]
			break
		}
	}
	for id := range m.answered {
		if _, ok := request.results[id]; !ok {
			return modelResponse{}, fmt.Errorf("history lost the result of %q", id)
		}
	}
	var fresh []string
	for id := range request.results {
		if !request.calls[id] {
			return modelResponse{}, fmt.Errorf("result %q has no tool call", id)
		}
		if !m.answered[id] {
			fresh = append(fresh, id)
		}
	}
	var want []shellCall
	if previous != nil {
		want = previous.calls
	}
	if err := checkFreshResults(fresh, want, request, m.provider); err != nil {
		return modelResponse{}, err
	}
	if previous == nil {
		for _, earlier := range m.turns[:m.turn+1] {
			if !slices.ContainsFunc(request.userTexts, func(text string) bool { return strings.Contains(text, earlier.prompt) }) {
				return modelResponse{}, fmt.Errorf("no user prompt %q", earlier.prompt)
			}
		}
	}
	response := turn.responses[m.step]
	if response.status == 0 {
		for _, id := range fresh {
			m.answered[id] = true
		}
	}
	m.step++
	if m.step == len(turn.responses) {
		m.turn, m.step = m.turn+1, 0
	}
	return response, nil
}

func checkFreshResults(fresh []string, want []shellCall, request providerRequest, provider string) error {
	if err := sameIDs("new tool results", fresh, want); err != nil {
		return err
	}
	for _, call := range want {
		result := request.results[call.id]
		if !strings.Contains(result.text, call.output) {
			return fmt.Errorf("result %q = %q, want %q", call.id, result.text, call.output)
		}
		// Codex reports a failed command only in its output text.
		if provider == "anthropic" && result.isError != call.failed {
			return fmt.Errorf("result %q is_error = %t, want %t", call.id, result.isError, call.failed)
		}
	}
	return nil
}

// verify reports recorded errors and checks that exactly the first turns of
// the script were consumed.
func (m *scriptedModel) verify(turns int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := errors.Join(m.errs...); err != nil {
		return err
	}
	if m.turn != turns || m.step != 0 {
		return fmt.Errorf("model script at turn %d step %d after %d invocation(s), want turn %d step 0", m.turn, m.step, turns, turns)
	}
	return nil
}

func (m *scriptedModel) requestCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requests
}

func (m *scriptedModel) requestLog() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.log)
}

type providerResult struct {
	text    string
	isError bool
}

type providerRequest struct {
	stream    bool
	userTexts []string
	calls     map[string]bool
	results   map[string]providerResult
}

func parseProviderRequest(provider string, body []byte) (providerRequest, error) {
	request := providerRequest{calls: map[string]bool{}, results: map[string]providerResult{}}
	addResult := func(id string, result providerResult) error {
		if _, ok := request.results[id]; ok {
			return fmt.Errorf("duplicate result %q", id)
		}
		request.results[id] = result
		return nil
	}
	if provider == "anthropic" {
		var wire struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &wire); err != nil {
			return request, fmt.Errorf("decode Messages request: %w", err)
		}
		request.stream = wire.Stream
		for _, message := range wire.Messages {
			var text string
			if json.Unmarshal(message.Content, &text) == nil {
				if message.Role == "user" {
					request.userTexts = append(request.userTexts, text)
				}
				continue
			}
			var blocks []struct {
				Type      string          `json:"type"`
				Text      string          `json:"text"`
				ID        string          `json:"id"`
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
				IsError   bool            `json:"is_error"`
			}
			if err := json.Unmarshal(message.Content, &blocks); err != nil {
				return request, fmt.Errorf("decode %s content: %w", message.Role, err)
			}
			for _, block := range blocks {
				switch {
				case block.Type == "text" && message.Role == "user":
					request.userTexts = append(request.userTexts, block.Text)
				case block.Type == "tool_use" && message.Role == "assistant":
					request.calls[block.ID] = true
				case block.Type == "tool_result" && message.Role == "user":
					if err := addResult(block.ToolUseID, providerResult{text: contentText(block.Content), isError: block.IsError}); err != nil {
						return request, err
					}
				}
			}
		}
		return request, nil
	}
	var wire struct {
		Stream bool `json:"stream"`
		Input  []struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
			CallID  string          `json:"call_id"`
			Output  json.RawMessage `json:"output"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return request, fmt.Errorf("decode Responses request: %w", err)
	}
	request.stream = wire.Stream
	for _, item := range wire.Input {
		switch item.Type {
		case "message":
			if item.Role == "user" {
				request.userTexts = append(request.userTexts, contentText(item.Content))
			}
		case "function_call":
			request.calls[item.CallID] = true
		case "function_call_output":
			if err := addResult(item.CallID, providerResult{text: contentText(item.Output)}); err != nil {
				return request, err
			}
		}
	}
	return request, nil
}

// contentText flattens a string or a list of text parts.
func contentText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	texts := make([]string, len(parts))
	for i, part := range parts {
		texts[i] = part.Text
	}
	return strings.Join(texts, "\n")
}

func anthropicEvents(response modelResponse, messageID string) []map[string]any {
	u := response.usage
	usage := func(output int64) map[string]any {
		return map[string]any{
			"input_tokens": u.input, "cache_read_input_tokens": u.cacheRead,
			"cache_creation_input_tokens": u.cacheWrite, "output_tokens": output,
		}
	}
	stop := "end_turn"
	if len(response.calls) > 0 {
		stop = "tool_use"
	}
	events := []map[string]any{{"type": "message_start", "message": map[string]any{
		"id": messageID, "type": "message", "role": "assistant", "model": fakeModelName, "content": []any{},
		// The final message_delta counter replaces this one.
		"usage": usage(1),
	}}}
	block := func(index int, start, delta map[string]any) {
		events = append(events,
			map[string]any{"type": "content_block_start", "index": index, "content_block": start},
			map[string]any{"type": "content_block_delta", "index": index, "delta": delta},
			map[string]any{"type": "content_block_stop", "index": index},
		)
	}
	index := 0
	if response.text != "" {
		block(index, map[string]any{"type": "text", "text": ""}, map[string]any{"type": "text_delta", "text": response.text})
		index++
	}
	for _, call := range response.calls {
		input, _ := json.Marshal(map[string]string{"command": call.command})
		block(index,
			map[string]any{"type": "tool_use", "id": call.id, "name": "Bash", "input": map[string]any{}},
			map[string]any{"type": "input_json_delta", "partial_json": string(input)})
		index++
	}
	return append(events,
		map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop}, "usage": usage(u.output)},
		map[string]any{"type": "message_stop"},
	)
}

func responsesEvents(response modelResponse, responseID string) []map[string]any {
	var items []map[string]any
	if response.text != "" {
		items = append(items, map[string]any{"type": "message", "id": "msg_" + responseID, "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": response.text}}})
	}
	for _, call := range response.calls {
		arguments, _ := json.Marshal(map[string]string{"cmd": call.command})
		items = append(items, map[string]any{"type": "function_call", "id": "fc_" + call.id, "call_id": call.id,
			"name": "exec_command", "arguments": string(arguments)})
	}
	events := []map[string]any{{"type": "response.created", "response": map[string]any{"id": responseID}}}
	for i, item := range items {
		events = append(events, map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
	}
	u := response.usage
	return append(events, map[string]any{"type": "response.completed", "response": map[string]any{"id": responseID, "usage": map[string]any{
		"input_tokens": u.input, "input_tokens_details": map[string]any{"cached_tokens": u.cacheRead},
		"output_tokens": u.output, "output_tokens_details": map[string]any{"reasoning_tokens": u.reasoning},
		"total_tokens": u.input + u.output,
	}}})
}

func writeSSE(w http.ResponseWriter, events ...map[string]any) {
	for _, event := range events {
		data, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
	}
}

func writeProviderError(w http.ResponseWriter, provider string, status int, message string) {
	var body map[string]any
	if provider == "anthropic" {
		kind := "invalid_request_error"
		if status >= 500 {
			kind = "api_error"
		}
		body = map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": message}}
	} else {
		kind := "invalid_request_error"
		if status >= 500 {
			kind = "server_error"
		}
		body = map[string]any{"error": map[string]any{"type": kind, "message": message, "param": nil, "code": nil}}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func anthropicRequestFixture(prompts []string, rounds [][]shellCall, results []shellCall, isError map[string]bool) string {
	messages := []any{}
	for _, prompt := range prompts {
		messages = append(messages, map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": prompt}}})
	}
	for _, round := range rounds {
		var uses []any
		for _, call := range round {
			uses = append(uses, map[string]any{"type": "tool_use", "id": call.id, "name": "Bash", "input": map[string]any{"command": call.command}})
		}
		messages = append(messages, map[string]any{"role": "assistant", "content": uses})
	}
	var outputs []any
	for _, call := range results {
		outputs = append(outputs, map[string]any{"type": "tool_result", "tool_use_id": call.id, "content": call.output, "is_error": isError[call.id]})
	}
	if len(outputs) > 0 {
		messages = append(messages, map[string]any{"role": "user", "content": outputs})
	}
	// Claude Code also sends system-role messages.
	messages = append(messages, map[string]any{"role": "system", "content": "reminder"})
	body, _ := json.Marshal(map[string]any{"model": "claude", "stream": true, "messages": messages})
	return string(body)
}

func responsesRequestFixture(prompt string, calls, results []shellCall) string {
	input := []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": prompt}}}}
	for _, call := range calls {
		input = append(input, map[string]any{"type": "function_call", "call_id": call.id, "name": "exec_command", "arguments": "{}"})
	}
	for _, call := range results {
		input = append(input, map[string]any{"type": "function_call_output", "call_id": call.id, "output": "Process exited with code 0\nOutput:\n" + call.output})
	}
	body, _ := json.Marshal(map[string]any{"model": fakeModelName, "stream": true, "input": input})
	return string(body)
}

func TestScriptedModel(t *testing.T) {
	a, b, c := echoCall("call_a", "out-a"), echoCall("call_b", "out-b"), echoCall("call_c", "out-c")
	failed := failingCall("call_f", "out-f")
	rounds := []modelTurn{{prompt: "first prompt", responses: []modelResponse{
		{calls: []shellCall{a, b}}, {calls: []shellCall{c}}, {text: "done"},
	}}}
	type step struct {
		body       string
		wantStatus int
	}
	for _, tc := range []struct {
		name     string
		provider string
		turns    []modelTurn
		steps    []step
		verify   int
		wantErr  string
	}{
		{name: "anthropic rounds with reordered results", provider: "anthropic", turns: rounds, verify: 1, steps: []step{
			{body: anthropicRequestFixture([]string{"first prompt"}, nil, nil, nil)},
			{body: anthropicRequestFixture([]string{"first prompt"}, [][]shellCall{{b, a}}, []shellCall{a, b}, nil)},
			{body: anthropicRequestFixture([]string{"first prompt"}, [][]shellCall{{a, b}, {c}}, []shellCall{a, b, c}, nil)},
		}},
		{name: "responses rounds", provider: "openai", turns: rounds, verify: 1, steps: []step{
			{body: responsesRequestFixture("first prompt", nil, nil)},
			{body: responsesRequestFixture("first prompt", []shellCall{a, b}, []shellCall{b, a})},
			{body: responsesRequestFixture("first prompt", []shellCall{a, b, c}, []shellCall{a, b, c})},
		}},
		{name: "missing result", provider: "openai", turns: rounds, wantErr: "new tool results [call_a], want [call_a call_b]", steps: []step{
			{body: responsesRequestFixture("first prompt", nil, nil)},
			{body: responsesRequestFixture("first prompt", []shellCall{a, b}, []shellCall{a}), wantStatus: 400},
		}},
		{name: "unrelated result", provider: "openai", turns: rounds, wantErr: "want [call_a call_b]", steps: []step{
			{body: responsesRequestFixture("first prompt", nil, nil)},
			{body: responsesRequestFixture("first prompt", []shellCall{a, b, c}, []shellCall{a, b, c}), wantStatus: 400},
		}},
		{name: "result without call", provider: "openai", turns: rounds, wantErr: `result "call_b" has no tool call`, steps: []step{
			{body: responsesRequestFixture("first prompt", nil, nil)},
			{body: responsesRequestFixture("first prompt", []shellCall{a}, []shellCall{a, b}), wantStatus: 400},
		}},
		{name: "lost history", provider: "openai", turns: rounds, wantErr: `history lost the result of "call_a"`, steps: []step{
			{body: responsesRequestFixture("first prompt", nil, nil)},
			{body: responsesRequestFixture("first prompt", []shellCall{a, b}, []shellCall{a, b})},
			{body: responsesRequestFixture("first prompt", []shellCall{b, c}, []shellCall{b, c}), wantStatus: 400},
		}},
		{name: "wrong output", provider: "anthropic", turns: rounds, wantErr: `want "out-b"`, steps: []step{
			{body: anthropicRequestFixture([]string{"first prompt"}, nil, nil, nil)},
			{body: anthropicRequestFixture([]string{"first prompt"}, [][]shellCall{{a, {id: "call_b", output: "wrong"}}}, []shellCall{a, {id: "call_b", output: "wrong"}}, nil), wantStatus: 400},
		}},
		{name: "missing prompt", provider: "anthropic", turns: rounds, wantErr: "no user prompt", steps: []step{
			{body: anthropicRequestFixture([]string{"other prompt"}, nil, nil, nil), wantStatus: 400},
		}},
		{name: "not streaming", provider: "openai", turns: rounds, wantErr: "not streaming", steps: []step{
			{body: `{"stream":false,"input":[]}`, wantStatus: 400},
		}},
		{name: "request after script", provider: "openai", turns: []modelTurn{{prompt: "p", responses: []modelResponse{{text: "done"}}}}, wantErr: "after the script ended", steps: []step{
			{body: responsesRequestFixture("p", nil, nil)},
			{body: responsesRequestFixture("p", nil, nil), wantStatus: 400},
		}},
		{name: "unconsumed script", provider: "openai", turns: rounds, verify: 1, wantErr: "turn 0 step 1", steps: []step{
			{body: responsesRequestFixture("first prompt", nil, nil)},
		}},
		{name: "failed result", provider: "anthropic", verify: 1, turns: []modelTurn{{prompt: "p", responses: []modelResponse{{calls: []shellCall{failed}}, {text: "done"}}}}, steps: []step{
			{body: anthropicRequestFixture([]string{"p"}, nil, nil, nil)},
			{body: anthropicRequestFixture([]string{"p"}, [][]shellCall{{failed}}, []shellCall{failed}, map[string]bool{"call_f": true})},
		}},
		{name: "failed result without is_error", provider: "anthropic", turns: []modelTurn{{prompt: "p", responses: []modelResponse{{calls: []shellCall{failed}}, {text: "done"}}}}, wantErr: "is_error = false, want true", steps: []step{
			{body: anthropicRequestFixture([]string{"p"}, nil, nil, nil)},
			{body: anthropicRequestFixture([]string{"p"}, [][]shellCall{{failed}}, []shellCall{failed}, nil), wantStatus: 400},
		}},
		{name: "transient error then retry", provider: "openai", verify: 1, turns: []modelTurn{{prompt: "p", responses: []modelResponse{{status: 503, message: "busy"}, {text: "done"}}}}, steps: []step{
			{body: responsesRequestFixture("p", nil, nil), wantStatus: 503},
			{body: responsesRequestFixture("p", nil, nil)},
		}},
		{name: "resumed turn keeps history", provider: "anthropic", verify: 2, turns: []modelTurn{
			{prompt: "first prompt", responses: []modelResponse{{calls: []shellCall{a}}, {text: "done"}}},
			{prompt: "second prompt", responses: []modelResponse{{text: "done again"}}},
		}, steps: []step{
			{body: anthropicRequestFixture([]string{"first prompt"}, nil, nil, nil)},
			{body: anthropicRequestFixture([]string{"first prompt"}, [][]shellCall{{a}}, []shellCall{a}, nil)},
			{body: anthropicRequestFixture([]string{"first prompt", "second prompt"}, [][]shellCall{{a}}, []shellCall{a}, nil)},
		}},
		{name: "resumed turn without earlier prompt", provider: "anthropic", turns: []modelTurn{
			{prompt: "first prompt", responses: []modelResponse{{text: "done"}}},
			{prompt: "second prompt", responses: []modelResponse{{text: "done again"}}},
		}, wantErr: `no user prompt "first prompt"`, steps: []step{
			{body: anthropicRequestFixture([]string{"first prompt"}, nil, nil, nil)},
			{body: anthropicRequestFixture([]string{"second prompt"}, nil, nil, nil), wantStatus: 400},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := newScriptedModel(tc.provider, tc.turns)
			for i, s := range tc.steps {
				recorder := httptest.NewRecorder()
				model.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, model.path(), strings.NewReader(s.body)))
				want := cmp.Or(s.wantStatus, http.StatusOK)
				require.Equal(t, want, recorder.Code, "step %d: %s", i, recorder.Body)
			}
			require.Equal(t, len(tc.steps), model.requestCount())
			err := model.verify(tc.verify)
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
			}
		})
	}
}

// TestScriptedModelResponses checks the rendered streams and errors that the
// real clients parse.
func TestScriptedModelResponses(t *testing.T) {
	a, b := echoCall("call_a", "out-a"), echoCall("call_b", "out-b")
	turns := []modelTurn{{prompt: "p", responses: []modelResponse{
		{calls: []shellCall{a, b}, usage: providerUsage{input: 11, cacheRead: 3, cacheWrite: 2, output: 5, reasoning: 1}},
		{status: 503, message: "fixture busy"},
		{text: "done"},
	}}}
	for _, provider := range []string{"anthropic", "openai"} {
		t.Run(provider, func(t *testing.T) {
			model := newScriptedModel(provider, turns)
			serve := func(body string) *httptest.ResponseRecorder {
				recorder := httptest.NewRecorder()
				model.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, model.path(), strings.NewReader(body)))
				return recorder
			}
			first, second, third := responsesRequestFixture("p", nil, nil), responsesRequestFixture("p", []shellCall{a, b}, []shellCall{a, b}), ""
			if provider == "anthropic" {
				first = anthropicRequestFixture([]string{"p"}, nil, nil, nil)
				second = anthropicRequestFixture([]string{"p"}, [][]shellCall{{a, b}}, []shellCall{a, b}, nil)
			}
			third = second

			calls := serve(first)
			require.Equal(t, http.StatusOK, calls.Code)
			busy := serve(second)
			require.Equal(t, http.StatusServiceUnavailable, busy.Code)
			require.Contains(t, busy.Body.String(), "fixture busy")
			reply := serve(third)
			require.Equal(t, http.StatusOK, reply.Code)
			require.NoError(t, model.verify(1))

			stream := calls.Body.String()
			for _, want := range []string{`"call_a"`, `"call_b"`, `echo out-a`, `echo out-b`} {
				require.Contains(t, stream, want)
			}
			require.Contains(t, reply.Body.String(), `"done"`)
			if provider == "anthropic" {
				require.NotEqual(t, calls.Header().Get("request-id"), reply.Header().Get("request-id"))
				require.NotEmpty(t, calls.Header().Get("request-id"))
				require.Contains(t, stream, `"index":1`)
				require.Contains(t, stream, `"cache_creation_input_tokens":2`)
				require.Contains(t, stream, `"stop_reason":"tool_use"`)
				require.Contains(t, busy.Body.String(), `"api_error"`)
			} else {
				require.Contains(t, stream, `"output_index":1`)
				require.Contains(t, stream, `"reasoning_tokens":1`)
				require.Contains(t, stream, `"total_tokens":16`)
				require.Contains(t, busy.Body.String(), `"server_error"`)
			}
		})
	}
}

// Dummy tokens that match redaction/patterns.json, one per captured field.
const (
	baselineSecret      = "glc_abcdefghijklmnopqrstuvwxyz"
	secretArgumentToken = "glc_argumentTOKEN0123456789abcdef"
	secretOutputToken   = "glsa_outputTOKEN0123456789abcdef"
	secretReplyToken    = "glc_replyTOKEN0123456789abcdef"
)

var agentCaseSecrets = []string{baselineSecret, secretArgumentToken, secretOutputToken, secretReplyToken}

var agentCaseNames = []string{"baseline", "text_only", "multi_tool", "failed_command", "nonzero_usage", "resume", "secrets", "model_error_503", "model_error_400"}

// agentCaseTurns returns the model script of one case. provider selects the
// usage fixtures, since Anthropic reports fresh input and cache writes and
// Responses reports inclusive input and reasoning.
func agentCaseTurns(name, provider string) []modelTurn {
	usage := func(anthropic, responses providerUsage) providerUsage {
		if provider == "anthropic" {
			return anthropic
		}
		return responses
	}
	usageTurn := modelTurn{prompt: "Run the nonzero-usage-prompt-marker command", responses: []modelResponse{
		{calls: []shellCall{echoCall("call_usage", "usage-output-marker")}, usage: usage(
			providerUsage{input: 11, cacheRead: 3, cacheWrite: 2, output: 5},
			providerUsage{input: 13, cacheRead: 2, output: 7, reasoning: 1})},
		{text: "usage-reply-marker", usage: usage(
			providerUsage{input: 17, cacheRead: 7, cacheWrite: 4, output: 9},
			providerUsage{input: 19, cacheRead: 5, output: 11, reasoning: 3})},
	}}
	switch name {
	case "baseline":
		// The prompt carries a secret so the guard checks redaction end to end.
		return []modelTurn{{prompt: "Run echo " + fakeToolOutput + " with token " + baselineSecret, markers: []string{"Run echo"}, exportedPrompt: "with token [REDACTED:grafana-cloud-token]", responses: []modelResponse{
			{calls: []shellCall{echoCall(fakeToolCallID, fakeToolOutput)}, usage: providerUsage{input: 10, output: 5}},
			{text: "Done.", usage: providerUsage{input: 10, output: 5}},
		}}}
	case "text_only":
		return []modelTurn{{prompt: "Answer the text-only-prompt-marker question", responses: []modelResponse{
			{text: "text-only-reply-marker", usage: providerUsage{input: 4, output: 3}},
		}}}
	case "multi_tool":
		return []modelTurn{{prompt: "Run the multi-tool-prompt-marker commands", responses: []modelResponse{
			{calls: []shellCall{echoCall("call_multi_a", "multi-output-a"), echoCall("call_multi_b", "multi-output-b")}, usage: providerUsage{input: 5, output: 2}},
			{calls: []shellCall{echoCall("call_multi_c", "multi-output-c")}, usage: providerUsage{input: 6, output: 2}},
			{text: "multi-tool-reply-marker", usage: providerUsage{input: 7, output: 3}},
		}}}
	case "failed_command":
		return []modelTurn{{prompt: "Run the failed-command-prompt-marker commands", responses: []modelResponse{
			{calls: []shellCall{failingCall("call_fail", "failing-output-marker")}, usage: providerUsage{input: 5, output: 2}},
			{calls: []shellCall{echoCall("call_recover", "recovery-output-marker")}, usage: providerUsage{input: 6, output: 2}},
			{text: "recovered-reply-marker", usage: providerUsage{input: 7, output: 3}},
		}}}
	case "nonzero_usage":
		return []modelTurn{usageTurn}
	case "resume":
		return []modelTurn{usageTurn, {prompt: "Run the resumed-prompt-marker command", responses: []modelResponse{
			{calls: []shellCall{echoCall("call_resume", "resumed-output-marker")}, usage: usage(
				providerUsage{input: 19, cacheRead: 5, cacheWrite: 3, output: 6},
				providerUsage{input: 23, cacheRead: 6, output: 17, reasoning: 4})},
			{text: "resumed-reply-marker", usage: usage(
				providerUsage{input: 23, cacheRead: 11, cacheWrite: 1, output: 8},
				providerUsage{input: 31, cacheRead: 9, output: 29, reasoning: 8})},
		}}}
	case "secrets":
		// The quote splits the output token in the command, so each token
		// reaches exactly one captured field.
		call := shellCall{
			id:             "call_secret",
			command:        `echo secret-output-marker glsa_"` + strings.TrimPrefix(secretOutputToken, "glsa_") + `" # secret-argument-marker ` + secretArgumentToken,
			marker:         "secret-argument-marker",
			output:         "secret-output-marker " + secretOutputToken,
			exportedInput:  "secret-argument-marker [REDACTED:grafana-cloud-token]",
			exportedResult: "secret-output-marker [REDACTED:grafana-service-account-token]",
		}
		return []modelTurn{{prompt: "Run the secret-prompt-marker command", markers: []string{
			"secret-argument-marker", "secret-output-marker", "secret-reply-marker",
		}, responses: []modelResponse{
			{calls: []shellCall{call}, usage: providerUsage{input: 5, output: 2}},
			{text: "secret-reply-marker " + secretReplyToken, exportedText: "secret-reply-marker [REDACTED:grafana-cloud-token]", usage: providerUsage{input: 6, output: 3}},
		}}}
	case "model_error_503":
		return []modelTurn{{prompt: "Answer the transient-error-prompt-marker question", responses: []modelResponse{
			{status: http.StatusServiceUnavailable, message: "transient-fixture-error"},
			{text: "transient-recovered-reply-marker", usage: providerUsage{input: 4, output: 3}},
		}}}
	case "model_error_400":
		rejected := modelResponse{status: http.StatusBadRequest, message: "terminal-fixture-error"}
		responses := []modelResponse{rejected}
		if provider == "anthropic" {
			// Claude Code 2.1.310 answers an HTTP 400 with three compatibility resends:
			// without thinking.display, then without deferred tool loading, then without
			// thinking.display again. CLAUDE_CODE_MAX_RETRIES=0 does not stop them.
			responses = []modelResponse{rejected, rejected, rejected, rejected}
		}
		return []modelTurn{{prompt: "Answer the terminal-error-prompt-marker question", responses: responses}}
	}
	panic("unknown agent case " + name)
}

// codexRun is what `codex exec --json` reported on stdout.
type codexRun struct {
	thread   string
	commands []codexCommand
}

type codexCommand struct {
	command  string
	exitCode *int
}

func parseCodexEvents(stdout string) (codexRun, error) {
	var run codexRun
	for line := range strings.Lines(stdout) {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var event struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Item     struct {
				Type     string `json:"type"`
				Command  string `json:"command"`
				ExitCode *int   `json:"exit_code"`
			} `json:"item"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return run, fmt.Errorf("decode codex event %q: %w", line, err)
		}
		switch {
		case event.Type == "thread.started":
			if run.thread != "" {
				return run, errors.New("more than one thread.started event")
			}
			run.thread = event.ThreadID
		case event.Type == "item.completed" && event.Item.Type == "command_execution":
			run.commands = append(run.commands, codexCommand{command: event.Item.Command, exitCode: event.Item.ExitCode})
		}
	}
	if run.thread == "" {
		return run, errors.New("no thread.started event")
	}
	return run, nil
}

// checkCommands requires exactly one completed command per call, with the
// exit status the script expects. The Codex hook carries no exit status, so
// this is the only evidence that a failing command failed.
func (r codexRun) checkCommands(calls []shellCall) error {
	if len(r.commands) != len(calls) {
		return fmt.Errorf("codex ran %d commands, want %d", len(r.commands), len(calls))
	}
	for _, call := range calls {
		i := slices.IndexFunc(r.commands, func(c codexCommand) bool { return strings.Contains(c.command, call.marker) })
		if i < 0 {
			return fmt.Errorf("no codex command for %q", call.id)
		}
		want := 0
		if call.failed {
			want = failedCommandExit
		}
		got := r.commands[i].exitCode
		if got == nil {
			return fmt.Errorf("codex command %q has no exit code, want %d", r.commands[i].command, want)
		}
		if *got != want {
			return fmt.Errorf("codex command %q exit %d, want %d", r.commands[i].command, *got, want)
		}
	}
	return nil
}

func TestParseCodexEvents(t *testing.T) {
	failed, recovered := failingCall("call_fail", "fail-marker"), echoCall("call_ok", "ok-marker")
	event := func(command string, exit any) string {
		return `{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"/bin/zsh -lc '` + command + `'","aggregated_output":"","exit_code":` + fmt.Sprint(exit) + `,"status":"completed"}}` + "\n"
	}
	started := `{"type":"thread.started","thread_id":"thread-1"}` + "\n"
	for _, tc := range []struct {
		name, stdout string
		calls        []shellCall
		wantErr      string
	}{
		{name: "failure and recovery", calls: []shellCall{failed, recovered},
			stdout: started + "not json\n" + `{"type":"turn.started"}` + "\n" + event(failed.command, 7) + event(recovered.command, 0) + `{"type":"turn.completed","usage":{}}` + "\n"},
		{name: "text only", stdout: started},
		{name: "missing thread", stdout: event(recovered.command, 0), calls: []shellCall{recovered}, wantErr: "no thread.started"},
		{name: "two threads", stdout: started + started, wantErr: "more than one"},
		{name: "invalid JSON", stdout: started + "{\n", wantErr: "decode codex event"},
		{name: "failure exits zero", stdout: started + event(failed.command, 0), calls: []shellCall{failed}, wantErr: "exit 0, want 7"},
		{name: "success exits nonzero", stdout: started + event(recovered.command, 7), calls: []shellCall{recovered}, wantErr: "exit 7, want 0"},
		{name: "missing exit", stdout: started + event(failed.command, "null"), calls: []shellCall{failed}, wantErr: "no exit code"},
		{name: "missing command", stdout: started + event(failed.command, 7), calls: []shellCall{failed, recovered}, wantErr: "ran 1 commands, want 2"},
		{name: "unrelated command", stdout: started + event("echo other", 0), calls: []shellCall{recovered}, wantErr: `no codex command for "call_ok"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run, err := parseCodexEvents(tc.stdout)
			if err == nil {
				require.Equal(t, "thread-1", run.thread)
				err = run.checkCommands(tc.calls)
			}
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
			}
		})
	}
}
