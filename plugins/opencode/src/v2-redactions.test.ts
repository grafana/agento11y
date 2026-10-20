import type { Message } from "@grafana/agento11y";
import { describe, expect, it } from "vitest";
import { createGuardRedactions } from "./v2-redactions.js";

const text = (role: string, value: string): Message => ({
  role,
  parts: [{ type: "text", text: value }],
});

describe("guard redactions", () => {
  it("replaces recorded text the guard changed, in that session only", () => {
    const redactions = createGuardRedactions();
    redactions.record(
      "s",
      [text("user", "Update on Project Falcon"), text("user", "unchanged")],
      [text("user", "Update on [REDACTED]"), text("user", "unchanged")],
    );

    expect(redactions.text("s", "Update on Project Falcon")).toBe(
      "Update on [REDACTED]",
    );
    expect(redactions.text("s", "unchanged")).toBe("unchanged");
    expect(redactions.text("other", "Update on Project Falcon")).toBe(
      "Update on Project Falcon",
    );
  });

  it("replaces a redacted part inside a longer recorded string", () => {
    const redactions = createGuardRedactions();
    redactions.record(
      "s",
      [text("system", "Escalate to Project Falcon leads")],
      [text("system", "Escalate to [REDACTED] leads")],
    );

    // The recorded system prompt joins instruction parts.
    expect(
      redactions.text("s", "Be brief.\n\nEscalate to Project Falcon leads"),
    ).toBe("Be brief.\n\nEscalate to [REDACTED] leads");
  });

  it("rewrites text parts and content strings, and leaves other parts alone", () => {
    const redactions = createGuardRedactions();
    redactions.record(
      "s",
      [text("user", "Falcon")],
      [{ role: "user", content: "[REDACTED]" }],
    );
    const tool = {
      type: "tool_call",
      toolCall: { id: "t1", name: "read", inputJSON: '{"q":"Falcon"}' },
    } as const;
    const recorded: Message[] = [
      { role: "user", parts: [{ type: "text", text: "Falcon" }, tool] },
      { role: "user", content: "Falcon" },
    ];

    const out = redactions.messages("s", recorded);

    expect(out[0]?.parts?.[0]).toEqual({ type: "text", text: "[REDACTED]" });
    expect(out[0]?.parts?.[1]).toBe(tool);
    expect(out[1]?.content).toBe("[REDACTED]");
    // The input array is not mutated.
    expect(recorded[0]?.parts?.[0]).toEqual({ type: "text", text: "Falcon" });
  });

  it("ignores misaligned transforms and empty text", () => {
    const redactions = createGuardRedactions();
    redactions.record("s", [text("user", "Falcon")], []);
    redactions.record("s", [text("user", "")], [text("user", "[REDACTED]")]);

    expect(redactions.text("s", "Falcon")).toBe("Falcon");
    expect(redactions.messages("s", [text("user", "Falcon")])).toEqual([
      text("user", "Falcon"),
    ]);
  });
});
