package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
		// A probe can read a version from a store entry that does not register the
		// plugin here. The version is dropped so the JSON report cannot contradict
		// the install state, the way the human report already can't.
		{name: "stalever", bin: "stalever", configBased: true, status: func(context.Context) (bool, string, error) { return false, "1.2.3", nil }},
		{name: "errcli", bin: "errcli", status: func(context.Context) (bool, string, error) { return false, "", errors.New("probe boom") }},
		{name: "cursor", bin: "cursor", status: nil, note: "hook-based"},
	}

	got := defaultCollectAgents(context.Background(), "9.9.9")
	byName := map[string]AgentStatus{}
	for _, a := range got {
		byName[a.Name] = a
	}

	if a := byName["claude"]; !a.OnPath || a.Install != InstallStateInstalled || a.Version != "0.3.0" || a.Health != HealthOK {
		t.Fatalf("claude = %+v", a)
	}
	if a := byName["codex"]; a.OnPath || a.Health != HealthSkipped || a.Install != InstallStateUnknown {
		t.Fatalf("codex = %+v, want not-on-path/skipped/unknown", a)
	}
	if a := byName["cfgcli"]; a.OnPath || a.Install != InstallStateInstalled || a.Version != "2.0.0" || a.Health != HealthOK {
		t.Fatalf("cfgcli = %+v, want not-on-path but installed/ok via config probe", a)
	} else if a.notInstalledLabel != "not configured" {
		t.Fatalf("cfgcli notInstalledLabel = %q, want propagated from probe", a.notInstalledLabel)
	}
	if a := byName["stalever"]; a.Install != InstallStateNotInstalled || a.Version != "" || a.Health != HealthWarn {
		t.Fatalf("stalever = %+v, want not-installed/no-version/warn", a)
	}
	// A probe that errors leaves the state unknown. Reporting not_installed
	// here would put a false negative in the --json contract.
	if a := byName["errcli"]; !a.OnPath || a.Health != HealthWarn || a.Install != InstallStateUnknown {
		t.Fatalf("errcli = %+v, want on-path/warn/unknown", a)
	} else if !strings.Contains(a.Note, "probe boom") {
		t.Fatalf("errcli note = %q, want the probe error", a.Note)
	}
	if a := byName["cursor"]; !a.OnPath || !a.HookBased || a.Version != "9.9.9" || a.Health != HealthOK {
		t.Fatalf("cursor = %+v, want on-path/hook-based/sigil-version/ok", a)
	}

	// The JSON contract carries the tri-state, not a bool that would read as
	// "not installed" for a state doctor never determined.
	encoded, err := json.Marshal(byName["errcli"])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if fields["install_state"] != string(InstallStateUnknown) {
		t.Fatalf("errcli install_state = %v, want %q", fields["install_state"], InstallStateUnknown)
	}
	if _, ok := fields["installed"]; ok {
		t.Fatalf("errcli JSON still carries an installed bool: %s", encoded)
	}
}

func TestCursorProbeUsesHookConfigurationWording(t *testing.T) {
	for _, probe := range agentProbes {
		if probe.name == "cursor" {
			if probe.notInstalledLabel != "not configured" {
				t.Fatalf("cursor notInstalledLabel = %q, want not configured", probe.notInstalledLabel)
			}
			return
		}
	}
	t.Fatal("cursor probe missing")
}

// An AgentStatus built without an install state must not read as a definite
// "not installed" in either output. The zero value is outside the domain, so
// both the renderer and the JSON contract map it to unknown.
func TestAgentStatus_UnsetInstallStateIsUnknown(t *testing.T) {
	a := AgentStatus{Name: "claude", OnPath: true, Health: HealthWarn}

	if got, want := describeAgent(palette{}, a), "install state unknown"; got != want {
		t.Fatalf("describeAgent = %q, want %q", got, want)
	}
	encoded, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"install_state":"unknown"`) {
		t.Fatalf("JSON = %s, want install_state unknown", encoded)
	}
}
