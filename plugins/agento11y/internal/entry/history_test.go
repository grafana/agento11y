package entry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/history"
	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
	"github.com/grafana/agento11y/plugins/agento11y/internal/login"
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
	withStubLoginRun(t, func(context.Context, login.RunOpts) (login.Result, error) {
		t.Fatal("dry run must not prompt for a destination")
		return login.Result{}, nil
	})

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

func TestHistoryEmptyPlanSkipsSetup(t *testing.T) {
	withHistoryNow(t)
	isolateDotenvHome(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	withStubLoginRun(t, func(context.Context, login.RunOpts) (login.Result, error) {
		t.Fatal("an empty import plan must not start setup")
		return login.Result{}, nil
	})

	var stdout, stderr bytes.Buffer
	err := historyImport(historyImportOptions{
		Agent: history.AgentClaudeCode,
		Since: historyFixedNow.Add(-history.DefaultSinceWindow),
	}, true, &stdout, &stderr)
	if err != nil {
		t.Fatalf("historyImport: %v", err)
	}
	if !strings.Contains(stdout.String(), "planned: 0 sessions") {
		t.Fatalf("stdout = %q, want an empty plan", stdout.String())
	}
}

func TestHistoryResolveDestination(t *testing.T) {
	setupErr := errors.New("broken login")
	tests := []struct {
		name            string
		opts            historyImportOptions
		localEnv        string
		localEnvKey     string
		endpoint        string
		credentials     bool
		interactive     bool
		loginResult     login.Result
		loginErr        error
		wantLocal       bool
		wantLocalEnvKey string
		wantLoginCalls  int
		wantOfferLocal  bool
		wantKeepLocal   bool
		wantErr         string
	}{
		{
			name:        "no-local beats local flag and environment",
			opts:        historyImportOptions{Local: true, NoLocal: true},
			localEnv:    "true",
			credentials: true,
			interactive: true,
		},
		{
			name:        "local flag needs no setup",
			opts:        historyImportOptions{Local: true},
			interactive: true,
			wantLocal:   true,
		},
		{
			name:            "local environment enables local destination",
			localEnv:        "true",
			localEnvKey:     envconfig.LegacyKey("LOCAL"),
			interactive:     true,
			wantLocal:       true,
			wantLocalEnvKey: envconfig.LegacyKey("LOCAL"),
		},
		{
			name:        "saved credentials select Cloud",
			credentials: true,
			interactive: true,
		},
		{
			name:        "loopback endpoint needs no setup",
			endpoint:    "http://127.0.0.1:8765",
			interactive: true,
		},
		{
			name:           "Cloud endpoint without credentials starts setup",
			endpoint:       "https://agento11y.example.com",
			interactive:    true,
			wantLoginCalls: 1,
			wantOfferLocal: true,
		},
		{
			name:        "non-interactive import leaves exporter to report missing credentials",
			interactive: false,
		},
		{
			name:           "local login answer selects local destination",
			interactive:    true,
			loginResult:    login.Result{LocalMode: true},
			wantLocal:      true,
			wantLoginCalls: 1,
			wantOfferLocal: true,
		},
		{
			name:           "Cloud login answer selects Cloud destination",
			interactive:    true,
			wantLoginCalls: 1,
			wantOfferLocal: true,
		},
		{
			name:           "no-local starts only Cloud setup and keeps saved local mode",
			opts:           historyImportOptions{Local: true, NoLocal: true},
			localEnv:       "true",
			interactive:    true,
			wantLoginCalls: 1,
			wantKeepLocal:  true,
		},
		{
			name:           "false local value skips the destination question",
			localEnv:       "false",
			interactive:    true,
			wantLoginCalls: 1,
		},
		{
			name:           "invalid local value still offers destination",
			localEnv:       "maybe",
			interactive:    true,
			wantLoginCalls: 1,
			wantOfferLocal: true,
		},
		{
			name:           "aborted setup stops import",
			interactive:    true,
			loginErr:       login.ErrAborted,
			wantLoginCalls: 1,
			wantOfferLocal: true,
			wantErr:        "nothing was imported because setup did not finish; run `agento11y login` or pass --local",
		},
		{
			name:           "login without terminal stops import",
			interactive:    true,
			loginErr:       login.ErrNotInteractive,
			wantLoginCalls: 1,
			wantOfferLocal: true,
			wantErr:        "nothing was imported because setup did not finish; run `agento11y login` or pass --local",
		},
		{
			name:           "refused credentials stop import with recovery paths",
			interactive:    true,
			loginErr:       login.ErrNotVerified,
			wantLoginCalls: 1,
			wantOfferLocal: true,
			wantErr:        "did not accept those credentials; run `agento11y login` to try again, `agento11y login --yes` to save them anyway, or pass --local",
		},
		{
			name:           "other setup failure is wrapped",
			interactive:    true,
			loginErr:       setupErr,
			wantLoginCalls: 1,
			wantOfferLocal: true,
			wantErr:        "setup: broken login",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateDotenvHome(t)
			localKey := tt.localEnvKey
			if localKey == "" {
				localKey = envconfig.PreferredKey("LOCAL")
			}
			t.Setenv(localKey, tt.localEnv)
			endpoint := tt.endpoint
			if tt.credentials {
				if endpoint == "" {
					endpoint = "https://agento11y.example.com"
				}
				t.Setenv(envconfig.PreferredKey("AUTH_TENANT_ID"), "tenant")
				t.Setenv(envconfig.PreferredKey("AUTH_TOKEN"), "token")
			}
			if endpoint != "" {
				t.Setenv(envconfig.PreferredKey("ENDPOINT"), endpoint)
			}

			loginCalls := 0
			withStubLoginRun(t, func(_ context.Context, opts login.RunOpts) (login.Result, error) {
				loginCalls++
				if opts.OfferLocal != tt.wantOfferLocal {
					t.Errorf("OfferLocal = %v, want %v", opts.OfferLocal, tt.wantOfferLocal)
				}
				if opts.KeepLocalSetting != tt.wantKeepLocal {
					t.Errorf("KeepLocalSetting = %v, want %v", opts.KeepLocalSetting, tt.wantKeepLocal)
				}
				return tt.loginResult, tt.loginErr
			})

			envLocal := localEnvRequest{on: envconfig.ParseBool(tt.localEnv), key: localKey}
			if tt.localEnv == "" {
				envLocal.key = ""
			}
			opts := tt.opts
			err := historyResolveDestination(context.Background(), &opts, envLocal, tt.interactive, io.Discard, log.New(io.Discard, "", 0))
			if tt.wantErr == "" && err != nil {
				t.Fatalf("historyResolveDestination() error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("historyResolveDestination() error = %v, want it to contain %q", err, tt.wantErr)
			}
			if opts.Local != tt.wantLocal {
				t.Errorf("Local = %v, want %v", opts.Local, tt.wantLocal)
			}
			if opts.LocalEnvKey != tt.wantLocalEnvKey {
				t.Errorf("LocalEnvKey = %q, want %q", opts.LocalEnvKey, tt.wantLocalEnvKey)
			}
			if loginCalls != tt.wantLoginCalls {
				t.Errorf("loginRun called %d times, want %d", loginCalls, tt.wantLoginCalls)
			}
		})
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

// TestHistoryNonInteractiveWithAllAndYesImports is the other half of the rule.
func TestHistoryNonInteractiveWithAllAndYesImports(t *testing.T) {
	withHistoryNow(t)
	isolateDotenvHome(t)
	writeClaudeHistory(t, "sess-recent", 24*time.Hour)

	exported := 0
	withStubHistoryExporter(t, &exported)

	stdout, stderr, code := runHistory(t, "history", "import", "claude-code", "--local", "--all", "--yes")
	if code != nil {
		t.Fatalf("exit = %d, want no exit (stderr=%q)", *code, stderr)
	}
	if exported != 1 {
		t.Fatalf("exported %d turns, want 1", exported)
	}
	if !strings.Contains(stdout, "Imported 1 turns from 1 sessions") {
		t.Errorf("stdout = %q, want the import summary", stdout)
	}
}

func TestHistoryImportUsesLocalModeFromConfigEnv(t *testing.T) {
	withHistoryNow(t)
	isolateDotenvHome(t)
	writeClaudeHistory(t, "sess-recent", 24*time.Hour)
	writeHistoryConfig(t, "AGENTO11Y_LOCAL=true\n")
	withStubLoginRun(t, func(context.Context, login.RunOpts) (login.Result, error) {
		t.Fatal("configured local mode must not prompt")
		return login.Result{}, nil
	})

	exported := 0
	withStubHistoryExporter(t, &exported)
	var destination string
	prevConfirm := historyConfirm
	t.Cleanup(func() { historyConfirm = prevConfirm })
	historyConfirm = func(opts historyImportOptions, _ []history.SessionPreview) (bool, error) {
		destination = historyDestinationName(opts)
		return true, nil
	}

	var stdout, stderr bytes.Buffer
	err := historyImport(historyImportOptions{
		Agent: history.AgentClaudeCode,
		Since: historyFixedNow.Add(-history.DefaultSinceWindow),
		All:   true,
	}, true, &stdout, &stderr)
	if err != nil {
		t.Fatalf("historyImport: %v", err)
	}
	if destination != "the local store on this machine" {
		t.Fatalf("confirmation destination = %q", destination)
	}
	if exported != 1 {
		t.Fatalf("exported %d turns, want 1", exported)
	}
}

func TestHistoryImportNoLocalOverridesLocalMode(t *testing.T) {
	for _, flags := range [][]string{{"--local", "--no-local"}, {"--no-local", "--local"}} {
		t.Run(strings.Join(flags, "_"), func(t *testing.T) {
			withHistoryNow(t)
			isolateDotenvHome(t)
			writeClaudeHistory(t, "sess-recent", 24*time.Hour)
			t.Setenv(envconfig.PreferredKey("LOCAL"), "true")

			exported := 0
			endpoint := newCountingIngest(t, &exported)
			setHistoryCloudCredentials(t, endpoint)
			withStubLoginRun(t, func(context.Context, login.RunOpts) (login.Result, error) {
				t.Fatal("saved Cloud credentials must not prompt")
				return login.Result{}, nil
			})

			args := []string{"history", "import", "claude-code"}
			args = append(args, flags...)
			args = append(args, "--all", "--yes")
			_, stderr, code := runHistory(t, args...)
			if code != nil {
				t.Fatalf("exit = %d, want no exit (stderr=%q)", *code, stderr)
			}
			if exported != 1 {
				t.Fatalf("exported %d turns to Cloud, want 1", exported)
			}
		})
	}
}

func TestHistoryImportSavedCredentialsSkipSetup(t *testing.T) {
	withHistoryNow(t)
	isolateDotenvHome(t)
	writeClaudeHistory(t, "sess-recent", 24*time.Hour)

	exported := 0
	endpoint := newCountingIngest(t, &exported)
	setHistoryCloudCredentials(t, endpoint)
	withStubLoginRun(t, func(context.Context, login.RunOpts) (login.Result, error) {
		t.Fatal("saved Cloud credentials must not prompt")
		return login.Result{}, nil
	})

	_, stderr, code := runHistory(t, "history", "import", "claude-code", "--all", "--yes")
	if code != nil {
		t.Fatalf("exit = %d, want no exit (stderr=%q)", *code, stderr)
	}
	if exported != 1 {
		t.Fatalf("exported %d turns to Cloud, want 1", exported)
	}
}

func TestHistoryImportFirstRunUsesLoginDestination(t *testing.T) {
	tests := []struct {
		name   string
		result login.Result
		cloud  bool
	}{
		{name: "local", result: login.Result{LocalMode: true}},
		{name: "Cloud", cloud: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withHistoryNow(t)
			isolateDotenvHome(t)
			writeClaudeHistory(t, "sess-recent", 24*time.Hour)

			exported := 0
			cloudEndpoint := ""
			if tt.cloud {
				cloudEndpoint = newCountingIngest(t, &exported)
			} else {
				withStubHistoryExporter(t, &exported)
			}
			loginCalls := 0
			withStubLoginRun(t, func(_ context.Context, opts login.RunOpts) (login.Result, error) {
				loginCalls++
				if !opts.OfferLocal {
					t.Error("first-run setup did not offer the destination question")
				}
				if cloudEndpoint != "" {
					setHistoryCloudCredentials(t, cloudEndpoint)
				}
				return tt.result, nil
			})

			var stdout, stderr bytes.Buffer
			err := historyImport(historyImportOptions{
				Agent: history.AgentClaudeCode,
				Since: historyFixedNow.Add(-history.DefaultSinceWindow),
				All:   true,
				Yes:   true,
			}, true, &stdout, &stderr)
			if err != nil {
				t.Fatalf("historyImport: %v", err)
			}
			if loginCalls != 1 {
				t.Fatalf("loginRun called %d times, want 1", loginCalls)
			}
			if exported != 1 {
				t.Fatalf("exported %d turns, want 1", exported)
			}
		})
	}
}

func TestHistoryImportAbortedSetupExportsNothing(t *testing.T) {
	withHistoryNow(t)
	isolateDotenvHome(t)
	writeClaudeHistory(t, "sess-recent", 24*time.Hour)
	withStubLoginRun(t, func(context.Context, login.RunOpts) (login.Result, error) {
		return login.Result{}, login.ErrAborted
	})

	exported := 0
	withStubHistoryExporter(t, &exported)
	var stdout, stderr bytes.Buffer
	err := historyImport(historyImportOptions{
		Agent: history.AgentClaudeCode,
		Since: historyFixedNow.Add(-history.DefaultSinceWindow),
		All:   true,
		Yes:   true,
	}, true, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "nothing was imported") {
		t.Fatalf("historyImport error = %v", err)
	}
	if exported != 0 {
		t.Fatalf("exported %d turns, want none", exported)
	}
}

func TestHistoryImportNonInteractiveMissingCredentialsDoesNotPrompt(t *testing.T) {
	withHistoryNow(t)
	isolateDotenvHome(t)
	writeClaudeHistory(t, "sess-recent", 24*time.Hour)
	t.Setenv(envconfig.PreferredKey("ENDPOINT"), "https://agento11y.example.com")
	withStubLoginRun(t, func(context.Context, login.RunOpts) (login.Result, error) {
		t.Fatal("non-interactive import must not prompt")
		return login.Result{}, nil
	})

	_, stderr, code := runHistory(t, "history", "import", "claude-code", "--all", "--yes")
	if code == nil || *code != 1 {
		t.Fatalf("exit = %v, want 1 (stderr=%q)", code, stderr)
	}
	want := "history: no credentials configured for import (run `agento11y login` or use --local)"
	if !strings.Contains(stderr, want) {
		t.Fatalf("stderr = %q, want it to contain %q", stderr, want)
	}
}

func TestHistoryImportLoopbackEndpointSkipsSetup(t *testing.T) {
	withHistoryNow(t)
	isolateDotenvHome(t)
	writeClaudeHistory(t, "sess-recent", 24*time.Hour)

	exported := 0
	writeHistoryConfig(t, envconfig.PreferredKey("ENDPOINT")+"="+newCountingIngest(t, &exported)+"\n")
	withStubLoginRun(t, func(context.Context, login.RunOpts) (login.Result, error) {
		t.Fatal("a configured loopback endpoint must not prompt")
		return login.Result{}, nil
	})
	prev := historyEnsureLocal
	t.Cleanup(func() { historyEnsureLocal = prev })
	historyEnsureLocal = func(context.Context) (string, error) {
		t.Error("a configured endpoint must not start another local receiver")
		return "", errors.New("unexpected local receiver start")
	}

	var stdout, stderr bytes.Buffer
	err := historyImport(historyImportOptions{
		Agent: history.AgentClaudeCode,
		Since: historyFixedNow.Add(-history.DefaultSinceWindow),
		All:   true,
		Yes:   true,
	}, true, &stdout, &stderr)
	if err != nil {
		t.Fatalf("historyImport: %v", err)
	}
	if exported != 1 {
		t.Fatalf("exported %d turns to the configured endpoint, want 1", exported)
	}
}

func TestHistoryLocalReceiverFailureNamesTheSetting(t *testing.T) {
	tests := []struct {
		name     string
		opts     historyImportOptions
		wantHint bool
	}{
		{
			name:     "environment selected local mode",
			opts:     historyImportOptions{Local: true, LocalEnvKey: envconfig.LegacyKey("LOCAL")},
			wantHint: true,
		},
		{
			name: "local flag selected local mode",
			opts: historyImportOptions{Local: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := historyEnsureLocal
			t.Cleanup(func() { historyEnsureLocal = prev })
			historyEnsureLocal = func(context.Context) (string, error) {
				return "", errors.New("agento11y local receiver is not supported on Windows")
			}

			_, err := historyTarget(context.Background(), tt.opts)
			if err == nil {
				t.Fatal("historyTarget() error = nil, want the receiver failure")
			}
			if !strings.Contains(err.Error(), "start the local receiver") {
				t.Errorf("error = %q, want it to name the failed step", err)
			}
			hasHint := strings.Contains(err.Error(), envconfig.LegacyKey("LOCAL")) && strings.Contains(err.Error(), "--no-local")
			if hasHint != tt.wantHint {
				t.Errorf("error = %q, setting hint = %v, want %v", err, hasHint, tt.wantHint)
			}
		})
	}
}

func TestHistoryImportReportsTheConfigEnvSpelling(t *testing.T) {
	withHistoryNow(t)
	isolateDotenvHome(t)
	writeClaudeHistory(t, "sess-recent", 24*time.Hour)
	writeHistoryConfig(t, "SIGIL_LOCAL=true\n")
	prev := historyEnsureLocal
	t.Cleanup(func() { historyEnsureLocal = prev })
	historyEnsureLocal = func(context.Context) (string, error) {
		return "", errors.New("no receiver here")
	}

	_, stderr, code := runHistory(t, "history", "import", "claude-code", "--all", "--yes")
	if code == nil || *code != 1 {
		t.Fatalf("exit = %v, want 1 (stderr=%q)", code, stderr)
	}
	for _, want := range []string{"start the local receiver", envconfig.LegacyKey("LOCAL"), "--no-local"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr, want)
		}
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
			Agent: history.AgentClaudeCode,
			Since: historyFixedNow.Add(-history.DefaultSinceWindow),
			Yes:   true,
			Local: true,
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

func writeHistoryConfig(t *testing.T, contents string) {
	t.Helper()
	configDir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "agento11y")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.env"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func setHistoryCloudCredentials(t *testing.T, endpoint string) {
	t.Helper()
	t.Setenv(envconfig.PreferredKey("ENDPOINT"), endpoint)
	t.Setenv(envconfig.PreferredKey("AUTH_TENANT_ID"), "tenant")
	t.Setenv(envconfig.PreferredKey("AUTH_TOKEN"), "token")
	prev := historyEnsureLocal
	t.Cleanup(func() { historyEnsureLocal = prev })
	historyEnsureLocal = func(context.Context) (string, error) {
		t.Error("Cloud import tried to start the local receiver")
		return "", errors.New("unexpected local receiver start")
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
