// Package registry defines the host agents that agento11y can inspect and
// configure.
package registry

import (
	"context"
	"errors"
	"io"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/copilot"
	cursorinstall "github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/install"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/opencode"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/pi"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe"
)

// StatusFunc inspects one agent's integration without modifying it.
type StatusFunc func(ctx context.Context) (installed bool, version string, err error)

// InstallFunc configures or removes one agent integration without launching
// the host or prompting for credentials.
type InstallFunc func(ctx context.Context, stdout io.Writer) (changed bool, err error)

type Agent struct {
	Name               string
	HostBinary         string
	StatusRequiresHost bool
	LegacyHookBased    bool
	Status             StatusFunc
	Install            InstallFunc
	Uninstall          InstallFunc
	IsMissingHost      func(error) bool
	NotInstalledLabel  string
	Note               string
}

var agents = []Agent{
	{
		Name:          "claude",
		HostBinary:    "claude",
		Status:        claudecode.Status,
		Install:       claudecode.Install,
		IsMissingHost: missingHost(claudecode.ErrCLINotFound),
	},
	{
		Name:               "codex",
		HostBinary:         "codex",
		StatusRequiresHost: true,
		Status:             codex.Status,
		Install:            codex.Install,
		IsMissingHost:      missingHost(codex.ErrCLINotFound),
	},
	{
		Name:              "copilot",
		HostBinary:        "copilot",
		Status:            copilot.Status,
		Install:           copilot.Install,
		Uninstall:         copilot.Uninstall,
		NotInstalledLabel: "not configured",
		Note:              "hook-based",
	},
	{
		Name:              "cursor",
		HostBinary:        "cursor",
		LegacyHookBased:   true,
		Status:            cursorinstall.Status,
		Install:           cursorinstall.Install,
		Uninstall:         cursorinstall.Uninstall,
		NotInstalledLabel: "not configured",
		Note:              "hook-based; configured in Cursor settings",
	},
	{
		Name:          "opencode",
		HostBinary:    "opencode",
		Status:        opencode.Status,
		Install:       opencode.Install,
		IsMissingHost: missingHost(opencode.ErrCLINotFound),
	},
	{
		Name:          "pi",
		HostBinary:    "pi",
		Status:        pi.Status,
		Install:       pi.Install,
		IsMissingHost: missingHost(pi.ErrCLINotFound),
	},
	{
		Name:              "vibe",
		HostBinary:        "vibe",
		Status:            vibe.Status,
		Install:           vibe.Install,
		Uninstall:         vibe.Uninstall,
		IsMissingHost:     missingHost(vibe.ErrCLINotFound),
		NotInstalledLabel: "not configured",
		Note:              "hook-based",
	},
}

func missingHost(target error) func(error) bool {
	return func(err error) bool { return errors.Is(err, target) }
}

// All returns all supported agents in CLI name order. The returned slice can
// be modified without changing the registry.
func All() []Agent {
	return append([]Agent(nil), agents...)
}

func Resolve(name string) (Agent, bool) {
	for _, agent := range agents {
		if agent.Name == name {
			return agent, true
		}
	}
	return Agent{}, false
}
