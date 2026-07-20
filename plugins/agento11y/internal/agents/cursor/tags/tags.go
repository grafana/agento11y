package tags

// BuiltinInputs carries the values used to populate built-in tag keys.
//
// `git.branch` resolution lives in the shared package
// plugins/agento11y/internal/gitbranch so codex/copilot can reuse it without
// importing this cursor-scoped package.
type BuiltinInputs struct {
	WorkspaceRoot     string
	Cwd               string
	Entrypoint        string
	GitBranch         string
	IsBackgroundAgent bool
}

// Build returns the per-generation built-in tags. SIGIL_TAGS-supplied values
// are layered in by the SDK at the client level; built-ins take precedence
// because the SDK merges per-generation Tags atop client-level Tags.
// Returns nil when no built-ins are present.
func Build(in BuiltinInputs) map[string]string {
	out := make(map[string]string, 4)
	if in.GitBranch != "" {
		out["git.branch"] = in.GitBranch
	}
	cwd := in.Cwd
	if cwd == "" {
		cwd = in.WorkspaceRoot
	}
	if cwd != "" {
		out["cwd"] = cwd
	}
	if in.Entrypoint != "" {
		out["entrypoint"] = in.Entrypoint
	}
	if in.IsBackgroundAgent {
		out["subagent"] = "true"
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
