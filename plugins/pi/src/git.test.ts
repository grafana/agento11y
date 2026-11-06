import { execFileSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { basename, dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { repoFromRemoteUrl, resolveGitBranch, resolveGitRepo } from "./git.js";

function git(cwd: string, args: string[]): string {
  return execFileSync("git", args, {
    cwd,
    stdio: ["ignore", "pipe", "pipe"],
    encoding: "utf-8",
    timeout: 5000,
  }).trim();
}

function initRepo(cwd: string, branch = "main"): void {
  // Use a fixed initial branch so the test doesn't depend on the host's
  // init.defaultBranch (which differs between machines and CI images).
  git(cwd, ["init", "-q", "-b", branch]);
  // user.* config is required for `git commit` to succeed in CI sandboxes.
  git(cwd, ["config", "user.email", "test@example.com"]);
  git(cwd, ["config", "user.name", "test"]);
  // Avoid inheriting developer/global commit signing settings in local runs.
  git(cwd, ["config", "commit.gpgsign", "false"]);
  git(cwd, ["commit", "-q", "--allow-empty", "-m", "init"]);
}

describe("resolveGitBranch", () => {
  let dir: string;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "sigil-pi-git-"));
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
  });

  it("returns the branch name on a normal checkout", () => {
    initRepo(dir, "feature-x");
    expect(resolveGitBranch(dir)).toBe("feature-x");
  });

  it("returns a short sha on detached HEAD", () => {
    initRepo(dir, "main");
    const sha = git(dir, ["rev-parse", "HEAD"]);
    git(dir, ["checkout", "-q", "--detach", sha]);

    const out = resolveGitBranch(dir);
    expect(out).toBeDefined();
    expect(out).not.toBe("HEAD");
    expect(out).toBe(sha.slice(0, 12));
  });

  it("returns undefined outside a git repo", () => {
    // mkdtemp roots have no `.git` ancestor on macOS/Linux.
    expect(resolveGitBranch(dir)).toBeUndefined();
  });

  it("returns undefined for empty cwd", () => {
    expect(resolveGitBranch("")).toBeUndefined();
  });
});

describe("repoFromRemoteUrl", () => {
  const realPlatform = process.platform;

  function stubPlatform(value: NodeJS.Platform): void {
    Object.defineProperty(process, "platform", {
      value,
      configurable: true,
      writable: true,
    });
  }

  afterEach(() => {
    stubPlatform(realPlatform);
  });

  // A single-letter prefix is ambiguous: a drive on Windows, an ssh `Host`
  // alias everywhere else, so the cases that set `platform` pin both readings.
  const cases: Array<{
    name: string;
    url: string;
    platform?: NodeJS.Platform;
    want?: string;
  }> = [
    {
      name: "scp-style ssh remote",
      url: "git@github.com:grafana/agento11y.git",
      want: "grafana/agento11y",
    },
    {
      name: "https remote",
      url: "https://github.com/grafana/agento11y.git",
      want: "grafana/agento11y",
    },
    {
      name: "ssh url without .git suffix",
      url: "ssh://git@github.com/grafana/agento11y",
      want: "grafana/agento11y",
    },
    {
      name: "ssh url with a port",
      url: "ssh://git@github.com:2222/grafana/agento11y.git",
      want: "grafana/agento11y",
    },
    {
      name: "nested namespace is kept whole",
      url: "git@gitlab.example.com:group/subgroup/repo.git",
      want: "group/subgroup/repo",
    },
    {
      name: "filesystem remote uses the directory name",
      url: "/srv/git/agento11y.git",
      want: "agento11y",
    },
    {
      name: "file url uses the directory name",
      url: "file:///srv/git/agento11y.git",
      want: "agento11y",
    },
    {
      name: "windows drive remote uses the directory name",
      url: "C:/repos/agento11y.git",
      platform: "win32",
      want: "agento11y",
    },
    {
      name: "single-letter host stays a host off windows",
      url: "C:/repos/agento11y.git",
      platform: "linux",
      want: "repos/agento11y",
    },
    { name: "blank", url: "   ", want: undefined },
    { name: "host without a path", url: "https://github.com", want: undefined },
  ];

  for (const tc of cases) {
    it(tc.name, () => {
      if (tc.platform) stubPlatform(tc.platform);
      expect(repoFromRemoteUrl(tc.url)).toBe(tc.want);
    });
  }
});

describe("resolveGitRepo", () => {
  let dir: string;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "agento11y-pi-repo-"));
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
  });

  it("reads the origin remote", () => {
    initRepo(dir);
    git(dir, [
      "remote",
      "add",
      "origin",
      "git@github.com:grafana/agento11y.git",
    ]);
    expect(resolveGitRepo(dir)).toBe("grafana/agento11y");
  });

  it("ignores remotes other than origin", () => {
    initRepo(dir);
    git(dir, [
      "remote",
      "add",
      "upstream",
      "git@github.com:upstream/other.git",
    ]);
    git(dir, [
      "remote",
      "add",
      "origin",
      "git@github.com:grafana/agento11y.git",
    ]);
    expect(resolveGitRepo(dir)).toBe("grafana/agento11y");
  });

  it("falls back to the checkout directory name without an origin remote", () => {
    const checkout = join(dir, "my-project");
    mkdirSync(checkout);
    initRepo(checkout);
    expect(resolveGitRepo(checkout)).toBe("my-project");
  });

  it("reads the main checkout from a linked worktree", () => {
    const main = join(dir, "agento11y");
    mkdirSync(main);
    initRepo(main, "main");
    git(main, [
      "remote",
      "add",
      "origin",
      "git@github.com:grafana/agento11y.git",
    ]);
    const worktree = join(dir, "wt");
    git(main, ["worktree", "add", "-q", "-b", "feature/auto-tags", worktree]);

    expect(resolveGitRepo(worktree)).toBe("grafana/agento11y");
    expect(resolveGitBranch(worktree)).toBe("feature/auto-tags");
  });

  it("names a worktree after the main checkout when there is no remote", () => {
    const main = join(dir, "agento11y");
    mkdirSync(main);
    initRepo(main, "main");
    const worktree = join(dir, "wt");
    git(main, ["worktree", "add", "-q", "-b", "feature/auto-tags", worktree]);

    expect(resolveGitRepo(worktree)).toBe("agento11y");
  });

  it("returns undefined outside a git repo", () => {
    expect(resolveGitRepo(dir)).toBeUndefined();
  });

  it("returns undefined for empty cwd", () => {
    expect(resolveGitRepo("")).toBeUndefined();
  });
});

// conformance/git-remotes/remotes.tsv is shared with the Go plugins and
// Hermes, so every agent gives one repository the same repo tag.
describe("repoFromRemoteUrl conformance", () => {
  const realPlatform = process.platform;

  beforeEach(() => {
    // The corpus is read on POSIX, where `C:/repos/x.git` is an scp remote.
    Object.defineProperty(process, "platform", {
      value: "linux",
      configurable: true,
      writable: true,
    });
  });

  afterEach(() => {
    Object.defineProperty(process, "platform", {
      value: realPlatform,
      configurable: true,
      writable: true,
    });
  });

  const corpus = readFileSync(
    join(
      dirname(fileURLToPath(import.meta.url)),
      "../../../conformance/git-remotes/remotes.tsv",
    ),
    "utf8",
  )
    .trimEnd()
    .split("\n");

  it("has the url, release and want columns", () => {
    expect(corpus[0]).toBe("url\trelease\twant");
  });

  for (const line of corpus.slice(1)) {
    const [url = "", , want = ""] = line.split("\t");
    it(`gives ${url} the tag ${want}`, () => {
      expect(repoFromRemoteUrl(url) ?? "").toBe(want);
    });
  }

  // A host outside the known case-insensitive list can be case-sensitive, so
  // it keeps the tag the released plugin gave it. The one exception is a
  // three-segment http(s) path under `scm/` on a host other than gitlab.com,
  // which loses that prefix.
  it("keeps the released tag for hosts outside the known list", () => {
    const hosts = [
      "gitlab.com",
      "GitLab.com",
      "github.example.com",
      "github.com.example.net",
      "notgithub.com",
      "bitbucket.org.example",
      "dev.azure.com.example",
      "notdev.azure.com",
      "visualstudio.com",
      "ssh.dev.azure.com.example",
      "git.example.com",
      "gh",
    ];
    const segments = [
      "v3",
      "_git",
      "_ssh",
      "DefaultCollection",
      "scm",
      "Org",
      "proj",
      "Repo",
      "a.git",
      "My%20Project",
      "",
    ];
    const users = ["", "git@", "org@", "user:pass@"];
    const ports = ["", ":22", ":7999"];
    // A fixed-seed linear congruential generator keeps failures reproducible.
    let seed = 1;
    const next = (n: number): number => {
      seed = (seed * 1103515245 + 12345) % 2147483648;
      return seed % n;
    };
    const pick = (list: string[]): string => list[next(list.length)] ?? "";
    for (let i = 0; i < 5000; i++) {
      const host = pick(hosts);
      const parts = Array.from({ length: 1 + next(5) }, () => pick(segments));
      let path = parts.join("/");
      if (next(4) === 0) path += ".git/";
      const url =
        [
          `${pick(users)}${host}:${path}`,
          `ssh://${pick(users)}${host}${pick(ports)}/${path}`,
          `git://${host}${pick(ports)}/${path}`,
          `http://${pick(users)}${host}${pick(ports)}/${path}`,
          `https://${pick(users)}${host}${pick(ports)}/${path}`,
        ][next(5)] ?? "";
      let want = releaseRepoFromRemoteUrl(url) ?? "";
      const rest = want.startsWith("scm/") ? want.slice(4) : "";
      if (
        url.startsWith("http") &&
        host.toLowerCase() !== "gitlab.com" &&
        rest.split("/").length === 2
      ) {
        want = rest;
      }
      expect(repoFromRemoteUrl(url) ?? "", url).toBe(want);
    }
  });
});

/**
 * `repoFromRemoteUrl` as released, kept as the oracle for the unknown-host
 * property.
 */
function releaseRepoFromRemoteUrl(raw: string): string | undefined {
  const url = raw.trim();
  if (!url) return undefined;
  const trim = (s: string) => s.replace(/\.git$/, "");
  const strip = (s: string) => s.replace(/\/+$/, "");
  if (url.startsWith("file://")) {
    return trim(basename(strip(url.slice("file://".length))));
  }
  let path: string;
  const scheme = /^[a-zA-Z][a-zA-Z0-9+.-]*:\/\//.exec(url);
  const colon = url.indexOf(":");
  const slash = url.indexOf("/");
  if (scheme) {
    const rest = url.slice(scheme[0].length);
    const at = rest.indexOf("/");
    if (at === -1) return undefined;
    path = rest.slice(at + 1);
  } else if (colon > 0 && (slash === -1 || colon < slash)) {
    path = url.slice(colon + 1);
  } else {
    return trim(basename(strip(url)));
  }
  path = strip(path).replace(/^\/+/, "");
  if (!path) return undefined;
  return trim(path);
}
