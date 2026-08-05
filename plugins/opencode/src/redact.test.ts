import { describe, expect, it } from "vitest";
import { createRedactor } from "./hooks.js";

/**
 * Pattern-by-pattern coverage lives in redact.conformance.test.ts, which runs
 * the shared fixtures from redaction/fixtures/strings.json through the plugin's
 * redactor. The other five engines run the same fixtures in their own suites.
 * This file keeps what the fixtures do not express: the redactor's behavior on
 * opencode-shaped payloads, and the plugin's own email choice. Which mode each
 * field gets is in mappers.test.ts.
 */
describe("createRedactor", () => {
  const redactor = createRedactor();

  describe("redact (full — tier 1 + tier 2)", () => {
    it("redacts a tier 1 token", () => {
      const result = redactor.redact(
        "token: glc_abcdefghijklmnopqrstuvwxyz1234",
      );
      expect(result).toBe("token: [REDACTED:grafana-cloud-token]");
    });

    it("redacts an env value and keeps the key", () => {
      const result = redactor.redact("DATABASE_PASSWORD=hunter2secret123");
      expect(result).toBe("DATABASE_PASSWORD=[REDACTED:env-secret-value]");
    });

    it("keeps tool-call JSON parseable", () => {
      const input = JSON.stringify({
        command: "export API_KEY=abc123",
        cwd: "/tmp",
      });
      const result = redactor.redact(input);
      expect(() => JSON.parse(result)).not.toThrow();
      expect(JSON.parse(result).cwd).toBe("/tmp");
    });

    it("does NOT redact normal text", () => {
      const input = "The function returns a list of users from the database.";
      expect(redactor.redact(input)).toBe(input);
    });

    it("does NOT redact UUIDs", () => {
      const input = "session-id: 550e8400-e29b-41d4-a716-446655440000";
      expect(redactor.redact(input)).toBe(input);
    });

    it("handles empty string", () => {
      expect(redactor.redact("")).toBe("");
    });
  });

  describe("redactLightweight (tier 1 only)", () => {
    it("redacts a tier 1 token", () => {
      const result = redactor.redactLightweight(
        "I found the token: glc_abcdefghijklmnopqrstuvwxyz1234",
      );
      expect(result).toBe("I found the token: [REDACTED:grafana-cloud-token]");
    });

    it("does NOT redact env file patterns (tier 2 only)", () => {
      const input = "The file contains DATABASE_PASSWORD=hunter2secret123";
      expect(redactor.redactLightweight(input)).toBe(input);
    });

    it("does NOT redact normal text", () => {
      const input =
        "The API key configuration is stored in the settings panel.";
      expect(redactor.redactLightweight(input)).toBe(input);
    });
  });

  // The shared table carries an email pattern the SDKs redact by default and
  // the plugin opts out of. See redaction/README.md.
  describe("email addresses", () => {
    it("leaves email addresses alone in both modes", () => {
      const input = "reported by person@example.com";
      expect(redactor.redact(input)).toBe(input);
      expect(redactor.redactLightweight(input)).toBe(input);
    });
  });
});
