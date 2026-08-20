import { describe, expect, it } from "vitest";
import { buildBuiltinTags } from "./tags.js";

describe("buildBuiltinTags", () => {
  const cases: {
    name: string;
    in: Parameters<typeof buildBuiltinTags>[0];
    want: ReturnType<typeof buildBuiltinTags>;
  }[] = [
    {
      name: "both keys populated",
      in: { cwd: "/repo", gitBranch: "main" },
      want: { "git.branch": "main", cwd: "/repo" },
    },
    {
      name: "branch only",
      in: { gitBranch: "main" },
      want: { "git.branch": "main" },
    },
    {
      name: "cwd only",
      in: { cwd: "/repo" },
      want: { cwd: "/repo" },
    },
    {
      name: "empty inputs return undefined",
      in: {},
      want: undefined,
    },
    {
      name: "empty-string inputs return undefined",
      in: { cwd: "", gitBranch: "" },
      want: undefined,
    },
  ];

  it.each(cases)("$name", ({ in: input, want }) => {
    expect(buildBuiltinTags(input)).toEqual(want);
  });
});
