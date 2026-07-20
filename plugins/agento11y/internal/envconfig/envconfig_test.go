package envconfig

import (
	"bytes"
	"log"
	"os"
	"reflect"
	"strings"
	"testing"
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

			var buf bytes.Buffer
			logger := log.New(&buf, "", 0)
			got := ResolveGuards(logger)
			if got != tt.want {
				t.Errorf("ResolveGuards() = %+v, want %+v", got, tt.want)
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

func TestExpandAliases(t *testing.T) {
	got := ExpandAliases(map[string]string{
		"SIGIL_ENDPOINT":       "https://x",
		"AGENTO11Y_AUTH_TOKEN": "tok",
		"SIGIL_TAGS":           "",
		"OTEL_SERVICE_NAME":    "svc",
	})
	want := map[string]string{
		"SIGIL_ENDPOINT":       "https://x",
		"AGENTO11Y_ENDPOINT":   "https://x",
		"AGENTO11Y_AUTH_TOKEN": "tok",
		"SIGIL_AUTH_TOKEN":     "tok",
		"SIGIL_TAGS":           "",
		"AGENTO11Y_TAGS":       "",
		"OTEL_SERVICE_NAME":    "svc",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExpandAliases() = %v, want %v", got, want)
	}
}
