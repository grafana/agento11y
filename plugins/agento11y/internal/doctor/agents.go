package doctor

import (
	"context"
	"os/exec"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/registry"
)

var (
	lookPath         = exec.LookPath
	registeredAgents = registry.All
)

// defaultCollectAgents runs the PATH sweep and per-agent read-only status
// probe.
func defaultCollectAgents(ctx context.Context, _ string) []AgentStatus {
	agents := registeredAgents()
	out := make([]AgentStatus, 0, len(agents))
	for _, agent := range agents {
		out = append(out, probeAgent(ctx, agent))
	}
	return out
}

func probeAgent(ctx context.Context, agent registry.Agent) AgentStatus {
	// Unknown until a probe says otherwise: every early return below leaves
	// the install state undetermined.
	a := AgentStatus{
		Name:              agent.Name,
		Install:           InstallStateUnknown,
		Note:              agent.Note,
		notInstalledLabel: agent.NotInstalledLabel,
	}
	_, lookErr := lookPath(agent.HostBinary)
	a.OnPath = lookErr == nil

	if !a.OnPath && agent.StatusRequiresHost {
		a.Health = HealthSkipped
		return a
	}

	installed, version, err := agent.Status(ctx)
	if err != nil {
		// The state stays unknown and the renderer says so; the note carries
		// the reason the probe could not answer.
		a.Health = HealthWarn
		a.Note = appendNote(a.Note, err.Error())
		return a
	}
	if installed {
		// A version belongs to an installed plugin, so both the human report and
		// the JSON one drop a version a probe reports next to "not installed".
		a.Version = version
		a.Install = InstallStateInstalled
		a.HookBased = agent.LegacyHookBased
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
