import { createHash } from "node:crypto";
import type { SessionOrigin } from "./sessionOrigin.js";

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
 * compaction, branch_summary, …). Only `type === "message"` entries with an
 * assistant message can be a lineage parent; the other types are walked
 * through. `compaction` and `branch_summary` entries get a generation id of
 * their own instead, see {@link resolvePiSummaryLineage}.
 */
export interface SessionEntryLike {
  type?: string;
  id?: string;
  parentId?: string | null;
  /** ISO-8601, set by pi when the entry is appended. */
  timestamp?: string;
  message?: { role?: string } | null;
}

/** Extra context that decides whether a parent edge is safe to emit. */
export interface ResolveLineageOptions {
  /** Where this session came from. See `sessionOrigin.ts`. */
  origin?: SessionOrigin;
}

/**
 * The turn that precedes the one being exported. Exactly one shape, so a
 * caller cannot emit an edge and a trunk pointer at the same time.
 */
export type PiGenerationParent =
  /** The parent generation exists under this conversation id. */
  | { kind: "own"; generationIds: string[] }
  /**
   * A fork copied the parent entry in, so its generation belongs to the
   * trunk conversation and no edge is emitted. `trunkGenerationId` names
   * that generation, and is absent when the trunk is unknown or cannot have
   * exported the entry.
   */
  | { kind: "trunk"; trunkGenerationId?: string }
  /**
   * A fork where either the header timestamp or the parent entry's
   * timestamp is missing or unparseable, so the parent cannot be placed on
   * either side of the fork. Nothing is emitted: an id built on a guess
   * would name a generation that may not exist.
   */
  | { kind: "unknown" };

/** What `resolvePiGenerationLineage` returns. Empty when no lineage. */
export interface PiGenerationLineage {
  generationId?: string;
  /** Absent for the first assistant turn on the branch. */
  parent?: PiGenerationParent;
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
 * Resolve `{ generationId, parent }` for the assistant turn currently being
 * exported.
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
 *
 * On a forked session the parent may be an entry copied from the trunk. See
 * `sessionOrigin.ts` for why such a parent gets no edge.
 */
export function resolvePiGenerationLineage(
  sessionManager: SessionManagerLike | undefined | null,
  assistantMessage: unknown,
  conversationId: string | undefined,
  options: ResolveLineageOptions = {},
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
  const parentEntry = findParentAssistantEntry(currentEntry, branch);
  const parentId = parentEntry?.id;
  if (!parentEntry || typeof parentId !== "string") return { generationId };

  return {
    generationId,
    parent: resolveParent(
      parentEntry,
      parentId,
      conversationId,
      options.origin,
    ),
  };
}

/**
 * Resolve `{ generationId, parent }` for a host summarization entry
 * (`compaction` or `branch_summary`) identified by its entry id.
 *
 * Unlike {@link resolvePiGenerationLineage}, the subject is not a message, so
 * there is nothing to match by object identity: pi hands us the entry itself.
 * The id is hashed straight from `entryId`, which means it stays deterministic
 * even on runtimes that do not expose `getBranch()`; only the parent lookup
 * needs the branch.
 *
 * The parent is the nearest ancestor assistant message entry. Pi appends the
 * summary entry as a child of the current leaf, which is frequently a
 * `toolResult` message or a `model_change` entry rather than an assistant
 * message, so the walk goes through the parentId chain instead of assuming the
 * immediate parent. On a fork that parent can be an entry copied from the
 * trunk, which gets no edge for the same reason a turn's does not.
 */
export function resolvePiSummaryLineage(
  sessionManager: SessionManagerLike | undefined | null,
  entryId: string,
  conversationId: string | undefined,
  options: ResolveLineageOptions = {},
): PiGenerationLineage {
  if (!conversationId || !entryId) return {};

  const generationId = stablePiGenerationId(conversationId, entryId);

  if (!sessionManager || typeof sessionManager.getBranch !== "function") {
    return { generationId };
  }

  let branch: SessionEntryLike[];
  try {
    branch = sessionManager.getBranch();
  } catch {
    return { generationId };
  }
  if (!Array.isArray(branch) || branch.length === 0) return { generationId };

  const entry = branch.find((e) => e.id === entryId);
  if (!entry) return { generationId };

  const parentEntry = findParentAssistantEntry(entry, branch);
  const parentId = parentEntry?.id;
  if (!parentEntry || typeof parentId !== "string") return { generationId };

  return {
    generationId,
    parent: resolveParent(
      parentEntry,
      parentId,
      conversationId,
      options.origin,
    ),
  };
}

/**
 * Build the pointer to the parent turn's generation. `entry` is the assistant
 * message entry that precedes the exported one, and `entryId` its id.
 */
function resolveParent(
  entry: SessionEntryLike,
  entryId: string,
  conversationId: string,
  origin: SessionOrigin | undefined,
): PiGenerationParent {
  switch (classifyParentEntry(entry, origin)) {
    case "own":
      return {
        kind: "own",
        generationIds: [stablePiGenerationId(conversationId, entryId)],
      };
    case "trunk":
      return {
        kind: "trunk",
        trunkGenerationId: trunkGenerationIdFor(entry, entryId, origin),
      };
    default:
      return { kind: "unknown" };
  }
}

/**
 * Decide which conversation the parent entry's generation belongs to.
 *
 * Outside a fork it is always this one: either this process exported the
 * parent turn, or an earlier process did under the same conversation id (a
 * `--session` continuation). The edge assumes that export succeeded; a turn
 * whose export threw leaves the next turn pointing at a generation that was
 * never ingested.
 *
 * Inside a fork the boundary is the header timestamp. A fork copies the
 * trunk's entries with their timestamps and stamps its header at fork time,
 * so every entry at or before that instant came from the trunk. Both
 * timestamps come from the fork's own session file, so the check works in
 * the process that forked and in any later one that resumes it.
 */
function classifyParentEntry(
  entry: SessionEntryLike,
  origin: SessionOrigin | undefined,
): "own" | "trunk" | "unknown" {
  if (!origin?.forked) return "own";
  const forkedAt = parseTimestamp(origin.forkedAt);
  const entryAt = parseTimestamp(entry.timestamp);
  if (forkedAt === undefined || entryAt === undefined) return "unknown";
  return entryAt <= forkedAt ? "trunk" : "own";
}

/**
 * Generation id of the copied parent turn in the trunk conversation.
 *
 * Absent when the trunk cannot have exported that turn. A fork of a fork
 * inherits entries the intermediate session copied in and never exported
 * itself; those are stamped before the trunk's own start, so the trunk holds
 * no generation for them and hashing one with the trunk's id would name
 * nothing.
 */
function trunkGenerationIdFor(
  entry: SessionEntryLike,
  entryId: string,
  origin: SessionOrigin | undefined,
): string | undefined {
  if (!origin?.trunkConversationId) return undefined;
  const trunkStartedAt = parseTimestamp(origin.trunkStartedAt);
  const entryAt = parseTimestamp(entry.timestamp);
  if (trunkStartedAt === undefined || entryAt === undefined) return undefined;
  if (entryAt <= trunkStartedAt) return undefined;
  return stablePiGenerationId(origin.trunkConversationId, entryId);
}

function parseTimestamp(value: string | undefined): number | undefined {
  if (typeof value !== "string") return undefined;
  const ms = Date.parse(value);
  return Number.isNaN(ms) ? undefined : ms;
}

/**
 * Walk the parentId chain from `entry` toward the root and return the
 * nearest ancestor that is an assistant message entry. Returns `undefined`
 * for the first assistant turn on the branch. `branch` is used to look up
 * entries by id.
 */
function findParentAssistantEntry(
  entry: SessionEntryLike,
  branch: SessionEntryLike[],
): SessionEntryLike | undefined {
  const byId = new Map<string, SessionEntryLike>();
  for (const e of branch) {
    if (typeof e.id === "string") byId.set(e.id, e);
  }

  let cursor = entry.parentId;
  const seen = new Set<string>();
  while (typeof cursor === "string" && !seen.has(cursor)) {
    seen.add(cursor);
    const parent = byId.get(cursor);
    if (!parent) return undefined;
    if (isAssistantMessageEntry(parent)) return parent;
    cursor = parent.parentId ?? null;
  }
  return undefined;
}

function isAssistantMessageEntry(entry: SessionEntryLike): boolean {
  if (!entry || entry.type !== "message") return false;
  const role = entry.message?.role;
  return role === "assistant";
}
