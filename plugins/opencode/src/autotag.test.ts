import { execFileSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { basename, dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import {
  type AutoTag,
  autoTagKey,
  parseAutoTags,
  parseTagPairs,
  resolveAutoTags,
  selectAutoTags,
} from "./autotag.js";
import { repoFromRemoteUrl, resolveGitBranch, resolveGitRepo } from "./git.js";

function git(cwd: string, args: string[]): string {
  return execFileSync("git", args, {
    cwd,
    stdio: ["ignore", "pipe", "pipe"],
    encoding: "utf-8",
    timeout: 5000,
  }).trim();
}

// initRepo uses a fixed initial branch and local user config so the fixture
// does not depend on the host's git defaults or global commit signing.
function initRepo(cwd: string, branch = "main"): void {
  git(cwd, ["init", "-q", "-b", branch]);
  git(cwd, ["config", "user.email", "test@example.com"]);
  git(cwd, ["config", "user.name", "test"]);
  git(cwd, ["config", "commit.gpgsign", "false"]);
  git(cwd, ["commit", "-q", "--allow-empty", "-m", "init"]);
}

function enable(...names: AutoTag[]): Set<AutoTag> {
  return new Set(names);
}

describe("parseAutoTags", () => {
  const cases: Array<{
    name: string;
    input: string;
    enabled: AutoTag[];
    unknown: string[];
  }> = [
    { name: "unset", input: "", enabled: [], unknown: [] },
    { name: "whitespace only", input: "  \t ", enabled: [], unknown: [] },
    { name: "single name", input: "user", enabled: ["user"], unknown: [] },
    {
      name: "two names",
      input: "user,repo",
      enabled: ["user", "repo"],
      unknown: [],
    },
    {
      name: "surrounding whitespace and empty entries",
      input: " user , , repo ,",
      enabled: ["user", "repo"],
      unknown: [],
    },
    {
      name: "all is shorthand for every name",
      input: "all",
      enabled: ["user", "repo", "branch"],
      unknown: [],
    },
    {
      name: "mixed case is accepted",
      input: "User,REPO,Branch",
      enabled: ["user", "repo", "branch"],
      unknown: [],
    },
    {
      name: "unknown name is reported and the rest still parse",
      input: "user,team",
      enabled: ["user"],
      unknown: ["team"],
    },
    {
      name: "only unknown names",
      input: "Team, squad",
      enabled: [],
      unknown: ["team", "squad"],
    },
    {
      name: "duplicate names collapse",
      input: "repo,repo,all",
      enabled: ["user", "repo", "branch"],
      unknown: [],
    },
  ];

  for (const tc of cases) {
    it(tc.name, () => {
      const got = parseAutoTags(tc.input);
      expect([...got.enabled]).toEqual(expect.arrayContaining(tc.enabled));
      expect(got.enabled.size).toBe(tc.enabled.length);
      expect(got.unknown).toEqual(tc.unknown);
    });
  }
});

describe("selectAutoTags", () => {
  // Lookup over a suffix -> value map, the shape config.ts passes from the
  // process environment.
  function lookupFrom(env: Record<string, string>) {
    return (suffix: string) => {
      const value = (env[suffix] ?? "").trim();
      return value === "" ? undefined : { value, key: `AGENTO11Y_${suffix}` };
    };
  }

  const cases: Array<{
    name: string;
    env: Record<string, string>;
    on: boolean;
    enabled: AutoTag[];
    unknown?: string[];
    namesSet?: boolean;
    warns?: string;
  }> = [
    { name: "switch unset resolves nothing", env: {}, on: false, enabled: [] },
    {
      name: "switch off resolves nothing",
      env: { AUTO_CODING_AGENT_TAGS: "false" },
      on: false,
      enabled: [],
    },
    {
      name: "switch on takes every name",
      env: { AUTO_CODING_AGENT_TAGS: "true" },
      on: true,
      enabled: ["user", "repo", "branch"],
    },
    {
      name: "the allowlist narrows the switch",
      env: {
        AUTO_CODING_AGENT_TAGS: "1",
        AUTO_CODING_AGENT_TAGS_NAMES: " User , repo ",
      },
      on: true,
      enabled: ["user", "repo"],
      namesSet: true,
    },
    {
      name: "an allowlist of all is the same as no allowlist",
      env: {
        AUTO_CODING_AGENT_TAGS: "yes",
        AUTO_CODING_AGENT_TAGS_NAMES: "all",
      },
      on: true,
      enabled: ["user", "repo", "branch"],
      namesSet: true,
    },
    {
      name: "an unsupported name is reported and the rest still apply",
      env: {
        AUTO_CODING_AGENT_TAGS: "true",
        AUTO_CODING_AGENT_TAGS_NAMES: "user,team",
      },
      on: true,
      enabled: ["user"],
      unknown: ["team"],
      namesSet: true,
      warns: "unsupported names team",
    },
    {
      name: "an allowlist of only unsupported names turns nothing on",
      env: {
        AUTO_CODING_AGENT_TAGS: "true",
        AUTO_CODING_AGENT_TAGS_NAMES: "team",
      },
      on: true,
      enabled: [],
      unknown: ["team"],
      namesSet: true,
      warns: "names no supported value",
    },
    {
      name: "an allowlist without the switch resolves nothing",
      env: { AUTO_CODING_AGENT_TAGS_NAMES: "user" },
      on: false,
      enabled: [],
      namesSet: true,
      warns: "is off",
    },
    {
      name: "a list in the switch is not a boolean and is redirected",
      env: { AUTO_CODING_AGENT_TAGS: "user,repo" },
      on: false,
      enabled: [],
      warns: "AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES",
    },
  ];

  for (const tc of cases) {
    it(tc.name, () => {
      const warnings: string[] = [];
      const got = selectAutoTags(lookupFrom(tc.env), (m) => warnings.push(m));

      expect(got.on).toBe(tc.on);
      expect([...got.enabled]).toEqual(tc.enabled);
      expect(got.unknown).toEqual(tc.unknown ?? []);
      expect(got.namesSet).toBe(tc.namesSet ?? false);
      if (tc.warns === undefined) {
        expect(warnings).toEqual([]);
      } else {
        expect(warnings.join(" ")).toContain(tc.warns);
      }
    });
  }
});

describe("autoTagKey", () => {
  it("writes the branch under the per-generation built-in key", () => {
    expect(autoTagKey("user")).toBe("user");
    expect(autoTagKey("repo")).toBe("repo");
    expect(autoTagKey("branch")).toBe("git.branch");
  });
});

describe("parseTagPairs", () => {
  it("parses pairs and skips malformed entries", () => {
    expect(parseTagPairs("repo=monorepo, git.branch=release")).toEqual({
      repo: "monorepo",
      "git.branch": "release",
    });
    expect(parseTagPairs("noequals,=novalue,key=")).toEqual({});
    expect(parseTagPairs(undefined)).toEqual({});
  });
});

describe("resolveAutoTags", () => {
  let dir: string;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "agento11y-opencode-autotag-"));
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
  });

  it("resolves nothing when no name is enabled", () => {
    expect(
      resolveAutoTags(enable(), { cwd: dir, userId: "alice" }),
    ).toBeUndefined();
  });

  it("resolves the repository and branch from the checkout", () => {
    initRepo(dir, "feature/auto-tags");
    git(dir, [
      "remote",
      "add",
      "origin",
      "git@github.com:grafana/agento11y.git",
    ]);

    expect(resolveAutoTags(enable("repo", "branch"), { cwd: dir })).toEqual({
      repo: "grafana/agento11y",
      "git.branch": "feature/auto-tags",
    });
  });

  it("omits repository and branch outside a checkout", () => {
    expect(
      resolveAutoTags(enable("repo", "branch"), { cwd: dir }),
    ).toBeUndefined();
  });

  it("uses the configured user id", () => {
    expect(
      resolveAutoTags(enable("user"), { userId: "alice@example.com" }),
    ).toEqual({ user: "alice@example.com" });
  });

  it("never falls back to the OS account name", () => {
    expect(resolveAutoTags(enable("user"), {})).toBeUndefined();
  });

  it("trims and caps a resolved value", () => {
    const tags = resolveAutoTags(enable("user"), {
      userId: `  ${"a".repeat(130)}  `,
    });
    expect(tags?.user).toBe("a".repeat(128));
  });

  it("leaves out a key an explicit tag already defines", () => {
    initRepo(dir, "main");
    git(dir, [
      "remote",
      "add",
      "origin",
      "git@github.com:grafana/agento11y.git",
    ]);

    expect(
      resolveAutoTags(enable("repo", "branch"), {
        cwd: dir,
        explicitTags: { repo: "monorepo" },
      }),
    ).toEqual({ "git.branch": "main" });
    expect(
      resolveAutoTags(enable("branch"), {
        cwd: dir,
        explicitTags: { "git.branch": "release" },
      }),
    ).toBeUndefined();
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
    dir = mkdtempSync(join(tmpdir(), "agento11y-opencode-repo-"));
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
