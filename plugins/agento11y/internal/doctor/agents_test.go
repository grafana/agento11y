package doctor

import (
	"context"
	"errors"
	"testing"
)

func TestDefaultCollectAgents(t *testing.T) {
	prevLook, prevProbes := lookPath, agentProbes
	t.Cleanup(func() { lookPath, agentProbes = prevLook, prevProbes })

	onPath := map[string]bool{"claude": true, "errcli": true, "cursor": true}
	lookPath = func(name string) (string, error) {
		if onPath[name] {
			return "/usr/local/bin/" + name, nil
		}
		return "", errors.New("not found on PATH")
	}

	agentProbes = []agentProbe{
		{name: "claude", bin: "claude", status: func(context.Context) (bool, string, error) { return true, "0.3.0", nil }},
		{name: "codex", bin: "codex", status: func(context.Context) (bool, string, error) {
			t.Error("a CLI-dependent status must not run for an agent that isn't on PATH")
			return false, "", nil
		}},
		// A config-based agent reads install state from files, so its probe
		// runs even when the binary is absent from PATH. notInstalledLabel must
		// propagate from the probe table into the status.
		{name: "cfgcli", bin: "cfgcli", configBased: true, notInstalledLabel: "not configured", status: func(context.Context) (bool, string, error) { return true, "2.0.0", nil }},
		{name: "errcli", bin: "errcli", status: func(context.Context) (bool, string, error) { return false, "", errors.New("probe boom") }},
		{name: "cursor", bin: "cursor", status: nil, note: "hook-based"},
	}

	got := defaultCollectAgents(context.Background(), "9.9.9")
	byName := map[string]AgentStatus{}
	for _, a := range got {
		byName[a.Name] = a
	}

	if a := byName["claude"]; !a.OnPath || !a.Installed || a.Version != "0.3.0" || a.Health != HealthOK {
		t.Fatalf("claude = %+v", a)
	}
	if a := byName["codex"]; a.OnPath || a.Health != HealthSkipped {
		t.Fatalf("codex = %+v, want not-on-path/skipped", a)
	}
	if a := byName["cfgcli"]; a.OnPath || !a.Installed || a.Version != "2.0.0" || a.Health != HealthOK {
		t.Fatalf("cfgcli = %+v, want not-on-path but installed/ok via config probe", a)
	} else if a.notInstalledLabel != "not configured" {
		t.Fatalf("cfgcli notInstalledLabel = %q, want propagated from probe", a.notInstalledLabel)
	}
	if a := byName["errcli"]; !a.OnPath || a.Health != HealthWarn || a.Installed {
		t.Fatalf("errcli = %+v, want on-path/warn/not-installed", a)
	} else if a.Note == "" {
		t.Fatalf("errcli note should record the probe error")
	}
	if a := byName["cursor"]; !a.OnPath || !a.HookBased || a.Version != "9.9.9" || a.Health != HealthOK {
		t.Fatalf("cursor = %+v, want on-path/hook-based/sigil-version/ok", a)
	}
}
