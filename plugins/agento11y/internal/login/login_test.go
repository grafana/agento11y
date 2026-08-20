package login

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/grafana/agento11y/plugins/agento11y/internal/doctor"
	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/skills"
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

	_, err := Run(context.Background(), RunOpts{
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
		{
			// Login writes back every value it seeds and no field can edit the
			// OTLP headers, so a one-off shell export must not be saved.
			name: "otlp headers are seeded from the file, not the shell",
			file: `OTEL_EXPORTER_OTLP_HEADERS="Authorization=Basic ZmlsZQ=="` + "\n",
			env:  map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "Authorization=Basic c2hlbGw="},
			want: map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "Authorization=Basic ZmlsZQ=="},
		},
		{
			// The saved stack pre-fills the question, so a re-run is one Enter.
			name: "the stack is seeded from the file, not the shell",
			file: stackURLKey + "=https://file.grafana.net\n",
			env:  map[string]string{stackURLKey: "https://shell.grafana.net"},
			want: map[string]string{stackURLKey: "https://file.grafana.net"},
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

// TestNormalizePastedBlock covers the paste field being a single-line input:
// the terminal hands the block over with every newline replaced by a space,
// and only the keys the loader accepts may start a new line.
func TestNormalizePastedBlock(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "a flattened block is split again",
			in:   "AGENTO11Y_ENDPOINT=https://api.example.invalid AGENTO11Y_AUTH_TOKEN=glc_test",
			want: "AGENTO11Y_ENDPOINT=https://api.example.invalid\nAGENTO11Y_AUTH_TOKEN=glc_test",
		},
		{
			name: "a block that kept its newlines is unchanged",
			in:   "AGENTO11Y_ENDPOINT=https://api.example.invalid\nAGENTO11Y_AUTH_TOKEN=glc_test",
			want: "AGENTO11Y_ENDPOINT=https://api.example.invalid\nAGENTO11Y_AUTH_TOKEN=glc_test",
		},
		{
			// Authorization and dGVzdA look like keys but the loader does not
			// accept them, so the value survives the flattening.
			name: "a header value is not split",
			in:   `AGENTO11Y_AUTH_TOKEN=glc_test OTEL_EXPORTER_OTLP_HEADERS="Authorization=Basic dGVzdA=="`,
			want: "AGENTO11Y_AUTH_TOKEN=glc_test\n" + `OTEL_EXPORTER_OTLP_HEADERS="Authorization=Basic dGVzdA=="`,
		},
		{
			name: "a leading comment becomes its own line",
			in:   "# copied from Grafana AGENTO11Y_AUTH_TOKEN=glc_test",
			want: "# copied from Grafana\nAGENTO11Y_AUTH_TOKEN=glc_test",
		},
		{
			name: "an export prefix stays with its key",
			in:   "export AGENTO11Y_ENDPOINT=https://api.example.invalid export AGENTO11Y_AUTH_TOKEN=glc_test",
			want: "export AGENTO11Y_ENDPOINT=https://api.example.invalid\nexport AGENTO11Y_AUTH_TOKEN=glc_test",
		},
		{
			name: "text with no assignment is left alone",
			in:   "nothing to see here",
			want: "nothing to see here",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizePastedBlock(c.in); got != c.want {
				t.Errorf("normalizePastedBlock(%q) =\n%q\nwant\n%q", c.in, got, c.want)
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
			// The OTLP Basic-auth user is the stack ID, which can differ from
			// the ingest tenant ID, so the pasted header is persisted as it came.
			name: "pasted OTLP headers are persisted unchanged",
			in: formValues{
				endpoint:     "https://sigil.example.com",
				tenantID:     "123",
				token:        "glc_abc",
				otelEndpoint: "https://otlp.example.com",
				otlpHeaders:  "Authorization=Basic c3RhY2s=",
			},
			want: map[string]string{
				"SIGIL_ENDPOINT":                    "https://sigil.example.com",
				"SIGIL_AUTH_TENANT_ID":              "123",
				"SIGIL_AUTH_TOKEN":                  "glc_abc",
				"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT": "https://otlp.example.com",
				"OTEL_EXPORTER_OTLP_HEADERS":        "Authorization=Basic c3RhY2s=",
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
			// Every run writes OTEL_EXPORTER_OTLP_HEADERS, so a case that
			// says nothing about it expects the key deleted.
			want := maps.Clone(c.want)
			if _, ok := want["OTEL_EXPORTER_OTLP_HEADERS"]; !ok {
				want["OTEL_EXPORTER_OTLP_HEADERS"] = ""
			}
			// Managed values are written and deleted under both spellings.
			want = envconfig.ExpandAliases(want)
			if got := buildUpdates(c.in); !reflect.DeepEqual(got, want) {
				t.Errorf("buildUpdates() =\n%v\nwant\n%v", got, want)
			}
		})
	}
}

// TestApplyPaste covers every block shape login accepts: the one the setup
// page renders, the OpenTelemetry tile, dotenv syntax, legacy spellings, and
// keys the launcher cannot use. Flags outrank all of them.
func TestApplyPaste(t *testing.T) {
	cases := []struct {
		name       string
		block      string
		fixed      fixedValues
		seed       formValues
		want       formValues
		wantFilled pasteFilled
	}{
		{
			name: "the block from the coding-agent page",
			block: "AGENTO11Y_ENDPOINT=https://api.example.invalid\n" +
				"AGENTO11Y_PROTOCOL=http\n" +
				"AGENTO11Y_AUTH_MODE=basic\n" +
				"AGENTO11Y_AUTH_TENANT_ID=123\n" +
				"AGENTO11Y_AUTH_TOKEN=glc_test\n" +
				"OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp.example.invalid\n" +
				`OTEL_EXPORTER_OTLP_HEADERS="Authorization=Basic dGVzdA=="` + "\n",
			want: formValues{
				endpoint:          "https://api.example.invalid",
				tenantID:          "123",
				token:             "glc_test",
				otelEndpoint:      "https://otlp.example.invalid",
				otlpHeaders:       "Authorization=Basic dGVzdA==",
				otlpHeadersPasted: true,
			},
			wantFilled: pasteFilled{endpoint: true, tenantID: true, token: true, otelEndpoint: true, otlpHeaders: true},
		},
		{
			// The same block after a single-line paste field replaced every
			// newline with a space, which is what login now receives.
			name: "the same block with its newlines flattened",
			block: "AGENTO11Y_ENDPOINT=https://api.example.invalid " +
				"AGENTO11Y_PROTOCOL=http " +
				"AGENTO11Y_AUTH_MODE=basic " +
				"AGENTO11Y_AUTH_TENANT_ID=123 " +
				"AGENTO11Y_AUTH_TOKEN=glc_test " +
				"OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp.example.invalid " +
				`OTEL_EXPORTER_OTLP_HEADERS="Authorization=Basic dGVzdA=="`,
			want: formValues{
				endpoint:          "https://api.example.invalid",
				tenantID:          "123",
				token:             "glc_test",
				otelEndpoint:      "https://otlp.example.invalid",
				otlpHeaders:       "Authorization=Basic dGVzdA==",
				otlpHeadersPasted: true,
			},
			wantFilled: pasteFilled{endpoint: true, tenantID: true, token: true, otelEndpoint: true, otlpHeaders: true},
		},
		{
			// A leading comment would otherwise swallow the whole block once
			// the newline after it is gone.
			name:       "a flattened block behind a comment",
			block:      "# copied from Grafana AGENTO11Y_AUTH_TOKEN=glc_test",
			want:       formValues{token: "glc_test"},
			wantFilled: pasteFilled{token: true},
		},
		{
			// The OpenTelemetry tile in Grafana Cloud hands out these two
			// keys and nothing else.
			name: "the OpenTelemetry tile block",
			block: "OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp.example.invalid\n" +
				`OTEL_EXPORTER_OTLP_HEADERS="Authorization=Basic c3RhY2s="` + "\n",
			want: formValues{
				otelEndpoint:      "https://otlp.example.invalid",
				otlpHeaders:       "Authorization=Basic c3RhY2s=",
				otlpHeadersPasted: true,
			},
			wantFilled: pasteFilled{otelEndpoint: true, otlpHeaders: true},
		},
		{
			// A user pointing login at their own collector must not get a
			// Grafana Cloud token written next to it.
			name: "--otlp-endpoint keeps the pasted headers out",
			block: "OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp.example.invalid\n" +
				`OTEL_EXPORTER_OTLP_HEADERS="Authorization=Basic Y2xvdWQ="` + "\n" +
				"AGENTO11Y_AUTH_TOKEN=glc_test\n",
			fixed:      fixedValues{otelEndpoint: "https://my-own-collector.example"},
			seed:       formValues{otelEndpoint: "https://my-own-collector.example"},
			want:       formValues{otelEndpoint: "https://my-own-collector.example", token: "glc_test"},
			wantFilled: pasteFilled{token: true},
		},
		{
			name:       "export prefix, quotes and a trailing comment",
			block:      `export AGENTO11Y_ENDPOINT="https://example.invalid" # copied from Grafana`,
			want:       formValues{endpoint: "https://example.invalid"},
			wantFilled: pasteFilled{endpoint: true},
		},
		{
			name: "legacy spellings fill the same fields",
			block: "SIGIL_ENDPOINT=https://legacy.example\n" +
				"SIGIL_AUTH_TENANT_ID=123\n" +
				"SIGIL_AUTH_TOKEN=glc_legacy\n" +
				"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT=https://legacy-otlp.example\n",
			want: formValues{
				endpoint:     "https://legacy.example",
				tenantID:     "123",
				token:        "glc_legacy",
				otelEndpoint: "https://legacy-otlp.example",
			},
			wantFilled: pasteFilled{endpoint: true, tenantID: true, token: true, otelEndpoint: true},
		},
		{
			name: "the preferred spelling wins inside one block",
			block: "SIGIL_ENDPOINT=https://legacy.example\n" +
				"AGENTO11Y_ENDPOINT=https://preferred.example\n",
			want:       formValues{endpoint: "https://preferred.example"},
			wantFilled: pasteFilled{endpoint: true},
		},
		{
			name:       "a flag outranks the paste",
			block:      "AGENTO11Y_ENDPOINT=https://old.example\n",
			fixed:      fixedValues{endpoint: "https://new.example"},
			seed:       formValues{endpoint: "https://new.example"},
			want:       formValues{endpoint: "https://new.example"},
			wantFilled: pasteFilled{},
		},
		{
			// A seed is the current configuration, which is exactly what the
			// paste is meant to replace.
			name:       "the paste replaces a seeded value",
			block:      "AGENTO11Y_ENDPOINT=https://pasted.example\n",
			seed:       formValues{endpoint: "https://seeded.example"},
			want:       formValues{endpoint: "https://pasted.example"},
			wantFilled: pasteFilled{endpoint: true},
		},
		{
			name: "keys the launcher cannot use are dropped",
			block: "AGENTO11Y_AUTH_TOKEN=glc_test\n" +
				"OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf\n" +
				"UNRELATED_SECRET=do-not-load\n",
			want:       formValues{token: "glc_test"},
			wantFilled: pasteFilled{token: true},
		},
		{
			name:       "an empty block changes nothing",
			block:      "   \n",
			seed:       formValues{endpoint: "https://seeded.example"},
			want:       formValues{endpoint: "https://seeded.example"},
			wantFilled: pasteFilled{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.seed
			filled := applyPaste(&got, c.fixed, c.block)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("applyPaste values = %+v, want %+v", got, c.want)
			}
			if filled != c.wantFilled {
				t.Errorf("applyPaste filled = %+v, want %+v", filled, c.wantFilled)
			}
		})
	}
}

// TestStackURLIsSavedButIsNotTheEndpoint keeps the two hosts apart. The stack
// is saved, under one key and one spelling, so a re-run can pre-fill the
// question. The ingest endpoint comes from the paste and no key mixes them up.
func TestStackURLIsSavedButIsNotTheEndpoint(t *testing.T) {
	const stack = "https://mystack.grafana.net"
	origin, err := stackOrigin("mystack.grafana.net")
	if err != nil {
		t.Fatalf("stackOrigin: %v", err)
	}
	if origin != stack {
		t.Fatalf("stackOrigin = %q, want %q", origin, stack)
	}

	v := formValues{stackURL: origin}
	applyPaste(&v, fixedValues{}, "AGENTO11Y_ENDPOINT=https://agento11y-prod-eu-west-2.grafana.net\n"+
		"AGENTO11Y_AUTH_TENANT_ID=123\nAGENTO11Y_AUTH_TOKEN=glc_test\n")

	saved := buildUpdates(v)
	for _, key := range []string{"AGENTO11Y_ENDPOINT", "SIGIL_ENDPOINT"} {
		if saved[key] != "https://agento11y-prod-eu-west-2.grafana.net" {
			t.Errorf("saved[%q] = %q, want the pasted ingest endpoint", key, saved[key])
		}
	}
	if saved[stackURLKey] != stack {
		t.Errorf("saved[%q] = %q, want %q", stackURLKey, saved[stackURLKey], stack)
	}
	if _, ok := saved["SIGIL_STACK_URL"]; ok {
		t.Error("the stack should be written under the preferred spelling alone")
	}
	for key, value := range saved {
		if key != stackURLKey && strings.Contains(value, "mystack.grafana.net") {
			t.Errorf("saved[%q] = %q carries the stack origin", key, value)
		}
	}

	// A promptless run seeds the stack from the file and must leave it alone,
	// not delete a value it never asked about.
	if _, ok := buildUpdates(formValues{})[stackURLKey]; ok {
		t.Errorf("an empty stack should not be written at all")
	}
}

func TestValidatePastedBlock(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		fixed   fixedValues
		wantErr string // substring the message must name; "" means no error
	}{
		{name: "empty box is a skip", in: "  \n"},
		{name: "one credential is enough", in: "AGENTO11Y_AUTH_TOKEN=glc_test"},
		{
			// What the single-line paste field actually receives: the terminal
			// replaces the block's newlines with spaces.
			name: "one line carrying several keys",
			in:   "AGENTO11Y_ENDPOINT=https://api.example.invalid AGENTO11Y_AUTH_TENANT_ID=123 AGENTO11Y_AUTH_TOKEN=glc_test",
		},
		{
			// The inner `=` of a header value must not read as a second
			// assignment.
			name: "a header value keeps its spaces",
			in:   `OTEL_EXPORTER_OTLP_HEADERS="Authorization=Basic dGVzdA=="`,
		},
		{
			name:    "a block with no credentials is rejected",
			in:      "UNRELATED_SECRET=do-not-load\n",
			wantErr: "no credentials",
		},
		{
			// The setup page renders a value it does not have yet as <…>, and
			// the paste replaces the field that would have caught it.
			name:    "a placeholder token is rejected",
			in:      "AGENTO11Y_ENDPOINT=https://api.example.invalid\nAGENTO11Y_AUTH_TOKEN=<create a token above>\n",
			wantErr: "placeholder",
		},
		{
			name:    "an endpoint that is not a URL is rejected",
			in:      "AGENTO11Y_ENDPOINT=api.example.invalid\n",
			wantErr: "endpoint",
		},
		{
			// --endpoint outranks the block, so the block's own endpoint is never
			// read and must not stand between the user and the token.
			name: "a flag-overridden value is not validated",
			in: "AGENTO11Y_ENDPOINT=<AI Observability API URL>\n" +
				"AGENTO11Y_AUTH_TOKEN=glc_test\n",
			fixed: fixedValues{endpoint: "https://good.example"},
		},
		{
			name: "a complete block passes the field validators",
			in: "AGENTO11Y_ENDPOINT=https://api.example.invalid\n" +
				"AGENTO11Y_AUTH_TENANT_ID=123\n" +
				"AGENTO11Y_AUTH_TOKEN=glc_test\n" +
				"OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp.example.invalid\n" +
				`OTEL_EXPORTER_OTLP_HEADERS='Authorization=Basic dGVzdA=='` + "\n",
		},
		{
			// Accepted by the dotenv loader, consumed by nothing: the
			// launcher hardcodes HTTP and Basic.
			name:    "protocol and auth mode alone fill nothing",
			in:      "AGENTO11Y_PROTOCOL=http\nAGENTO11Y_AUTH_MODE=basic\n",
			wantErr: "no credentials",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validatePastedBlock(c.fixed, c.in)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("validatePastedBlock(%q) = %v, want no error", c.in, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("validatePastedBlock(%q) = %v, want a message naming %q", c.in, err, c.wantErr)
			}
		})
	}
}

func TestStackOrigin(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "bare host", in: "mystack.grafana.net", want: "https://mystack.grafana.net"},
		{name: "origin", in: "https://mystack.grafana.net", want: "https://mystack.grafana.net"},
		{name: "trailing slash", in: "https://mystack.grafana.net/", want: "https://mystack.grafana.net"},
		{
			name: "deep link inside the stack",
			in:   "https://mystack.grafana.net/a/grafana-agento11y-app/conversations",
			want: "https://mystack.grafana.net",
		},
		{name: "surrounding whitespace", in: "  mystack.grafana.net ", want: "https://mystack.grafana.net"},
		// Someone copying the address bar drops the scheme but keeps the path.
		{
			name: "bare host with a path",
			in:   "mystack.grafana.net/a/grafana-agento11y-app/setup-coding-agent",
			want: "https://mystack.grafana.net",
		},
		{name: "port is kept", in: "https://mystack.grafana.net:8443/test", want: "https://mystack.grafana.net:8443"},
		{name: "query and fragment", in: "https://mystack.grafana.net/a/foo?x=1#frag", want: "https://mystack.grafana.net"},
		// A host is case-insensitive; the printed link should not shout.
		{name: "uppercase is lowercased", in: "MyStack.Grafana.Net", want: "https://mystack.grafana.net"},
		{name: "uppercase scheme", in: "HTTPS://mystack.grafana.net", want: "https://mystack.grafana.net"},
		// A Grafana on this machine serves plain HTTP, so the bare form and
		// the spelled-out one have to agree.
		{name: "loopback host defaults to http", in: "localhost:3000", want: "http://localhost:3000"},
		{name: "loopback url", in: "http://localhost:3000", want: "http://localhost:3000"},
		{name: "loopback deep link", in: "http://localhost:3000/a/grafana-agento11y-app/conversations", want: "http://localhost:3000"},
		{name: "loopback address", in: "127.0.0.1:3000", want: "http://127.0.0.1:3000"},
		{name: "empty", in: "   ", wantErr: true},
		{name: "spaces inside", in: "not a stack", wantErr: true},
		{name: "unsupported scheme", in: "ftp://mystack.grafana.net", wantErr: true},
		{name: "scheme without a host", in: "https://", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := stackOrigin(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("stackOrigin(%q) = %q, want an error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("stackOrigin(%q): %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("stackOrigin(%q) = %q, want %q", c.in, got, c.want)
			}
			if err := validateStack(c.in); err != nil {
				t.Errorf("validateStack(%q) = %v, want no error", c.in, err)
			}
		})
	}
}

// TestStackList pins the order the stack question answers itself in: the saved
// answer, then gcx's stacks, each host once. The first entry is what the select
// opens on and what the input is pre-filled with, so this order is the whole
// behaviour of the step.
func TestStackList(t *testing.T) {
	cases := []struct {
		name    string
		origins []string
		saved   string
		want    []string
	}{
		{
			name: "nothing known",
			want: []string{},
		},
		{
			name:  "saved answer, no gcx",
			saved: "https://alpha.grafana.net",
			want:  []string{"https://alpha.grafana.net"},
		},
		{
			name:    "gcx stacks, nothing saved: gcx order kept",
			origins: []string{"https://alpha.grafana.net", "https://beta.grafana.net"},
			want:    []string{"https://alpha.grafana.net", "https://beta.grafana.net"},
		},
		{
			name:    "saved stack gcx knows leads, and is not repeated",
			origins: []string{"https://alpha.grafana.net", "https://beta.grafana.net"},
			saved:   "https://beta.grafana.net",
			want:    []string{"https://beta.grafana.net", "https://alpha.grafana.net"},
		},
		{
			name:    "saved stack gcx does not know leads",
			origins: []string{"https://alpha.grafana.net"},
			saved:   "https://own.example.com",
			want:    []string{"https://own.example.com", "https://alpha.grafana.net"},
		},
		{
			name:    "saved stack is the only gcx stack",
			origins: []string{"https://alpha.grafana.net"},
			saved:   "https://alpha.grafana.net",
			want:    []string{"https://alpha.grafana.net"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stackList(tc.origins, tc.saved); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("stackList(%v, %q) = %v, want %v", tc.origins, tc.saved, got, tc.want)
			}
		})
	}
}

// TestStackListDoesNotMutateItsInput pins that ordering around the saved stack
// leaves the slice gcxStacks returned alone.
func TestStackListDoesNotMutateItsInput(t *testing.T) {
	for _, saved := range []string{"", "https://own.example.com", "https://beta.grafana.net"} {
		origins := []string{"https://alpha.grafana.net", "https://beta.grafana.net"}
		before := append([]string(nil), origins...)

		stackList(origins, saved)

		if !reflect.DeepEqual(origins, before) {
			t.Errorf("saved %q: origins = %v, want %v unchanged", saved, origins, before)
		}
	}
}

// TestStackOptions pins when the question becomes a list. One URL is not a list:
// the pre-filled field says the same thing in one row, so stackOptions returns
// nothing and promptStack falls through to it.
func TestStackOptions(t *testing.T) {
	cases := []struct {
		name   string
		stacks []string
		want   []string
	}{
		{"no stacks", nil, nil},
		{"one stack is not a list", []string{"https://alpha.grafana.net"}, nil},
		{
			name:   "two stacks plus the row that opens the field",
			stacks: []string{"https://alpha.grafana.net", "https://beta.grafana.net"},
			want:   []string{"https://alpha.grafana.net", "https://beta.grafana.net", manualStackChoice},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			options := stackOptions(tc.stacks)
			var values []string
			for _, opt := range options {
				values = append(values, opt.Value)
				if opt.Key == "" {
					t.Errorf("option %q has no label", opt.Value)
				}
			}
			if !reflect.DeepEqual(values, tc.want) {
				t.Errorf("option values = %v, want %v", values, tc.want)
			}
		})
	}
}

func TestShouldOfferLocal(t *testing.T) {
	tests := []struct {
		name      string
		askUser   bool
		requested bool
		supported bool
		want      bool
	}{
		{name: "interactive first run", askUser: true, requested: true, supported: true, want: true},
		{name: "promptless run", requested: true, supported: true},
		{name: "destination already set", askUser: true, supported: true},
		{name: "receiver unsupported", askUser: true, requested: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldOfferLocal(tc.askUser, tc.requested, tc.supported); got != tc.want {
				t.Errorf("shouldOfferLocal() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDestinationQuestionShape(t *testing.T) {
	if destinationTitle != "Where should sessions go?" {
		t.Errorf("destination title = %q", destinationTitle)
	}
	options := destinationOptions()
	if len(options) != 2 {
		t.Fatalf("destination options = %d, want 2", len(options))
	}
	want := []huh.Option[bool]{
		huh.NewOption("Local only", true),
		huh.NewOption("Grafana Cloud", false),
	}
	for i := range want {
		if options[i].Key != want[i].Key || options[i].Value != want[i].Value {
			t.Errorf("option %d = %q, %v, want %q, %v", i, options[i].Key, options[i].Value, want[i].Key, want[i].Value)
		}
	}
}

// TestDestinationFormFirstFrame renders the question the way a user first sees
// it. Both options have to be on that frame, with the cursor on Local only: huh
// scrolls the option viewport to whatever the select was bound to when its
// options were set, so binding the value after the options leaves the cursor
// above the visible rows and the first frame shows Grafana Cloud alone.
func TestDestinationFormFirstFrame(t *testing.T) {
	localMode := true
	form := destinationForm(&localMode)
	form.Init()
	view := form.View()

	var cursorLine string
	for line := range strings.SplitSeq(view, "\n") {
		if strings.Contains(line, "›") {
			cursorLine = line
		}
	}
	for _, want := range []string{"Local only", "Grafana Cloud"} {
		if !strings.Contains(view, want) {
			t.Errorf("first frame does not show %q:\n%s", want, view)
		}
	}
	if !strings.Contains(cursorLine, "Local only") {
		t.Errorf("cursor line = %q, want it on Local only:\n%s", cursorLine, view)
	}
}

// TestStackQuestionShape composes the decisions promptStack makes, which it
// cannot be tested through because it runs the forms. It pins the fallback in
// particular: with no gcx installed, gcxStacks reports nothing and the question
// is the input this flow always had, empty on a first run.
func TestStackQuestionShape(t *testing.T) {
	cases := []struct {
		name       string
		origins    []string
		saved      string
		wantSelect bool
		wantValue  string
		wantDesc   string
	}{
		{
			name:      "no gcx, first run",
			wantValue: "",
			wantDesc:  "e.g. mystack.grafana.net",
		},
		{
			name:      "no gcx, re-run",
			saved:     "https://alpha.grafana.net",
			wantValue: "https://alpha.grafana.net",
			wantDesc:  "Press Enter to keep it, or type another stack.",
		},
		{
			name:      "one gcx stack, first run",
			origins:   []string{"https://alpha.grafana.net"},
			wantValue: "https://alpha.grafana.net",
			wantDesc:  "Found in your gcx config. Press Enter to use it.",
		},
		{
			name:      "one gcx stack, re-run on it",
			origins:   []string{"https://alpha.grafana.net"},
			saved:     "https://alpha.grafana.net",
			wantValue: "https://alpha.grafana.net",
			wantDesc:  "Press Enter to keep it, or type another stack.",
		},
		{
			name:       "one gcx stack, re-run on another",
			origins:    []string{"https://alpha.grafana.net"},
			saved:      "https://own.example.com",
			wantSelect: true,
		},
		{
			name:       "several gcx stacks",
			origins:    []string{"https://alpha.grafana.net", "https://beta.grafana.net"},
			wantSelect: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stacks := stackList(tc.origins, tc.saved)
			if got := stackOptions(stacks) != nil; got != tc.wantSelect {
				t.Fatalf("select = %v, want %v", got, tc.wantSelect)
			}
			if tc.wantSelect {
				return
			}
			value, from := stackPrefill(stacks, tc.saved)
			if value != tc.wantValue {
				t.Errorf("pre-filled value = %q, want %q", value, tc.wantValue)
			}
			if got := stackDescription(from); got != tc.wantDesc {
				t.Errorf("description = %q, want %q", got, tc.wantDesc)
			}
		})
	}
}

// TestStackAnswer pins how the form's two values resolve into one URL: a listed
// row answers for itself, the typed field wins once the row that opens it was
// chosen, and an empty field is a request for the list back rather than an answer.
func TestStackAnswer(t *testing.T) {
	const listed = "https://alpha.grafana.net"
	cases := []struct {
		name   string
		choice string
		typed  string
		want   string
		wantOK bool
	}{
		{"a listed URL", listed, "", listed, true},
		{"a listed URL, ignoring a stale typed value", listed, "https://own.example.com", listed, true},
		{"typed", manualStackChoice, "https://own.example.com", "https://own.example.com", true},
		{"typed is normalised like a listed one", manualStackChoice, "MyStack.Grafana.net/", "https://mystack.grafana.net", true},
		{"typed loopback is kept, unlike in the list", manualStackChoice, "localhost:3000", "http://localhost:3000", true},
		{"empty field asks for the list again", manualStackChoice, "", "", false},
		{"blank field asks for the list again", manualStackChoice, "   ", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := stackAnswer(tc.choice, tc.typed)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("stackAnswer(%q, %q) = %q, %v, want %q, %v",
					tc.choice, tc.typed, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestStackOptionsSeparatesTheLastRow pins the divider between the URLs and the
// row that opens the field, which without it reads as one more URL. It belongs to
// the label above it, so that URL's own value has to stay clean: it is what gets
// saved.
func TestStackOptionsSeparatesTheLastRow(t *testing.T) {
	stacks := []string{"https://alpha.grafana.net", "https://beta-is-the-longest.grafana.net"}
	options := stackOptions(stacks)
	if len(options) != 3 {
		t.Fatalf("got %d options, want 3", len(options))
	}

	last := options[len(options)-2]
	lines := strings.Split(last.Key, "\n")
	if len(lines) != 3 || lines[0] != stacks[1] || lines[2] != "" {
		t.Errorf("label of the last URL = %q, want the URL, a rule, then a blank line", last.Key)
	}
	// The rule spans the column, so it can never be wider than a row already on
	// screen.
	if got, want := lipgloss.Width(lines[1]), lipgloss.Width(stacks[1]); got != want {
		t.Errorf("rule width = %d, want %d, the longest URL", got, want)
	}
	if last.Value != stacks[1] {
		t.Errorf("value of the last URL = %q, want the plain origin", last.Value)
	}

	manual := options[len(options)-1]
	if manual.Value != manualStackChoice {
		t.Errorf("value of the last row = %q, want the sentinel", manual.Value)
	}
	if strings.Contains(manual.Key, "\n") {
		t.Errorf("label of the last row = %q, want one line", manual.Key)
	}
	// The row opens with the prompt glyph of the field it leads to. huh draws its
	// own cursor on the selected row, so that row reads "› > or another URL…";
	// using huh's glyph here as well would make it "› ›".
	if !strings.Contains(manual.Key, "> ") || !strings.Contains(manual.Key, "or another URL") {
		t.Errorf("label of the last row = %q, want the prompt glyph and what it does", manual.Key)
	}
	if strings.Contains(manual.Key, "\u203a") {
		t.Errorf("label of the last row = %q, want huh's cursor glyph left to huh", manual.Key)
	}
}

// TestStackKeyMap pins that Esc means "back to the list" and nothing else. huh
// binds it to no key of its own here, and the one key that abandons login has to
// stay Ctrl-C alone: an Esc that quits would throw away the whole form.
func TestStackKeyMap(t *testing.T) {
	km := stackKeyMap()
	if keys := km.Input.Prev.Keys(); !slices.Contains(keys, "esc") || !slices.Contains(keys, "shift+tab") {
		t.Errorf("Input.Prev keys = %v, want esc added to huh's own shift+tab", keys)
	}
	if got := km.Input.Prev.Help().Key; got != "esc" {
		t.Errorf("Input.Prev help key = %q, want esc: it is what the footer shows", got)
	}
	if slices.Contains(km.Quit.Keys(), "esc") {
		t.Errorf("Quit keys = %v, want esc to navigate rather than abandon login", km.Quit.Keys())
	}
	if !slices.Contains(km.Quit.Keys(), "ctrl+c") {
		t.Errorf("Quit keys = %v, want ctrl+c", km.Quit.Keys())
	}
}

// TestManualStackChoiceCannotBeAnOrigin pins the property the sentinel relies on:
// huh puts the cursor on the option whose value equals the bound one, so a
// sentinel a URL could equal would either steal the cursor or be selected by one.
func TestManualStackChoiceCannotBeAnOrigin(t *testing.T) {
	if _, err := stackOrigin(manualStackChoice); err == nil {
		t.Errorf("stackOrigin(%q) = nil error, want the sentinel to be unusable as a URL", manualStackChoice)
	}
	if manualStackChoice == "" {
		t.Error("manualStackChoice is empty, so a first run would open the list on it")
	}
}

// TestStackPrefill pins what the free-form input opens with once the list is
// out of the picture: the saved answer, else the single stack gcx named, else
// nothing.
func TestStackPrefill(t *testing.T) {
	cases := []struct {
		name     string
		stacks   []string
		saved    string
		want     string
		wantFrom stackSource
	}{
		{"nothing known", nil, "", "", stackSourceNone},
		{"one gcx stack", []string{"https://alpha.grafana.net"}, "", "https://alpha.grafana.net", stackSourceGcx},
		{"saved answer, no gcx", nil, "https://own.example.com", "https://own.example.com", stackSourceSaved},
		{
			// stackList collapses these into one entry.
			name:     "saved answer is the gcx stack",
			stacks:   []string{"https://alpha.grafana.net"},
			saved:    "https://alpha.grafana.net",
			want:     "https://alpha.grafana.net",
			wantFrom: stackSourceSaved,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, from := stackPrefill(tc.stacks, tc.saved)
			if got != tc.want || from != tc.wantFrom {
				t.Errorf("stackPrefill(%v, %q) = %q, %d, want %q, %d",
					tc.stacks, tc.saved, got, from, tc.want, tc.wantFrom)
			}
		})
	}
}

// TestStackDescription pins that the field says where its pre-filled value came
// from, and that an empty field falls back to the example it always showed.
func TestStackDescription(t *testing.T) {
	cases := []struct {
		from stackSource
		want string
	}{
		{stackSourceGcx, "Found in your gcx config. Press Enter to use it."},
		{stackSourceSaved, "Press Enter to keep it, or type another stack."},
		{stackSourceNone, "e.g. mystack.grafana.net"},
	}
	for _, tc := range cases {
		if got := stackDescription(tc.from); got != tc.want {
			t.Errorf("stackDescription(%d) = %q, want %q", tc.from, got, tc.want)
		}
	}
}

// TestAllowEmptyStack pins that the field the list hands over to accepts an
// empty value. huh validates a field as it loses focus and will not leave a
// group holding an error, so a required-value validator there is what would keep
// Esc from reaching the list.
func TestAllowEmptyStack(t *testing.T) {
	for _, s := range []string{"", "   "} {
		if err := allowEmptyStack(s); err != nil {
			t.Errorf("allowEmptyStack(%q) = %v, want nil", s, err)
		}
	}
	if err := allowEmptyStack("mystack.grafana.net"); err != nil {
		t.Errorf("allowEmptyStack(host) = %v, want nil", err)
	}
	if err := allowEmptyStack("://nope"); err == nil {
		t.Error("allowEmptyStack(garbage) = nil, want an error")
	}
}

// TestSetupPageLink covers what the user sees before the paste box: one link to
// their own stack, handed to the browser when there is a stack to open, and
// still printed when opening fails.
func TestSetupPageLink(t *testing.T) {
	cases := []struct {
		name     string
		origin   string
		openErr  error
		wantURL  string
		wantOpen bool
	}{
		{
			name:     "a collected stack is opened",
			origin:   "https://mystack.grafana.net",
			wantURL:  "https://mystack.grafana.net/a/grafana-agento11y-app/setup-coding-agent",
			wantOpen: true,
		},
		{
			name:     "a browser that will not open is not an error",
			origin:   "https://mystack.grafana.net",
			openErr:  errors.New("exec: \"xdg-open\": executable file not found in $PATH"),
			wantURL:  "https://mystack.grafana.net/a/grafana-agento11y-app/setup-coding-agent",
			wantOpen: true,
		},
		{
			name:    "no stack keeps the placeholder and opens nothing",
			wantURL: "https://<your-stack>.grafana.net/a/grafana-agento11y-app/setup-coding-agent",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var opened []string
			prev := openURL
			t.Cleanup(func() { openURL = prev })
			openURL = func(target string) error {
				opened = append(opened, target)
				return c.openErr
			}

			got := setupPageLink(c.origin)
			if !strings.Contains(got, c.wantURL) {
				t.Errorf("output missing %q:\n%s", c.wantURL, got)
			}
			if strings.Contains(got, "browser") {
				t.Errorf("opening is unreported, so the link must say nothing about a browser:\n%s", got)
			}
			wantOpened := []string(nil)
			if c.wantOpen {
				wantOpened = []string{c.wantURL}
			}
			if !reflect.DeepEqual(opened, wantOpened) {
				t.Errorf("opened = %v, want %v", opened, wantOpened)
			}
		})
	}
}

// TestRows pins the count promptValues erases with. Reporting fewer rows than
// were printed leaves debris on screen.
func TestRows(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		width int
		want  int
	}{
		{name: "one line", in: "hello", width: 80, want: 1},
		{name: "three lines", in: "a\nb\nc", width: 80, want: 3},
		{name: "trailing newline is its own row", in: "a\n", width: 80, want: 2},
		{name: "a full row does not wrap", in: strings.Repeat("x", 80), width: 80, want: 1},
		{name: "one column over wraps once", in: strings.Repeat("x", 81), width: 80, want: 2},
		{name: "two rows over wraps twice", in: strings.Repeat("x", 161), width: 80, want: 3},
		{name: "an unknown width assumes no wrapping", in: strings.Repeat("x", 200), want: 1},
		{
			// lipgloss styles the box, and ANSI escapes take no columns.
			name:  "styling does not count as width",
			in:    bannerURL.Render(strings.Repeat("x", 70)),
			width: 80,
			want:  1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rows(c.in, c.width); got != c.want {
				t.Errorf("rows(%q, %d) = %d, want %d", c.in, c.width, got, c.want)
			}
		})
	}
}

// TestSetupPageLinkWrapsOnANarrowTerminal pins the case the row count exists
// for: a long stack name pushes the link past 80 columns, and an uncounted
// wrapped row survives the erase.
func TestSetupPageLinkWrapsOnANarrowTerminal(t *testing.T) {
	prev := openURL
	t.Cleanup(func() { openURL = prev })
	openURL = func(string) error { return nil }

	link := setupPageLink("https://grafanaassistantdev.grafana.net")
	if got, plain := rows(link, 80), rows(link, 0); got <= plain {
		t.Errorf("link of %d lines reported %d rows at 80 columns; a wrapped row is uncounted", plain, got)
	}
}

func TestWelcomeBanner(t *testing.T) {
	if banner := welcomeBanner(false); strings.Contains(banner, "<your-stack>") {
		t.Error("the welcome banner should leave the setup link to setupPageLink")
	}
	if banner := welcomeBanner(true); !strings.Contains(banner, "Choose where to keep your sessions.") {
		t.Errorf("destination banner has the Cloud-only subtitle:\n%s", banner)
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
	_, err := Run(context.Background(), opts)
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
			_, err := Run(context.Background(), RunOpts{
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

func TestEnableLocalMode(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	envconfig.SetBothEnv("LOCAL_FORWARD", "true")
	path := writeDotenv(t, "AGENTO11Y_LOCAL_FORWARD=true\nSIGIL_LOCAL_FORWARD=true\n")
	var stderr bytes.Buffer

	result, err := enableLocalMode(path, RunOpts{Stderr: &stderr})
	if err != nil {
		t.Fatalf("enableLocalMode: %v", err)
	}
	if !result.LocalMode {
		t.Error("LocalMode = false, want true")
	}
	want := map[string]string{
		"AGENTO11Y_AUTO_CODING_AGENT_TAGS": "true",
		"AGENTO11Y_LOCAL":                  "true",
		"AGENTO11Y_LOCAL_FORWARD":          "false",
		"SIGIL_AUTO_CODING_AGENT_TAGS":     "true",
		"SIGIL_LOCAL":                      "true",
		"SIGIL_LOCAL_FORWARD":              "false",
	}
	if got := dotenv.LoadDotenv(path, nil); !reflect.DeepEqual(got, want) {
		t.Errorf("saved config = %v, want %v", got, want)
	}
	for key, value := range want {
		if got := os.Getenv(key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
	if !strings.Contains(stderr.String(), "captured on this machine") {
		t.Errorf("stderr does not confirm the local destination:\n%s", stderr.String())
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
			name: "Cloud credentials disable saved local mode",
			file: existing + "AGENTO11Y_LOCAL=true\nSIGIL_LOCAL=true\n",
			opts: RunOpts{
				Endpoint:   "https://new.example",
				TenantID:   "222",
				Token:      "new-token",
				OfferLocal: true,
			},
			want: map[string]string{
				"AGENTO11Y_ENDPOINT":       "https://new.example",
				"AGENTO11Y_AUTH_TENANT_ID": "222",
				"AGENTO11Y_AUTH_TOKEN":     "new-token",
				"SIGIL_ENDPOINT":           "https://new.example",
				"AGENTO11Y_LOCAL":          "false",
				"SIGIL_LOCAL":              "false",
			},
		},
		{
			name: "one-run Cloud override keeps saved local mode",
			file: existing + "AGENTO11Y_LOCAL=true\nSIGIL_LOCAL=true\n",
			env: map[string]string{
				"AGENTO11Y_LOCAL": "true",
				"SIGIL_LOCAL":     "true",
			},
			opts: RunOpts{
				Endpoint:         "https://new.example",
				TenantID:         "222",
				Token:            "new-token",
				KeepLocalSetting: true,
			},
			want: map[string]string{
				"AGENTO11Y_AUTH_TOKEN": "new-token",
				"AGENTO11Y_LOCAL":      "true",
				"SIGIL_LOCAL":          "true",
			},
		},
		{
			name: "one-run Cloud override does not save a local setting",
			opts: RunOpts{
				Endpoint:         "https://new.example",
				TenantID:         "222",
				Token:            "new-token",
				KeepLocalSetting: true,
			},
			want: map[string]string{
				"AGENTO11Y_LOCAL": "",
				"SIGIL_LOCAL":     "",
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
			// The saved header carries its own copy of the credential and no
			// field can edit it, so a new token would otherwise leave OTLP
			// authenticating with the old one.
			name: "a new token drops the saved OTLP headers",
			file: existing + `OTEL_EXPORTER_OTLP_HEADERS="Authorization=Basic b2xk"` + "\n",
			opts: RunOpts{
				Endpoint: "https://old.example",
				TenantID: "111",
				Token:    "new-token",
			},
			want: map[string]string{
				"SIGIL_AUTH_TOKEN":           "new-token",
				"OTEL_EXPORTER_OTLP_HEADERS": "",
			},
		},
		{
			name: "the same token keeps the saved OTLP headers",
			file: existing + `OTEL_EXPORTER_OTLP_HEADERS="Authorization=Basic b2xk"` + "\n",
			opts: RunOpts{
				Endpoint: "https://old.example",
				TenantID: "111",
				Token:    "old-token",
			},
			want: map[string]string{
				"SIGIL_AUTH_TOKEN":           "old-token",
				"OTEL_EXPORTER_OTLP_HEADERS": "Authorization=Basic b2xk",
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

			_, err := Run(context.Background(), opts)
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
				if k == envconfig.PreferredKey("LOCAL") || k == envconfig.LegacyKey("LOCAL") {
					if got := os.Getenv(k); got != want {
						t.Errorf("%s = %q, want %q in process env", k, got, want)
					}
				}
			}
		})
	}
}

// TestPrintNextStep pins which diagnostic command each outcome names, and
// that the observability link follows the stack the user gave.
func TestPrintNextStep(t *testing.T) {
	cases := []struct {
		name    string
		outcome verifyOutcome
		origin  string
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
		{
			name:    "no stack collected keeps the placeholder host",
			outcome: verifyPassed,
			want:    []string{"https://<your-stack>.grafana.net/a/grafana-agento11y-app"},
		},
		{
			name:    "collected stack names the user's own host",
			outcome: verifyPassed,
			origin:  "https://mystack.grafana.net",
			want:    []string{"https://mystack.grafana.net/a/grafana-agento11y-app"},
			deny:    []string{"<your-stack>"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			printNextStep(&buf, c.outcome, c.origin)
			got := buf.String()
			if !strings.Contains(got, "agento11y claude") {
				t.Errorf("next step must still name the launchers:\n%s", got)
			}
			// The skill line comes after the doctor line: a user who ran login
			// reads the diagnostic first and the setup handoff second.
			doctorAt := strings.Index(got, "agento11y doctor")
			skillAt := strings.Index(got, skills.SetupCodingAgentCommand)
			if skillAt < 0 {
				t.Errorf("next step does not name %q:\n%s", skills.SetupCodingAgentCommand, got)
			} else if doctorAt < 0 || skillAt < doctorAt {
				t.Errorf("the skill line must follow the doctor line:\n%s", got)
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
			// The launcher's automatic login leaves ShowNextStep false and is
			// about to start the agent, so it must not print the setup handoff
			// either.
			if got := strings.Contains(stderr, skills.SetupCodingAgentCommand); got != show {
				t.Errorf("skill hint printed = %v, want %v:\n%s", got, show, stderr)
			}
		})
	}
}

// TestPasteFieldReceivesAFlattenedBlock drives the real huh field the paste
// prompt builds. The single-line input is the reason normalizePastedBlock
// exists, and the sanitizer that flattens the block lives in a dependency, so
// this pins the behaviour rather than the reasoning about it: a bracketed
// paste must not submit the form on the newlines it carries, and the value
// that arrives must still fill every credential.
func TestPasteFieldReceivesAFlattenedBlock(t *testing.T) {
	block := "AGENTO11Y_ENDPOINT=https://api.example.invalid\n" +
		"AGENTO11Y_AUTH_TENANT_ID=123\n" +
		"AGENTO11Y_AUTH_TOKEN=glc_test\n"

	var pasted string
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("Paste from Grafana").
			EchoMode(huh.EchoModePassword).
			Validate(pastedBlockValidator(fixedValues{})).
			Value(&pasted),
	)).WithInput(strings.NewReader("\x1b[200~" + block + "\x1b[201~\r")).WithOutput(io.Discard)
	if err := form.Run(); err != nil {
		t.Fatalf("form.Run: %v", err)
	}

	if strings.Contains(pasted, "\n") {
		t.Fatalf("the field kept the newlines, so this test no longer covers what it claims: %q", pasted)
	}

	var v formValues
	filled := applyPaste(&v, fixedValues{}, pasted)
	want := pasteFilled{endpoint: true, tenantID: true, token: true}
	if filled != want {
		t.Errorf("applyPaste filled = %+v, want %+v", filled, want)
	}
	if v.endpoint != "https://api.example.invalid" || v.tenantID != "123" || v.token != "glc_test" {
		t.Errorf("applyPaste gave endpoint=%q tenant=%q token=%q", v.endpoint, v.tenantID, v.token)
	}
}
