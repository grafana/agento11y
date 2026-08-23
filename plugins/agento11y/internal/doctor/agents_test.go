package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/registry"
)

func TestDefaultCollectAgents(t *testing.T) {
	prevLook, prevAgents := lookPath, registeredAgents
	t.Cleanup(func() { lookPath, registeredAgents = prevLook, prevAgents })

	onPath := map[string]bool{"claude": true, "errcli": true, "cursor": true}
	lookPath = func(name string) (string, error) {
		if onPath[name] {
			return "/usr/local/bin/" + name, nil
		}
		return "", errors.New("not found on PATH")
	}

	registeredAgents = func() []registry.Agent {
		return []registry.Agent{
			{Name: "claude", HostBinary: "claude", Status: func(context.Context) (bool, string, error) { return true, "0.3.0", nil }},
			{Name: "codex", HostBinary: "codex", StatusRequiresHost: true, Status: func(context.Context) (bool, string, error) {
				t.Error("a host-dependent status must not run for an agent that isn't on PATH")
				return false, "", nil
			}},
			// A config-based agent reads install state from files, so its probe
			// runs even when the binary is absent from PATH. NotInstalledLabel must
			// propagate from the registry into the status.
			{Name: "cfgcli", HostBinary: "cfgcli", NotInstalledLabel: "not configured", Status: func(context.Context) (bool, string, error) { return true, "2.0.0", nil }},
			// A probe can read a version from a store entry that does not register the
			// plugin here. The version is dropped so the JSON report cannot contradict
			// the install state, the way the human report already can't.
			{Name: "stalever", HostBinary: "stalever", Status: func(context.Context) (bool, string, error) { return false, "1.2.3", nil }},
			{Name: "errcli", HostBinary: "errcli", Status: func(context.Context) (bool, string, error) { return false, "", errors.New("probe boom") }},
			{Name: "cursor", HostBinary: "cursor", LegacyHookBased: true, NotInstalledLabel: "not configured", Note: "hook-based; configured in Cursor settings", Status: func(context.Context) (bool, string, error) { return true, "", nil }},
		}
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
	if a := byName["cursor"]; !a.OnPath || a.Install != InstallStateInstalled || a.Version != "" || !a.HookBased || a.Health != HealthOK {
		t.Fatalf("cursor = %+v, want on-path/installed/hook-based/no-version/ok", a)
	} else if a.notInstalledLabel != "not configured" {
		t.Fatalf("cursor notInstalledLabel = %q, want not configured", a.notInstalledLabel)
	}
	cursorJSON, err := json.Marshal(byName["cursor"])
	if err != nil {
		t.Fatalf("marshal cursor: %v", err)
	}
	if !strings.Contains(string(cursorJSON), `"hook_based":true`) {
		t.Fatalf("cursor JSON = %s, want hook_based compatibility field", cursorJSON)
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

func TestDefaultCollectAgentsLeavesUpdateStateUnchanged(t *testing.T) {
	prevLook, prevAgents := lookPath, registeredAgents
	t.Cleanup(func() { lookPath, registeredAgents = prevLook, prevAgents })
	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	registeredAgents = registry.All
	t.Setenv("PATH", "")

	for _, tc := range []struct {
		name      string
		installed bool
	}{
		{name: "missing"},
		{name: "installed", installed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Chdir(root)
			for key, dir := range map[string]string{
				"HOME":                "home",
				"XDG_CONFIG_HOME":     "config",
				"XDG_STATE_HOME":      "state",
				"XDG_CACHE_HOME":      "cache",
				"CLAUDE_CONFIG_DIR":   "claude",
				"COPILOT_HOME":        "copilot",
				"PI_CODING_AGENT_DIR": "pi",
				"VIBE_HOME":           "vibe",
			} {
				path := filepath.Join(root, dir)
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatal(err)
				}
				t.Setenv(key, path)
			}

			files := map[string]string{
				"state/agento11y/update-checks/agent-plugin.stamp": "v1.2.3",
			}
			if tc.installed {
				files["claude/plugins/installed_plugins.json"] = `{"version":2,"plugins":{"agento11y-claude-code@agento11y":[{"scope":"user","version":"1.2.3"}]}}`
				files["copilot/hooks/sigil.json"] = `{}`
				files["config/opencode/opencode.json"] = `{"plugin":["@grafana/agento11y-opencode@0.6.0"]}`
				files["pi/settings.json"] = `{"packages":["npm:@grafana/agento11y-pi@0.1.1"]}`
				files["home/.cursor/hooks.json"] = `{"version":1,"hooks":{
					"sessionStart":[{"command":"sigil cursor hook"}],
					"beforeSubmitPrompt":[{"command":"sigil cursor hook"}],
					"preToolUse":[{"command":"sigil cursor hook"}],
					"afterAgentResponse":[{"command":"sigil cursor hook"}],
					"afterAgentThought":[{"command":"sigil cursor hook"}],
					"postToolUse":[{"command":"sigil cursor hook"}],
					"postToolUseFailure":[{"command":"sigil cursor hook"}],
					"stop":[{"command":"sigil cursor hook"}],
					"sessionEnd":[{"command":"sigil cursor hook"}]
				}}`
				files["vibe/hooks.toml"] = `[[hooks]]
name = "agento11y"
type = "post_agent"
command = "agento11y vibe hook"
[[hooks]]
name = "agento11y-before-tool"
type = "pre_tool"
command = "agento11y vibe hook"
[[hooks]]
name = "agento11y-after-tool"
type = "post_tool"
command = "agento11y vibe hook"
`
			}
			mtime := time.Unix(1_700_000_000, 0)
			for path, content := range files {
				path = filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, mtime, mtime); err != nil {
					t.Fatal(err)
				}
			}

			type fileState struct {
				content string
				mode    os.FileMode
				mtime   int64
			}
			snapshot := func() map[string]fileState {
				t.Helper()
				state := map[string]fileState{}
				err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
					if err != nil {
						return err
					}
					var content []byte
					if !info.IsDir() {
						content, err = os.ReadFile(path)
						if err != nil {
							return err
						}
					}
					state[path] = fileState{content: string(content), mode: info.Mode(), mtime: info.ModTime().UnixNano()}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				return state
			}

			before := snapshot()
			got := defaultCollectAgents(context.Background(), "v9.9.9")
			after := snapshot()
			if len(after) != len(before) {
				t.Errorf("fixture entries = %d, want %d", len(after), len(before))
			}
			for path, want := range before {
				if after[path] != want {
					t.Errorf("%s changed or disappeared during status collection", path)
				}
			}

			wantVersions := map[string]string{
				"claude": "1.2.3", "codex": "", "copilot": "", "cursor": "",
				"opencode": "0.6.0", "pi": "0.1.1", "vibe": "",
			}
			if len(got) != len(wantVersions) {
				t.Fatalf("agents = %d, want %d", len(got), len(wantVersions))
			}
			for _, a := range got {
				version, ok := wantVersions[a.Name]
				if !ok {
					t.Fatalf("unexpected or duplicate agent %q", a.Name)
				}
				delete(wantVersions, a.Name)
				install, health := InstallStateNotInstalled, HealthWarn
				if tc.installed {
					install, health = InstallStateInstalled, HealthOK
				} else {
					version = ""
				}
				if a.Name == "codex" {
					install, health = InstallStateUnknown, HealthSkipped
				}
				if a.OnPath || a.Install != install || a.Health != health || a.Version != version {
					t.Errorf("%s = %+v, want off-path/%s/%s/version %q", a.Name, a, install, health, version)
				}
			}
		})
	}
}

func TestCursorProbeUsesHookConfigurationWording(t *testing.T) {
	agent, ok := registry.Resolve("cursor")
	if !ok {
		t.Fatal("cursor registry entry missing")
	}
	if agent.NotInstalledLabel != "not configured" {
		t.Fatalf("cursor NotInstalledLabel = %q, want not configured", agent.NotInstalledLabel)
	}
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
