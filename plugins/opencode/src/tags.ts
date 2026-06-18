// Build per-generation built-in tags for the opencode plugin.
//
// Mirrors the Go side in plugins/sigil/internal/agents/cursor/tags/tags.go
// `Build`: only emits keys whose values are non-empty, returns `undefined`
// when no inputs are populated. User-supplied `SIGIL_TAGS` is layered in by
// the SDK at the client level; the seed/built-in tags merged here take
// precedence on collisions because the SDK merges per-generation Tags atop
// client-level Tags.

export interface BuiltinTagInputs {
  cwd?: string;
  gitBranch?: string;
}

export function buildBuiltinTags(
  in_: BuiltinTagInputs,
): Record<string, string> | undefined {
  const out: Record<string, string> = {};
  if (in_.gitBranch) {
    out["git.branch"] = in_.gitBranch;
  }
  if (in_.cwd) {
    out.cwd = in_.cwd;
  }
  if (Object.keys(out).length === 0) {
    return undefined;
  }
  return out;
}
