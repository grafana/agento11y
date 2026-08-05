import { execFileSync } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import {
  type AutoTag,
  autoTagKey,
  parseAutoTags,
  parseTagPairs,
  resolveAutoTags,
  selectAutoTags,
} from "./autotag.js";

function git(cwd: string, args: string[]): string {
  return execFileSync("git", args, {
    cwd,
    stdio: ["ignore", "pipe", "pipe"],
    encoding: "utf-8",
    timeout: 5000,
  }).trim();
}

// initRepo mirrors git.test.ts: a fixed initial branch and local user config so
// the fixture does not depend on the host's git defaults.
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
    dir = mkdtempSync(join(tmpdir(), "agento11y-pi-autotag-"));
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

  it("prefers the configured user id over the OS account name", () => {
    expect(
      resolveAutoTags(enable("user"), { userId: "alice@example.com" }),
    ).toEqual({ user: "alice@example.com" });
  });

  it("falls back to the OS account name", () => {
    const tags = resolveAutoTags(enable("user"), {});
    expect(tags?.user).toBeTruthy();
    expect(tags?.user).not.toBe("");
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
