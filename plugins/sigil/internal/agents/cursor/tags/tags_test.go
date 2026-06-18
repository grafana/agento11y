package tags

import "testing"

func TestBuild(t *testing.T) {
	cases := []struct {
		name  string
		in    BuiltinInputs
		check func(t *testing.T, got map[string]string)
	}{
		{
			name: "git branch and explicit cwd populated",
			in: BuiltinInputs{
				WorkspaceRoot: "/ws",
				Cwd:           "/real-cwd",
				GitBranch:     "main",
			},
			check: func(t *testing.T, got map[string]string) {
				if got["git.branch"] != "main" {
					t.Errorf("git.branch = %q; want main", got["git.branch"])
				}
				if got["cwd"] != "/real-cwd" {
					t.Errorf("cwd = %q; want /real-cwd", got["cwd"])
				}
			},
		},
		{
			name: "cwd falls back to workspace root",
			in:   BuiltinInputs{WorkspaceRoot: "/ws"},
			check: func(t *testing.T, got map[string]string) {
				if got["cwd"] != "/ws" {
					t.Errorf("cwd should fall back to workspace root; got %q", got["cwd"])
				}
			},
		},
		{
			name: "no inputs returns nil",
			in:   BuiltinInputs{},
			check: func(t *testing.T, got map[string]string) {
				if got != nil {
					t.Errorf("Build with no inputs must return nil; got %v", got)
				}
			},
		},
		{
			name: "subagent set when background",
			in:   BuiltinInputs{IsBackgroundAgent: true, WorkspaceRoot: "/ws"},
			check: func(t *testing.T, got map[string]string) {
				if got["subagent"] != "true" {
					t.Errorf("subagent should be set; got %q", got["subagent"])
				}
			},
		},
		{
			name: "subagent absent when not background",
			in:   BuiltinInputs{WorkspaceRoot: "/ws"},
			check: func(t *testing.T, got map[string]string) {
				if _, ok := got["subagent"]; ok {
					t.Errorf("subagent should be absent for non-background agent")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, Build(tc.in))
		})
	}
}
