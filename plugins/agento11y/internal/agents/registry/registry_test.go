package registry

import (
	"slices"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe"
)

func TestAll(t *testing.T) {
	got := All()
	wantNames := []string{"claude", "codex", "copilot", "cursor", "opencode", "pi", "vibe"}
	gotNames := make([]string, 0, len(got))
	hookBasedNames := make([]string, 0, 1)
	for _, agent := range got {
		gotNames = append(gotNames, agent.Name)
		if agent.LegacyHookBased {
			hookBasedNames = append(hookBasedNames, agent.Name)
		}
		if agent.HostBinary == "" {
			t.Errorf("%s has no host binary", agent.Name)
		}
		if agent.Status == nil {
			t.Errorf("%s has no status probe", agent.Name)
		}
		if agent.Install == nil {
			t.Errorf("%s has no install function", agent.Name)
		}
	}
	if !slices.Equal(gotNames, wantNames) {
		t.Fatalf("agent names = %v, want %v", gotNames, wantNames)
	}
	if !slices.Equal(hookBasedNames, []string{"cursor"}) {
		t.Fatalf("hook-based compatibility entries = %v, want [cursor]", hookBasedNames)
	}

	got[0].Name = "changed"
	if next := All()[0].Name; next != "claude" {
		t.Fatalf("All returned registry storage directly; first name = %q", next)
	}
}

func TestResolve(t *testing.T) {
	agent, ok := Resolve("cursor")
	if !ok {
		t.Fatal("Resolve(cursor) did not find the registered agent")
	}
	if agent.Install == nil || agent.Uninstall == nil {
		t.Fatalf("cursor operations are incomplete: %+v", agent)
	}

	vibeAgent, ok := Resolve("vibe")
	if !ok {
		t.Fatal("Resolve(vibe) did not find the registered agent")
	}
	if vibeAgent.IsMissingHost == nil || !vibeAgent.IsMissingHost(vibe.ErrCLINotFound) {
		t.Fatal("vibe missing host error is not classified as missing_host")
	}

	if _, ok := Resolve("unknown"); ok {
		t.Fatal("Resolve(unknown) found an agent")
	}
}
