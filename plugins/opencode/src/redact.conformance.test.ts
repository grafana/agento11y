import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import {
  redactSecretText,
  redactSecretTextLightweight,
} from "@grafana/agento11y-core";
import { describe, expect, it } from "vitest";
import { createRedactor } from "./hooks.js";
import type { Redactor } from "./mappers.js";

/**
 * The opencode plugin redacts in its own mappers, so it needs its own proof
 * that it produces the same bytes as the four SDKs and the shared Go binary.
 * All six load this same fixture file.
 */
const fixturesDir = join(
  dirname(fileURLToPath(import.meta.url)),
  "../../../redaction/fixtures/",
);

interface StringCase {
  id: string;
  mode: "full" | "light";
  emails: boolean;
  input: string;
  expected: string;
}

const stringCases: StringCase[] = JSON.parse(
  readFileSync(join(fixturesDir, "strings.json"), "utf8"),
).cases;

/**
 * Production opencode always keeps email addresses, so `createRedactor` covers
 * the `emails: false` cases and the email-enabled cases go straight to the
 * shared helpers.
 */
function redactorFor(emails: boolean): Redactor {
  if (!emails) {
    return createRedactor();
  }
  return {
    redact: (text: string) =>
      redactSecretText(text, { redactEmailAddresses: true }),
    redactLightweight: (text: string) =>
      redactSecretTextLightweight(text, { redactEmailAddresses: true }),
  };
}

describe("conformance: redaction strings", () => {
  it("loads the shared fixtures", () => {
    expect(stringCases.length).toBeGreaterThan(0);
  });

  for (const testCase of stringCases) {
    it(testCase.id, () => {
      const redactor = redactorFor(testCase.emails);
      const actual =
        testCase.mode === "full"
          ? redactor.redact(testCase.input)
          : redactor.redactLightweight(testCase.input);
      expect(actual).toBe(testCase.expected);
    });
  }
});
