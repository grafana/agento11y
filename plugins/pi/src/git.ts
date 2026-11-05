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
 * `canonicalRepo` then gives each spelling of one repository the same label
 * on the hosts where that is known to be safe.
 *
 * Mirrors `repoFromRemoteURL` in plugins/agento11y/internal/gitbranch; both
 * must give the `want` column of conformance/git-remotes/remotes.tsv.
 */
export function repoFromRemoteUrl(raw: string | undefined): string | undefined {
  const url = (raw ?? "").trim();
  if (!url) return undefined;
  if (url.startsWith("file://")) {
    return trimRepoName(
      basename(stripTrailingSlashes(url.slice("file://".length))),
    );
  }
  let authority: string;
  let path: string;
  let overHttp = false;
  const scheme = /^[a-zA-Z][a-zA-Z0-9+.-]*:\/\//.exec(url);
  if (scheme) {
    const rest = url.slice(scheme[0].length);
    const slash = rest.indexOf("/");
    if (slash === -1) return undefined;
    authority = rest.slice(0, slash);
    path = rest.slice(slash + 1);
    overHttp = /^https?:\/\/$/i.test(scheme[0]);
  } else if (isScpLike(url)) {
    const colon = url.indexOf(":");
    authority = url.slice(0, colon);
    path = url.slice(colon + 1);
  } else {
    // A local path remote (/srv/git/name.git, ../name, ~/name).
    return trimRepoName(basename(stripTrailingSlashes(url)));
  }
  path = stripTrailingSlashes(path).replace(/^\/+/, "");
  if (!path) return undefined;
  return canonicalRepo(remoteHost(authority), overHttp, path);
}

/**
 * Hosts that resolve a repository path regardless of case, so
 * `Grafana/Agento11y` and `grafana/agento11y` name one repository there.
 * gitlab.com is not listed: GitLab does not document case-insensitive path
 * lookup. Any other host may be a case-sensitive server, such as gitolite or a
 * self-hosted GitLab, where lowercasing could merge two repositories.
 */
const CASE_INSENSITIVE_HOSTS = new Set([
  "github.com",
  "ssh.github.com",
  "bitbucket.org",
]);

/**
 * Lowercased host name of a URL authority or scp-style prefix, without user
 * info or port. An authority it can't read, such as a bracketed IPv6 address,
 * yields a name that matches no known host.
 */
function remoteHost(authority: string): string {
  const host = authority.slice(authority.lastIndexOf("@") + 1);
  return host.replace(/:.*$/s, "").toLowerCase();
}

/**
 * Rewrite the path of a remote on a known host into the one label that host's
 * other remote forms also get, and drop a `.git` suffix. Known hosts ignore
 * case, so there the suffix is matched in any case too. A host it doesn't
 * know keeps the path as the remote spells it, apart from Bitbucket Server's
 * `scm/`.
 */
function canonicalRepo(host: string, overHttp: boolean, path: string): string {
  if (CASE_INSENSITIVE_HOSTS.has(host)) {
    return path.toLowerCase().replace(/\.git$/, "");
  }
  if (
    host === "dev.azure.com" ||
    host === "ssh.dev.azure.com" ||
    host.endsWith(".visualstudio.com")
  ) {
    return azureRepo(host, path);
  }
  const repo = trimRepoName(path);
  // Bitbucket Server serves `ssh://host:7999/proj/repo` over HTTPS as
  // `https://host/scm/proj/repo`. Exactly three segments leaves out a context
  // path (`/bitbucket/scm/proj/repo`), which stays as it is. gitlab.com is a
  // known host where `scm` is only a group name, so its HTTPS and SSH remotes
  // keep the same label.
  if (overHttp && host !== "gitlab.com" && repo.startsWith("scm/")) {
    const rest = repo.slice("scm/".length);
    if (rest.split("/").length === 2) return rest;
  }
  return repo;
}

/**
 * Reduce every Azure DevOps remote form to lowercase `org/project/repo`. SSH
 * remotes spell it `v3/org/project/repo`, HTTPS ones `org/project/_git/repo`,
 * and legacy visualstudio.com ones carry the org in the host name and may add
 * `DefaultCollection/` or use `_ssh` for `_git`. Azure ignores case, so the
 * markers and a `.git` suffix match in any case. A path in any other shape
 * keeps the release label.
 */
function azureRepo(host: string, path: string): string {
  let segments = path
    .toLowerCase()
    .replace(/\.git$/, "")
    .split("/");
  if (host === "ssh.dev.azure.com" || host === "vs-ssh.visualstudio.com") {
    if (segments.length !== 4 || segments[0] !== "v3") {
      return trimRepoName(path);
    }
    return segments.slice(1).join("/");
  }
  if (host.endsWith(".visualstudio.com")) {
    segments = [host.slice(0, -".visualstudio.com".length), ...segments];
  }
  if (segments[1] === "defaultcollection") segments.splice(1, 1);
  if (
    segments.length !== 4 ||
    (segments[2] !== "_git" && segments[2] !== "_ssh")
  ) {
    return trimRepoName(path);
  }
  return `${segments[0]}/${segments[1]}/${segments[3]}`;
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
