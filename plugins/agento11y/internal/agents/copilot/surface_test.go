package copilot

import (
	"os"
	"testing"
)

// fakeProc models a synthetic process tree for hasCopilotAncestor: each pid
// maps to its command and parent pid.
type fakeProc struct {
	comm string
	ppid int
}

func withProcessTree(t *testing.T, tree map[int]fakeProc) {
	t.Helper()
	prev := processInfoFn
	t.Cleanup(func() { processInfoFn = prev })
	processInfoFn = func(pid int) (string, int, bool) {
		p, ok := tree[pid]
		if !ok {
			return "", 0, false
		}
		return p.comm, p.ppid, true
	}
}

func TestDetectSurface_EnvOverrideWins(t *testing.T) {
	t.Setenv("SIGIL_COPILOT_HOOK_SURFACE", "copilot-cli")
	// Even with a non-copilot tree, the explicit env must win.
	withProcessTree(t, map[int]fakeProc{})
	if got := detectSurface(); got != "copilot-cli" {
		t.Fatalf("detectSurface() = %q, want copilot-cli", got)
	}
}

func TestHasCopilotAncestor(t *testing.T) {
	cases := []struct {
		name string
		tree map[int]fakeProc
		want bool
	}{
		{
			name: "copilot is direct parent",
			// start=10 -> sh(10) parent copilot(20) -> root(1)
			tree: map[int]fakeProc{
				10: {comm: "-zsh", ppid: 20},
				20: {comm: "/opt/homebrew/bin/copilot", ppid: 1},
			},
			want: true,
		},
		{
			name: "copilot deeper in chain (CLI in vscode terminal)",
			tree: map[int]fakeProc{
				10: {comm: "sh", ppid: 11},
				11: {comm: "copilot", ppid: 12},
				12: {comm: "node", ppid: 13},
				13: {comm: "Code Helper (Plugin)", ppid: 1},
			},
			want: true,
		},
		{
			name: "vscode extension host, no copilot ancestor",
			tree: map[int]fakeProc{
				10: {comm: "sh", ppid: 11},
				11: {comm: "Code Helper (Plugin)", ppid: 12},
				12: {comm: "Electron", ppid: 1},
			},
			want: false,
		},
		{
			name: "unknown pid degrades to false",
			tree: map[int]fakeProc{},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withProcessTree(t, tc.tree)
			if got := hasCopilotAncestor(10, maxSurfaceAncestry); got != tc.want {
				t.Fatalf("hasCopilotAncestor() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDetectSurface_ProcessTree(t *testing.T) {
	t.Setenv("SIGIL_COPILOT_HOOK_SURFACE", "")
	// detectSurface starts its walk from the real parent PID.
	start := os.Getppid()

	t.Run("copilot ancestor yields copilot-cli", func(t *testing.T) {
		withProcessTree(t, map[int]fakeProc{
			start: {comm: "copilot", ppid: 1},
		})
		if got := detectSurface(); got != "copilot-cli" {
			t.Fatalf("detectSurface() = %q, want copilot-cli", got)
		}
	})

	t.Run("no copilot ancestor yields vscode", func(t *testing.T) {
		withProcessTree(t, map[int]fakeProc{
			start: {comm: "node", ppid: 1},
		})
		if got := detectSurface(); got != "vscode" {
			t.Fatalf("detectSurface() = %q, want vscode", got)
		}
	})
}
