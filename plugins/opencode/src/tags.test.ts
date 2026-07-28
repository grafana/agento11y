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
    {
      name: "subagent set alongside the other keys",
      in: { cwd: "/repo", gitBranch: "main", isSubagent: true },
      want: { "git.branch": "main", cwd: "/repo", subagent: "true" },
    },
    {
      name: "subagent absent when false",
      in: { cwd: "/workspace/repo", gitBranch: "main", isSubagent: false },
      want: { "git.branch": "main", cwd: "/workspace/repo" },
    },
    {
      name: "subagent only",
      in: { isSubagent: true },
      want: { subagent: "true" },
    },
    {
      name: "empty-string inputs with subagent false return undefined",
      in: { cwd: "", gitBranch: "", isSubagent: false },
      want: undefined,
    },
  ];

  it.each(cases)("$name", ({ in: input, want }) => {
    expect(buildBuiltinTags(input)).toEqual(want);
  });
});
