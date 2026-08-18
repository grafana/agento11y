package entry

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/capturemode"
	"github.com/grafana/agento11y/plugins/agento11y/internal/history"
	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
)

// historyFixedNow pins the clock so the 90-day default boundary is checkable.
var historyFixedNow = time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

func withHistoryNow(t *testing.T) {
	t.Helper()
	prev := historyNow
	t.Cleanup(func() { historyNow = prev })
	historyNow = func() time.Time { return historyFixedNow }
}

// writeClaudeHistory writes a Claude transcript whose last activity is `age`
// before the pinned clock, and points CLAUDE_CONFIG_DIR at its root.
func writeClaudeHistory(t *testing.T, sessionID string, age time.Duration) string {
	t.Helper()
	configDir := t.TempDir()
	last := historyFixedNow.Add(-age)
	line := func(fields map[string]any) string {
		data, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return string(data) + "\n"
	}
	body := line(map[string]any{
		"type": "user", "sessionId": sessionID, "cwd": "/work/repo",
		"timestamp": last.Add(-time.Minute).Format(time.RFC3339),
		"message":   map[string]any{"role": "user", "content": "explain the build"},
	}) + line(map[string]any{
		"type": "assistant", "sessionId": sessionID, "cwd": "/work/repo",
		"timestamp": last.Format(time.RFC3339), "requestId": "req-1",
		"message": map[string]any{
			"model": "claude-sonnet-4-20250514", "stop_reason": "end_turn",
			"usage":   map[string]any{"input_tokens": 10, "output_tokens": 5},
			"content": []map[string]any{{"type": "text", "text": "It compiles."}},
		},
	})

	path := filepath.Join(configDir, "projects", "-work-repo", sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, last, last); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	return path
}

// runHistory drives the CLI with a non-terminal stdin and returns its streams
// plus the exit code, if any.
func runHistory(t *testing.T, args ...string) (stdout, stderr string, code *int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = withExit(t, func() {
		run(args, strings.NewReader(""), &out, &errOut)
	})
	return out.String(), errOut.String(), code
}

func TestHistoryUsageComesFromTheRegistry(t *testing.T) {
	// Every registered agent must appear in usage, so a new importer needs no
	// edit in this package.
	for _, spec := range history.Specs() {
		if !strings.Contains(usageLine(), string(spec.ID)) {
			t.Errorf("usage line does not mention %q: %s", spec.ID, usageLine())
		}
		if !strings.Contains(historyUsageLine(), string(spec.ID)) {
			t.Errorf("history usage line does not mention %q: %s", spec.ID, historyUsageLine())
		}
		found := false
		for _, line := range historyAgentTable() {
			if strings.Contains(line, string(spec.ID)) && strings.Contains(line, spec.DisplayName) {
				found = true
			}
		}
		if !found {
			t.Errorf("agent table has no row for %q: %v", spec.ID, historyAgentTable())
		}
	}
	if len(history.Specs()) == 0 {
		t.Fatal("no importers registered; the registry-derived usage test proves nothing")
	}
}

func TestHistoryArgumentValidation(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantExit   int
		wantStderr string
	}{
		{
			name:       "no verb",
			args:       []string{"history"},
			wantExit:   2,
			wantStderr: "usage: agento11y history import",
		},
		{
			name:       "unknown verb",
			args:       []string{"history", "export"},
			wantExit:   2,
			wantStderr: `unknown history verb "export"`,
		},
		{
			name:       "no agent",
			args:       []string{"history", "import"},
			wantExit:   2,
			wantStderr: "usage: agento11y history import",
		},
		{
			name:       "unknown agent",
			args:       []string{"history", "import", "aider"},
			wantExit:   2,
			wantStderr: `unknown history agent "aider"`,
		},
		{
			name:       "extra argument",
			args:       []string{"history", "import", "claude-code", "codex"},
			wantExit:   2,
			wantStderr: `unexpected argument "codex"`,
		},
		{
			name:       "invalid since",
			args:       []string{"history", "import", "claude-code", "--since", "last tuesday", "--dry-run"},
			wantExit:   2,
			wantStderr: "invalid --since",
		},
		{
			name:       "invalid until",
			args:       []string{"history", "import", "claude-code", "--until", "soon", "--dry-run"},
			wantExit:   2,
			wantStderr: "invalid --until",
		},
		{
			name:       "until before since",
			args:       []string{"history", "import", "claude-code", "--since", "30d", "--until", "60d", "--dry-run"},
			wantExit:   2,
			wantStderr: "is before --since",
		},
		{
			name:       "negative max-sessions",
			args:       []string{"history", "import", "claude-code", "--max-sessions", "-1", "--dry-run"},
			wantExit:   2,
			wantStderr: "cannot be negative",
		},
		{
			name:       "help lists the Cloud opt-out",
			args:       []string{"history", "import", "claude-code", "--help"},
			wantExit:   2,
			wantStderr: "no-local",
		},
		{
			name:       "unknown flag",
			args:       []string{"history", "import", "claude-code", "--nope"},
			wantExit:   2,
			wantStderr: "flag provided but not defined",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withHistoryNow(t)
			isolateDotenvHome(t)
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

			_, stderr, code := runHistory(t, tt.args...)
			if code == nil || *code != tt.wantExit {
				t.Fatalf("exit = %v, want %d (stderr=%q)", code, tt.wantExit, stderr)
			}
			if !strings.Contains(stderr, tt.wantStderr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, tt.wantStderr)
			}
		})
	}
}

// TestHistoryResolvesAliases covers a registered alias on the command line.
// The plan still reports the canonical name, so an alias is an input spelling
// rather than a second identity.
func TestHistoryResolvesAliases(t *testing.T) {
	spec, ok := history.Spec(history.AgentClaudeCode)
	if !ok || len(spec.Aliases) == 0 {
		t.Skip("claude-code has no registered alias")
	}
	for _, alias := range append(spec.Aliases, strings.ToUpper(spec.Aliases[0])) {
		t.Run(alias, func(t *testing.T) {
			withHistoryNow(t)
			isolateDotenvHome(t)
			writeClaudeHistory(t, "sess-recent", 24*time.Hour)

			stdout, stderr, code := runHistory(t, "history", "import", alias, "--dry-run")
			if code != nil {
				t.Fatalf("exit = %d for alias %q (stderr=%q)", *code, alias, stderr)
			}
			if !strings.Contains(stdout, spec.DisplayName) {
				t.Fatalf("stdout = %q, want the canonical %q", stdout, spec.DisplayName)
			}
			if !strings.Contains(stdout, "planned: 1 sessions") {
				t.Fatalf("stdout = %q, want the alias to reach the claude-code importer", stdout)
			}
		})
	}
}

func TestHistoryUnknownAgentListsTheRegisteredOnes(t *testing.T) {
	withHistoryNow(t)
	isolateDotenvHome(t)
	_, stderr, _ := runHistory(t, "history", "import", "aider")
	for _, spec := range history.Specs() {
		if !strings.Contains(stderr, string(spec.ID)) {
			t.Errorf("stderr does not list %q: %s", spec.ID, stderr)
		}
	}
}

func TestHistoryDryRunReportsThePlan(t *testing.T) {
	withHistoryNow(t)
	isolateDotenvHome(t)
	writeClaudeHistory(t, "sess-recent", 24*time.Hour)

	stdout, stderr, code := runHistory(t, "history", "import", "claude-code", "--dry-run")
	if code != nil {
		t.Fatalf("exit = %d, want no exit (stderr=%q)", *code, stderr)
	}
	for _, want := range []string{
		"Claude Code history since",
		"planned: 1 sessions, 1 turns",
		"Dry run: nothing was decoded, exported, or stored.",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
}

// TestHistoryDefaultsToNinetyDays pins the default window. An unbounded first
// import would put every turn ever recorded into a linear-scan JSONL store.
func TestHistoryDefaultsToNinetyDays(t *testing.T) {
	withHistoryNow(t)
	isolateDotenvHome(t)
	writeClaudeHistory(t, "sess-old", 120*24*time.Hour)

	stdout, _, _ := runHistory(t, "history", "import", "claude-code", "--dry-run")
	boundary := historyFixedNow.Add(-history.DefaultSinceWindow).Format(time.RFC3339)
	if !strings.Contains(stdout, boundary) {
		t.Errorf("stdout = %q, want the effective 90-day boundary %s", stdout, boundary)
	}
	if !strings.Contains(stdout, "planned: 0 sessions") {
		t.Errorf("stdout = %q, want a 120-day-old session excluded", stdout)
	}
	if !strings.Contains(stdout, "out_of_range") {
		t.Errorf("stdout = %q, want the skip reason reported", stdout)
	}
}

func TestHistorySinceOverridesTheDefault(t *testing.T) {
	withHistoryNow(t)
	isolateDotenvHome(t)
	writeClaudeHistory(t, "sess-old", 120*24*time.Hour)

	stdout, _, _ := runHistory(t, "history", "import", "claude-code", "--since", "200d", "--dry-run")
	if !strings.Contains(stdout, "planned: 1 sessions") {
		t.Errorf("stdout = %q, want the older session included by --since 200d", stdout)
	}
}

// TestHistoryNonInteractiveForcesADryRun is the safety rule: without a
// terminal there is no picker and no confirmation, so an import that was not
// explicitly asked for must not run.
func TestHistoryNonInteractiveForcesADryRun(t *testing.T) {
	withHistoryNow(t)
	isolateDotenvHome(t)
	writeClaudeHistory(t, "sess-recent", 24*time.Hour)

	exported := 0
	withStubHistoryExporter(t, &exported)

	stdout, stderr, code := runHistory(t, "history", "import", "claude-code", "--local")
	if code != nil {
		t.Fatalf("exit = %d, want no exit (stderr=%q)", *code, stderr)
	}
	if exported != 0 {
		t.Fatalf("exported %d turns, want none without a terminal", exported)
	}
	if !strings.Contains(stderr, "pass --all --yes") {
		t.Errorf("stderr = %q, want it to name the missing flags", stderr)
	}
	if !strings.Contains(stdout, "Dry run") {
		t.Errorf("stdout = %q, want the dry-run plan", stdout)
	}
}

// TestHistoryImportWithoutCredentialsDefaultsLocal covers both halves of the
// non-interactive rule and the credential-aware destination: --all --yes runs
// the import, and no credentials or destination flag selects the local daemon.
func TestHistoryImportWithoutCredentialsDefaultsLocal(t *testing.T) {
	withHistoryNow(t)
	isolateDotenvHome(t)
	writeClaudeHistory(t, "sess-recent", 24*time.Hour)

	exported := 0
	withStubHistoryExporter(t, &exported)

	stdout, stderr, code := runHistory(t, "history", "import", "claude-code", "--all", "--yes")
	if code != nil {
		t.Fatalf("exit = %d, want no exit (stderr=%q)", *code, stderr)
	}
	if exported != 1 {
		t.Fatalf("exported %d turns, want 1", exported)
	}
	if !strings.Contains(stdout, "destination: the local store on this machine") {
		t.Errorf("stdout = %q, want the plan to name the local destination", stdout)
	}
	if !strings.Contains(stdout, "Imported 1 turns from 1 sessions into the local store on this machine") {
		t.Errorf("stdout = %q, want the import summary to name the local destination", stdout)
	}
	if !strings.Contains(stdout, "import ledger is shared") {
		t.Errorf("stdout = %q, want the cross-destination --force warning", stdout)
	}
}

func TestHistoryNoLocalSelectsCloudWithoutCredentials(t *testing.T) {
	withHistoryNow(t)
	isolateDotenvHome(t)
	writeClaudeHistory(t, "sess-recent", 24*time.Hour)

	prev := historyEnsureLocal
	t.Cleanup(func() { historyEnsureLocal = prev })
	historyEnsureLocal = func(context.Context) (string, error) {
		t.Fatal("--no-local must not start the local receiver")
		return "", nil
	}

	_, stderr, code := runHistory(t,
		"history", "import", "claude-code", "--local", "--no-local", "--all", "--yes")
	if code == nil || *code != 1 {
		t.Fatalf("exit = %v, want 1 (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stderr, "Grafana Cloud import has no endpoint configured") {
		t.Fatalf("stderr = %q, want the resolved Cloud destination", stderr)
	}
}

// TestHistoryPickerSelectsSessions drives the interactive path with a stubbed
// picker, following the login.Run precedent for a huh form under test.
func TestHistoryPickerSelectsSessions(t *testing.T) {
	withHistoryNow(t)
	isolateDotenvHome(t)
	writeClaudeHistory(t, "sess-recent", 24*time.Hour)

	exported := 0
	withStubHistoryExporter(t, &exported)

	var offered []history.SessionPreview
	prevSelect := historySelect
	t.Cleanup(func() { historySelect = prevSelect })
	historySelect = func(sessions []history.SessionPreview) ([]history.SessionPreview, error) {
		offered = sessions
		return nil, nil // the user deselected everything
	}

	var out, errOut bytes.Buffer
	code := withExit(t, func() {
		// historyImport is called directly: run() would see a non-terminal
		// stdin and force a dry run before the picker.
		if err := historyImport(historyImportOptions{
			Agent:       history.AgentClaudeCode,
			Since:       historyFixedNow.Add(-history.DefaultSinceWindow),
			Yes:         true,
			CaptureFlag: capturemode.FlagLocal,
		}, true, &out, &errOut); err != nil {
			t.Fatalf("historyImport: %v", err)
		}
	})
	if code != nil {
		t.Fatalf("exit = %d", *code)
	}
	if len(offered) != 1 {
		t.Fatalf("picker was offered %d sessions, want 1", len(offered))
	}
	if offered[0].SessionID != "sess-recent" {
		t.Fatalf("picker was offered %q", offered[0].SessionID)
	}
	if exported != 0 {
		t.Fatalf("exported %d turns, want none after deselecting everything", exported)
	}
	if !strings.Contains(out.String(), "No sessions selected.") {
		t.Errorf("stdout = %q, want the empty-selection message", out.String())
	}
}

// TestHistorySourceFilterReportsNoMatch covers --source pointing outside the
// agent's roots, which filters discovery rather than adding a root.
func TestHistorySourceFilterReportsNoMatch(t *testing.T) {
	withHistoryNow(t)
	isolateDotenvHome(t)
	writeClaudeHistory(t, "sess-recent", 24*time.Hour)

	stdout, stderr, code := runHistory(t,
		"history", "import", "claude-code", "--dry-run", "--source", "/elsewhere/transcripts")
	if code != nil {
		t.Fatalf("exit = %d, want no exit (stderr=%q)", *code, stderr)
	}
	if !strings.Contains(stdout, "planned: 0 sessions") {
		t.Errorf("stdout = %q, want no planned sessions", stdout)
	}
	if !strings.Contains(stderr, "it cannot add a new root") {
		t.Errorf("stderr = %q, want the --source explanation", stderr)
	}
}

func TestHistoryMaxTurnsCapsEachSession(t *testing.T) {
	withHistoryNow(t)
	isolateDotenvHome(t)
	writeClaudeHistory(t, "sess-recent", 24*time.Hour)

	exported := 0
	withStubHistoryExporter(t, &exported)

	_, stderr, code := runHistory(t,
		"history", "import", "claude-code", "--local", "--all", "--yes", "--max-turns", "0")
	if code != nil {
		t.Fatalf("exit = %d (stderr=%q)", *code, stderr)
	}
	if exported != 1 {
		t.Fatalf("exported %d turns with no cap, want 1", exported)
	}
}

// TestHistoryImportFailureExitsNonZero pins that a scripted import can tell it
// went wrong. The counters are printed either way, but only the exit status
// reaches a script.
func TestHistoryImportFailureExitsNonZero(t *testing.T) {
	withHistoryNow(t)
	isolateDotenvHome(t)
	writeClaudeHistory(t, "sess-recent", 24*time.Hour)

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	t.Cleanup(broken.Close)
	prev := historyEnsureLocal
	t.Cleanup(func() { historyEnsureLocal = prev })
	historyEnsureLocal = func(context.Context) (string, error) { return broken.URL, nil }

	stdout, stderr, code := runHistory(t,
		"history", "import", "claude-code", "--local", "--all", "--yes")
	if code == nil || *code == 0 {
		t.Fatalf("exit = %v, want non-zero (stdout=%q stderr=%q)", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "1 failed") {
		t.Fatalf("stdout = %q, want the failed count", stdout)
	}
	if !strings.Contains(stderr, "failed to export") {
		t.Fatalf("stderr = %q, want the reason", stderr)
	}
}

func TestParseHistoryBound(t *testing.T) {
	now := historyFixedNow
	fallback := now.Add(-history.DefaultSinceWindow)
	tests := []struct {
		name    string
		raw     string
		want    time.Time
		wantErr bool
	}{
		{name: "empty uses the fallback", raw: "", want: fallback},
		{name: "RFC3339", raw: "2026-01-02T03:04:05Z", want: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{name: "days", raw: "30d", want: now.Add(-30 * 24 * time.Hour)},
		{name: "hours", raw: "12h", want: now.Add(-12 * time.Hour)},
		{name: "negative duration", raw: "-5d", wantErr: true},
		{name: "nonsense", raw: "yesterday", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseHistoryBound(tt.raw, now, fallback)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && !got.Equal(tt.want) {
				t.Fatalf("bound = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestHistorySessionLabelCarriesNoPromptText(t *testing.T) {
	label := historySessionLabel(history.SessionPreview{
		SessionID:      "sess-1",
		Title:          "sess-1",
		Workspace:      "/work/repo",
		TurnCount:      12,
		ApproxTurns:    true,
		LastActivityAt: historyFixedNow,
	})
	if !strings.Contains(label, "/work/repo") || !strings.Contains(label, "about 12 turns") {
		t.Fatalf("label = %q, want the workspace and an approximate turn count", label)
	}
}

// withStubHistoryExporter replaces the local daemon and the export pipeline so
// the CLI tests never start a daemon or open a socket. It counts exported
// turns through a fake ingest endpoint.
func withStubHistoryExporter(t *testing.T, exported *int) {
	t.Helper()
	srv := newCountingIngest(t, exported)
	prev := historyEnsureLocal
	t.Cleanup(func() { historyEnsureLocal = prev })
	historyEnsureLocal = func(context.Context) (string, error) { return srv, nil }
}

// newCountingIngest starts a fake generation-export endpoint and returns its
// base URL. It counts accepted generations and asserts that every request
// carries the local daemon's forward marker. That marker is what keeps an
// import off the Cloud forwarding path.
func newCountingIngest(t *testing.T, exported *int) string {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(local.ForwardMarkerHeader) == "" {
			t.Errorf("%s %s arrived without the forward marker; the daemon would relay it to Cloud", r.Method, r.URL.Path)
		}
		if r.URL.Path != "/api/v1/generations:export" {
			w.WriteHeader(http.StatusOK)
			return
		}
		var req struct {
			Generations []json.RawMessage `json:"generations"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		results := make([]map[string]any, len(req.Generations))
		for i := range req.Generations {
			results[i] = map[string]any{"accepted": true}
		}
		mu.Lock()
		*exported += len(req.Generations)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}
