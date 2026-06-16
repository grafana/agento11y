import { createHash } from "node:crypto";

/**
 * Deterministic OpenCode generation ID: SHA-256 of
 * `${sessionID}\0${messageID}` truncated to 24 hex chars and prefixed with
 * `opencode-`. Matches the convention in
 * `plugins/sigil/internal/agents/{codex,copilot}/mapper/mapper.go` and
 * `plugins/pi/src/lineage.ts` (`stablePiGenerationId`), but uses an
 * opencode-specific prefix so generation IDs identify the producer plugin.
 *
 * The same assistant message therefore always maps to the same generation
 * id. Re-exporting it (on resume, restart, or a double-firing event) hits
 * the backend's first-write-wins dedup and is rejected as a no-op instead
 * of inflating token and cost totals.
 */
export function stableOpencodeGenerationId(
  sessionID: string,
  messageID: string,
): string {
  const hex = createHash("sha256")
    .update(`${sessionID}\0${messageID}`)
    .digest("hex");
  return `opencode-${hex.slice(0, 24)}`;
}
