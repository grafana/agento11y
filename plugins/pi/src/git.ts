import { execFileSync } from "node:child_process";
import { basename, dirname, isAbsolute, resolve } from "node:path";

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

/**
 * Resolve the repository the checkout at `cwd` belongs to, as `owner/name`
 * taken from the `origin` remote URL. Nested namespaces are kept whole, so a
 * GitLab subgroup remote resolves to `group/subgroup/name`. Without an origin
 * remote the repository is named after its checkout directory. Returns
 * undefined when git is unavailable or `cwd` is not in a repo.
 *
 * Mirrors `gitbranch.Repo` in plugins/agento11y/internal/gitbranch: in a
 * linked worktree both the remote URL and the directory fallback describe the
 * main checkout, because the remote and the shared git directory live there.
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
 * Reduce a remote URL to the path that identifies the repository on its host:
 * `git@host:owner/name.git`, `https://host/owner/name` and
 * `ssh://git@host:2222/owner/name` all yield `owner/name`. A filesystem remote
 * has no host-side path, so it yields the directory name alone.
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
    // A local path remote (/srv/git/name.git, ../name, ~/name).
    return trimRepoName(basename(stripTrailingSlashes(url)));
  }
  path = stripTrailingSlashes(path).replace(/^\/+/, "");
  if (!path) return undefined;
  return trimRepoName(path);
}

/**
 * Report whether `url` uses git's scp-style syntax, `[user@]host:path`. The
 * marker is a colon before the first slash: `git@github.com:owner/name` is
 * scp-style, `/srv/git:odd/name` is a filesystem path.
 *
 * On Windows a single-letter prefix is a drive designator, so
 * `C:/repos/name.git` is a local path there. The check is platform-gated
 * because git gates it the same way: its drive-prefix test is a no-op on POSIX
 * builds, where `g:owner/name.git` is a working remote that reaches host `g`
 * through an ssh `Host g` alias.
 */
function isScpLike(url: string): boolean {
  const colon = url.indexOf(":");
  if (colon <= 0) return false;
  if (process.platform === "win32" && hasDrivePrefix(url)) return false;
  const slash = url.indexOf("/");
  return slash === -1 || colon < slash;
}

/** Report whether `url` starts with a Windows drive designator, `C:` or `c:`. */
function hasDrivePrefix(url: string): boolean {
  return /^[a-zA-Z]:/.test(url);
}

/**
 * Name the repository after its directory, used when it has no origin remote.
 * `--git-common-dir` is the shared git directory, so a linked worktree reports
 * the main checkout rather than the worktree.
 */
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
