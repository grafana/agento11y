package login

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/huh"
	"github.com/grafana/agento11y/plugins/agento11y/internal/doctor"
	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// nonTTYStdin returns a file that is guaranteed not to be a terminal.
// We use the read end of a pipe so .Fd() is valid but term.IsTerminal
// returns false.
func nonTTYStdin(t *testing.T) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	return r
}

// writeDotenv creates a config.env file in a fresh temp dir and returns
// its path. An empty contents string skips file creation so callers can
// exercise the missing-file branch.
func writeDotenv(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.env")
	if contents == "" {
		return path
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write dotenv: %v", err)
	}
	return path
}

// clearSeededEnv wipes both spellings of every family loadSeeds reads from
// the process env. Tests need this because the host shell may have some of
// them exported (the developer running `go test` is the same user who
// uses sigil), which would otherwise leak into table cases that intend
// to exercise the "env unset" path.
func clearSeededEnv(t *testing.T) {
	t.Helper()
	for _, suffix := range seededSuffixes {
		t.Setenv(envconfig.PreferredKey(suffix), "")
		t.Setenv(envconfig.LegacyKey(suffix), "")
	}
}

// TestRun_NoTTYReturnsErrNotInteractive covers the only branch of Run that
// is reachable without driving huh's TUI: when stdin is not a terminal we
// must bail with ErrNotInteractive and leave the dotenv file untouched.
// The interactive form itself is exercised by the cmd/sigil end-to-end
// tests that stub loginRun, not here.
func TestRun_NoTTYReturnsErrNotInteractive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.env")

	err := Run(context.Background(), RunOpts{
		ConfigPath: path,
		Stdin:      nonTTYStdin(t),
	})
	if !errors.Is(err, ErrNotInteractive) {
		t.Fatalf("Run err = %v, want %v", err, ErrNotInteractive)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("dotenv was written despite ErrNotInteractive: %v", statErr)
	}
}

// TestLoadSeeds covers the precedence rules loadSeeds enforces:
// process env wins over the dotenv file (the bug fix — launcher
// auto-prompts must pre-fill from SIGIL_* vars already in the user's
// shell instead of showing empty fields), the file is the fallback,
// and whitespace-only env values mirror dotenv.ApplyEnv by being
// treated as unset.
func TestLoadSeeds(t *testing.T) {
	cases := []struct {
		name string
		file string            // dotenv contents; "" means no file on disk
		env  map[string]string // process env; every key from seededKeys is asserted
		want map[string]string // "" means key must be absent/empty from seeds
	}{
		{
			name: "process env overlays dotenv file",
			file: "SIGIL_ENDPOINT=https://stale.example.com\n" +
				"SIGIL_AUTH_TENANT_ID=stale-tenant\n",
			env: map[string]string{
				"SIGIL_ENDPOINT":       "https://fresh.example.com",
				"SIGIL_AUTH_TENANT_ID": "fresh-tenant",
			},
			want: map[string]string{
				"SIGIL_ENDPOINT":                    "https://fresh.example.com",
				"SIGIL_AUTH_TENANT_ID":              "fresh-tenant",
				"SIGIL_AUTH_TOKEN":                  "",
				"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT": "",
			},
		},
		{
			name: "dotenv file used when env unset",
			file: "SIGIL_ENDPOINT=https://file.example.com\n" +
				"SIGIL_AUTH_TENANT_ID=file-tenant\n" +
				"SIGIL_AUTH_TOKEN=file-token\n",
			want: map[string]string{
				"SIGIL_ENDPOINT":                    "https://file.example.com",
				"SIGIL_AUTH_TENANT_ID":              "file-tenant",
				"SIGIL_AUTH_TOKEN":                  "file-token",
				"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT": "",
			},
		},
		{
			name: "whitespace env does not overlay dotenv",
			file: "SIGIL_ENDPOINT=https://file.example.com\n",
			env:  map[string]string{"SIGIL_ENDPOINT": "   "},
			want: map[string]string{
				"SIGIL_ENDPOINT":                    "https://file.example.com",
				"SIGIL_AUTH_TENANT_ID":              "",
				"SIGIL_AUTH_TOKEN":                  "",
				"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT": "",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearSeededEnv(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			seeds := loadSeeds(writeDotenv(t, c.file), nil)
			for k, want := range c.want {
				if got := seeds[k]; got != want {
					t.Errorf("seeds[%q] = %q, want %q", k, got, want)
				}
			}
		})
	}
}

func TestRequireURL(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"empty", "", true},
		{"missing scheme", "sigil.example.com", true},
		{"unsupported scheme", "ftp://sigil.example.com", true},
		{"missing host", "https://", true},
		{"valid http", "http://localhost:8080", false},
		{"valid https", "https://sigil.example.com/path", false},
		{"trims whitespace", "  https://sigil.example.com  ", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := requireURL(c.in)
			if (err != nil) != c.wantErr {
				t.Errorf("requireURL(%q) err = %v, wantErr = %v", c.in, err, c.wantErr)
			}
		})
	}
}

func TestNormalizeContentMode(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "metadata_only"},
		{"   ", "metadata_only"},
		{"garbage", "metadata_only"},
		{"metadata_only", "metadata_only"},
		{"FULL", "full"},
		{"  no_tool_content  ", "no_tool_content"},
		{"full_with_metadata_spans", "full_with_metadata_spans"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := normalizeContentMode(c.in); got != c.want {
				t.Errorf("normalizeContentMode(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestSeedGuards(t *testing.T) {
	cases := []struct {
		name     string
		enabled  string
		failOpen string
		want     string
	}{
		{"unset defaults off", "", "", guardsOff},
		{"disabled", "false", "true", guardsOff},
		{"enabled defaults fail-open", "true", "", guardsOpen},
		{"enabled fail-open explicit", "1", "yes", guardsOpen},
		{"enabled fail-closed", "on", "false", guardsClosed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := seedGuards(c.enabled, c.failOpen); got != c.want {
				t.Errorf("seedGuards(%q, %q) = %q, want %q", c.enabled, c.failOpen, got, c.want)
			}
		})
	}
}

// TestSeedAutoTagNames pins what the checklist starts with. A saved allowlist
// preselects the names it holds. No saved allowlist preselects every supported
// name. A saved allowlist that holds no supported name preselects nothing: that
// config attaches no tags, so the checklist must not show it as all three.
func TestSeedAutoTagNames(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"unset preselects every name", "", []string{"user", "repo", "branch"}},
		{"all preselects every name", "all", []string{"user", "repo", "branch"}},
		{"saved list is preselected in order", "branch, user", []string{"user", "branch"}},
		{"unsupported names are dropped", "team,user", []string{"user"}},
		{"only unsupported names preselects nothing", "team", nil},
		{"separators alone preselect nothing", ",", nil},
		{"blank preselects every name", "   ", []string{"user", "repo", "branch"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := seedAutoTagNames(c.raw); !reflect.DeepEqual(got, c.want) {
				t.Errorf("seedAutoTagNames(%q) = %v, want %v", c.raw, got, c.want)
			}
		})
	}
}

// TestAutoTagNamesValue pins the persisted allowlist: normalized to
// AutoTagOrder, and empty when the selection covers every supported name.
func TestAutoTagNamesValue(t *testing.T) {
	cases := []struct {
		name     string
		selected []string
		want     string
	}{
		{"every name writes no list", []string{"branch", "user", "repo"}, ""},
		{"click order is normalized", []string{"branch", "user"}, "user,branch"},
		{"one name", []string{"repo"}, "repo"},
		{"nothing selected writes no list", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := autoTagNamesValue(c.selected); got != c.want {
				t.Errorf("autoTagNamesValue(%v) = %q, want %q", c.selected, got, c.want)
			}
		})
	}
}

// TestValidateAutoTagNames pins that the checklist cannot be submitted empty:
// with the switch on and no name selected, nothing would be attached, which is
// what answering No already means.
func TestValidateAutoTagNames(t *testing.T) {
	if err := validateAutoTagNames(nil); err == nil {
		t.Error("validateAutoTagNames(nil) = nil, want an error")
	}
	if err := validateAutoTagNames([]string{"user"}); err != nil {
		t.Errorf("validateAutoTagNames([user]) = %v, want nil", err)
	}
}

// TestAutoTagOptions pins that the checklist offers every supported name, in
// AutoTagOrder, that each one carries a label describing where its value comes
// from, and that the saved names are ticked.
func TestAutoTagOptions(t *testing.T) {
	options := autoTagOptions([]string{"user", "branch"})

	var values []string
	for _, opt := range options {
		values = append(values, opt.Value)
		if !strings.Contains(opt.Key, opt.Value+" — ") {
			t.Errorf("option %q label %q does not describe the value", opt.Value, opt.Key)
		}
	}
	if want := []string{"user", "repo", "branch"}; !reflect.DeepEqual(values, want) {
		t.Fatalf("option values = %v, want %v", values, want)
	}

	// huh keeps the ticked state in an unexported field, so compare against
	// options built the way a ticked one is built.
	want := []huh.Option[string]{
		huh.NewOption(options[0].Key, "user").Selected(true),
		huh.NewOption(options[1].Key, "repo"),
		huh.NewOption(options[2].Key, "branch").Selected(true),
	}
	if !reflect.DeepEqual(options, want) {
		t.Errorf("options = %+v, want %+v", options, want)
	}
}

func TestValidateTags(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"empty ok", "", false},
		{"whitespace ok", "   ", false},
		{"single pair", "team=ai", false},
		{"multiple pairs", "team=ai,project=demo", false},
		{"trailing comma tolerated", "team=ai,", false},
		{"empty value rejected", "team=", true},
		{"whitespace value rejected", "team=  ", true},
		{"missing equals", "team", true},
		{"empty key", "=ai", true},
		{"whitespace key", "  =ai", true},
		{"one bad among good", "team=ai,bogus", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateTags(c.in)
			if (err != nil) != c.wantErr {
				t.Errorf("validateTags(%q) err = %v, wantErr = %v", c.in, err, c.wantErr)
			}
		})
	}
}

func TestValidateGuardTimeout(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"empty ok", "", false},
		{"whitespace ok", "  ", false},
		{"positive", "1500", false},
		{"padded positive", " 2000 ", false},
		{"zero", "0", true},
		{"negative", "-1", true},
		{"non-numeric", "1.5", true},
		{"word", "soon", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateGuardTimeout(c.in)
			if (err != nil) != c.wantErr {
				t.Errorf("validateGuardTimeout(%q) err = %v, wantErr = %v", c.in, err, c.wantErr)
			}
		})
	}
}

// TestBuildUpdates pins the dotenv write rules for the optional preferences:
// the four capture keys are written only when the form ran; content capture
// mode and the guard-enabled flag are then always written; tags and OTLP
// delete on empty; guard timeout and fail mode only appear when guards are
// enabled.
func TestBuildUpdates(t *testing.T) {
	cases := []struct {
		name string
		in   formValues
		want map[string]string
	}{
		{
			// The form never ran, so the capture keys stay out of the update
			// map and WriteDotenv leaves whatever the file holds. The values
			// below are seeds, and a seed can come from an AGENTO11Y_*
			// variable exported in the current shell.
			name: "promptless run leaves the capture keys alone",
			in: formValues{
				endpoint:    "https://sigil.example.com",
				tenantID:    "123",
				token:       "glc_abc",
				contentMode: "full",
				tags:        "session=demo",
				guards:      guardsOpen,
			},
			want: map[string]string{
				"SIGIL_ENDPOINT":                    "https://sigil.example.com",
				"SIGIL_AUTH_TENANT_ID":              "123",
				"SIGIL_AUTH_TOKEN":                  "glc_abc",
				"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT": "",
			},
		},
		{
			name: "credentials only, guards off",
			in: formValues{
				endpoint:        "https://sigil.example.com",
				tenantID:        "123",
				token:           "glc_abc",
				contentMode:     "metadata_only",
				guards:          guardsOff,
				capturePrompted: true,
				// guardTimeout is ignored while guards are off
				guardTimeout: "1500",
			},
			want: map[string]string{
				"SIGIL_ENDPOINT":                     "https://sigil.example.com",
				"SIGIL_AUTH_TENANT_ID":               "123",
				"SIGIL_AUTH_TOKEN":                   "glc_abc",
				"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT":  "",
				"SIGIL_CONTENT_CAPTURE_MODE":         "metadata_only",
				"SIGIL_TAGS":                         "",
				"SIGIL_AUTO_CODING_AGENT_TAGS":       "false",
				"SIGIL_AUTO_CODING_AGENT_TAGS_NAMES": "",
				"SIGIL_GUARDS_ENABLED":               "false",
			},
		},
		{
			name: "stale content mode normalised, tags trimmed",
			in: formValues{
				endpoint:        "https://sigil.example.com",
				tenantID:        "123",
				token:           "glc_abc",
				contentMode:     "bogus",
				tags:            "  team=ai  ",
				guards:          guardsOff,
				capturePrompted: true,
			},
			want: map[string]string{
				"SIGIL_ENDPOINT":                     "https://sigil.example.com",
				"SIGIL_AUTH_TENANT_ID":               "123",
				"SIGIL_AUTH_TOKEN":                   "glc_abc",
				"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT":  "",
				"SIGIL_CONTENT_CAPTURE_MODE":         "metadata_only",
				"SIGIL_TAGS":                         "team=ai",
				"SIGIL_AUTO_CODING_AGENT_TAGS":       "false",
				"SIGIL_AUTO_CODING_AGENT_TAGS_NAMES": "",
				"SIGIL_GUARDS_ENABLED":               "false",
			},
		},
		{
			name: "guards fail-open with timeout",
			in: formValues{
				endpoint:        "https://sigil.example.com",
				tenantID:        "123",
				token:           "glc_abc",
				otelEndpoint:    "https://otlp.example.com",
				contentMode:     "full",
				guards:          guardsOpen,
				guardTimeout:    " 2000 ",
				capturePrompted: true,
			},
			want: map[string]string{
				"SIGIL_ENDPOINT":                     "https://sigil.example.com",
				"SIGIL_AUTH_TENANT_ID":               "123",
				"SIGIL_AUTH_TOKEN":                   "glc_abc",
				"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT":  "https://otlp.example.com",
				"SIGIL_CONTENT_CAPTURE_MODE":         "full",
				"SIGIL_TAGS":                         "",
				"SIGIL_AUTO_CODING_AGENT_TAGS":       "false",
				"SIGIL_AUTO_CODING_AGENT_TAGS_NAMES": "",
				"SIGIL_GUARDS_ENABLED":               "true",
				"SIGIL_GUARDS_FAIL_OPEN":             "true",
				"SIGIL_GUARDS_TIMEOUT_MS":            "2000",
			},
		},
		{
			name: "guards fail-closed, blank timeout clears key",
			in: formValues{
				endpoint:        "https://sigil.example.com",
				tenantID:        "123",
				token:           "glc_abc",
				contentMode:     "no_tool_content",
				guards:          guardsClosed,
				guardTimeout:    "   ",
				capturePrompted: true,
			},
			want: map[string]string{
				"SIGIL_ENDPOINT":                     "https://sigil.example.com",
				"SIGIL_AUTH_TENANT_ID":               "123",
				"SIGIL_AUTH_TOKEN":                   "glc_abc",
				"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT":  "",
				"SIGIL_CONTENT_CAPTURE_MODE":         "no_tool_content",
				"SIGIL_TAGS":                         "",
				"SIGIL_AUTO_CODING_AGENT_TAGS":       "false",
				"SIGIL_AUTO_CODING_AGENT_TAGS_NAMES": "",
				"SIGIL_GUARDS_ENABLED":               "true",
				"SIGIL_GUARDS_FAIL_OPEN":             "false",
				"SIGIL_GUARDS_TIMEOUT_MS":            "", // empty deletes via WriteDotenv
			},
		},
		{
			// Every supported name selected, so no allowlist is persisted: the
			// switch alone already means all of them, and an empty value deletes
			// a narrower list from a previous run.
			name: "automatic tags on with every name",
			in: formValues{
				endpoint:        "https://sigil.example.com",
				tenantID:        "123",
				token:           "glc_abc",
				contentMode:     "metadata_only",
				guards:          guardsOff,
				autoTags:        true,
				autoTagNames:    []string{"user", "repo", "branch"},
				capturePrompted: true,
			},
			want: map[string]string{
				"SIGIL_ENDPOINT":                     "https://sigil.example.com",
				"SIGIL_AUTH_TENANT_ID":               "123",
				"SIGIL_AUTH_TOKEN":                   "glc_abc",
				"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT":  "",
				"SIGIL_CONTENT_CAPTURE_MODE":         "metadata_only",
				"SIGIL_TAGS":                         "",
				"SIGIL_AUTO_CODING_AGENT_TAGS":       "true",
				"SIGIL_AUTO_CODING_AGENT_TAGS_NAMES": "",
				"SIGIL_GUARDS_ENABLED":               "false",
			},
		},
		{
			name: "automatic tags narrowed to two names",
			in: formValues{
				endpoint:    "https://sigil.example.com",
				tenantID:    "123",
				token:       "glc_abc",
				contentMode: "metadata_only",
				guards:      guardsOff,
				autoTags:    true,
				// Click order, which the persisted value normalises.
				autoTagNames:    []string{"branch", "user"},
				capturePrompted: true,
			},
			want: map[string]string{
				"SIGIL_ENDPOINT":                     "https://sigil.example.com",
				"SIGIL_AUTH_TENANT_ID":               "123",
				"SIGIL_AUTH_TOKEN":                   "glc_abc",
				"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT":  "",
				"SIGIL_CONTENT_CAPTURE_MODE":         "metadata_only",
				"SIGIL_TAGS":                         "",
				"SIGIL_AUTO_CODING_AGENT_TAGS":       "true",
				"SIGIL_AUTO_CODING_AGENT_TAGS_NAMES": "user,branch",
				"SIGIL_GUARDS_ENABLED":               "false",
			},
		},
		{
			// The switch is off, so buildUpdates deletes the allowlist instead of
			// keeping it. Doctor warns about an allowlist the switch cannot use,
			// and a later run that turns the switch on from the shell would read
			// it.
			name: "automatic tags off deletes the allowlist",
			in: formValues{
				endpoint:        "https://sigil.example.com",
				tenantID:        "123",
				token:           "glc_abc",
				contentMode:     "metadata_only",
				guards:          guardsOff,
				autoTagNames:    []string{"user"},
				capturePrompted: true,
			},
			want: map[string]string{
				"SIGIL_ENDPOINT":                     "https://sigil.example.com",
				"SIGIL_AUTH_TENANT_ID":               "123",
				"SIGIL_AUTH_TOKEN":                   "glc_abc",
				"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT":  "",
				"SIGIL_CONTENT_CAPTURE_MODE":         "metadata_only",
				"SIGIL_TAGS":                         "",
				"SIGIL_AUTO_CODING_AGENT_TAGS":       "false",
				"SIGIL_AUTO_CODING_AGENT_TAGS_NAMES": "",
				"SIGIL_GUARDS_ENABLED":               "false",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Managed values are written and deleted under both spellings.
			want := envconfig.ExpandAliases(c.want)
			if got := buildUpdates(c.in); !reflect.DeepEqual(got, want) {
				t.Errorf("buildUpdates() =\n%v\nwant\n%v", got, want)
			}
		})
	}
}

func TestAllowEmptyURL(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"empty", "", false},
		{"whitespace only", "   ", false},
		{"valid url", "https://otlp.example", false},
		{"non-empty bad url", "not a url", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := allowEmptyURL(c.in)
			if (err != nil) != c.wantErr {
				t.Errorf("allowEmptyURL(%q) err = %v, wantErr = %v", c.in, err, c.wantErr)
			}
		})
	}
}

// probeCall records one credential check so tests can assert both the values
// the probe received and the moment it ran.
type probeCall struct {
	endpoint      string
	tenant        string
	token         string
	insecure      bool
	configWritten bool
}

// stubProbe returns a ProbeFunc that always answers res and appends its
// arguments to calls. configPath is stat-ed on every call so a test can prove
// the check ran before the dotenv file was written.
func stubProbe(res *doctor.ProbeResult, configPath string, calls *[]probeCall) ProbeFunc {
	return func(_ context.Context, endpoint, tenant, token string, insecure bool) *doctor.ProbeResult {
		_, err := os.Stat(configPath)
		*calls = append(*calls, probeCall{
			endpoint:      endpoint,
			tenant:        tenant,
			token:         token,
			insecure:      insecure,
			configWritten: err == nil,
		})
		return res
	}
}

// runNonInteractive drives Run the way `agento11y login --endpoint --tenant
// --token` does: every required value arrives as a flag, so no terminal and
// no form are involved.
func runNonInteractive(t *testing.T, opts RunOpts) (string, error) {
	t.Helper()
	clearSeededEnv(t)
	envconfig.PinAliasEnvBlank(t)
	var stderr bytes.Buffer
	opts.Stdin = nonTTYStdin(t)
	opts.Stderr = &stderr
	err := Run(context.Background(), opts)
	return stderr.String(), err
}

// TestRun_VerifiesBeforeWriting covers every outcome of the credential check:
// what the user is told, and whether the dotenv file is written.
func TestRun_VerifiesBeforeWriting(t *testing.T) {
	const (
		endpoint = "https://agento11y.example.com"
		tenant   = "123"
		token    = "glc_secret"
	)
	cases := []struct {
		name       string
		res        *doctor.ProbeResult
		skipVerify bool
		assumeYes  bool
		wantErr    error
		wantProbe  bool
		wantWrite  bool
		wantStderr []string
		denyStderr []string
	}{
		{
			name:       "accepted",
			res:        &doctor.ProbeResult{URL: endpoint + "/api/v1/generations:export", StatusCode: 200, OK: true},
			wantProbe:  true,
			wantWrite:  true,
			wantStderr: []string{"accepted these credentials"},
		},
		{
			name:      "unauthorized",
			res:       &doctor.ProbeResult{URL: endpoint + "/api/v1/generations:export", StatusCode: 401, Message: "endpoint rejected auth — token likely missing sigil:write scope"},
			wantErr:   ErrNotVerified,
			wantProbe: true,
			wantStderr: []string{
				"rejected these credentials (HTTP 401)",
				`Tenant ID "123"`,
				"auth token",
				"sigil:write",
			},
		},
		{
			name:      "forbidden",
			res:       &doctor.ProbeResult{URL: endpoint + "/api/v1/generations:export", StatusCode: 403},
			wantErr:   ErrNotVerified,
			wantProbe: true,
			wantStderr: []string{
				"rejected these credentials (HTTP 403)",
				`Tenant ID "123"`,
				"sigil:write",
			},
		},
		{
			name:      "other status",
			res:       &doctor.ProbeResult{URL: endpoint + "/api/v1/generations:export", StatusCode: 502},
			wantErr:   ErrNotVerified,
			wantProbe: true,
			wantStderr: []string{
				"answered HTTP 502",
				endpoint + "/api/v1/generations:export",
			},
			denyStderr: []string{"sigil:write"},
		},
		{
			// doctor leaves a 4xx other than 401/403 healthy, because the
			// probe's `{}` body can draw a benign 400 or 415 from an endpoint
			// that validates the body before auth. Login is about to write
			// these values to disk, so it reports the status and asks first.
			name:      "benign 4xx still blocks an unattended save",
			res:       &doctor.ProbeResult{URL: endpoint + "/api/v1/generations:export", StatusCode: 400},
			wantErr:   ErrNotVerified,
			wantProbe: true,
			wantStderr: []string{
				"answered HTTP 400",
				endpoint + "/api/v1/generations:export",
			},
		},
		{
			name:      "transport failure",
			res:       &doctor.ProbeResult{URL: endpoint, Message: "dial tcp 10.0.0.1:443: connect: connection refused"},
			wantErr:   ErrNotVerified,
			wantProbe: true,
			wantStderr: []string{
				"Could not reach " + endpoint,
				"connection refused",
			},
		},
		{
			name:      "timeout",
			res:       &doctor.ProbeResult{URL: endpoint, Message: "context deadline exceeded (Client.Timeout exceeded while awaiting headers)"},
			wantErr:   ErrNotVerified,
			wantProbe: true,
			wantStderr: []string{
				"Could not reach " + endpoint,
				"context deadline exceeded",
			},
		},
		{
			name:      "save override accepted with --yes",
			res:       &doctor.ProbeResult{URL: endpoint, StatusCode: 403},
			assumeYes: true,
			wantProbe: true,
			wantWrite: true,
			wantStderr: []string{
				"rejected these credentials (HTTP 403)",
			},
		},
		{
			name:       "verification skipped",
			skipVerify: true,
			wantWrite:  true,
			denyStderr: []string{"accepted these credentials"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.env")
			var calls []probeCall
			probe := stubProbe(c.res, path, &calls)
			if !c.wantProbe {
				probe = func(context.Context, string, string, string, bool) *doctor.ProbeResult {
					t.Fatal("probe must not run")
					return nil
				}
			}

			stderr, err := runNonInteractive(t, RunOpts{
				ConfigPath: path,
				Endpoint:   endpoint,
				TenantID:   tenant,
				Token:      token,
				SkipVerify: c.skipVerify,
				AssumeYes:  c.assumeYes,
				Probe:      probe,
			})
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("Run err = %v, want %v", err, c.wantErr)
			}
			if c.wantProbe {
				if len(calls) != 1 {
					t.Fatalf("probe ran %d times, want 1", len(calls))
				}
				got := calls[0]
				if got.endpoint != endpoint || got.tenant != tenant || got.token != token {
					t.Errorf("probe args = %+v, want endpoint %q tenant %q token %q", got, endpoint, tenant, token)
				}
				if got.configWritten {
					t.Error("dotenv file already existed when the probe ran; the check must come first")
				}
			}
			_, statErr := os.Stat(path)
			if written := statErr == nil; written != c.wantWrite {
				t.Errorf("dotenv written = %v, want %v", written, c.wantWrite)
			}
			for _, want := range c.wantStderr {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr missing %q:\n%s", want, stderr)
				}
			}
			for _, deny := range c.denyStderr {
				if strings.Contains(stderr, deny) {
					t.Errorf("stderr must not contain %q:\n%s", deny, stderr)
				}
			}
		})
	}
}

// TestRun_ProbeResolvesInsecureLikeExporter pins that the check inherits the
// INSECURE family, so a scheme-less endpoint is probed over http exactly as
// the exporter would send it.
func TestRun_ProbeResolvesInsecureLikeExporter(t *testing.T) {
	for _, c := range []struct {
		name string
		file string
		env  map[string]string
		want bool
	}{
		{name: "unset", want: false},
		{name: "from dotenv", file: "SIGIL_INSECURE=true\n", want: true},
		{name: "from process env", env: map[string]string{"AGENTO11Y_INSECURE": "true"}, want: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			clearSeededEnv(t)
			envconfig.PinAliasEnvBlank(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			path := writeDotenv(t, c.file)
			var calls []probeCall
			var stderr bytes.Buffer
			err := Run(context.Background(), RunOpts{
				ConfigPath: path,
				Stdin:      nonTTYStdin(t),
				Stderr:     &stderr,
				Endpoint:   "https://agento11y.example.com",
				TenantID:   "123",
				Token:      "glc_secret",
				Probe:      stubProbe(&doctor.ProbeResult{StatusCode: 200, OK: true}, path, &calls),
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(calls) != 1 {
				t.Fatalf("probe ran %d times, want 1", len(calls))
			}
			if calls[0].insecure != c.want {
				t.Errorf("probe insecure = %v, want %v", calls[0].insecure, c.want)
			}
		})
	}
}

// TestRun_ExplicitValues covers the flag path: explicit values beat every
// seed, all three required values remove the need for a terminal, and a
// missing one still refuses to run without a prompt.
func TestRun_ExplicitValues(t *testing.T) {
	const existing = "SIGIL_ENDPOINT=https://old.example\n" +
		"SIGIL_AUTH_TENANT_ID=111\n" +
		"SIGIL_AUTH_TOKEN=old-token\n"

	cases := []struct {
		name string
		file string
		env  map[string]string
		opts RunOpts
		// wantErr is a sentinel the error must wrap; wantErrText is a
		// substring the message must name, used where the complaint points
		// at a flag rather than a sentinel.
		wantErr     error
		wantErrText string
		want        map[string]string
	}{
		{
			name: "flags override existing configuration",
			file: existing,
			opts: RunOpts{
				Endpoint: "https://new.example",
				TenantID: "222",
				Token:    "new-token",
			},
			want: map[string]string{
				"AGENTO11Y_ENDPOINT":       "https://new.example",
				"AGENTO11Y_AUTH_TENANT_ID": "222",
				"AGENTO11Y_AUTH_TOKEN":     "new-token",
				"SIGIL_ENDPOINT":           "https://new.example",
			},
		},
		{
			name: "otlp endpoint is persisted",
			opts: RunOpts{
				Endpoint:     "https://new.example",
				TenantID:     "222",
				Token:        "new-token",
				OTLPEndpoint: "https://otlp.example",
			},
			want: map[string]string{
				"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": "https://otlp.example",
				"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT":     "https://otlp.example",
			},
		},
		{
			name: "token flag overrides the seeded token",
			file: existing,
			opts: RunOpts{
				Endpoint: "https://new.example",
				TenantID: "222",
				Token:    "kept-explicitly",
			},
			want: map[string]string{"AGENTO11Y_AUTH_TOKEN": "kept-explicitly"},
		},
		{
			// A run that never opens the form leaves every capture setting as
			// it is on disk.
			name: "capture settings on disk survive a promptless run",
			file: "SIGIL_CONTENT_CAPTURE_MODE=full\n" +
				"SIGIL_TAGS=team=ai\n" +
				"SIGIL_GUARDS_ENABLED=true\n" +
				"SIGIL_GUARDS_FAIL_OPEN=false\n" +
				"SIGIL_GUARDS_TIMEOUT_MS=2500\n",
			opts: RunOpts{
				Endpoint: "https://new.example",
				TenantID: "222",
				Token:    "new-token",
			},
			want: map[string]string{
				"SIGIL_CONTENT_CAPTURE_MODE": "full",
				"SIGIL_TAGS":                 "team=ai",
				"SIGIL_GUARDS_ENABLED":       "true",
				"SIGIL_GUARDS_FAIL_OPEN":     "false",
				"SIGIL_GUARDS_TIMEOUT_MS":    "2500",
			},
		},
		{
			// The launcher writes `agento11y claude --tag session=demo` into
			// AGENTO11Y_TAGS before it auto-fires login, and a user can
			// export AGENTO11Y_CONTENT_CAPTURE_MODE in their shell. Neither
			// is a saved preference, so a run that never shows the form must
			// not persist them.
			name: "capture settings from the shell are not persisted",
			env: map[string]string{
				"AGENTO11Y_TAGS":                 "session=demo",
				"AGENTO11Y_CONTENT_CAPTURE_MODE": "full",
			},
			opts: RunOpts{
				Endpoint: "https://new.example",
				TenantID: "222",
				Token:    "new-token",
			},
			want: map[string]string{
				"SIGIL_TAGS":                     "",
				"AGENTO11Y_TAGS":                 "",
				"SIGIL_CONTENT_CAPTURE_MODE":     "",
				"AGENTO11Y_CONTENT_CAPTURE_MODE": "",
			},
		},
		{
			name:    "missing token still needs a terminal",
			file:    "",
			opts:    RunOpts{Endpoint: "https://new.example", TenantID: "222"},
			wantErr: ErrNotInteractive,
		},
		{
			name:    "missing endpoint still needs a terminal",
			opts:    RunOpts{TenantID: "222", Token: "new-token"},
			wantErr: ErrNotInteractive,
		},
		{
			name:        "malformed endpoint flag is rejected before prompting",
			opts:        RunOpts{Endpoint: "not-a-url", TenantID: "222", Token: "new-token"},
			wantErrText: "--endpoint",
		},
		{
			name:        "malformed otlp endpoint flag is rejected",
			opts:        RunOpts{Endpoint: "https://new.example", TenantID: "222", Token: "t", OTLPEndpoint: "not-a-url"},
			wantErrText: "--otlp-endpoint",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearSeededEnv(t)
			envconfig.PinAliasEnvBlank(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			path := writeDotenv(t, c.file)
			opts := c.opts
			opts.ConfigPath = path
			opts.Stdin = nonTTYStdin(t)
			opts.Stderr = io.Discard
			opts.SkipVerify = true

			err := Run(context.Background(), opts)
			if c.wantErrText != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErrText) {
					t.Fatalf("Run err = %v, want a complaint naming %s", err, c.wantErrText)
				}
				return
			}
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("Run err = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			saved := dotenv.LoadDotenv(path, nil)
			for k, want := range c.want {
				if saved[k] != want {
					t.Errorf("saved[%q] = %q, want %q", k, saved[k], want)
				}
			}
		})
	}
}

// TestPrintNextStep pins which diagnostic command each outcome names.
func TestPrintNextStep(t *testing.T) {
	cases := []struct {
		name    string
		outcome verifyOutcome
		want    []string
		deny    []string
	}{
		{
			name:    "verified",
			outcome: verifyPassed,
			want:    []string{"agento11y doctor", "if the data does not appear"},
			deny:    []string{"--probe"},
		},
		{
			name:    "skipped",
			outcome: verifySkipped,
			want:    []string{"Verification was skipped", "agento11y doctor"},
			deny:    []string{"--probe"},
		},
		{
			name:    "saved after a failed check",
			outcome: verifyOverridden,
			want:    []string{"did not accept these credentials", "agento11y doctor"},
			deny:    []string{"--probe"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			printNextStep(&buf, c.outcome)
			got := buf.String()
			if !strings.Contains(got, "agento11y claude") {
				t.Errorf("next step must still name the launchers:\n%s", got)
			}
			for _, want := range c.want {
				if !strings.Contains(got, want) {
					t.Errorf("output missing %q:\n%s", want, got)
				}
			}
			for _, deny := range c.deny {
				if strings.Contains(got, deny) {
					t.Errorf("output must not contain %q:\n%s", deny, got)
				}
			}
		})
	}
}

// TestRun_NextStepOnlyWhenAsked pins that the automatic login the launchers
// fire (ShowNextStep false) still saves and verifies, but stays quiet about
// what to run next — the launcher is about to start the agent anyway.
func TestRun_NextStepOnlyWhenAsked(t *testing.T) {
	for _, show := range []bool{true, false} {
		t.Run(fmt.Sprintf("ShowNextStep=%v", show), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.env")
			var calls []probeCall
			stderr, err := runNonInteractive(t, RunOpts{
				ConfigPath:   path,
				ShowNextStep: show,
				Endpoint:     "https://agento11y.example.com",
				TenantID:     "123",
				Token:        "glc_secret",
				Probe:        stubProbe(&doctor.ProbeResult{StatusCode: 200, OK: true}, path, &calls),
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(calls) != 1 {
				t.Fatalf("probe ran %d times, want 1", len(calls))
			}
			if _, statErr := os.Stat(path); statErr != nil {
				t.Fatalf("dotenv not written: %v", statErr)
			}
			if got := strings.Contains(stderr, "agento11y doctor"); got != show {
				t.Errorf("next-step hint printed = %v, want %v:\n%s", got, show, stderr)
			}
		})
	}
}
