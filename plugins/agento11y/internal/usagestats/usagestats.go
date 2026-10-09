// Package usagestats implements the agento11y CLI's usage statistics: one flat
// event per invocation describing the shape of usage (command path, flag names,
// outcome) and never its content. The only correlator is a random, resettable,
// per-installation id.
//
// Not called "telemetry" because in this binary that word means the product, so
// AGENTO11Y_TELEMETRY=disabled would read as "stop capturing my sessions".
//
// Library only — emission is wired in internal/entry.
package usagestats

import (
	"os"
	"strings"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// Mode is the resolved reporting state for an invocation.
type Mode string

const (
	// ModeEnabled emits the event.
	ModeEnabled Mode = "enabled"
	// ModeDisabled emits nothing.
	ModeDisabled Mode = "disabled"
	// ModeLog prints the event to stderr and sends nothing.
	ModeLog Mode = "log"
)

// defaultMode is the mode when nothing else applies. Reporting is opt-out.
const defaultMode = ModeEnabled

// EnvSuffix is the alias family controlling reporting. It must be registered
// in envconfig.AliasSuffixes; see TestEnvSuffixIsRegistered for why.
const EnvSuffix = "ANONYMOUS_USAGE_STATS"

const (
	// EnvDoNotTrack follows the cross-tool DO_NOT_TRACK convention. Unbranded,
	// so it is read from the shell only.
	EnvDoNotTrack = "DO_NOT_TRACK"

	// EnvEndpoint overrides the destination, for pointing a dev build at a
	// test receiver. Read with os.Getenv rather than registered as an alias
	// family so it behaves the same whether or not ApplyEnv has run.
	EnvEndpoint = "AGENTO11Y_ANONYMOUS_USAGE_STATS_ENDPOINT"
)

// ResolveMode resolves the reporting mode. Precedence, highest first: the alias
// family in the shell, DO_NOT_TRACK, the same key in config.env, the default.
//
// configValue is a func so the file read is skipped when the environment
// already decided, and so this package never triggers dotenv.ApplyEnv.
func ResolveMode(configValue func() string) Mode {
	return resolveMode(envconfig.LookupEnv, os.Getenv, configValue)
}

func resolveMode(lookup envconfig.Lookup, getenv func(string) string, configValue func() string) Mode {
	if value, _, ok := lookup(EnvSuffix); ok {
		if m, parsed := parseMode(value); parsed {
			return m
		}
	}
	if isDoNotTrack(getenv(EnvDoNotTrack)) {
		return ModeDisabled
	}
	if configValue != nil {
		if m, parsed := parseMode(configValue()); parsed {
			return m
		}
	}
	return defaultMode
}

// parseMode maps a raw value to a mode. ok is false only for an empty value,
// so the caller falls through to the next precedence level.
//
// Accepts the boolean vocabulary as well as the three mode words, since
// envconfig uses 1/true/yes/on elsewhere. Unrecognised values resolve to
// disabled: the default is enabled, so a typo in an opt-out must not opt the
// user in. That is why this is not envconfig.ParseBoolDefault.
func parseMode(raw string) (mode Mode, ok bool) {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return "", false
	}
	if trimmed == string(ModeLog) {
		return ModeLog, true
	}
	if value, recognised := envconfig.ParseBoolValue(trimmed); recognised {
		if value {
			return ModeEnabled, true
		}
		return ModeDisabled, true
	}
	if Mode(trimmed) == ModeEnabled {
		return ModeEnabled, true
	}
	return ModeDisabled, true
}

// isDoNotTrack honours the published convention. Anything else, "0" included,
// leaves the decision to the next level.
func isDoNotTrack(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true":
		return true
	default:
		return false
	}
}
