import {
  linkSync,
  mkdirSync,
  mkdtempSync,
  rmSync,
  statSync,
  symlinkSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import { homedir, tmpdir } from "node:os";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import {
  matchSkillRead,
  snapshotSkills,
  validateSkillName,
  verifySkillRead,
} from "./skills.js";

async function nativeRead(path: string, cwd: string) {
  const moduleURL = new URL(
    "../node_modules/@mariozechner/pi-coding-agent/dist/core/tools/read.js",
    import.meta.url,
  ).href;
  const native = await import(/* @vite-ignore */ moduleURL);
  return native.createReadTool(cwd).execute("identity-control", { path });
}

const aliasSpellings = [
  ["NFC to NFD", "café.md", "café.md".normalize("NFD")],
  ["NFD to NFC", "café.md".normalize("NFD"), "café.md"],
  ["lowercase to uppercase", "guide.md", "GUIDE.md"],
  ["uppercase to lowercase", "GUIDE.md", "guide.md"],
] as const;

describe("skill read identities", () => {
  let cwd: string;
  beforeEach(() => {
    cwd = mkdtempSync(join(tmpdir(), "pi-skills-"));
  });
  afterEach(() => {
    rmSync(cwd, { recursive: true, force: true });
  });

  it.each([
    "review",
    "My.Plugin:Review_2",
    "org:plugin:review",
    "é".repeat(128),
    "a".repeat(256),
  ])("keeps structural name %s", (name) => {
    expect(validateSkillName(` ${name} `)).toBe(name);
  });
  it.each([
    undefined,
    {},
    "",
    "a".repeat(257),
    "é".repeat(129),
    "review\n",
    "a\u0085b",
    "foo/bar",
    "foo\\bar",
    ".",
    "..",
    "./review",
    "https://example.com",
    "file:review",
    ":review",
    "org::review",
    "review instructions",
    "review@host",
    "glc_abcdefghijklmnopqrstuvwxyz",
    "plugin:glc_abcdefghijklmnopqrstuvwxyz",
  ])("rejects unsafe name %s", (name) => {
    // A colon is a namespace separator; a URL-like scheme without slashes is
    // structurally indistinguishable, except known URI schemes.
    expect(validateSkillName(name)).toBeUndefined();
  });

  it("requires a fresh regular file even for exact catalog paths", () => {
    const catalog = snapshotSkills(
      { skills: [{ name: "review", filePath: "guide.md" }] },
      cwd,
    );
    const verify = () =>
      verifySkillRead(catalog, "read", { path: "guide.md" }, cwd);
    expect(matchSkillRead(catalog, "read", { path: "guide.md" }, cwd)).toBe(
      "review",
    );
    expect(verify()).toBeUndefined();
    writeFileSync(join(cwd, "guide.md"), "");
    expect(verify()).toBe("review");
    unlinkSync(join(cwd, "guide.md"));
    mkdirSync(join(cwd, "guide.md"));
    expect(verify()).toBeUndefined();
  });

  it.each([
    "exact",
    "symlink",
    "hardlink",
  ])("rejects ambiguous catalog identities via %s", (spelling) => {
    writeFileSync(join(cwd, "guide.md"), "");
    linkSync(join(cwd, "guide.md"), join(cwd, "other.md"));
    symlinkSync("guide.md", join(cwd, "alias.md"));
    const catalog = snapshotSkills(
      {
        skills: [
          { name: "review", filePath: "guide.md" },
          { name: "other", filePath: "other.md" },
        ],
      },
      cwd,
    );
    const path =
      spelling === "exact"
        ? "guide.md"
        : spelling === "symlink"
          ? "alias.md"
          : "other.md";
    expect(verifySkillRead(catalog, "read", { path }, cwd)).toBeUndefined();
    expect(
      verifySkillRead([catalog[0]!, catalog[0]!], "read", { path }, cwd),
    ).toBeUndefined();
    expect(verifySkillRead(catalog.slice(0, 1), "read", { path }, cwd)).toBe(
      "review",
    );
  });

  it("snapshots only final name and filePath, including standalone Markdown", () => {
    const options = {
      skills: [{ name: " old ", filePath: "guide.md", description: "private" }],
    };
    const catalog = snapshotSkills(options, cwd);
    options.skills[0]!.name = "later";
    expect(catalog).toEqual([{ name: "old", filePath: join(cwd, "guide.md") }]);
    expect(matchSkillRead(catalog, "read", { path: "guide.md" }, cwd)).toBe(
      "old",
    );
    for (const options of [
      undefined,
      {},
      { skills: [] },
      { skills: null },
      {
        get skills() {
          throw new Error("unavailable");
        },
      },
    ]) {
      expect(snapshotSkills(options, cwd)).toEqual([]);
    }
  });

  it("uses only raw native read paths and never infers from basenames or output", () => {
    const catalog = snapshotSkills(
      { skills: [{ name: "review", filePath: "guide.md" }] },
      cwd,
    );
    for (const [tool, args] of [
      ["Read", { path: "guide.md" }],
      ["bash", { path: "guide.md" }],
      ["read", '{"path":"guide.md"}'],
      ["read", { filePath: "guide.md" }],
      ["read", { path: "other/guide.md" }],
      ["read", { path: " guide.md " }],
      ["read", { path: "@@guide.md" }],
      ["read", { path: "guide.md/resource" }],
      [
        "read",
        {
          get path() {
            throw new Error("lookup");
          },
        },
      ],
    ] as const) {
      expect(matchSkillRead(catalog, tool, args, cwd)).toBeUndefined();
      expect(verifySkillRead(catalog, tool, args, cwd)).toBeUndefined();
    }
    for (const path of [
      "guide.md",
      "@guide.md",
      "./dir/../guide.md",
      pathToFileURL(join(cwd, "guide.md")).href,
    ]) {
      expect(matchSkillRead(catalog, "read", { path }, cwd)).toBe("review");
    }
  });

  it("expands tilde paths without reading the host home directory", () => {
    const catalog = snapshotSkills(
      {
        skills: [
          { name: "review", filePath: join(homedir(), "pi-test-guide.md") },
        ],
      },
      cwd,
    );
    expect(
      matchSkillRead(catalog, "read", { path: "@~/pi-test-guide.md" }, cwd),
    ).toBe("review");
    expect(
      matchSkillRead(
        catalog,
        "read",
        { path: "~someone/pi-test-guide.md" },
        cwd,
      ),
    ).toBeUndefined();
    expect(
      matchSkillRead(catalog, "read", { path: "file://%invalid" }, cwd),
    ).toBeUndefined();
  });

  it.each([
    "original",
    "amPm",
    "nfd",
    "curly",
    "nfdCurly",
  ])("agrees with pinned pi 0.87.1 read resolution for %s", async (variant) => {
    const moduleURL = new URL(
      "../node_modules/@mariozechner/pi-coding-agent/dist/core/tools/path-utils.js",
      import.meta.url,
    ).href;
    const native = await import(/* @vite-ignore */ moduleURL);
    const original = "Capture d'écran 1 PM.md";
    const variants: Record<string, string> = {
      original,
      amPm: original.replace(" PM.", "\u202fPM."),
      nfd: original.normalize("NFD"),
      curly: original.replace("'", "’"),
      nfdCurly: original.normalize("NFD").replace("'", "’"),
    };
    writeFileSync(join(cwd, variants[variant]!), "");
    const selected = native.resolveReadPath(original, cwd);
    const catalog = snapshotSkills(
      { skills: [{ name: "review", filePath: selected }] },
      cwd,
    );
    expect(verifySkillRead(catalog, "read", { path: original }, cwd)).toBe(
      "review",
    );
    expect(matchSkillRead(catalog, "read", { path: original }, cwd)).toBe(
      "review",
    );
  });

  it.for(aliasSpellings)("matches existing filesystem aliases: %s", async ([
    _label,
    file,
    alias,
  ], context) => {
    writeFileSync(join(cwd, file), "instruction identity control");
    const fileStat = statSync(join(cwd, file), { bigint: true });
    const aliasStat = statSync(join(cwd, alias), {
      bigint: true,
      throwIfNoEntry: false,
    });
    if (
      !aliasStat ||
      aliasStat.dev !== fileStat.dev ||
      aliasStat.ino !== fileStat.ino
    ) {
      return context.skip(
        "Filesystem does not support this spelling as an alias",
      );
    }
    expect([aliasStat.dev, aliasStat.ino]).toEqual([
      fileStat.dev,
      fileStat.ino,
    ]);
    expect((await nativeRead(alias, cwd)).content).toEqual([
      { type: "text", text: "instruction identity control" },
    ]);
    const catalog = snapshotSkills(
      { skills: [{ name: "review", filePath: file }] },
      cwd,
    );
    expect(matchSkillRead(catalog, "read", { path: alias }, cwd)).toBe(
      "review",
    );
    expect(verifySkillRead(catalog, "read", { path: alias }, cwd)).toBe(
      "review",
    );
  });

  it.for(
    aliasSpellings,
  )("does not equate distinct existing spellings: %s", async ([
    _label,
    file,
    other,
  ], context) => {
    writeFileSync(join(cwd, file), "instruction");
    const fileStat = statSync(join(cwd, file), { bigint: true });
    const otherPath = join(cwd, other);
    if (statSync(otherPath, { throwIfNoEntry: false })) {
      return context.skip(
        "Filesystem aliases these spellings instead of separating them",
      );
    }
    writeFileSync(otherPath, "not the instruction", { flag: "wx" });
    const otherStat = statSync(otherPath, { bigint: true });
    expect([otherStat.dev, otherStat.ino]).not.toEqual([
      fileStat.dev,
      fileStat.ino,
    ]);
    expect((await nativeRead(other, cwd)).content).toEqual([
      { type: "text", text: "not the instruction" },
    ]);
    const catalog = snapshotSkills(
      { skills: [{ name: "review", filePath: file }] },
      cwd,
    );
    expect(
      matchSkillRead(catalog, "read", { path: other }, cwd),
    ).toBeUndefined();
  });

  it.each([
    { name: "alpha", otherName: "beta" },
    { name: "beta", otherName: "beta" },
    { name: "alpha", otherName: "unsafe display" },
    { name: "unsafe display", otherName: "unsafe display" },
  ])("checks all hardlinked native skills: $name / $otherName", async ({
    name,
    otherName,
  }) => {
    for (const directory of ["alpha", otherName]) {
      mkdirSync(join(cwd, directory));
    }
    writeFileSync(
      join(cwd, "alpha/SKILL.md"),
      "---\ndescription: Review changes\n---\nShared instructions\n",
    );
    linkSync(join(cwd, "alpha/SKILL.md"), join(cwd, otherName, "SKILL.md"));
    const moduleURL = new URL(
      "../node_modules/@mariozechner/pi-coding-agent/dist/core/skills.js",
      import.meta.url,
    ).href;
    const { loadSkills } = await import(/* @vite-ignore */ moduleURL);
    const { skills } = loadSkills({
      cwd,
      agentDir: cwd,
      skillPaths: [join(cwd, "alpha"), join(cwd, otherName)],
      includeDefaults: false,
    });
    const catalog = snapshotSkills({ skills }, cwd);
    expect(skills).toHaveLength(2);
    expect(catalog.map((skill) => skill.name)).toEqual([
      "alpha",
      validateSkillName(otherName),
    ]);
    expect(
      verifySkillRead(
        catalog,
        "read",
        { path: join(cwd, name, "SKILL.md") },
        cwd,
      ),
    ).toBeUndefined();
    expect(
      matchSkillRead(
        catalog,
        "read",
        { path: join(cwd, name, "SKILL.md") },
        cwd,
      ),
    ).toBe(validateSkillName(name) ?? "alpha");
  });

  it("does not match an unrelated file with identical contents", async () => {
    for (const file of ["guide.md", "unrelated.md"]) {
      writeFileSync(join(cwd, file), "same content");
    }
    const guide = statSync(join(cwd, "guide.md"), { bigint: true });
    const unrelated = statSync(join(cwd, "unrelated.md"), { bigint: true });
    expect([unrelated.dev, unrelated.ino]).not.toEqual([guide.dev, guide.ino]);
    expect((await nativeRead("unrelated.md", cwd)).content).toEqual([
      { type: "text", text: "same content" },
    ]);
    const catalog = snapshotSkills(
      { skills: [{ name: "review", filePath: "guide.md" }] },
      cwd,
    );
    expect(
      matchSkillRead(catalog, "read", { path: "unrelated.md" }, cwd),
    ).toBeUndefined();
  });

  it.each(
    aliasSpellings,
  )("uses only lexical identity for missing files: %s", async (_label, file, alias) => {
    const catalog = snapshotSkills(
      { skills: [{ name: "review", filePath: file }] },
      cwd,
    );
    await expect(nativeRead(file, cwd)).rejects.toThrow();
    expect(matchSkillRead(catalog, "read", { path: file }, cwd)).toBe("review");
    expect(
      matchSkillRead(catalog, "read", { path: alias }, cwd),
    ).toBeUndefined();
  });

  it("normalizes Unicode spaces without trimming", () => {
    const catalog = snapshotSkills(
      { skills: [{ name: "review", filePath: "my guide.md" }] },
      cwd,
    );
    for (const space of ["\u00a0", "\u2002", "\u202f", "\u3000"]) {
      expect(
        matchSkillRead(catalog, "read", { path: `my${space}guide.md` }, cwd),
      ).toBe("review");
    }
  });

  it("selects the first existing native variant, not any catalog match", async () => {
    const original = "Capture d'écran 1 PM.md";
    const amPm = original.replace(" PM.", "\u202fPM.");
    const nfd = original.normalize("NFD");
    const curly = original.replace("'", "’");
    const combined = nfd.replace("'", "’");
    const variants = [original, amPm, nfd, curly, combined];
    for (const file of variants) {
      writeFileSync(join(cwd, file), file === amPm ? "instruction" : "other");
    }
    const originalStat = statSync(join(cwd, original), { bigint: true });
    const amPmStat = statSync(join(cwd, amPm), { bigint: true });
    expect([originalStat.dev, originalStat.ino]).not.toEqual([
      amPmStat.dev,
      amPmStat.ino,
    ]);
    expect((await nativeRead(original, cwd)).content).toEqual([
      { type: "text", text: "other" },
    ]);
    // macOS may treat NFC and NFD as the same filename.
    const catalog = snapshotSkills(
      { skills: [{ name: "review", filePath: amPm }] },
      cwd,
    );
    expect(
      matchSkillRead(catalog, "read", { path: original }, cwd),
    ).toBeUndefined();
    expect(
      verifySkillRead(catalog, "read", { path: original }, cwd),
    ).toBeUndefined();
    unlinkSync(join(cwd, original));
    expect(verifySkillRead(catalog, "read", { path: original }, cwd)).toBe(
      "review",
    );
    expect((await nativeRead(original, cwd)).content).toEqual([
      { type: "text", text: "instruction" },
    ]);
    expect(matchSkillRead(catalog, "read", { path: original }, cwd)).toBe(
      "review",
    );
  });

  it("does not combine AM/PM fallback with curly apostrophes", () => {
    const original = "Capture d'écran 1 PM.md";
    const wrong = original.replace(" PM.", "\u202fPM.").replace("'", "’");
    writeFileSync(join(cwd, wrong), "");
    const catalog = snapshotSkills(
      { skills: [{ name: "review", filePath: wrong }] },
      cwd,
    );
    expect(
      matchSkillRead(catalog, "read", { path: original }, cwd),
    ).toBeUndefined();
  });

  it("refreshes symlinks and falls back to lexical equality for failed loads", () => {
    writeFileSync(join(cwd, "one.md"), "");
    writeFileSync(join(cwd, "two.md"), "");
    symlinkSync("one.md", join(cwd, "alias.md"));
    const catalog = snapshotSkills(
      { skills: [{ name: "review", filePath: "alias.md" }] },
      cwd,
    );
    expect(matchSkillRead(catalog, "read", { path: "one.md" }, cwd)).toBe(
      "review",
    );
    unlinkSync(join(cwd, "alias.md"));
    symlinkSync("two.md", join(cwd, "alias.md"));
    expect(
      matchSkillRead(catalog, "read", { path: "one.md" }, cwd),
    ).toBeUndefined();
    expect(matchSkillRead(catalog, "read", { path: "two.md" }, cwd)).toBe(
      "review",
    );
    expect(
      verifySkillRead(catalog, "read", { path: "one.md" }, cwd),
    ).toBeUndefined();
    expect(verifySkillRead(catalog, "read", { path: "two.md" }, cwd)).toBe(
      "review",
    );
    unlinkSync(join(cwd, "two.md"));
    expect(matchSkillRead(catalog, "read", { path: "alias.md" }, cwd)).toBe(
      "review",
    );
    expect(
      verifySkillRead(catalog, "read", { path: "alias.md" }, cwd),
    ).toBeUndefined();
  });
});
