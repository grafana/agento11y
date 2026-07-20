// Package mapperutil holds helpers shared by the per-agent mappers that turn
// captured hook fragments into Sigil generations: deterministic ID hashing,
// content-mode normalization, tool-definition building, map cloning, and
// model→provider inference.
//
// Behaviour here is the common denominator across adapters. Mappers that need
// different semantics keep their own local helper and document the difference
// (e.g. copilot uses a stricter provider matcher than InferProvider).
package mapperutil

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"sort"
	"strings"

	"github.com/grafana/agento11y/go/agento11y"
)

// Clone returns a shallow copy of in, or nil when in is empty. The mappers
// use it to keep the GenerationStart and Generation tag/metadata maps
// independent, so a caller mutating one (e.g. adding a start-only tag) cannot
// leak into the other.
func Clone[K comparable, V any](in map[K]V) map[K]V {
	if len(in) == 0 {
		return nil
	}
	out := make(map[K]V, len(in))
	maps.Copy(out, in)
	return out
}

// DeterministicID derives a stable, collision-resistant ID from parts. The
// parts are joined with a NUL separator (so "a","bc" and "ab","c" cannot
// collide), hashed with SHA-256, and the first 24 hex chars are appended to
// prefix as "<prefix>-<hash>". codex and copilot use it to derive generation
// IDs from (sessionID, turnID).
func DeterministicID(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return prefix + "-" + hex.EncodeToString(sum[:])[:24]
}

// NormalizeStartContentMode resolves the mode used on GenerationStart.
// Default falls back to MetadataOnly, matching envconfig resolution for callers
// that bypass the config layer. FullWithMetadataSpans is preserved because the
// SDK uses it to keep content out of OTLP span attributes while exporting full
// content over gRPC.
func NormalizeStartContentMode(mode agento11y.ContentCaptureMode) agento11y.ContentCaptureMode {
	if mode == agento11y.ContentCaptureModeDefault {
		return agento11y.ContentCaptureModeMetadataOnly
	}
	return mode
}

// NormalizePayloadContentMode collapses ContentCaptureMode to the three modes
// mappers actually branch on when building Generation input/output payloads.
// FullWithMetadataSpans becomes Full because the gRPC payload is identical; the
// difference only matters for GenerationStart and span attributes.
func NormalizePayloadContentMode(mode agento11y.ContentCaptureMode) agento11y.ContentCaptureMode {
	switch mode {
	case agento11y.ContentCaptureModeDefault:
		return agento11y.ContentCaptureModeMetadataOnly
	case agento11y.ContentCaptureModeFullWithMetadataSpans:
		return agento11y.ContentCaptureModeFull
	default:
		return mode
	}
}

// SortedToolDefinitions returns deduplicated, name-sorted function tool
// definitions built from names. Empty names are skipped and nil is returned
// when no non-empty names remain. Sorting keeps the emitted Tools slice stable
// across turns and across tool subsets, which downstream consumers (tests, log
// diffing, dashboards) rely on.
func SortedToolDefinitions(names []string) []agento11y.ToolDefinition {
	seen := make(map[string]struct{}, len(names))
	uniq := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		uniq = append(uniq, name)
	}
	if len(uniq) == 0 {
		return nil
	}
	sort.Strings(uniq)
	out := make([]agento11y.ToolDefinition, len(uniq))
	for i, name := range uniq {
		out[i] = agento11y.ToolDefinition{Name: name, Type: "function"}
	}
	return out
}

// InferProvider maps a model name to a Sigil provider using loose substring
// matching for Claude/Gemini and prefix matching for the OpenAI families.
// Returns "" when nothing matches so callers can supply their own fallback.
//
// copilot intentionally uses a stricter matcher (hyphenated prefixes) and does
// not call this.
func InferProvider(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "claude"):
		return "anthropic"
	case strings.HasPrefix(m, "gpt"),
		strings.HasPrefix(m, "o1"),
		strings.HasPrefix(m, "o3"),
		strings.HasPrefix(m, "o4"):
		return "openai"
	case strings.Contains(m, "gemini"):
		return "google"
	}
	return ""
}
