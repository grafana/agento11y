import { execFileSync } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
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
