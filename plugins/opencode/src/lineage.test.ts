import { createHash } from "node:crypto";
import { describe, expect, it } from "vitest";
import { stableOpencodeGenerationId } from "./lineage.js";

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
