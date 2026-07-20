package doctor

import (
	"context"
	"os/exec"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/copilot"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/opencode"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/pi"
)

// statusFn is the non-mutating per-agent probe each launcher package exposes.
// It returns install state and a best-effort version, and must not install,
// update, or write any state.
type statusFn func(ctx context.Context) (installed bool, version string, err error)

// agentProbe describes how doctor detects and probes one host agent.
type agentProbe struct {
	// name is the CLI/host name shown in the report.
	name string
	// bin is the executable looked up on PATH.
	bin string
	// status is the package's read-only install probe, or nil for hook-only
	// agents (cursor) that the agento11y binary never installs.
	status statusFn
	// configBased is true when status reads install state from files and needs
	// no binary on PATH (claude, opencode, pi, copilot). For these, doctor
	// reports install state even when the CLI is absent. CLI-dependent probes
	// (codex) shell out to the binary, so they're skipped when it's missing.
	configBased bool
	// notInstalledLabel overrides the default "plugin not installed" wording
	// for agents whose capture isn't plugin-based (copilot uses hooks).
	notInstalledLabel string
	// note annotates the agent in the report.
	note string
}

// Test seam.
var lookPath = exec.LookPath

// agentProbes is the detection/probe table. cursor is hook-only (no launcher),
// so it has no install probe: its capture is wired into Cursor's own hook
// settings, and its effective version is the agento11y binary's.
var agentProbes = []agentProbe{
	{name: "claude", bin: "claude", status: claudecode.Status, configBased: true},
	{name: "codex", bin: "codex", status: codex.Status},
	{name: "copilot", bin: "copilot", status: copilot.Status, configBased: true, notInstalledLabel: "not configured", note: "hook-based"},
	{name: "opencode", bin: "opencode", status: opencode.Status, configBased: true},
	{name: "pi", bin: "pi", status: pi.Status, configBased: true},
	{name: "cursor", bin: "cursor", status: nil, note: "hook-based; configured in Cursor settings"},
}

// defaultCollectAgents runs the PATH sweep and per-agent read-only status
// probe. binaryVersion is reported as cursor's version (its hooks call the
// agento11y binary, so they move together).
func defaultCollectAgents(ctx context.Context, binaryVersion string) []AgentStatus {
	out := make([]AgentStatus, 0, len(agentProbes))
	for _, probe := range agentProbes {
		out = append(out, probeAgent(ctx, probe, binaryVersion))
	}
	return out
}

func probeAgent(ctx context.Context, probe agentProbe, binaryVersion string) AgentStatus {
	a := AgentStatus{Name: probe.name, Note: probe.note, notInstalledLabel: probe.notInstalledLabel}
	_, lookErr := lookPath(probe.bin)
	a.OnPath = lookErr == nil

	// Hook-only agent (cursor): no install probe, detection is PATH presence.
	// Capture is configured in the host's own settings, which the agento11y binary
	// doesn't manage, so report it as present with the agento11y binary version.
	if probe.status == nil {
		if !a.OnPath {
			a.Health = HealthSkipped
			return a
		}
		a.HookBased = true
		a.Version = binaryVersion
		a.Health = HealthOK
		return a
	}

	// A CLI-dependent probe (codex, copilot) shells out to the binary to read
	// install state, so skip it when the binary is absent. Config-based probes
	// (claude, opencode, pi) read state from files and run regardless of PATH.
	if !a.OnPath && !probe.configBased {
		a.Health = HealthSkipped
		return a
	}

	installed, version, err := probe.status(ctx)
	if err != nil {
		a.Health = HealthWarn
		a.Note = appendNote(a.Note, "install state unknown: "+err.Error())
		return a
	}
	a.Installed = installed
	a.Version = version
	if installed {
		a.Health = HealthOK
	} else {
		a.Health = HealthWarn
	}
	return a
}

func appendNote(existing, extra string) string {
	if existing == "" {
		return extra
	}
	return existing + "; " + extra
}
