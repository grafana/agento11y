package envconfig

import (
	"bytes"
	"log"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
)

func TestParseBool(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"true", true},
		{"TRUE", true},
		{"1", true},
		{"yes", true},
		{"on", true},
		{" true ", true},
		{"false", false},
		{"0", false},
		{"", false},
		{"random", false},
	}
	for _, tc := range cases {
		if got := ParseBool(tc.in); got != tc.want {
			t.Errorf("ParseBool(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestParseBoolValue covers the whole whitelist, which the README documents as
// the accepted values for AGENTO11Y_LOCAL, and the ok flag callers use to tell
// an unrecognised value from a false one.
func TestParseBoolValue(t *testing.T) {
	cases := []struct {
		in     string
		want   bool
		wantOK bool
	}{
		{in: "1", want: true, wantOK: true},
		{in: "true", want: true, wantOK: true},
		{in: "yes", want: true, wantOK: true},
		{in: "on", want: true, wantOK: true},
		{in: " ON ", want: true, wantOK: true},
		{in: "0", wantOK: true},
		{in: "false", wantOK: true},
		{in: "no", wantOK: true},
		{in: "off", wantOK: true},
		{in: ""},
		{in: "enabled"},
	}
	for _, tc := range cases {
		got, gotOK := ParseBoolValue(tc.in)
		if got != tc.want || gotOK != tc.wantOK {
			t.Errorf("ParseBoolValue(%q) = (%v, %v), want (%v, %v)", tc.in, got, gotOK, tc.want, tc.wantOK)
		}
		// An unrecognised value is the only case where the default shows.
		if got := ParseBoolDefault(tc.in, true); got != (tc.want || !tc.wantOK) {
			t.Errorf("ParseBoolDefault(%q, true) = %v, want %v", tc.in, got, tc.want || !tc.wantOK)
		}
		if got := ParseBoolDefault(tc.in, false); got != tc.want {
			t.Errorf("ParseBoolDefault(%q, false) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestParseIntValue pins the rule IntValue falls back on, which doctor reads to
// name a rejected timeout without restating what counts as valid.
func TestParseIntValue(t *testing.T) {
	cases := []struct {
		in     string
		want   int
		wantOK bool
	}{
		{in: "1500", want: 1500, wantOK: true},
		{in: " 500 ", want: 500, wantOK: true},
		{in: ""},
		{in: "abc"},
		{in: "0"},
		{in: "-1"},
		{in: "1.5"},
	}
	for _, tc := range cases {
		got, gotOK := ParseIntValue(tc.in)
		if got != tc.want || gotOK != tc.wantOK {
			t.Errorf("ParseIntValue(%q) = (%d, %v), want (%d, %v)", tc.in, got, gotOK, tc.want, tc.wantOK)
		}
		// IntValue treats an empty value as "unset" rather than a fault, so it is
		// the one input where the two disagree.
		wantIntValue := 42
		if tc.wantOK {
			wantIntValue = tc.want
		}
		if got := IntValue(nil, "AGENTO11Y_GUARDS_TIMEOUT_MS", tc.in, 42); got != wantIntValue {
			t.Errorf("IntValue(%q, def 42) = %d, want %d", tc.in, got, wantIntValue)
		}
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("SIGIL_TEST_PRESENT", "present")
	t.Setenv("SIGIL_TEST_EMPTY", "")
	if got := EnvOr("SIGIL_TEST_PRESENT", "fallback"); got != "present" {
		t.Errorf("EnvOr(present) = %q, want %q", got, "present")
	}
	if got := EnvOr("SIGIL_TEST_EMPTY", "fallback"); got != "fallback" {
		t.Errorf("EnvOr(empty) = %q, want %q", got, "fallback")
	}
	if got := EnvOr("SIGIL_TEST_MISSING", "fallback"); got != "fallback" {
		t.Errorf("EnvOr(missing) = %q, want %q", got, "fallback")
	}
}

func TestMissingEnvVars(t *testing.T) {
	order := []string{"A", "B", "C"}
	vars := map[string]string{"A": "x", "B": "", "C": "y"}
	got := MissingEnvVars(order, vars)
	want := []string{"B"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MissingEnvVars = %v, want %v", got, want)
	}
}

func TestParseExtraTags(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]string
	}{
		{"", nil},
		{"  ", nil},
		{"a=1", map[string]string{"a": "1"}},
		{"a=1,b=2", map[string]string{"a": "1", "b": "2"}},
		{"a=1, b=2 ", map[string]string{"a": "1", "b": "2"}},
		{"a=,b=2", map[string]string{"b": "2"}},
		{"=1,b=2", map[string]string{"b": "2"}},
		{"justakey", nil},
	}
	for _, tc := range cases {
		got := ParseExtraTags(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseExtraTags(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseAutoTags(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		want        map[AutoTag]bool
		wantUnknown []string
	}{
		{name: "unset", in: ""},
		{name: "whitespace only", in: "   \t "},
		{name: "single name", in: "user", want: map[AutoTag]bool{AutoTagUser: true}},
		{
			name: "two names",
			in:   "user,repo",
			want: map[AutoTag]bool{AutoTagUser: true, AutoTagRepo: true},
		},
		{
			name: "surrounding whitespace and empty entries",
			in:   " user , , repo ,",
			want: map[AutoTag]bool{AutoTagUser: true, AutoTagRepo: true},
		},
		{
			name: "all is shorthand for every name",
			in:   "all",
			want: map[AutoTag]bool{AutoTagUser: true, AutoTagRepo: true, AutoTagBranch: true},
		},
		{
			name: "mixed case is accepted",
			in:   "User,REPO,Branch",
			want: map[AutoTag]bool{AutoTagUser: true, AutoTagRepo: true, AutoTagBranch: true},
		},
		{
			name:        "unknown name is reported and the rest still parse",
			in:          "user,team",
			want:        map[AutoTag]bool{AutoTagUser: true},
			wantUnknown: []string{"team"},
		},
		{
			name:        "only unknown names",
			in:          "Team, squad",
			wantUnknown: []string{"team", "squad"},
		},
		{
			name: "duplicate names collapse",
			in:   "repo,repo,all",
			want: map[AutoTag]bool{AutoTagUser: true, AutoTagRepo: true, AutoTagBranch: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enabled, unknown := ParseAutoTags(tc.in)
			if !reflect.DeepEqual(enabled, tc.want) {
				t.Errorf("enabled = %v, want %v", enabled, tc.want)
			}
			if !reflect.DeepEqual(unknown, tc.wantUnknown) {
				t.Errorf("unknown = %v, want %v", unknown, tc.wantUnknown)
			}
		})
	}
}

// TestAutoTagsAreAliasFamilies pins both automatic-tag variables to the alias
// list: without them the SIGIL_ spelling would not resolve and dotenv would not
// materialize the keys.
func TestAutoTagsAreAliasFamilies(t *testing.T) {
	for _, suffix := range []string{AutoTagsSuffix, AutoTagNamesSuffix} {
		if !slices.Contains(AliasSuffixes, suffix) {
			t.Errorf("%s missing from AliasSuffixes", suffix)
		}
	}
}

func TestExportTimeoutIsAliasFamily(t *testing.T) {
	if !slices.Contains(AliasSuffixes, "EXPORT_TIMEOUT_MS") {
		t.Fatal("EXPORT_TIMEOUT_MS missing from AliasSuffixes")
	}
}

// TestAllAutoTags pins the default set to every supported name: the switch on
// its own resolves all of them, and the allowlist only narrows that.
func TestAllAutoTags(t *testing.T) {
	want := map[AutoTag]bool{AutoTagUser: true, AutoTagRepo: true, AutoTagBranch: true}
	if got := AllAutoTags(); !reflect.DeepEqual(got, want) {
		t.Fatalf("AllAutoTags() = %v, want %v", got, want)
	}
}

// TestResolveGuards drives both entry points over the same cases: ResolveGuards
// reads the process env, and ResolveGuardsWith reads the same values through a
// map-backed Lookup, which is how doctor resolves them from its pre-merge
// snapshot. Sharing the table anchors both to a concrete GuardsConfig, so the
// two cannot drift into agreeing on a wrong answer.
func TestResolveGuards(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    GuardsConfig
		wantLog string
	}{
		{
			name: "defaults_no_env",
			env:  nil,
			want: GuardsConfig{Enabled: false, TimeoutMs: 1500, FailOpen: true},
		},
		{
			name: "explicit_enable_true",
			env:  map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			want: GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
		},
		{
			name: "explicit_enable_yes",
			env:  map[string]string{"SIGIL_GUARDS_ENABLED": "yes"},
			want: GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
		},
		{
			name: "explicit_enable_1",
			env:  map[string]string{"SIGIL_GUARDS_ENABLED": "1"},
			want: GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
		},
		{
			name: "explicit_disable_false",
			env:  map[string]string{"SIGIL_GUARDS_ENABLED": "false"},
			want: GuardsConfig{Enabled: false, TimeoutMs: 1500, FailOpen: true},
		},
		{
			name: "explicit_disable_0",
			env:  map[string]string{"SIGIL_GUARDS_ENABLED": "0"},
			want: GuardsConfig{Enabled: false, TimeoutMs: 1500, FailOpen: true},
		},
		{
			name: "explicit_disable_no",
			env:  map[string]string{"SIGIL_GUARDS_ENABLED": "no"},
			want: GuardsConfig{Enabled: false, TimeoutMs: 1500, FailOpen: true},
		},
		{
			name: "whitespace_enabled",
			env:  map[string]string{"SIGIL_GUARDS_ENABLED": " true "},
			want: GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
		},
		{
			name: "fail_open_disabled",
			env:  map[string]string{"SIGIL_GUARDS_FAIL_OPEN": "false"},
			want: GuardsConfig{Enabled: false, TimeoutMs: 1500, FailOpen: false},
		},
		{
			name: "custom_timeout",
			env:  map[string]string{"SIGIL_GUARDS_TIMEOUT_MS": "500"},
			want: GuardsConfig{Enabled: false, TimeoutMs: 500, FailOpen: true},
		},
		{
			name:    "invalid_timeout_string",
			env:     map[string]string{"SIGIL_GUARDS_TIMEOUT_MS": "abc"},
			want:    GuardsConfig{Enabled: false, TimeoutMs: 1500, FailOpen: true},
			wantLog: `invalid SIGIL_GUARDS_TIMEOUT_MS="abc"`,
		},
		{
			name:    "zero_timeout",
			env:     map[string]string{"SIGIL_GUARDS_TIMEOUT_MS": "0"},
			want:    GuardsConfig{Enabled: false, TimeoutMs: 1500, FailOpen: true},
			wantLog: `invalid SIGIL_GUARDS_TIMEOUT_MS="0"`,
		},
		{
			name:    "negative_timeout",
			env:     map[string]string{"SIGIL_GUARDS_TIMEOUT_MS": "-1"},
			want:    GuardsConfig{Enabled: false, TimeoutMs: 1500, FailOpen: true},
			wantLog: `invalid SIGIL_GUARDS_TIMEOUT_MS="-1"`,
		},
		{
			name: "all_three_set",
			env: map[string]string{
				"SIGIL_GUARDS_ENABLED":    "true",
				"SIGIL_GUARDS_FAIL_OPEN":  "false",
				"SIGIL_GUARDS_TIMEOUT_MS": "2000",
			},
			want: GuardsConfig{Enabled: true, TimeoutMs: 2000, FailOpen: false},
		},
		{
			name:    "invalid_enabled_typo_uses_default",
			env:     map[string]string{"SIGIL_GUARDS_ENABLED": "ture"},
			want:    GuardsConfig{Enabled: false, TimeoutMs: 1500, FailOpen: true},
			wantLog: `invalid SIGIL_GUARDS_ENABLED="ture"`,
		},
		{
			name:    "invalid_fail_open_typo_uses_default",
			env:     map[string]string{"SIGIL_GUARDS_FAIL_OPEN": "fals"},
			want:    GuardsConfig{Enabled: false, TimeoutMs: 1500, FailOpen: true},
			wantLog: `invalid SIGIL_GUARDS_FAIL_OPEN="fals"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, suffix := range []string{"GUARDS_ENABLED", "GUARDS_FAIL_OPEN", "GUARDS_TIMEOUT_MS"} {
				t.Setenv(PreferredKey(suffix), "")
				t.Setenv(LegacyKey(suffix), "")
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			for _, entry := range []struct {
				name    string
				resolve func(*log.Logger) GuardsConfig
			}{
				{"ResolveGuards", ResolveGuards},
				{"ResolveGuardsWith", func(l *log.Logger) GuardsConfig {
					return ResolveGuardsWith(l, mapLookup(tt.env))
				}},
			} {
				var buf bytes.Buffer
				got := entry.resolve(log.New(&buf, "", 0))
				if got != tt.want {
					t.Errorf("%s() = %+v, want %+v", entry.name, got, tt.want)
				}
				if tt.wantLog != "" && !strings.Contains(buf.String(), tt.wantLog) {
					t.Errorf("%s log output = %q, want substring %q", entry.name, buf.String(), tt.wantLog)
				}
				if tt.wantLog == "" && buf.Len() != 0 {
					t.Errorf("%s unexpected log output: %q", entry.name, buf.String())
				}
			}
		})
	}
}

// mapLookup is a Lookup backed by a plain map, standing in for the pre-merge
// env snapshot doctor resolves from.
func mapLookup(env map[string]string) Lookup {
	return func(suffix string) (value, key string, ok bool) {
		return LookupMap(env, suffix)
	}
}

// TestResolveAgentName covers the precedence between the two spellings and the
// fall-back to the adapter's product name. A blank value must not become the
// exported agent_name, otherwise a stray empty variable silently unnames every
// generation.
func TestResolveAgentName(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "unset keeps the default", want: "claude-code"},
		{name: "preferred spelling", env: map[string]string{"AGENTO11Y_AGENT_NAME": "claude-code-e2e"}, want: "claude-code-e2e"},
		{name: "legacy spelling", env: map[string]string{"SIGIL_AGENT_NAME": "legacy-name"}, want: "legacy-name"},
		{
			name: "preferred wins over legacy",
			env: map[string]string{
				"AGENTO11Y_AGENT_NAME": "preferred-name",
				"SIGIL_AGENT_NAME":     "legacy-name",
			},
			want: "preferred-name",
		},
		{
			name: "blank preferred falls through to legacy",
			env: map[string]string{
				"AGENTO11Y_AGENT_NAME": "   ",
				"SIGIL_AGENT_NAME":     "legacy-name",
			},
			want: "legacy-name",
		},
		{
			name: "both blank keeps the default",
			env: map[string]string{
				"AGENTO11Y_AGENT_NAME": " ",
				"SIGIL_AGENT_NAME":     "",
			},
			want: "claude-code",
		},
		{name: "value is trimmed", env: map[string]string{"AGENTO11Y_AGENT_NAME": "  spaced  "}, want: "spaced"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			PinAliasEnvBlank(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			if got := ResolveAgentName("claude-code"); got != tt.want {
				t.Errorf("ResolveAgentName() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolveContentMode covers the process-env wrapper: which spelling supplies
// the mode, which is the one thing the value-level table below cannot reach.
func TestResolveContentMode(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want agento11y.ContentCaptureMode
	}{
		{name: "unset is metadata_only", want: agento11y.ContentCaptureModeMetadataOnly},
		{name: "preferred spelling", env: map[string]string{"AGENTO11Y_CONTENT_CAPTURE_MODE": "full"}, want: agento11y.ContentCaptureModeFull},
		{name: "legacy spelling", env: map[string]string{"SIGIL_CONTENT_CAPTURE_MODE": "no_tool_content"}, want: agento11y.ContentCaptureModeNoToolContent},
		{
			name: "preferred wins over legacy",
			env: map[string]string{
				"AGENTO11Y_CONTENT_CAPTURE_MODE": "full",
				"SIGIL_CONTENT_CAPTURE_MODE":     "no_tool_content",
			},
			want: agento11y.ContentCaptureModeFull,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			PinAliasEnvBlank(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			if got := ResolveContentMode(nil); got != tt.want {
				t.Errorf("ResolveContentMode() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveRedactInput(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    bool
		wantLog string
	}{
		{name: "defaults_on_with_no_env", want: true},
		{name: "blank_value_defaults_on", env: map[string]string{"AGENTO11Y_REDACT_INPUT_MESSAGES": "   "}, want: true},
		{name: "explicit_false_opts_out", env: map[string]string{"AGENTO11Y_REDACT_INPUT_MESSAGES": "false"}, want: false},
		{name: "explicit_0_opts_out", env: map[string]string{"AGENTO11Y_REDACT_INPUT_MESSAGES": "0"}, want: false},
		{name: "explicit_off_opts_out", env: map[string]string{"AGENTO11Y_REDACT_INPUT_MESSAGES": "off"}, want: false},
		{name: "explicit_true_stays_on", env: map[string]string{"AGENTO11Y_REDACT_INPUT_MESSAGES": "true"}, want: true},
		{name: "legacy_spelling_opts_out", env: map[string]string{"SIGIL_REDACT_INPUT_MESSAGES": "false"}, want: false},
		{name: "preferred_wins_over_legacy", env: map[string]string{"AGENTO11Y_REDACT_INPUT_MESSAGES": "true", "SIGIL_REDACT_INPUT_MESSAGES": "false"}, want: true},
		{
			name:    "typo_logs_and_stays_on",
			env:     map[string]string{"AGENTO11Y_REDACT_INPUT_MESSAGES": "flase"},
			want:    true,
			wantLog: `invalid AGENTO11Y_REDACT_INPUT_MESSAGES="flase"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(PreferredKey("REDACT_INPUT_MESSAGES"), "")
			t.Setenv(LegacyKey("REDACT_INPUT_MESSAGES"), "")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			var buf bytes.Buffer
			logger := log.New(&buf, "", 0)
			if got := ResolveRedactInput(logger); got != tt.want {
				t.Errorf("ResolveRedactInput() = %v, want %v", got, tt.want)
			}
			if tt.wantLog != "" && !strings.Contains(buf.String(), tt.wantLog) {
				t.Errorf("log output = %q, want substring %q", buf.String(), tt.wantLog)
			}
			if tt.wantLog == "" && buf.Len() != 0 {
				t.Errorf("unexpected log output: %q", buf.String())
			}
		})
	}
}

func TestLookupEnv(t *testing.T) {
	cases := []struct {
		name      string
		env       map[string]string
		wantValue string
		wantKey   string
		wantOK    bool
	}{
		{name: "unset", wantOK: false},
		{name: "preferred only", env: map[string]string{"AGENTO11Y_ENDPOINT": "p"}, wantValue: "p", wantKey: "AGENTO11Y_ENDPOINT", wantOK: true},
		{name: "legacy only", env: map[string]string{"SIGIL_ENDPOINT": "l"}, wantValue: "l", wantKey: "SIGIL_ENDPOINT", wantOK: true},
		{name: "preferred wins on conflict", env: map[string]string{"AGENTO11Y_ENDPOINT": "p", "SIGIL_ENDPOINT": "l"}, wantValue: "p", wantKey: "AGENTO11Y_ENDPOINT", wantOK: true},
		{name: "blank preferred falls through", env: map[string]string{"AGENTO11Y_ENDPOINT": "   ", "SIGIL_ENDPOINT": "l"}, wantValue: "l", wantKey: "SIGIL_ENDPOINT", wantOK: true},
		{name: "value trimmed", env: map[string]string{"AGENTO11Y_ENDPOINT": "  p  "}, wantValue: "p", wantKey: "AGENTO11Y_ENDPOINT", wantOK: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENTO11Y_ENDPOINT", "")
			t.Setenv("SIGIL_ENDPOINT", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			value, key, ok := LookupEnv("ENDPOINT")
			if value != tc.wantValue || key != tc.wantKey || ok != tc.wantOK {
				t.Errorf("LookupEnv() = (%q, %q, %v), want (%q, %q, %v)", value, key, ok, tc.wantValue, tc.wantKey, tc.wantOK)
			}
			// LookupMap applies the same precedence to a config map, which is
			// what the settings page and the local forwarder read.
			mapValue, mapKey, mapOK := LookupMap(tc.env, "ENDPOINT")
			if mapValue != tc.wantValue || mapKey != tc.wantKey || mapOK != tc.wantOK {
				t.Errorf("LookupMap() = (%q, %q, %v), want (%q, %q, %v)", mapValue, mapKey, mapOK, tc.wantValue, tc.wantKey, tc.wantOK)
			}
		})
	}
}

func TestSetBothEnv(t *testing.T) {
	t.Setenv("AGENTO11Y_ENDPOINT", "")
	t.Setenv("SIGIL_ENDPOINT", "")
	SetBothEnv("ENDPOINT", "https://x")
	if got := Getenv("ENDPOINT"); got != "https://x" {
		t.Errorf("Getenv = %q", got)
	}
	for _, key := range []string{"AGENTO11Y_ENDPOINT", "SIGIL_ENDPOINT"} {
		if got := os.Getenv(key); got != "https://x" {
			t.Errorf("%s = %q, want https://x", key, got)
		}
	}
}

func TestBoolValue(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		def     bool
		want    bool
		wantLog string
	}{
		{name: "empty_uses_default_true", raw: "", def: true, want: true},
		{name: "empty_uses_default_false", raw: "", def: false, want: false},
		{name: "whitespace_uses_default", raw: "   ", def: true, want: true},
		{name: "true", raw: "true", want: true},
		{name: "mixed_case_on", raw: "On", want: true},
		{name: "one", raw: "1", want: true},
		{name: "yes", raw: "yes", want: true},
		{name: "padded_true", raw: "  true ", want: true},
		{name: "false_overrides_default", raw: "false", def: true, want: false},
		{name: "off_overrides_default", raw: "off", def: true, want: false},
		{name: "zero_overrides_default", raw: "0", def: true, want: false},
		{name: "typo_logs_and_uses_default", raw: "ture", def: true, want: true, wantLog: `invalid AGENTO11Y_LOCAL_FORWARD="ture"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := log.New(&buf, "", 0)
			if got := BoolValue(logger, PreferredKey("LOCAL_FORWARD"), tc.raw, tc.def); got != tc.want {
				t.Errorf("BoolValue(%q, def=%v) = %v, want %v", tc.raw, tc.def, got, tc.want)
			}
			if tc.wantLog != "" && !strings.Contains(buf.String(), tc.wantLog) {
				t.Errorf("log output = %q, want substring %q", buf.String(), tc.wantLog)
			}
			if tc.wantLog == "" && buf.Len() != 0 {
				t.Errorf("unexpected log output: %q", buf.String())
			}
		})
	}
}

func TestResolveContentModeValue(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		raw     string
		want    agento11y.ContentCaptureMode
		wantLog string
	}{
		{name: "empty_is_metadata_only", raw: "", want: agento11y.ContentCaptureModeMetadataOnly},
		{name: "whitespace_is_metadata_only", raw: "  ", want: agento11y.ContentCaptureModeMetadataOnly},
		{name: "full", raw: "full", want: agento11y.ContentCaptureModeFull},
		{name: "padded_full", raw: " full ", want: agento11y.ContentCaptureModeFull},
		{name: "no_tool_content", raw: "no_tool_content", want: agento11y.ContentCaptureModeNoToolContent},
		{name: "explicit_default_is_metadata_only", raw: "default", want: agento11y.ContentCaptureModeMetadataOnly},
		{
			name:    "unknown_logs_reported_key",
			key:     LegacyKey("CONTENT_CAPTURE_MODE"),
			raw:     "fully",
			want:    agento11y.ContentCaptureModeMetadataOnly,
			wantLog: `unknown SIGIL_CONTENT_CAPTURE_MODE="fully"`,
		},
		{
			name:    "unknown_without_key_names_preferred",
			raw:     "fully",
			want:    agento11y.ContentCaptureModeMetadataOnly,
			wantLog: `unknown AGENTO11Y_CONTENT_CAPTURE_MODE="fully"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := log.New(&buf, "", 0)
			if got := ResolveContentModeValue(logger, tc.key, tc.raw); got != tc.want {
				t.Errorf("ResolveContentModeValue(%q) = %v, want %v", tc.raw, got, tc.want)
			}
			if tc.wantLog != "" && !strings.Contains(buf.String(), tc.wantLog) {
				t.Errorf("log output = %q, want substring %q", buf.String(), tc.wantLog)
			}
			if tc.wantLog == "" && buf.Len() != 0 {
				t.Errorf("unexpected log output: %q", buf.String())
			}
		})
	}
}

// Local settings use alias-family dotenv precedence across the AGENTO11Y_ and
// SIGIL_ spellings.
func TestAliasSuffixesCoversLocalFamilies(t *testing.T) {
	for _, suffix := range []string{"LOCAL", "LOCAL_FORWARD", "LOCAL_ALLOWED_HOSTS", "THEME"} {
		if !slices.Contains(AliasSuffixes, suffix) {
			t.Fatalf("AliasSuffixes must contain %s", suffix)
		}
	}
}

func TestExpandAliases(t *testing.T) {
	got := ExpandAliases(map[string]string{
		"SIGIL_ENDPOINT":       "https://x",
		"AGENTO11Y_AUTH_TOKEN": "tok",
		"SIGIL_TAGS":           "",
		"SIGIL_THEME":          "light",
		"OTEL_SERVICE_NAME":    "svc",
	})
	want := map[string]string{
		"SIGIL_ENDPOINT":       "https://x",
		"AGENTO11Y_ENDPOINT":   "https://x",
		"AGENTO11Y_AUTH_TOKEN": "tok",
		"SIGIL_AUTH_TOKEN":     "tok",
		"SIGIL_TAGS":           "",
		"AGENTO11Y_TAGS":       "",
		"SIGIL_THEME":          "light",
		"AGENTO11Y_THEME":      "light",
		"OTEL_SERVICE_NAME":    "svc",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExpandAliases() = %v, want %v", got, want)
	}
}

func TestUpdateExistingLegacyAliases(t *testing.T) {
	current := map[string]string{
		"SIGIL_ENDPOINT":   "https://old",
		"SIGIL_AUTH_TOKEN": "old-token",
		"SIGIL_THEME":      "dark",
	}
	got := UpdateExistingLegacyAliases(current, map[string]string{
		"AGENTO11Y_ENDPOINT":   "https://new",
		"AGENTO11Y_AUTH_TOKEN": "new-token",
		"SIGIL_AUTH_TOKEN":     "keep explicit value",
		"AGENTO11Y_TAGS":       "team=ai",
		"OTEL_SERVICE_NAME":    "svc",
	})
	want := map[string]string{
		"AGENTO11Y_ENDPOINT":   "https://new",
		"SIGIL_ENDPOINT":       "https://new",
		"AGENTO11Y_AUTH_TOKEN": "new-token",
		"SIGIL_AUTH_TOKEN":     "keep explicit value",
		"AGENTO11Y_TAGS":       "team=ai",
		"OTEL_SERVICE_NAME":    "svc",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("UpdateExistingLegacyAliases() = %v, want %v", got, want)
	}
}
