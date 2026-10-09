package usagestats

import (
	"slices"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// TestEnvSuffixIsRegistered: a missing entry silently breaks two things — the
// config.env opt-out stops reaching launched children, and PinAliasEnvBlank
// stops pinning it, so the suite could POST real events.
func TestEnvSuffixIsRegistered(t *testing.T) {
	if !slices.Contains(envconfig.AliasSuffixes, EnvSuffix) {
		t.Fatalf("%q missing from envconfig.AliasSuffixes; the config.env opt-out and test env pinning both depend on it", EnvSuffix)
	}
}

// TestEnvSuffixIsWritableToConfig: the settings page must be able to persist
// an opt-out.
func TestEnvSuffixIsWritableToConfig(t *testing.T) {
	for _, key := range []string{envconfig.PreferredKey(EnvSuffix), envconfig.LegacyKey(EnvSuffix)} {
		if !dotenv.AllowedDotenvKey(key) {
			t.Errorf("dotenv.AllowedDotenvKey(%q) = false; the opt-out cannot be persisted", key)
		}
	}
}

func TestResolveMode(t *testing.T) {
	const preferred = "AGENTO11Y_ANONYMOUS_USAGE_STATS"
	const legacy = "SIGIL_ANONYMOUS_USAGE_STATS"

	tests := []struct {
		name   string
		env    map[string]string
		config string
		want   Mode
	}{
		{name: "default is enabled", want: ModeEnabled},

		// The three mode words.
		{name: "enabled", env: map[string]string{preferred: "enabled"}, want: ModeEnabled},
		{name: "disabled", env: map[string]string{preferred: "disabled"}, want: ModeDisabled},
		{name: "log", env: map[string]string{preferred: "log"}, want: ModeLog},
		{name: "case insensitive", env: map[string]string{preferred: "DISABLED"}, want: ModeDisabled},
		{name: "surrounding space", env: map[string]string{preferred: "  log  "}, want: ModeLog},

		// This repo's boolean vocabulary: envconfig taught users these and the
		// settings page writes a literal "false".
		{name: "true", env: map[string]string{preferred: "true"}, want: ModeEnabled},
		{name: "yes", env: map[string]string{preferred: "yes"}, want: ModeEnabled},
		{name: "on", env: map[string]string{preferred: "on"}, want: ModeEnabled},
		{name: "one", env: map[string]string{preferred: "1"}, want: ModeEnabled},
		{name: "false", env: map[string]string{preferred: "false"}, want: ModeDisabled},
		{name: "no", env: map[string]string{preferred: "no"}, want: ModeDisabled},
		{name: "off", env: map[string]string{preferred: "off"}, want: ModeDisabled},
		{name: "zero", env: map[string]string{preferred: "0"}, want: ModeDisabled},

		// Fail toward privacy: the default is enabled, so a typo must not
		// opt the user back in.
		{name: "typo disables", env: map[string]string{preferred: "disabledd"}, want: ModeDisabled},
		{name: "gibberish disables", env: map[string]string{preferred: "yolo"}, want: ModeDisabled},

		// Blank falls through.
		{name: "blank falls through to default", env: map[string]string{preferred: "   "}, want: ModeEnabled},
		{name: "blank falls through to config", env: map[string]string{preferred: ""}, config: "disabled", want: ModeDisabled},

		// Alias precedence.
		{name: "legacy spelling honoured", env: map[string]string{legacy: "disabled"}, want: ModeDisabled},
		{name: "preferred beats legacy", env: map[string]string{preferred: "enabled", legacy: "disabled"}, want: ModeEnabled},

		// DO_NOT_TRACK sits between the shell and config.env.
		{name: "do not track 1", env: map[string]string{EnvDoNotTrack: "1"}, want: ModeDisabled},
		{name: "do not track true", env: map[string]string{EnvDoNotTrack: "true"}, want: ModeDisabled},
		{name: "do not track 0 is ignored", env: map[string]string{EnvDoNotTrack: "0"}, want: ModeEnabled},
		{name: "env beats do not track", env: map[string]string{preferred: "enabled", EnvDoNotTrack: "1"}, want: ModeEnabled},
		{name: "do not track beats config", env: map[string]string{EnvDoNotTrack: "1"}, config: "enabled", want: ModeDisabled},

		// config.env is lowest above the default.
		{name: "config disables", config: "disabled", want: ModeDisabled},
		{name: "config log", config: "log", want: ModeLog},
		{name: "env beats config", env: map[string]string{preferred: "enabled"}, config: "disabled", want: ModeEnabled},
		{name: "config typo disables", config: "nope", want: ModeDisabled},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mode, _, _ := envconfig.LookupMap(tc.env, EnvSuffix)
			shell := ShellEnv{Mode: mode, DoNotTrack: tc.env[EnvDoNotTrack]}
			configValue := func() string { return tc.config }

			if got := ResolveMode(shell, configValue); got != tc.want {
				t.Errorf("ResolveMode() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveModeToleratesNilConfigValue covers early paths that run before a
// config reader exists.
func TestResolveModeToleratesNilConfigValue(t *testing.T) {
	if got := ResolveMode(ShellEnv{}, nil); got != ModeEnabled {
		t.Errorf("ResolveMode() = %q, want %q", got, ModeEnabled)
	}
}

// TestDoNotTrackSurvivesAConfigValue is the regression test for a config.env
// value defeating DO_NOT_TRACK.
//
// ResolveMode used to read the alias family from the live environment. Once
// dotenv.ApplyEnv had merged config.env, the file's value sat in the
// environment under the same name, could not be told from a shell setting, and
// so outranked DO_NOT_TRACK. Taking the shell values before the merge is what
// keeps the two sources distinguishable.
func TestDoNotTrackSurvivesAConfigValue(t *testing.T) {
	shell := ShellEnv{DoNotTrack: "1"}
	configEnabled := func() string { return "enabled" }

	if got := ResolveMode(shell, configEnabled); got != ModeDisabled {
		t.Errorf("ResolveMode() = %q with DO_NOT_TRACK=1 and config.env enabled, want %q", got, ModeDisabled)
	}

	// A shell setting still wins: that is an explicit per-invocation choice,
	// unlike a value that merely lives in a file.
	shell.Mode = "enabled"
	if got := ResolveMode(shell, configEnabled); got != ModeEnabled {
		t.Errorf("ResolveMode() = %q with an explicit shell opt-in, want %q", got, ModeEnabled)
	}
}

// TestCaptureShellEnvReadsBothSpellings pins that the snapshot honours the
// alias family rather than only the preferred name.
func TestCaptureShellEnvReadsBothSpellings(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	t.Setenv(envconfig.LegacyKey(EnvSuffix), "log")
	t.Setenv(EnvDoNotTrack, "1")

	got := CaptureShellEnv()
	if got.Mode != "log" {
		t.Errorf("Mode = %q, want %q from the legacy spelling", got.Mode, "log")
	}
	if got.DoNotTrack != "1" {
		t.Errorf("DoNotTrack = %q, want %q", got.DoNotTrack, "1")
	}
}

// TestDefaultIsOptOut makes flipping the product decision deliberate rather
// than a side effect of refactoring parseMode.
func TestDefaultIsOptOut(t *testing.T) {
	if defaultMode != ModeEnabled {
		t.Fatalf("defaultMode = %q; reporting is opt-out by design and the first-run notice is written on that basis", defaultMode)
	}
}
