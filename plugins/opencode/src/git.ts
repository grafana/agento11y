import { execFileSync } from "node:child_process";

/**
 * Resolve the current git branch via `git rev-parse`.
 *
 * Returns the branch name on a normal checkout, a 12-char short sha on
 * detached HEAD (matching plugins/cursor), or undefined when git is
 * unavailable or `cwd` is not in a repo.
 */
export function resolveGitBranch(cwd: string): string | undefined {
  if (!cwd) return undefined;
  const branch = runGit(["rev-parse", "--abbrev-ref", "HEAD"], cwd);
  if (!branch) return undefined;
  if (branch !== "HEAD") return branch;
  // Detached HEAD: fall back to a short sha.
  return runGit(["rev-parse", "--short=12", "HEAD"], cwd);
}

function runGit(args: string[], cwd: string): string | undefined {
  try {
    const out = execFileSync("git", args, {
      cwd,
      stdio: ["ignore", "pipe", "ignore"],
      encoding: "utf-8",
      timeout: 1000,
    }).trim();
    return out.length > 0 ? out : undefined;
  } catch {
    return undefined;
  }
}
