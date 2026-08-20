import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { Context } from "@deepseek-ai/cordis";

const bundleUrl = new URL("../dist/index.js", import.meta.url);
const source = await readFile(bundleUrl, "utf8");
for (const dependency of ["@deepseek-ai/cordis", "@deepseek-ai/dsh-llm"]) {
  assert.ok(
    !source.includes(dependency),
    `${dependency} appeared in the production bundle`,
  );
}

const plugin = await import(bundleUrl.href);
const ctx = new Context();
try {
  await ctx.plugin(plugin);
} finally {
  await ctx.fiber.dispose();
}
