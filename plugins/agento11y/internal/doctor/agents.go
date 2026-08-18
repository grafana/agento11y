package doctor

import (
	"context"
	"os/exec"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/copilot"
	cursorinstall "github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/install"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/opencode"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/pi"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe"
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
	// status is the package's read-only install probe.
	status statusFn
	// configBased is true when status reads install state from files and needs
	// no binary on PATH (claude, opencode, pi, copilot, vibe). For these, doctor
	// reports install state even when the CLI is absent. CLI-dependent probes
	// (codex) shell out to the binary, so they're skipped when it's missing.
	configBased bool
	// fallbackVersion is used by integrations whose hooks invoke the shared
	// agento11y binary instead of shipping their own independently versioned
	// plugin.
	fallbackVersion bool
	// notInstalledLabel overrides the default "plugin not installed" wording
	// for agents whose capture isn't plugin-based (copilot and vibe use hooks).
	notInstalledLabel string
	// note annotates the agent in the report.
	note string
}

// Test seam.
var lookPath = exec.LookPath

// agentProbes is the detection/probe table. Cursor is hook-based and its
// effective version is the shared agento11y binary's.
var agentProbes = []agentProbe{
	{name: "claude", bin: "claude", status: claudecode.Status, configBased: true},
	{name: "codex", bin: "codex", status: codex.Status},
	{name: "copilot", bin: "copilot", status: copilot.Status, configBased: true, notInstalledLabel: "not configured", note: "hook-based"},
	{name: "opencode", bin: "opencode", status: opencode.Status, configBased: true},
	{name: "pi", bin: "pi", status: pi.Status, configBased: true},
	{name: "vibe", bin: "vibe", status: vibe.Status, configBased: true, notInstalledLabel: "not configured", note: "hook-based"},
	{name: "cursor", bin: "cursor", status: func(context.Context) (bool, string, error) {
		installed, err := cursorinstall.Status()
		return installed, "", err
	}, configBased: true, fallbackVersion: true, notInstalledLabel: "not configured", note: "hook-based; configured in Cursor settings"},
}

// defaultCollectAgents runs the PATH sweep and per-agent read-only status
// probe.
func defaultCollectAgents(ctx context.Context, binaryVersion string) []AgentStatus {
	out := make([]AgentStatus, 0, len(agentProbes))
	for _, probe := range agentProbes {
		out = append(out, probeAgent(ctx, probe, binaryVersion))
	}
	return out
}

func probeAgent(ctx context.Context, probe agentProbe, binaryVersion string) AgentStatus {
	// Unknown until a probe says otherwise: every early return below leaves
	// the install state undetermined.
	a := AgentStatus{Name: probe.name, Install: InstallStateUnknown, Note: probe.note, notInstalledLabel: probe.notInstalledLabel}
	_, lookErr := lookPath(probe.bin)
	a.OnPath = lookErr == nil

	// Keep the nil-status behaviour for test probes and future integrations
	// whose host only establishes availability by being on PATH.
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
		// The state stays unknown and the renderer says so; the note carries
		// the reason the probe could not answer.
		a.Health = HealthWarn
		a.Note = appendNote(a.Note, err.Error())
		return a
	}
	if installed {
		if version == "" && probe.fallbackVersion {
			version = binaryVersion
		}
		// A version belongs to an installed plugin, so both the human report and
		// the JSON one drop a version a probe reports next to "not installed".
		a.Version = version
		a.Install = InstallStateInstalled
		a.HookBased = probe.fallbackVersion
		a.Health = HealthOK
	} else {
		a.Install = InstallStateNotInstalled
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
