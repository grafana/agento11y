/**
 * Where the current pi session came from: a fresh start, a resume, or a
 * fork.
 *
 * `pi --fork`, the in-TUI `/fork` and `/clone` write a new session file
 * whose header carries `parentSession`, the path of the session the entries
 * came from, and a `timestamp` stamped at fork time
 * (`SessionManager.forkFrom`, `SessionManager.createBranchedSession`,
 * `SessionManager.newSession`). A session started fresh has no
 * `parentSession`. A resume keeps whatever the header already had, so a
 * resumed fork still reports one.
 *
 * Lineage needs both fields. A fork copies the trunk's entries keeping
 * their ids and timestamps. The trunk already exported those entries as
 * `stablePiGenerationId(trunkId, entryId)`, so hashing one with the fork's
 * conversation id points at a generation that exists nowhere. The header
 * timestamp separates the copied entries from the fork's own. Both values
 * live in the fork's own file, so they hold after the fork is resumed in
 * another process.
 *
 * An unpersisted fork writes no `parentSession` at all. `resolveSessionStart`
 * covers that case from what this process has already seen.
 */
import { closeSync, openSync, readSync } from "node:fs";
import { isAbsolute, resolve } from "node:path";
import { isMissingFileError } from "./fsErrors.js";
import { logger } from "./logger.js";

/** Origin of a pi session. */
export interface SessionOrigin {
  /** True for a fork or an unknown origin whose parent edges need suppression. */
  readonly forked: boolean;
  /**
   * ISO-8601 header timestamp of the fork. Entries stamped at or before it
   * were copied from the trunk. Absent when the session is not a fork, or
   * when its header carries no timestamp.
   */
  readonly forkedAt?: string;
  /**
   * Conversation id of the session this one was forked from. Absent when
   * the session is not a fork, or when the trunk file could not be read.
   */
  readonly trunkConversationId?: string;
  /**
   * ISO-8601 time the trunk conversation began: its own header timestamp.
   * A parent entry stamped at or before it was inherited by the trunk from
   * an older session rather than created in it, so the trunk exported no
   * generation for it. Absent when the trunk file could not be read.
   */
  readonly trunkStartedAt?: string;
}

/** A conversation this process has been attributing turns to. */
interface ConversationRecord {
  readonly id: string;
  readonly startedAt?: string;
  readonly origin: SessionOrigin;
}

const registryKey = Symbol.for("grafana.agento11y.pi.session-origin.v1");
const processRegistry = globalThis as typeof globalThis & {
  [registryKey]?: WeakMap<SessionHeaderSourceLike, ConversationRecord>;
};
// Pi replaces extension factories while retaining and mutating the public manager.
const origins: WeakMap<SessionHeaderSourceLike, ConversationRecord> =
  processRegistry[registryKey] ?? new WeakMap();
processRegistry[registryKey] = origins;
const UNKNOWN_ORIGIN: SessionOrigin = Object.freeze({ forked: true });

/**
 * Shared answer for a session that is not a fork. Frozen: callers cache it
 * and pass it around by reference.
 */
export const NOT_FORKED: SessionOrigin = Object.freeze({ forked: false });

/**
 * Subset of the pi session manager used to locate the session header. Both
 * methods are part of `ReadonlySessionManager` in pi-coding-agent, and both
 * follow a mid-process fork: the runtime reads `ctx.sessionManager` per
 * event, and the fork either replaces the manager or rewrites its header in
 * place.
 */
export interface SessionHeaderSourceLike {
  getHeader?: () => SessionHeaderLike | null | undefined;
  getSessionFile?: () => string | undefined;
  getSessionId?: () => string | undefined;
}

/** Minimal `SessionHeader` shape: just the fields we read. */
export interface SessionHeaderLike {
  type?: string;
  id?: string;
  timestamp?: string;
  parentSession?: string;
  /** Working directory pi recorded for the session. */
  cwd?: string;
}

/**
 * Only the first line of a session file is needed. Session files grow to
 * tens of megabytes on long conversations, so read a bounded prefix rather
 * than the whole file. 64 KiB is far more than a header needs and still one
 * cheap syscall.
 */
const HEADER_READ_BYTES = 64 * 1024;

/**
 * Resolve the origin of the session `sessionManager` currently points at.
 *
 * Without a usable header, ancestry is unknown and parent edges stay suppressed.
 */
export function resolveSessionOrigin(
  sessionManager: SessionHeaderSourceLike | undefined | null,
): SessionOrigin {
  return originFromHeader(resolveSessionHeader(sessionManager));
}

/** What the `session_start` handler learns from the new session's header. */
export interface SessionStartFacts {
  /**
   * ISO-8601 time this conversation began. Absent when neither the header nor a
   * cached snapshot for the same manager and session ID has a usable timestamp.
   */
  readonly startedAt?: string;
  /** Where the session came from. */
  readonly origin: SessionOrigin;
}

export function resolveSessionStart(
  sessionManager: SessionHeaderSourceLike | undefined | null,
  options: { forked: boolean },
): SessionStartFacts {
  const header = resolveSessionHeader(sessionManager);
  const startedAt = usableTimestamp(header?.timestamp);
  let id: string | undefined;
  try {
    id = nonEmptyString(sessionManager?.getSessionId?.());
  } catch {
    // Header identity remains usable when the public accessor fails.
  }
  id ??= nonEmptyString(header?.id);
  const previous = sessionManager ? origins.get(sessionManager) : undefined;
  let origin = originFromHeader(header);
  // A persisted parentSession takes precedence over process observations.
  if (!nonEmptyString(header?.parentSession)) {
    if (id && previous?.id === id) {
      origin = previous.origin;
    } else if (options.forked) {
      origin = {
        forked: true,
        forkedAt: startedAt,
        trunkConversationId: id ? previous?.id : undefined,
        trunkStartedAt: id ? previous?.startedAt : undefined,
      };
    }
  }
  const facts = {
    startedAt:
      startedAt ?? (previous?.id === id ? previous?.startedAt : undefined),
    origin: Object.freeze({ ...origin }),
  };
  if (sessionManager) {
    if (id) origins.set(sessionManager, Object.freeze({ id, ...facts }));
    else origins.delete(sessionManager);
  }
  return facts;
}

/**
 * Header of the session `sessionManager` currently points at.
 *
 * Prefers `getHeader()`, which returns the in-memory header and needs no
 * file access. Falls back to a bounded read of `getSessionFile()` for
 * runtimes (or test doubles) that do not expose `getHeader`.
 */
function resolveSessionHeader(
  sessionManager: SessionHeaderSourceLike | undefined | null,
): SessionHeaderLike | undefined {
  if (!sessionManager) return undefined;

  const header =
    callGetHeader(sessionManager) ??
    readSessionHeader(callGetSessionFile(sessionManager));
  if (!header) {
    logger.debug("no session header available");
  }
  return header;
}

function originFromHeader(
  header: SessionHeaderLike | undefined,
): SessionOrigin {
  const parentSession = nonEmptyString(header?.parentSession);
  if (!parentSession) {
    return nonEmptyString(header?.id) && usableTimestamp(header?.timestamp)
      ? NOT_FORKED
      : UNKNOWN_ORIGIN;
  }

  const forkedAt = nonEmptyString(header?.timestamp);
  if (!forkedAt) {
    logger.debug(
      `fork header has no timestamp, parent edges stay suppressed (parentSession=${parentSession})`,
    );
  }
  const trunkPath = trunkSessionPath(parentSession, header?.cwd);
  // The trunk file's own header holds its authoritative conversation id and
  // start time, so we never parse either out of the file name.
  const trunkHeader = readSessionHeader(trunkPath);
  const trunkConversationId = nonEmptyString(trunkHeader?.id);
  if (!trunkConversationId) {
    logger.debug(`no readable conversation id in trunk ${trunkPath}`);
  }
  return {
    forked: true,
    forkedAt,
    trunkConversationId,
    trunkStartedAt: nonEmptyString(trunkHeader?.timestamp),
  };
}

/**
 * Absolute path of the trunk session file.
 *
 * `pi --fork ../elsewhere/sess.jsonl` stores the path as typed
 * (`resolveSessionPath` in pi's `main.js` returns a path argument
 * unchanged), so a relative `parentSession` is relative to the cwd the
 * header records, not to the cwd of whichever process reads it later.
 */
function trunkSessionPath(
  parentSession: string,
  headerCwd: string | undefined,
): string {
  if (isAbsolute(parentSession)) return parentSession;
  const cwd = nonEmptyString(headerCwd);
  return cwd ? resolve(cwd, parentSession) : parentSession;
}

function callGetHeader(
  sessionManager: SessionHeaderSourceLike,
): SessionHeaderLike | undefined {
  if (typeof sessionManager.getHeader !== "function") return undefined;
  try {
    const header = sessionManager.getHeader();
    return isSessionHeader(header) ? header : undefined;
  } catch (err) {
    logger.debug("getHeader failed", err);
    return undefined;
  }
}

function callGetSessionFile(
  sessionManager: SessionHeaderSourceLike,
): string | undefined {
  if (typeof sessionManager.getSessionFile !== "function") return undefined;
  try {
    return sessionManager.getSessionFile();
  } catch (err) {
    logger.debug("getSessionFile failed", err);
    return undefined;
  }
}

/**
 * Parse the first line of `sessionFile` as a pi session header. Returns
 * `undefined` when the file is missing, the first line is not JSON, or the
 * parsed object is not a session header.
 */
function readSessionHeader(
  sessionFile: string | undefined,
): SessionHeaderLike | undefined {
  if (typeof sessionFile !== "string" || sessionFile.length === 0) {
    return undefined;
  }

  const line = readFirstLine(sessionFile);
  if (line === undefined) return undefined;

  try {
    const parsed: unknown = JSON.parse(line);
    return isSessionHeader(parsed) ? parsed : undefined;
  } catch (err) {
    logger.debug(`session header of ${sessionFile} is not JSON`, err);
    return undefined;
  }
}

function readFirstLine(path: string): string | undefined {
  let fd: number | undefined;
  try {
    fd = openSync(path, "r");
    const buffer = Buffer.allocUnsafe(HEADER_READ_BYTES);
    const bytesRead = readSync(fd, buffer, 0, HEADER_READ_BYTES, 0);
    if (bytesRead === 0) return undefined;

    const chunk = buffer.subarray(0, bytesRead);
    const newline = chunk.indexOf(0x0a);
    if (newline >= 0) return chunk.subarray(0, newline).toString("utf-8");
    // No newline in the prefix. A file shorter than the buffer is a
    // complete single line; a longer one has a header we refuse to read.
    if (bytesRead < HEADER_READ_BYTES) return chunk.toString("utf-8");
    logger.debug(
      `session header of ${path} exceeds ${HEADER_READ_BYTES} bytes`,
    );
    return undefined;
  } catch (err) {
    if (!isMissingFileError(err)) {
      logger.debug(`failed to read session header of ${path}`, err);
    }
    return undefined;
  } finally {
    if (fd !== undefined) {
      try {
        closeSync(fd);
      } catch {
        // Nothing useful to do if the descriptor is already gone.
      }
    }
  }
}

function isSessionHeader(value: unknown): value is SessionHeaderLike {
  return (
    typeof value === "object" &&
    value !== null &&
    (value as { type?: unknown }).type === "session"
  );
}

function usableTimestamp(value: unknown): string | undefined {
  const text = nonEmptyString(value);
  return text && Number.isFinite(Date.parse(text)) ? text : undefined;
}

function nonEmptyString(value: unknown): string | undefined {
  return typeof value === "string" && value.length > 0 ? value : undefined;
}
