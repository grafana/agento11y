import { describe, expect, it } from "vitest";
import { detectSkillName, verifyNativeSkill } from "./skills.js";

describe("native catalog verification", () => {
  it.each([
    ["opaque/id", "opaque/id", "review", "/repo", "review"],
    [" id ", "id", "review", "/repo", undefined],
    ["ID", "id", "review", "/repo", undefined],
    ["unknown", "id", "review", "/repo", undefined],
    ["id", "id", "../review", "/repo", undefined],
    ["id", "id", " review ", "/repo", undefined],
    [
      "id",
      "id",
      "ghp_1234567890abcdefghijklmnopqrstuvwxyz",
      "/repo",
      undefined,
    ],
    ["id", "id", "review", "/other", undefined],
  ])("matches raw %s against catalog %s name %s location %s", (raw, id, name, directory, expected) => {
    expect(
      verifyNativeSkill(
        raw,
        {
          location: { directory },
          data: [
            {
              id,
              name,
              path: "/private/SKILL.md",
              get content() {
                throw new Error("must not read content");
              },
            },
          ],
        },
        "/repo",
      ),
    ).toBe(expected);
  });

  it.each([
    null,
    {},
    [],
    {
      location: { directory: "/repo" },
      data: [
        { id: "id", name: "one" },
        { id: "id", name: "two" },
      ],
    },
  ])("rejects missing or ambiguous catalogs %#", (catalog) => {
    expect(verifyNativeSkill("id", catalog, "/repo")).toBeUndefined();
  });
});

describe("detectSkillName", () => {
  it.each([
    "review",
    "team:review.v1",
    "a:b:c",
    "déploiement",
    "a".repeat(256),
    "é".repeat(128),
  ])("accepts structural identity %s", (name) =>
    expect(detectSkillName("skill", { name: ` ${name} ` })).toBe(name));

  it.each([
    undefined,
    null,
    42,
    true,
    {},
    [],
    ["review"],
    { name: null },
    { name: [] },
    { name: 1 },
    { description: "review", path: "review", uri: "review", id: "review" },
    ...[
      "",
      " ",
      "two words",
      "a\nb",
      "a\tb",
      "review\u0000",
      "a\u007f",
      "a\u0085",
      "a\u200bb",
      "a".repeat(257),
      "é".repeat(129),
      "/repo/review",
      "./review",
      "../review",
      "~/.skills/review",
      "C:\\skills\\review",
      "C:/skills/review",
      "https://example.test/review",
      "file:///repo/review",
      "file:review",
      "https:review",
      "git:review",
      ".",
      "..",
      ":review",
      "team:",
      "team::review",
      "team/review",
      "glc_abcdefghijklmnopqrstuv",
      "ghp_1234567890abcdefghijklmnopqrstuvwxyz",
    ].map((name) => ({ name })),
  ])("rejects malformed or private identity %#", (input) => {
    expect(detectSkillName("skill", input)).toBeUndefined();
  });

  it.each([
    "Skill",
    "load_skill",
    "read",
    "mcp_skill",
    "skills",
    "",
  ])("does not reinterpret tool %s", (tool) =>
    expect(detectSkillName(tool, { name: "review" })).toBeUndefined());

  it("ignores content and locations, and does not mutate input", () => {
    const input = Object.freeze({
      name: " team:review ",
      description: "private description glc_abcdefghijklmnopqrstuv",
      path: "/private/review/SKILL.md",
      uri: "file:///private/review/SKILL.md",
    });
    expect(detectSkillName("skill", input)).toBe("team:review");
    expect(input.name).toBe(" team:review ");
  });

  it("uses only the requested field for the pinned v2 contract", () => {
    expect(
      detectSkillName("skill", { id: "v2:review", name: "v1:review" }, "id"),
    ).toBe("v2:review");
    expect(
      detectSkillName("skill", { name: "v1:review" }, "id"),
    ).toBeUndefined();
    expect(
      detectSkillName("skill", { id: "glc_abcdefghijklmnopqrstuv" }, "id"),
    ).toBeUndefined();
  });
});
