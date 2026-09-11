// Match the Go built-in tag builder. Per-generation tags override SDK client
// tags when keys collide.

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
