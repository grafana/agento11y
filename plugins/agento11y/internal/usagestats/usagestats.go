// Package usagestats implements the agento11y CLI's usage statistics: one
// flat event per invocation describing the shape of usage (command path, flag
// names, outcome) and never its content (argument values, endpoints, tenant
// ids, tags, paths). The only correlator is a random, resettable,
// per-installation id.
//
// This is deliberately not called "telemetry". In this binary that word
// already means the product — internal/otel, internal/emit, the OTLP
// pipelines, the agent sessions the user installed agento11y to capture. A
// user reading AGENTO11Y_TELEMETRY=disabled would reasonably expect it to stop
// capturing their coding sessions. These statistics are about the CLI itself
// and travel a different path to a different destination.
//
// This package is a library only: event construction and emission are wired at
// the CLI lifecycle boundaries (internal/entry), not here. Nothing in this
// package reaches for process state beyond the environment, so every piece is
// testable without a running invocation.
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
	// ModeLog prints the event that would be sent to stderr and sends nothing.
	ModeLog Mode = "log"
)

// defaultMode is the resolved mode when no environment variable or config
// value applies. Reporting is opt-out: enabled by default, disabled via the
// AGENTO11Y_ANONYMOUS_USAGE_STATS alias family, DO_NOT_TRACK, or the same key
// in config.env. Interactive users are told once by the first-run notice.
const defaultMode = ModeEnabled

// EnvSuffix is the alias family controlling reporting. It resolves as
// AGENTO11Y_ANONYMOUS_USAGE_STATS with a SIGIL_ANONYMOUS_USAGE_STATS fallback,
// like every other branded variable, and must be registered in
// envconfig.AliasSuffixes — see TestEnvSuffixIsRegistered for why that is not
// merely tidiness.
const EnvSuffix = "ANONYMOUS_USAGE_STATS"

const (
	// EnvDoNotTrack follows the cross-tool DO_NOT_TRACK convention
	// (https://consoledonottrack.com/). It is unbranded and read from the
	// shell only, because that is the whole point of a cross-tool variable.
	EnvDoNotTrack = "DO_NOT_TRACK"

	// EnvEndpoint overrides the destination, for pointing a dev build at a
	// test receiver. Read with os.Getenv rather than registered as an alias
	// family so it behaves identically whether or not dotenv.ApplyEnv has run
	// in the branch that reached the emitter.
	EnvEndpoint = "AGENTO11Y_ANONYMOUS_USAGE_STATS_ENDPOINT"
)

// ResolveMode resolves the reporting mode for this invocation. Precedence,
// highest first: the alias family in the shell, DO_NOT_TRACK, the same key in
// config.env, the built-in default. Empty values fall through to the next
// level; unrecognised non-empty values resolve to disabled.
//
// configValue is a func so callers pay the config-file read only when the
// environment has not already decided, and — more importantly — so this
// package never triggers dotenv.ApplyEnv. The merge points in internal/entry
// are per-branch on purpose (the local banner reports which spelling the user
// set, and doctor snapshots the environment before the merge to attribute each
// value to the shell or config.env), so the emitter must not move them.
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
// The accepted spellings include this repo's own boolean vocabulary
// (envconfig.ParseBoolValue) and not just gcx's three words. That is not
// optional: envconfig taught users 1/true/yes/on and 0/false/no/off, and the
// local viewer's settings page writes a literal "false". An
// AGENTO11Y_ANONYMOUS_USAGE_STATS=true that resolved to disabled would be
// indefensible.
//
// The default case is deliberately NOT envconfig.ParseBoolDefault. Everywhere
// else in this binary an unrecognised boolean falls back to the documented
// default; here that default is enabled, so a typo in an opt-out would
// silently opt the user in. Unrecognised values fail toward privacy instead.
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
	switch Mode(trimmed) {
	case ModeEnabled:
		return ModeEnabled, true
	case ModeDisabled:
		return ModeDisabled, true
	default:
		return ModeDisabled, true
	}
}

// isDoNotTrack honours the published convention: the variable is set to "1".
// "true" is accepted too because tools in the wild use it interchangeably.
// Anything else — including "0" — leaves the decision to the next level.
func isDoNotTrack(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true":
		return true
	default:
		return false
	}
}
