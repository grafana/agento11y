package main

// TestGoldenIntegration replays representative agent hook events through the
// consolidated `sigil <agent> hook` dispatcher and asserts that the normalized
// HTTP request bodies sent to /api/v1/generations:export match per-scenario
// golden files checked in under testdata/golden/.
//
// Scenario fixture layout
//
//	testdata/golden/<scenario>/
//	    scenario.json   - the agent, env, transcripts, ordered events and
//	                      secret-leak guards. Field reference:
//	                      {
//	                        "agent": "claude-code|codex|copilot|cursor",
//	                        "env":   {"OPTIONAL_VAR": "value"},
//	                        "transcripts": {
//	                          "NAME": ["jsonl line 1", "jsonl line 2"]
//	                        },
//	                        "events": [
//	                          {
//	                            "_env": {"SIGIL_COPILOT_HOOK_EVENT": "agentStop"},
//	                            "hook_event_name": "Stop",
//	                            ...
//	                          }
//	                        ],
//	                        "raw_secrets": ["sk-..."]
//	                      }
//	                      Each event is a JSON object that becomes the stdin
//	                      payload of one `sigil <agent> hook` invocation.
//	                      Optional `_env` is removed from stdin and applied
//	                      only while that event is replayed. The placeholder
//	                      "{{transcript:NAME}}" inside any event is replaced
//	                      with the absolute path of the named transcript file
//	                      written to a tempdir.
//	    export.golden.json - the expected normalized list of generation export
//	                         payloads, one entry per captured HTTP request.
//	                         Set UPDATE_GOLDENS=1 to regenerate.
//
// Adding a new scenario (including tool calls/results) only requires creating
// the directory, writing scenario.json with the desired events, and running
// the harness once with UPDATE_GOLDENS=1 to seed the golden.
//
// Not every event produces a generation. claude-code derives multiple
// generations from a single Stop event by replaying the transcript;
// codex/copilot/cursor accumulate state across events and emit on Stop /
// SessionEnd. Set `invariants.export_count` and `invariants.generations`
// to anchor the expected counts when those matter for the scenario.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
)

// scenario is the deserialized testdata/golden/<scenario>/scenario.json shape.
// Events are loose JSON objects so each agent can supply its own payload
// schema without leaking the schema into the harness.
type scenario struct {
	Agent       string              `json:"agent"`
	Env         map[string]string   `json:"env,omitempty"`
	Transcripts map[string][]string `json:"transcripts,omitempty"`
	// SiblingFiles is an optional {basename -> body} map of plain files
	// dropped next to every transcript so agents that read transcript
	// siblings (e.g. vibe needs meta.json beside messages.jsonl) work
	// against a realistic on-disk layout.
	SiblingFiles map[string]string            `json:"sibling_files,omitempty"`
	Events       []map[string]json.RawMessage `json:"events"`
	RawSecrets   []string                     `json:"raw_secrets,omitempty"`
	// Invariants documents the per-scenario field invariants asserted in
	// addition to the golden comparison. Only the subset of fields each
	// scenario needs to anchor is listed.
	Invariants scenarioInvariants `json:"invariants"`
}

// scenarioInvariants documents the fields each scenario asserts on every
// captured generation export request body in addition to the golden diff.
type scenarioInvariants struct {
	// ExportCount is the expected number of export requests. Zero means
	// "no assertion" — useful for scenarios that only verify side effects.
	ExportCount int `json:"export_count"`
	// Generations is the expected number of Generation entries across all
	// captured requests (sum). Zero means "no assertion".
	Generations int `json:"generations"`
	// AnyGeneration must hold true on at least one captured generation;
	// each entry is checked as a substring on the canonical JSON of every
	// generation in turn. Useful for invariants like agent_name, model.
	AnyGeneration []string `json:"any_generation,omitempty"`
	// EveryGeneration must hold on every captured generation.
	EveryGeneration []string `json:"every_generation,omitempty"`
	// ExportPath is the expected URL path. Defaults to
	// /api/v1/generations:export when empty.
	ExportPath string `json:"export_path,omitempty"`
}

// goldenExport is the normalized payload written to export.golden.json. We
// keep generations as raw JSON so the golden file is human-readable and the
// comparison is structural rather than string-based.
type goldenExport struct {
	Path        string            `json:"path"`
	Generations []json.RawMessage `json:"generations"`
}

func TestGoldenIntegration(t *testing.T) {
	for _, name := range goldenScenarioNames(t) {
		t.Run(name, func(t *testing.T) {
			runGoldenScenario(t, name)
		})
	}
}

func goldenScenarioNames(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join("testdata", "golden")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read golden scenarios: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name(), "scenario.json")
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatalf("no golden scenarios found under %s", dir)
	}
	return names
}

func runGoldenScenario(t *testing.T, name string) {
	t.Helper()

	dir := filepath.Join("testdata", "golden", name)
	sc := loadScenario(t, dir)

	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)
	t.Setenv("HOME", stateDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(stateDir, "config"))

	transcriptPaths := writeTranscripts(t, sc.Transcripts, sc.SiblingFiles)

	capture := &exportCapture{}
	server := newGoldenServer(t, capture)
	defer server.Close()

	setHookExportEnv(t, server.URL)
	for k, v := range sc.Env {
		t.Setenv(k, v)
	}

	eventOutput := make([]string, 0, len(sc.Events))
	for i, raw := range sc.Events {
		eventEnv, payload := splitEventEnv(t, raw)
		evt := substituteTranscriptPaths(t, payload, transcriptPaths)
		var stdoutText, stderrText string
		func() {
			restoreEnv := applyEventEnv(eventEnv)
			defer restoreEnv()
			stdoutText, stderrText = runHookEvent(t, sc.Agent, evt, i)
		}()
		eventOutput = append(eventOutput, fmt.Sprintf("event[%d] stdout=%q stderr=%q", i, stdoutText, stderrText))
	}

	captured := capture.snapshot()

	// On failure, surface the SIGIL_DEBUG log file and per-event hook output
	// so the developer can see which dispatch path each event took without
	// having to re-run by hand.
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		logPath := filepath.Join(stateDir, "sigil", "logs", "sigil.log")
		if data, err := os.ReadFile(logPath); err == nil {
			t.Logf("sigil debug log (%s):\n%s", logPath, data)
		}
		for _, line := range eventOutput {
			t.Log(line)
		}
	})

	bodies := normalizeCapturedBodies(t, captured)

	// Secret-leak guard runs against the raw captured bodies so any
	// normalization that drops or rewrites content cannot hide a leak.
	for _, secret := range sc.RawSecrets {
		for i, cap := range captured {
			if strings.Contains(string(cap.body), secret) {
				t.Fatalf("scenario %s: raw secret %q leaked in export request[%d]: %s",
					name, secret, i, cap.body)
			}
		}
	}

	wantPath := sc.Invariants.ExportPath
	if wantPath == "" {
		wantPath = "/api/v1/generations:export"
	}
	if sc.Invariants.ExportCount > 0 && len(captured) != sc.Invariants.ExportCount {
		t.Fatalf("scenario %s: export request count = %d, want %d", name, len(captured), sc.Invariants.ExportCount)
	}
	for i, cap := range captured {
		if cap.path != wantPath {
			t.Fatalf("scenario %s: export[%d] path = %q, want %q", name, i, cap.path, wantPath)
		}
	}

	totalGens := 0
	allGenJSON := make([]string, 0)
	for _, b := range bodies {
		totalGens += len(b.Generations)
		for _, g := range b.Generations {
			allGenJSON = append(allGenJSON, string(g))
		}
	}
	if sc.Invariants.Generations > 0 && totalGens != sc.Invariants.Generations {
		t.Fatalf("scenario %s: total generations = %d, want %d", name, totalGens, sc.Invariants.Generations)
	}
	for _, want := range sc.Invariants.AnyGeneration {
		found := false
		for _, g := range allGenJSON {
			if strings.Contains(g, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("scenario %s: no generation contained %q", name, want)
		}
	}
	for _, want := range sc.Invariants.EveryGeneration {
		for i, g := range allGenJSON {
			if !strings.Contains(g, want) {
				t.Fatalf("scenario %s: generation[%d] missing %q: %s", name, i, want, g)
			}
		}
	}

	goldenPath := filepath.Join(dir, "export.golden.json")
	assertGoldenJSON(t, goldenPath, bodies)
}

// loadScenario parses scenario.json. The file is allowed to be missing the
// optional sections (Env, Transcripts, RawSecrets, Invariants).
func loadScenario(t *testing.T, dir string) scenario {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "scenario.json"))
	if err != nil {
		t.Fatalf("read scenario.json: %v", err)
	}
	var sc scenario
	if err := json.Unmarshal(data, &sc); err != nil {
		t.Fatalf("decode scenario.json: %v", err)
	}
	if sc.Agent == "" {
		t.Fatalf("scenario %s: missing agent field", dir)
	}
	if len(sc.Events) == 0 {
		t.Fatalf("scenario %s: events is empty", dir)
	}
	return sc
}

// writeTranscripts writes each named transcript to a unique path under the
// test's tempdir and returns the {name -> absolute path} map. Lines are
// joined with newlines; a trailing newline is added so JSONL readers that
// split on '\n' treat the last line as complete.
//
// Each entry in siblings is written verbatim into the same tempdir under
// its basename, so an agent reading e.g. <transcript_dir>/meta.json finds
// it next to the JSONL transcript.
func writeTranscripts(t *testing.T, transcripts map[string][]string, siblings map[string]string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(transcripts))
	if len(transcripts) == 0 && len(siblings) == 0 {
		return out
	}
	dir := t.TempDir()
	for name, lines := range transcripts {
		path := filepath.Join(dir, name+".jsonl")
		body := strings.Join(lines, "\n")
		if body != "" {
			body += "\n"
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write transcript %s: %v", name, err)
		}
		out[name] = path
	}
	for basename, body := range siblings {
		path := filepath.Join(dir, basename)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write sibling %s: %v", basename, err)
		}
	}
	return out
}

var transcriptPlaceholder = regexp.MustCompile(`{{transcript:([^}]+)}}`)

// splitEventEnv extracts optional per-event environment variables from the
// scenario-only `_env` key. The key is not sent to the hook stdin payload.
func splitEventEnv(t *testing.T, evt map[string]json.RawMessage) (map[string]string, map[string]json.RawMessage) {
	t.Helper()
	env := map[string]string{}
	payload := make(map[string]json.RawMessage, len(evt))
	for k, v := range evt {
		if k == "_env" {
			if err := json.Unmarshal(v, &env); err != nil {
				t.Fatalf("decode event _env: %v", err)
			}
			continue
		}
		payload[k] = v
	}
	return env, payload
}

func applyEventEnv(env map[string]string) func() {
	if len(env) == 0 {
		return func() {}
	}
	previous := make(map[string]*string, len(env))
	for k, v := range env {
		if old, ok := os.LookupEnv(k); ok {
			oldCopy := old
			previous[k] = &oldCopy
		} else {
			previous[k] = nil
		}
		_ = os.Setenv(k, v)
	}
	return func() {
		for k, old := range previous {
			if old == nil {
				_ = os.Unsetenv(k)
				continue
			}
			_ = os.Setenv(k, *old)
		}
	}
}

func TestSplitEventEnv(t *testing.T) {
	tests := []struct {
		name            string
		raw             string
		wantEnv         map[string]string
		wantPayloadKeys []string
	}{
		{
			name:            "no per-event env",
			raw:             `{"hook_event_name":"Stop","session_id":"sess"}`,
			wantEnv:         map[string]string{},
			wantPayloadKeys: []string{"hook_event_name", "session_id"},
		},
		{
			name:            "per-event env stripped from payload",
			raw:             `{"_env":{"SIGIL_COPILOT_HOOK_EVENT":"agentStop"},"session_id":"sess"}`,
			wantEnv:         map[string]string{"SIGIL_COPILOT_HOOK_EVENT": "agentStop"},
			wantPayloadKeys: []string{"session_id"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var evt map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tt.raw), &evt); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}

			gotEnv, gotPayload := splitEventEnv(t, evt)
			if !stringMapsEqual(gotEnv, tt.wantEnv) {
				t.Fatalf("env = %#v, want %#v", gotEnv, tt.wantEnv)
			}
			if _, ok := gotPayload["_env"]; ok {
				t.Fatalf("payload still contained _env: %#v", gotPayload)
			}
			for _, key := range tt.wantPayloadKeys {
				if _, ok := gotPayload[key]; !ok {
					t.Fatalf("payload missing key %q: %#v", key, gotPayload)
				}
			}
			if len(gotPayload) != len(tt.wantPayloadKeys) {
				t.Fatalf("payload keys = %#v, want only %v", gotPayload, tt.wantPayloadKeys)
			}
		})
	}
}

func TestApplyEventEnvRestoresEnvironment(t *testing.T) {
	const existingKey = "SIGIL_GOLDEN_TEST_EXISTING_ENV"
	const missingKey = "SIGIL_GOLDEN_TEST_MISSING_ENV"

	oldMissing, hadMissing := os.LookupEnv(missingKey)
	t.Cleanup(func() {
		if hadMissing {
			_ = os.Setenv(missingKey, oldMissing)
			return
		}
		_ = os.Unsetenv(missingKey)
	})

	t.Setenv(existingKey, "before")
	_ = os.Unsetenv(missingKey)

	restore := applyEventEnv(map[string]string{
		existingKey: "during",
		missingKey:  "set",
	})
	if got := os.Getenv(existingKey); got != "during" {
		t.Fatalf("%s during apply = %q, want during", existingKey, got)
	}
	if got := os.Getenv(missingKey); got != "set" {
		t.Fatalf("%s during apply = %q, want set", missingKey, got)
	}

	restore()
	if got := os.Getenv(existingKey); got != "before" {
		t.Fatalf("%s after restore = %q, want before", existingKey, got)
	}
	if _, ok := os.LookupEnv(missingKey); ok {
		t.Fatalf("%s remained set after restore", missingKey)
	}
}

func stringMapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		if bv, ok := b[k]; !ok || bv != av {
			return false
		}
	}
	return true
}

// substituteTranscriptPaths replaces every "{{transcript:NAME}}" occurrence
// inside the serialized event JSON with the path written for transcript
// NAME. The substitution is string-level rather than per-field so any
// payload that references a transcript by path (claude-code, codex,
// copilot) can use the same placeholder without the harness knowing each
// agent's field name.
func substituteTranscriptPaths(t *testing.T, evt map[string]json.RawMessage, paths map[string]string) []byte {
	t.Helper()
	raw, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	out := transcriptPlaceholder.ReplaceAllFunc(raw, func(match []byte) []byte {
		key := strings.TrimSuffix(strings.TrimPrefix(string(match), "{{transcript:"), "}}")
		path, ok := paths[key]
		if !ok {
			t.Fatalf("event references missing transcript %q", key)
		}
		escaped, err := json.Marshal(path)
		if err != nil {
			t.Fatalf("escape transcript path %q: %v", key, err)
		}
		return escaped[1 : len(escaped)-1]
	})
	return out
}

// runHookEvent invokes `sigil <agent> hook` once with the event payload as
// stdin, intercepting os.Exit so a non-zero exit terminates this event
// instead of the whole test. Returns the dispatcher's stdout/stderr so the
// caller can surface them on assertion failure.
func runHookEvent(t *testing.T, agent string, payload []byte, idx int) (stdoutText, stderrText string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	var exitCode *int
	prev := exit
	exit = func(code int) {
		v := code
		exitCode = &v
		panic(exitSentinel{})
	}
	defer func() { exit = prev }()
	defer func() {
		stdoutText = stdout.String()
		stderrText = stderr.String()
		if r := recover(); r != nil {
			if _, ok := r.(exitSentinel); !ok {
				t.Fatalf("event[%d] (%s hook) panicked: %v", idx, agent, r)
			}
			if exitCode == nil {
				t.Fatalf("event[%d] (%s hook) exited without a code", idx, agent)
			}
			if *exitCode != 0 {
				t.Fatalf("event[%d] (%s hook) exited with code %d\nstdout=%q\nstderr=%q", idx, agent, *exitCode, stdoutText, stderrText)
			}
		}
	}()
	run([]string{agent, "hook"}, bytes.NewReader(payload), &stdout, &stderr)
	return stdout.String(), stderr.String()
}

// exportCapture records each POST to /api/v1/generations:export.
type exportCapture struct {
	mu       sync.Mutex
	requests []capturedRequest
}

type capturedRequest struct {
	path string
	body []byte
}

func (c *exportCapture) snapshot() []capturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]capturedRequest, len(c.requests))
	copy(out, c.requests)
	return out
}

// newGoldenServer starts a 127.0.0.1 httptest server. We bind to
// 127.0.0.1 explicitly so the kontora/agent-safehouse sandbox accepts the
// listen, matching the pattern in codex/hook/handlers_test.go.
func newGoldenServer(t *testing.T, capture *exportCapture) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listen unavailable: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		capture.mu.Lock()
		capture.requests = append(capture.requests, capturedRequest{path: r.URL.Path, body: body})
		capture.mu.Unlock()
		writeAcceptedGenerationResponse(w, body)
	}))
	srv.Listener = listener
	srv.Start()
	return srv
}

// writeAcceptedGenerationResponse returns one accepted result per generation
// in the request so the SDK exporter does not retry.
func writeAcceptedGenerationResponse(w http.ResponseWriter, body []byte) {
	var request struct {
		Generations []struct {
			ID string `json:"id"`
		} `json:"generations"`
	}
	_ = json.Unmarshal(body, &request)
	results := make([]map[string]any, 0, len(request.Generations))
	for _, g := range request.Generations {
		results = append(results, map[string]any{
			"generation_id": g.ID,
			"accepted":      true,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
}

// normalizeCapturedBodies decodes each captured request body and returns the
// normalized goldenExport list. Generations are sorted within each request
// by their `id` field so insertion order from the SDK exporter (which may
// reorder concurrent enqueues) does not cause flaky diffs.
func normalizeCapturedBodies(t *testing.T, captured []capturedRequest) []goldenExport {
	t.Helper()
	out := make([]goldenExport, 0, len(captured))
	for i, cap := range captured {
		var body struct {
			Generations []json.RawMessage `json:"generations"`
		}
		if err := json.Unmarshal(cap.body, &body); err != nil {
			t.Fatalf("decode export request[%d]: %v\nbody=%s", i, err, cap.body)
		}
		gens := make([]json.RawMessage, 0, len(body.Generations))
		for _, g := range body.Generations {
			var decoded any
			if err := json.Unmarshal(g, &decoded); err != nil {
				t.Fatalf("decode generation[%d][%d]: %v", i, len(gens), err)
			}
			decoded = normalizeAny(decoded)
			normalized, err := json.Marshal(decoded)
			if err != nil {
				t.Fatalf("marshal normalized generation[%d][%d]: %v", i, len(gens), err)
			}
			gens = append(gens, json.RawMessage(normalized))
		}
		sort.Slice(gens, func(i, j int) bool {
			return generationSortKey(gens[i]) < generationSortKey(gens[j])
		})
		out = append(out, goldenExport{Path: cap.path, Generations: gens})
	}
	return out
}

// generationSortKey returns the `id` field of a generation so requests with
// multiple generations land in a stable order in the golden.
func generationSortKey(g json.RawMessage) string {
	var probe struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(g, &probe)
	return probe.ID
}

// normalizeFields lists the JSON keys whose values are scrubbed before
// comparison. These are dynamic at runtime (timestamps, trace IDs,
// installation-specific user IDs, SDK version) and have no value in a
// golden diff.
//
// `effective_version` is normalized in addition to `agent_version` because
// it is a sha256 derived from agent_version — normalizing only one would
// leave the hash visible while the input is scrubbed, hiding agent_version
// drift behind an opaque value.
var normalizeFields = map[string]string{
	"started_at":          "<NORMALIZED>",
	"completed_at":        "<NORMALIZED>",
	"timestamp":           "<NORMALIZED>",
	"trace_id":            "<NORMALIZED>",
	"span_id":             "<NORMALIZED>",
	"parent_span_id":      "<NORMALIZED>",
	"sigil.sdk.version":   "<NORMALIZED>",
	"sigil.sdk.commit":    "<NORMALIZED>",
	"agent_version":       "<NORMALIZED>",
	"effective_version":   "<NORMALIZED>",
	"sigil.user_id":       "<NORMALIZED>",
	"user_id":             "<NORMALIZED>",
	"sigil.host.hostname": "<NORMALIZED>",
	"host.name":           "<NORMALIZED>",
}

// normalizeKeySuffixes are suffix matches applied to map keys. Any tag/
// metadata key that ends with one of these suffixes has its value
// replaced. This covers vendor-prefixed timestamp keys like
// `sigil.generation.started_at` without an exact entry.
var normalizeKeySuffixes = []string{".started_at", ".completed_at", ".timestamp"}

func normalizeAny(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if replacement, ok := normalizeFields[k]; ok {
				t[k] = replacement
				continue
			}
			matched := false
			for _, suf := range normalizeKeySuffixes {
				if strings.HasSuffix(k, suf) {
					t[k] = "<NORMALIZED>"
					matched = true
					break
				}
			}
			if matched {
				continue
			}
			t[k] = normalizeAny(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = normalizeAny(val)
		}
		return t
	default:
		return v
	}
}

// assertGoldenJSON compares the captured exports against the file at path.
// When UPDATE_GOLDENS=1 is set the harness writes the captured output back
// instead of failing — the prescribed workflow for seeding new scenarios.
func assertGoldenJSON(t *testing.T, path string, got []goldenExport) {
	t.Helper()
	wantBuf, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	wantBuf = append(wantBuf, '\n')

	if os.Getenv("UPDATE_GOLDENS") == "1" {
		if err := os.WriteFile(path, wantBuf, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		t.Logf("UPDATE_GOLDENS=1: wrote %s", path)
		return
	}

	existing, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with UPDATE_GOLDENS=1 to seed): %v", path, err)
	}
	if !bytes.Equal(bytes.TrimSpace(existing), bytes.TrimSpace(wantBuf)) {
		t.Fatalf("golden mismatch for %s\nwant:\n%s\ngot:\n%s", path, existing, wantBuf)
	}
}

// setHookExportEnv configures the canonical SIGIL_* env vars to point at the
// fake export server and disables OTel transport so spans/metrics do not try
// to reach a non-existent collector during the test.
func setHookExportEnv(t *testing.T, endpoint string) {
	t.Helper()
	t.Setenv("SIGIL_ENDPOINT", endpoint)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")
	t.Setenv("SIGIL_AUTH_MODE", "basic")
	t.Setenv("SIGIL_PROTOCOL", "http")
	t.Setenv("SIGIL_USER_ID", "test-user")
	t.Setenv("SIGIL_TAGS", "")
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "full")
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	// Enable SIGIL_DEBUG so the dispatcher writes per-event logs into
	// $XDG_STATE_HOME/sigil/logs/sigil.log. When a scenario fails the test
	// reads that file back so the failure message includes the trail.
	t.Setenv("SIGIL_DEBUG", "true")
}
