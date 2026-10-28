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

describe("guard redactions under review cases", () => {
  const user = (value: string) => text("user", value);

  it("keeps every redaction while capture holds the session, however many there are", () => {
    const redactions = createGuardRedactions();
    // More than the old caps: 256 pairs per session and 512 sessions.
    for (let i = 0; i < 300; i++) {
      redactions.record(
        "s",
        [user(`code-${i} here`)],
        [user("[REDACTED] here")],
      );
    }
    for (let i = 0; i < 600; i++) {
      redactions.record(`other-${i}`, [user("Falcon")], [user("[REDACTED]")]);
    }

    expect(redactions.text("s", "code-0 here")).toBe("[REDACTED] here");
    expect(redactions.text("other-0", "Falcon")).toBe("[REDACTED]");

    redactions.forget("s");
    expect(redactions.text("s", "code-0 here")).toBe("code-0 here");
  });

  it("composes a redaction of already-redacted text", () => {
    const redactions = createGuardRedactions();
    const original = "Ping jane@example.com about Project Falcon";
    // A first guard removes the address, then a second one the project.
    redactions.record(
      "s",
      [user(original)],
      [user("Ping [REDACTED] about Project Falcon")],
    );
    redactions.record(
      "s",
      [user("Ping [REDACTED] about Project Falcon")],
      [user("Ping [REDACTED] about [REDACTED]")],
    );

    expect(redactions.text("s", original)).toBe(
      "Ping [REDACTED] about [REDACTED]",
    );
  });

  it("redacts overlapping values the same way whichever was recorded first", () => {
    const joined = "Notes: Falcon Project Falcon";
    const shortFirst = createGuardRedactions();
    shortFirst.record("s", [user("Falcon")], [user("[REDACTED]")]);
    shortFirst.record("s", [user("Project Falcon")], [user("[REDACTED]")]);
    const longFirst = createGuardRedactions();
    longFirst.record("s", [user("Project Falcon")], [user("[REDACTED]")]);
    longFirst.record("s", [user("Falcon")], [user("[REDACTED]")]);

    expect(shortFirst.text("s", joined)).toBe("Notes: [REDACTED] [REDACTED]");
    expect(longFirst.text("s", joined)).toBe("Notes: [REDACTED] [REDACTED]");
  });

  it("redacts a shortened or cut-off title", () => {
    const redactions = createGuardRedactions();
    redactions.record(
      "s",
      [user("Status update on Project Falcon for the board")],
      [user("Status update on [REDACTED] for the board")],
    );

    expect(redactions.title("s", "Update on Project Falcon")).toBe(
      "Update on [REDACTED]",
    );
    expect(redactions.title("s", "Status update on Project Fal…")).toBe(
      "Status update on [REDACTED]",
    );
    expect(redactions.title("s", "Board status")).toBe("Board status");
  });

  it("treats a redaction to empty text as removing the whole part", () => {
    const redactions = createGuardRedactions();
    redactions.record("s", [user("Project Falcon")], [user("")]);

    expect(redactions.text("s", "Project Falcon")).toBe("[REDACTED]");
  });
});
