import { describe, expect, it } from "vitest";
import {
  encodeAndRedactToolJSON,
  redactError,
  redactFullText,
  redactTitle,
  redactToolJSON,
} from "./redact.js";

const token = () => "ghp_" + "0123456789abcdefghijklmnopqrstuvwxyz";

describe("dsh redaction policy", () => {
  it("uses lightweight redaction for titles and error copies", () => {
    const secret = token();
    const input = `${secret} TOKEN=plain-value`;
    expect(redactTitle(input)).not.toContain(secret);
    expect(redactTitle(input)).toContain("TOKEN=plain-value");

    const original = new Error(input);
    const redacted = redactError(original);
    expect(redacted).not.toBe(original);
    expect(redacted.message).not.toContain(secret);
    expect(redacted.message).toContain("TOKEN=plain-value");
    expect(redacted.stack).not.toContain(secret);
  });

  it("uses full redaction for tool text", () => {
    const secret = token();
    const redacted = redactFullText(`${secret} TOKEN=plain-value`);
    expect(redacted).not.toContain(secret);
    expect(redacted).not.toContain("plain-value");
  });

  it("keeps ordinary JSON parseable while redacting secret values", () => {
    const secret = token();
    const redacted = redactToolJSON(
      JSON.stringify({ command: `echo ${secret}`, token: "plain-value" }),
    );
    expect(() => JSON.parse(redacted)).not.toThrow();
    expect(redacted).not.toContain(secret);
    expect(redacted).not.toContain("plain-value");
  });

  it("replaces a valid payload when text redaction breaks its JSON", () => {
    const redacted = redactToolJSON(
      JSON.stringify({ command: 'run "token: plain-value" now' }),
    );
    expect(JSON.parse(redacted)).toBe("[REDACTED:json]");
    expect(redacted).not.toContain("plain-value");
  });

  it("best-effort redacts malformed model arguments without throwing", () => {
    const secret = token();
    const redacted = redactToolJSON(`{"token":"${secret}`);
    expect(redacted).not.toContain(secret);
  });

  it("omits values that JSON.stringify cannot encode", () => {
    expect(encodeAndRedactToolJSON(1n)).toBeUndefined();
  });
});
