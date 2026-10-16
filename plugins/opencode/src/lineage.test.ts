import { createHash } from "node:crypto";
import { describe, expect, it } from "vitest";
import {
  stableOpencodeCompactionGenerationId,
  stableOpencodeGenerationId,
} from "./lineage.js";

describe("stableOpencodeCompactionGenerationId", () => {
  it("is stable on replay and uses the compaction namespace", () => {
    const id = stableOpencodeCompactionGenerationId("sess-1", "event-1");
    expect(id).toBe(stableOpencodeCompactionGenerationId("sess-1", "event-1"));
    expect(id).toBe(stableOpencodeGenerationId("sess-1", "compaction:event-1"));
    expect(id).toMatch(/^opencode-[0-9a-f]{24}$/);
  });

  it("is isolated from the assistant ID for the same event string", () => {
    expect(stableOpencodeCompactionGenerationId("sess-1", "event-1")).not.toBe(
      stableOpencodeGenerationId("sess-1", "event-1"),
    );
  });

  it.each([
    ["sess-2", "event-1"],
    ["sess-1", "event-2"],
  ])("is isolated across sessions and terminals (%s, %s)", (sessionID, eventID) => {
    expect(stableOpencodeCompactionGenerationId("sess-1", "event-1")).not.toBe(
      stableOpencodeCompactionGenerationId(sessionID, eventID),
    );
  });
});

describe("stableOpencodeGenerationId", () => {
  it("is deterministic for identical inputs", () => {
    expect(stableOpencodeGenerationId("sess-1", "msg-1")).toBe(
      stableOpencodeGenerationId("sess-1", "msg-1"),
    );
  });

  it("differs for a different message id", () => {
    expect(stableOpencodeGenerationId("sess-1", "msg-1")).not.toBe(
      stableOpencodeGenerationId("sess-1", "msg-2"),
    );
  });

  it("differs for a different session id", () => {
    expect(stableOpencodeGenerationId("sess-1", "msg-1")).not.toBe(
      stableOpencodeGenerationId("sess-2", "msg-1"),
    );
  });

  it("uses the opencode- prefix and a 24-hex-char digest", () => {
    const id = stableOpencodeGenerationId("sess-1", "msg-1");
    expect(id).toMatch(/^opencode-[0-9a-f]{24}$/);
  });

  it("matches the documented sha256(sessionID\\0messageID)[:24] shape", () => {
    const expected = `opencode-${createHash("sha256")
      .update("sess-1\0msg-1")
      .digest("hex")
      .slice(0, 24)}`;
    expect(stableOpencodeGenerationId("sess-1", "msg-1")).toBe(expected);
  });
});
