package weavertest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Finding is one live-check advice entry, reduced to the fields that
// identify it. Weaver's rendered message is not recorded: it repeats the
// identity in prose and would churn on any upstream wording change.
//
// [Violation] is the equivalent for [Start]. The two differ because Weaver
// writes a different document for a file live-check than the one its admin
// endpoint returns.
type Finding struct {
	// Span is the name of the span the finding is about. A fixture can
	// hold several spans, and without this a finding moving from one to
	// another would compare equal.
	Span string `json:"span"`
	// ID is Weaver's finding id, for example type_mismatch or
	// genai_expected_attribute_missing.
	ID string `json:"id"`
	// Level is violation, improvement, or information.
	Level string `json:"level"`
	// Target is the attribute the finding is about, and empty for a
	// span-level finding, which is about Span itself.
	Target string `json:"target,omitempty"`
	// Detail is Weaver's context for the finding, rendered as sorted
	// key=value pairs. It carries what the id and the target leave out:
	// which attribute a span is missing, the undocumented enum value, the
	// observed type beside the expected one.
	Detail string `json:"detail,omitempty"`
}

// LevelViolation is the only level the recorded verdicts keep. Every
// GenAI attribute is development-stability, so each one draws a
// not_stable finding at improvement level; recording those would make
// the file 40 lines of noise per fixture and would churn on every
// stability change upstream.
const LevelViolation = "violation"

// LiveCheck runs Weaver's live-check over the samples and returns the
// violations it reports, sorted so the result is stable across runs.
// Weaver's own exit status is ignored via --fail-on none: a finding is
// the result here, not an error.
func LiveCheck(ctx context.Context, assets Assets, samples []Sample) ([]Finding, error) {
	binary, err := exec.LookPath("weaver")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotInstalled, err)
	}

	dir, err := os.MkdirTemp("", "weaver-livecheck-")
	if err != nil {
		return nil, fmt.Errorf("create live-check working directory: %w", err)
	}
	defer os.RemoveAll(dir)

	input := filepath.Join(dir, "samples.json")
	payload, err := json.Marshal(samples)
	if err != nil {
		return nil, fmt.Errorf("encode samples: %w", err)
	}
	if err := os.WriteFile(input, payload, 0o600); err != nil {
		return nil, fmt.Errorf("write samples: %w", err)
	}

	output := filepath.Join(dir, "report")
	command := exec.CommandContext(ctx, binary,
		"registry", "live-check",
		"--quiet",
		"--registry", assets.Registry,
		"--config", assets.Config,
		"--advice-policies", assets.Policies,
		"--advice-data", assets.AdviceData,
		"--input-source", input,
		"--input-format", "json",
		"--format", "json",
		"--fail-on", "none",
		"--output", output,
	)
	var diagnostics bytes.Buffer
	command.Stderr = &diagnostics
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("weaver registry live-check: %w\n%s", err, diagnostics.String())
	}

	report, err := os.ReadFile(filepath.Join(output, "live_check.json"))
	if err != nil {
		return nil, fmt.Errorf("read live-check report: %w\n%s", err, diagnostics.String())
	}
	return violations(report)
}

// liveCheckReport is the part of Weaver's report the verdicts are built
// from. Findings hang off the span and off each of its attributes.
type liveCheckReport struct {
	Samples []struct {
		Span struct {
			Name       string `json:"name"`
			Attributes []struct {
				Name   string `json:"name"`
				Result *struct {
					Advice []advice `json:"all_advice"`
				} `json:"live_check_result"`
			} `json:"attributes"`
			Result *struct {
				Advice []advice `json:"all_advice"`
			} `json:"live_check_result"`
		} `json:"span"`
	} `json:"samples"`
}

type advice struct {
	ID      string         `json:"id"`
	Level   string         `json:"level"`
	Context map[string]any `json:"context"`
}

func violations(report []byte) ([]Finding, error) {
	parsed := liveCheckReport{}
	if err := json.Unmarshal(report, &parsed); err != nil {
		return nil, fmt.Errorf("decode live-check report: %w", err)
	}

	findings := []Finding{}
	for _, sample := range parsed.Samples {
		span := sample.Span
		if span.Result != nil {
			findings = append(findings, collect(span.Result.Advice, span.Name, "")...)
		}
		for _, attribute := range span.Attributes {
			if attribute.Result == nil {
				continue
			}
			findings = append(findings, collect(attribute.Result.Advice, span.Name, attribute.Name)...)
		}
	}

	sort.Slice(findings, func(i, j int) bool {
		left, right := findings[i], findings[j]
		if left.Span != right.Span {
			return left.Span < right.Span
		}
		if left.ID != right.ID {
			return left.ID < right.ID
		}
		if left.Target != right.Target {
			return left.Target < right.Target
		}
		return left.Detail < right.Detail
	})
	return findings, nil
}

func collect(entries []advice, span, target string) []Finding {
	out := make([]Finding, 0, len(entries))
	for _, entry := range entries {
		if entry.Level != LevelViolation {
			continue
		}
		out = append(out, Finding{
			Span:   span,
			ID:     entry.ID,
			Level:  entry.Level,
			Target: target,
			Detail: detailOf(entry, span, target),
		})
	}
	return out
}

// detailOf includes every context field that does not repeat the span or
// attribute. Which fields distinguish two findings depends on the finding ID.
// Sorting the keys keeps the output stable.
func detailOf(entry advice, span, target string) string {
	keys := make([]string, 0, len(entry.Context))
	for key, value := range entry.Context {
		if text, ok := value.(string); ok && (text == span || text == target) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+renderValue(entry.Context[key]))
	}
	return strings.Join(parts, ", ")
}

func renderValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(encoded)
}
