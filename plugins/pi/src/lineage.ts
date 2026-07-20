import { createHash } from "node:crypto";

/**
 * Lineage helpers for deterministic, branch-aware Pi generation IDs.
 *
 * Pi persists conversation history as an append-only entry tree
 * (`SessionManager`), where each `SessionEntry` carries a stable `id` and a
 * `parentId`. The active branch is the path from the current leaf back to
 * the root, returned by `getBranch()`. We hash `conversationId + entryId`
 * for the assistant message entry of the turn being exported, and look up
 * the nearest previous assistant entry on the same branch as the parent.
 *
 * The Pi runtime types are referenced structurally so this plugin keeps
 * working against minor changes in `@mariozechner/pi-coding-agent`. Unknown
 * shapes degrade to an empty result, which lets the SDK fall back to its
 * usual `gen-*` ID behavior.
 */

/**
 * Subset of the Pi session manager shape consumed by the lineage helper.
 * Mirrors `ReadonlySessionManager` in pi-coding-agent, but only the methods
 * we actually use, so the helper stays usable with future signature drift.
 */
export interface SessionManagerLike {
  getBranch?: (fromId?: string) => SessionEntryLike[];
}

/**
 * Minimal `SessionEntry` shape: just the fields we read. `getBranch()`
 * returns mixed entry types (message, thinking_level_change, model_change,
 * compaction, …); we only treat `type === "message"` entries with an
 * assistant message as relevant.
 */
export interface SessionEntryLike {
  type?: string;
  id?: string;
  parentId?: string | null;
  message?: { role?: string } | null;
}

/** What `resolvePiGenerationLineage` returns. Empty when no lineage. */
export interface PiGenerationLineage {
  generationId?: string;
  parentGenerationIds?: string[];
}

/**
 * Deterministic Pi generation ID: SHA-256 of `${conversationId}\0${entryId}`
 * truncated to 24 hex chars and prefixed with `pi-`. Matches the convention
 * in `plugins/agento11y/internal/agents/{codex,copilot}/mapper/mapper.go`, but
 * uses a Pi-specific prefix so generation IDs identify the producer plugin.
 */
export function stablePiGenerationId(
  conversationId: string,
  entryId: string,
): string {
  const hex = createHash("sha256")
    .update(`${conversationId}\0${entryId}`)
    .digest("hex");
  return `pi-${hex.slice(0, 24)}`;
}

/**
 * Resolve `{ generationId, parentGenerationIds }` for the assistant turn
 * currently being exported.
 *
 * Strategy:
 *  1. Read the active branch via `sessionManager.getBranch()`. Bail with an
 *     empty result if the runtime does not expose it.
 *  2. Locate the assistant message entry that corresponds to `assistantMessage`.
 *     Prefer object identity (the same `Message` instance pi handed us in
 *     `turn_end`), and fall back to the latest assistant entry on the branch.
 *  3. Hash it as `generationId`. Then walk earlier along the branch for the
 *     nearest other assistant message entry and hash that as the parent.
 *
 * The first assistant turn on a branch produces no parent. When the branch
 * is unavailable (older runtimes) or no assistant entry can be found, the
 * helper returns `{}` so the SDK keeps its existing fallback behavior.
 */
export function resolvePiGenerationLineage(
  sessionManager: SessionManagerLike | undefined | null,
  assistantMessage: unknown,
  conversationId: string | undefined,
): PiGenerationLineage {
  if (!conversationId) return {};
  if (!sessionManager || typeof sessionManager.getBranch !== "function") {
    return {};
  }

  let branch: SessionEntryLike[];
  try {
    branch = sessionManager.getBranch();
  } catch {
    return {};
  }
  if (!Array.isArray(branch) || branch.length === 0) return {};

  // `getBranch()` returns the path from the root down to the leaf (see
  // SessionManager.getBranch, which `unshift`s while walking parentId
  // upward). We still resolve the parent via the parentId chain rather
  // than positional order to stay robust against future changes and to
  // make the intent obvious on branched trees.
  const assistantEntries = branch.filter(isAssistantMessageEntry);
  if (assistantEntries.length === 0) return {};

  // Object-identity match: pi appends the assistant message into the session
  // tree right before `turn_end` fires (session-manager appendMessage),
  // and the event payload carries the same `Message` reference. Identity is
  // therefore the most precise way to pin down which branch entry to use.
  let currentIndex = assistantEntries.findIndex(
    (entry) => entry.message === assistantMessage,
  );
  if (currentIndex === -1) {
    // Fallback when the event payload's message is not the same object
    // reference as the one persisted in the session tree (e.g. extensions
    // that clone messages). Pick the last assistant entry in `branch` order,
    // which in practice points at the just-appended turn under the current
    // pi runtime.
    currentIndex = assistantEntries.length - 1;
  }

  const currentEntry = assistantEntries[currentIndex];
  if (!currentEntry || typeof currentEntry.id !== "string") return {};

  const generationId = stablePiGenerationId(conversationId, currentEntry.id);

  // Walk the parentId chain upward through the branch to find the nearest
  // ancestor assistant entry. On linear branches this is the previous
  // assistant turn; on branched trees it is the assistant turn at the
  // branch point, not the most recent chronological assistant entry from
  // a sibling branch.
  const parentId = findParentAssistantEntryId(
    currentEntry,
    branch,
    assistantEntries,
  );
  if (!parentId) return { generationId };

  return {
    generationId,
    parentGenerationIds: [stablePiGenerationId(conversationId, parentId)],
  };
}

/**
 * Walk the parentId chain from `entry` toward the root and return the id of
 * the nearest ancestor that is an assistant message entry. Returns
 * `undefined` for the first assistant turn on the branch.
 *
 * `branch` is used to look up entries by id; `assistantEntries` is the
 * pre-filtered list, used to detect when an ancestor is itself an
 * assistant entry.
 */
function findParentAssistantEntryId(
  entry: SessionEntryLike,
  branch: SessionEntryLike[],
  assistantEntries: SessionEntryLike[],
): string | undefined {
  const byId = new Map<string, SessionEntryLike>();
  for (const e of branch) {
    if (typeof e.id === "string") byId.set(e.id, e);
  }
  const assistantIds = new Set<string>();
  for (const e of assistantEntries) {
    if (typeof e.id === "string") assistantIds.add(e.id);
  }

  let cursor = entry.parentId;
  const seen = new Set<string>();
  while (typeof cursor === "string" && !seen.has(cursor)) {
    seen.add(cursor);
    const parent = byId.get(cursor);
    if (!parent) return undefined;
    if (assistantIds.has(cursor)) return cursor;
    cursor = parent.parentId ?? null;
  }
  return undefined;
}

function isAssistantMessageEntry(entry: SessionEntryLike): boolean {
  if (!entry || entry.type !== "message") return false;
  const role = entry.message?.role;
  return role === "assistant";
}
