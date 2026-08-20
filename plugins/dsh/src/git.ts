import { execFileSync } from "node:child_process";
import { basename, dirname, isAbsolute, resolve } from "node:path";

/** Matches plugins/cursor: detached HEAD resolves to a 12-character SHA. */
export function resolveGitBranch(cwd: string): string | undefined {
  if (!cwd) return undefined;
  const branch = runGit(["rev-parse", "--abbrev-ref", "HEAD"], cwd);
  if (!branch) return undefined;
  if (branch !== "HEAD") return branch;
  return runGit(["rev-parse", "--short=12", "HEAD"], cwd);
}

/**
 * Mirrors `gitbranch.Repo` in plugins/agento11y/internal/gitbranch.
 * Repository IDs preserve the full origin namespace. Without an origin,
 * linked worktrees use the shared Git directory to name the main checkout.
 */
export function resolveGitRepo(cwd: string): string | undefined {
  if (!cwd) return undefined;
  const fromRemote = repoFromRemoteUrl(
    runGit(["config", "--get", "remote.origin.url"], cwd),
  );
  if (fromRemote) return fromRemote;
  return repoFromGitDir(cwd);
}

/**
 * Returns the host-side repository path and preserves nested namespaces.
 * Filesystem remotes return only their directory name.
 */
export function repoFromRemoteUrl(raw: string | undefined): string | undefined {
  const url = (raw ?? "").trim();
  if (!url) return undefined;
  if (url.startsWith("file://")) {
    return trimRepoName(
      basename(stripTrailingSlashes(url.slice("file://".length))),
    );
  }
  let path: string;
  const scheme = /^[a-zA-Z][a-zA-Z0-9+.-]*:\/\//.exec(url);
  if (scheme) {
    const rest = url.slice(scheme[0].length);
    const slash = rest.indexOf("/");
    if (slash === -1) return undefined;
    path = rest.slice(slash + 1);
  } else if (isScpLike(url)) {
    path = url.slice(url.indexOf(":") + 1);
  } else {
    return trimRepoName(basename(stripTrailingSlashes(url)));
  }
  path = stripTrailingSlashes(path).replace(/^\/+/, "");
  if (!path) return undefined;
  return trimRepoName(path);
}

/**
 * Git treats a colon before the first slash as scp syntax. Only Windows treats
 * a drive prefix as local; on POSIX, `g:owner/name.git` can target SSH host `g`.
 */
function isScpLike(url: string): boolean {
  const colon = url.indexOf(":");
  if (colon <= 0) return false;
  if (process.platform === "win32" && hasDrivePrefix(url)) return false;
  const slash = url.indexOf("/");
  return slash === -1 || colon < slash;
}

function hasDrivePrefix(url: string): boolean {
  return /^[a-zA-Z]:/.test(url);
}

// `--git-common-dir` is shared by linked worktrees, so the fallback names the
// main checkout.
function repoFromGitDir(cwd: string): string | undefined {
  const raw = runGit(["rev-parse", "--git-common-dir"], cwd);
  if (!raw) return undefined;
  const gitDir = isAbsolute(raw) ? raw : resolve(cwd, raw);
  let base = basename(gitDir);
  if (base === ".git") base = basename(dirname(gitDir));
  base = trimRepoName(base);
  if (!base || base === "." || base === "/") return undefined;
  return base;
}

function trimRepoName(path: string): string {
  return path.replace(/\.git$/, "");
}

function stripTrailingSlashes(s: string): string {
  return s.replace(/\/+$/, "");
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
