import { describe, expect, it } from "vitest";
import {
  encodeAndRedactToolJSON,
  MAX_TOOL_INPUT_BYTES,
  redactError,
  redactFullText,
  redactTitle,
  redactToolJSON,
  redactToolResult,
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

  it.each([
    { name: "exact limit", text: "x".repeat(1 << 20), clipped: false },
    { name: "ASCII", text: "x".repeat((1 << 20) + 1), clipped: true },
    { name: "multibyte", text: `x${"é".repeat(1 << 19)}`, clipped: true },
    {
      name: "surrogate pair",
      text: `xxx${"🙂".repeat(1 << 18)}`,
      clipped: true,
    },
    { name: "control", text: "\0".repeat(200_000), clipped: true },
    { name: "newline", text: "\n".repeat((1 << 19) + 1), clipped: true },
    { name: "quote", text: '"'.repeat((1 << 19) + 1), clipped: true },
    { name: "backslash", text: "\\".repeat((1 << 19) + 1), clipped: true },
    {
      name: "unpaired surrogate",
      text: "\ud800".repeat(200_000),
      clipped: true,
    },
  ])("limits JSON-encoded tool result bytes: $name", ({ text, clipped }) => {
    const result = redactToolResult(text);
    const suffix = clipped ? " [truncated]" : "";
    expect(result.endsWith(suffix)).toBe(true);
    expect(Buffer.byteLength(JSON.stringify(result))).toBeLessThanOrEqual(
      (1 << 20) + '" [truncated]"'.length,
    );
    const prefix = clipped ? result.slice(0, -suffix.length) : result;
    expect(text.startsWith(prefix)).toBe(true);
    if (clipped) {
      const next = Array.from(text.slice(prefix.length))[0] ?? "";
      expect(
        Buffer.byteLength(JSON.stringify(prefix + next)) - 2,
      ).toBeGreaterThan(1 << 20);
    } else {
      expect(result).toBe(text);
    }
  });

  it.each([
    { name: "ASCII", value: "x".repeat(5_000) },
    { name: "multibyte", value: "é".repeat(3_000) },
    { name: "surrogate pair", value: "🙂".repeat(2_000) },
    { name: "escaped control", value: "\0".repeat(1_000) },
  ])("keeps clipped tool arguments parseable: $name", ({ value }) => {
    const raw = JSON.stringify({ text: value });
    const result = redactToolJSON(raw, MAX_TOOL_INPUT_BYTES);
    const decoded = JSON.parse(result);
    expect(decoded).toContain("[truncated]");
    expect(raw.startsWith(decoded.slice(0, -" [truncated]".length))).toBe(true);
    expect(Buffer.byteLength(result)).toBeLessThanOrEqual(
      4096 + '" [truncated]"'.length,
    );
    expect(encodeAndRedactToolJSON({ text: value }, MAX_TOOL_INPUT_BYTES)).toBe(
      result,
    );
    expect(redactToolJSON(raw)).toBe(raw);
  });

  it("redacts secrets before clipping tool content", () => {
    const secret = token();
    const raw = `{"text":"${"x".repeat(4070)} ${secret}"}`;
    const result = redactToolJSON(raw, MAX_TOOL_INPUT_BYTES);
    expect(result).not.toContain(secret.slice(0, 10));
    expect(JSON.parse(result)).toContain("[REDACTED:");
    expect(
      redactToolResult(`${"x".repeat((1 << 20) - 10)} ${secret}`),
    ).not.toContain(secret.slice(0, 8));
  });

  it("omits values that JSON.stringify cannot encode", () => {
    expect(encodeAndRedactToolJSON(1n)).toBeUndefined();
  });
});
