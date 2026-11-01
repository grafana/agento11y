import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { basename, join } from "node:path";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const { loggerMock } = vi.hoisted(() => ({
  loggerMock: { debug: vi.fn(), warn: vi.fn(), error: vi.fn() },
}));

vi.mock("./logger.js", () => ({ logger: loggerMock }));

import {
  resolveSessionOrigin,
  resolveSessionStart,
  type SessionOrigin,
} from "./sessionOrigin.js";

const TRUNK_ID = "0199aaaa-1111-7000-8000-aaaaaaaaaaaa";
const FORK_ID = "0199bbbb-2222-7000-8000-bbbbbbbbbbbb";
const TRUNK_STARTED_AT = "2020-01-01T00:00:00.000Z";
const FORKED_AT = "2020-01-01T00:01:00.000Z";

let dir: string;

beforeEach(() => {
  dir = mkdtempSync(join(tmpdir(), "pi-session-origin-"));
  loggerMock.debug.mockClear();
});

afterEach(() => {
  rmSync(dir, { recursive: true, force: true });
});

/** Write a session file: header line followed by a couple of entries. */
function writeSession(
  name: string,
  header: Record<string, unknown>,
  entries: Record<string, unknown>[] = [],
): string {
  const path = join(dir, name);
  const lines = [header, ...entries].map((o) => JSON.stringify(o));
  writeFileSync(path, `${lines.join("\n")}\n`);
  return path;
}

function trunkHeader(): Record<string, unknown> {
  return {
    type: "session",
    version: 3,
    id: TRUNK_ID,
    timestamp: TRUNK_STARTED_AT,
    cwd: "/fixture/worktree",
  };
}

/** A fork header: the trunk's shape plus a new id, time and parentSession. */
function forkHeader(
  parentSession: string,
  overrides: Record<string, unknown> = {},
): Record<string, unknown> {
  return {
    ...trunkHeader(),
    id: FORK_ID,
    timestamp: FORKED_AT,
    parentSession,
    ...overrides,
  };
}

function messageEntry(id: string, parentId: string | null) {
  return {
    type: "message",
    id,
    parentId,
    timestamp: "2020-01-01T00:00:01.000Z",
    message: { role: "assistant" },
  };
}

/**
 * A session manager double that only knows its file, which is the fallback
 * path in `resolveSessionOrigin`: `getHeader` is absent, so the header comes
 * off disk.
 */
function fileOnlyManager(sessionFile: string | undefined) {
  return { getSessionFile: () => sessionFile };
}

describe("resolveSessionOrigin", () => {
  it("resolves the trunk conversation id and start time from a forked header", () => {
    const trunk = writeSession("trunk.jsonl", trunkHeader(), [
      messageEntry("e1", null),
    ]);
    const fork = writeSession(
      "fork.jsonl",
      forkHeader(trunk),
      // A fork copies the trunk's entries keeping their ids.
      [messageEntry("e1", null)],
    );

    expect(resolveSessionOrigin(fileOnlyManager(fork))).toEqual<SessionOrigin>({
      forked: true,
      forkedAt: FORKED_AT,
      trunkConversationId: TRUNK_ID,
      trunkStartedAt: TRUNK_STARTED_AT,
    });
  });

  it("resolves a relative parentSession against the cwd in the header", () => {
    // `pi --fork ../elsewhere/sess.jsonl` stores the path as typed, so a
    // process running elsewhere must not resolve it against its own cwd.
    const trunk = writeSession("trunk.jsonl", trunkHeader());
    const fork = writeSession(
      "fork.jsonl",
      forkHeader(basename(trunk), { cwd: dir }),
    );

    expect(resolveSessionOrigin(fileOnlyManager(fork))).toEqual<SessionOrigin>({
      forked: true,
      forkedAt: FORKED_AT,
      trunkConversationId: TRUNK_ID,
      trunkStartedAt: TRUNK_STARTED_AT,
    });
  });

  it("prefers getHeader over reading the session file", () => {
    const trunk = writeSession("trunk.jsonl", trunkHeader());
    const getSessionFile = vi.fn(() => join(dir, "never-read.jsonl"));

    const origin = resolveSessionOrigin({
      getHeader: () => ({
        type: "session",
        id: FORK_ID,
        timestamp: FORKED_AT,
        parentSession: trunk,
      }),
      getSessionFile,
    });

    expect(origin).toEqual<SessionOrigin>({
      forked: true,
      forkedAt: FORKED_AT,
      trunkConversationId: TRUNK_ID,
      trunkStartedAt: TRUNK_STARTED_AT,
    });
    expect(getSessionFile).not.toHaveBeenCalled();
  });

  it.each([
    { name: "getHeader is absent", getHeader: undefined },
    { name: "getHeader returns null", getHeader: () => null },
  ])("falls back to the session file when $name", ({ getHeader }) => {
    const trunk = writeSession("trunk.jsonl", trunkHeader());
    const fork = writeSession("fork.jsonl", forkHeader(trunk));

    expect(
      resolveSessionOrigin({ getHeader, getSessionFile: () => fork }),
    ).toEqual<SessionOrigin>({
      forked: true,
      forkedAt: FORKED_AT,
      trunkConversationId: TRUNK_ID,
      trunkStartedAt: TRUNK_STARTED_AT,
    });
  });

  it.each([
    {
      name: "a header without parentSession",
      sm: () => fileOnlyManager(writeSession("plain.jsonl", trunkHeader())),
    },
    {
      name: "a header with an empty parentSession",
      sm: () =>
        fileOnlyManager(
          writeSession("empty-parent.jsonl", {
            ...trunkHeader(),
            parentSession: "",
          }),
        ),
    },
    {
      name: "a missing file",
      sm: () => fileOnlyManager(join(dir, "does-not-exist.jsonl")),
    },
    {
      name: "an empty file",
      sm: () => {
        const path = join(dir, "empty.jsonl");
        writeFileSync(path, "");
        return fileOnlyManager(path);
      },
    },
    {
      name: "a first line that is not JSON",
      sm: () => {
        const path = join(dir, "garbage.jsonl");
        writeFileSync(path, "not json at all\n");
        return fileOnlyManager(path);
      },
    },
    {
      name: "a first line that is an entry, not a header",
      sm: () => {
        const path = join(dir, "headerless.jsonl");
        writeFileSync(path, `${JSON.stringify(messageEntry("e1", null))}\n`);
        return fileOnlyManager(path);
      },
    },
    {
      name: "a header line longer than the bounded read",
      sm: () =>
        fileOnlyManager(
          writeSession(
            "huge.jsonl",
            forkHeader(join(dir, "trunk.jsonl"), {
              padding: "x".repeat(70 * 1024),
            }),
          ),
        ),
    },
    { name: "an empty path", sm: () => fileOnlyManager("") },
    { name: "a directory, not a file", sm: () => fileOnlyManager(dir) },
    { name: "no session manager", sm: () => undefined },
    { name: "a null session manager", sm: () => null },
    { name: "a session manager with neither method", sm: () => ({}) },
    {
      name: "getHeader reporting a non-forked session",
      sm: () => ({ getHeader: () => trunkHeader() }),
    },
    {
      name: "a getSessionFile that points at a missing file",
      sm: () => fileOnlyManager("pi-session.jsonl"),
    },
    {
      name: "a getSessionFile returning undefined (in-memory session)",
      sm: () => fileOnlyManager(undefined),
    },
  ])("resolves safe ancestry for $name", ({ name, sm }) => {
    expect(resolveSessionOrigin(sm())).toEqual<SessionOrigin>({
      forked: ![
        "a header without parentSession",
        "a header with an empty parentSession",
        "getHeader reporting a non-forked session",
      ].includes(name),
    });
  });

  it("logs when no header can be read at all", () => {
    expect(
      resolveSessionOrigin(fileOnlyManager(join(dir, "gone.jsonl"))),
    ).toEqual<SessionOrigin>({ forked: true });
    expect(loggerMock.debug).toHaveBeenCalledWith(
      expect.stringContaining("no session header available"),
    );
  });

  it.each([
    {
      name: "the trunk file is gone",
      trunk: () => join(dir, "deleted-trunk.jsonl"),
    },
    {
      name: "the trunk file is empty",
      trunk: () => {
        const path = join(dir, "empty-trunk.jsonl");
        writeFileSync(path, "");
        return path;
      },
    },
    {
      name: "the trunk's first line is an entry, not a header",
      trunk: () => {
        const path = join(dir, "headerless-trunk.jsonl");
        writeFileSync(path, `${JSON.stringify(messageEntry("e1", null))}\n`);
        return path;
      },
    },
    {
      name: "the trunk header has no id",
      trunk: () =>
        writeSession("idless-trunk.jsonl", {
          type: "session",
          version: 3,
          cwd: "/fixture/worktree",
        }),
    },
  ])("reports forked without a trunk id, and logs, when $name", ({ trunk }) => {
    const fork = writeSession("fork.jsonl", forkHeader(trunk()));

    expect(resolveSessionOrigin(fileOnlyManager(fork))).toEqual<SessionOrigin>({
      forked: true,
      forkedAt: FORKED_AT,
    });
    expect(loggerMock.debug).toHaveBeenCalledWith(
      expect.stringContaining("no readable conversation id in trunk"),
    );
  });

  it("reports forked without a trunk start time when the trunk header has no timestamp", () => {
    const header = trunkHeader();
    delete header.timestamp;
    const trunk = writeSession("trunk.jsonl", header);
    const fork = writeSession("fork.jsonl", forkHeader(trunk));

    expect(resolveSessionOrigin(fileOnlyManager(fork))).toEqual<SessionOrigin>({
      forked: true,
      forkedAt: FORKED_AT,
      trunkConversationId: TRUNK_ID,
    });
  });

  it("reports forked without a fork time when the header has no timestamp", () => {
    const trunk = writeSession("trunk.jsonl", trunkHeader());
    const header = forkHeader(trunk);
    delete header.timestamp;
    const fork = writeSession("fork.jsonl", header);

    expect(resolveSessionOrigin(fileOnlyManager(fork))).toEqual<SessionOrigin>({
      forked: true,
      trunkConversationId: TRUNK_ID,
      trunkStartedAt: TRUNK_STARTED_AT,
    });
    expect(loggerMock.debug).toHaveBeenCalledWith(
      expect.stringContaining("fork header has no timestamp"),
    );
  });

  it("reads a header file that has no trailing newline", () => {
    const trunk = join(dir, "no-newline-trunk.jsonl");
    writeFileSync(trunk, JSON.stringify(trunkHeader()));
    const fork = writeSession("fork.jsonl", forkHeader(trunk));

    expect(resolveSessionOrigin(fileOnlyManager(fork))).toEqual<SessionOrigin>({
      forked: true,
      forkedAt: FORKED_AT,
      trunkConversationId: TRUNK_ID,
      trunkStartedAt: TRUNK_STARTED_AT,
    });
  });

  it.each([
    {
      name: "getHeader",
      sm: {
        getHeader: () => {
          throw new Error("session gone");
        },
      },
    },
    {
      name: "getSessionFile",
      sm: {
        getSessionFile: () => {
          throw new Error("session gone");
        },
      },
    },
  ])("suppresses ancestry when $name throws", ({ sm }) => {
    expect(resolveSessionOrigin(sm)).toEqual<SessionOrigin>({ forked: true });
    expect(loggerMock.debug).toHaveBeenCalled();
  });
});

describe("resolveSessionStart", () => {
  it("reports when a fresh session began, and that it is not a fork", () => {
    const session = writeSession("plain.jsonl", trunkHeader());

    expect(
      resolveSessionStart(fileOnlyManager(session), { forked: false }),
    ).toEqual({
      startedAt: TRUNK_STARTED_AT,
      origin: { forked: false },
    });
  });

  it("takes the header's answer for a persisted fork", () => {
    const trunk = writeSession("trunk.jsonl", trunkHeader());
    const fork = writeSession("fork.jsonl", forkHeader(trunk));

    expect(
      resolveSessionStart(fileOnlyManager(fork), {
        forked: true,
      }),
    ).toEqual({
      startedAt: FORKED_AT,
      origin: {
        forked: true,
        forkedAt: FORKED_AT,
        trunkConversationId: TRUNK_ID,
        trunkStartedAt: TRUNK_STARTED_AT,
      },
    });
  });

  it("reports a fork from the header even when pi calls the start something else", () => {
    // `--session <fork file>` resumes a fork: the header still names the
    // trunk, and the copied entries still belong to it.
    const trunk = writeSession("trunk.jsonl", trunkHeader());
    const fork = writeSession("fork.jsonl", forkHeader(trunk));

    expect(
      resolveSessionStart(fileOnlyManager(fork), { forked: false }).origin,
    ).toEqual<SessionOrigin>({
      forked: true,
      forkedAt: FORKED_AT,
      trunkConversationId: TRUNK_ID,
      trunkStartedAt: TRUNK_STARTED_AT,
    });
  });

  it.each([
    { name: "observed trunk", timestamp: TRUNK_STARTED_AT },
    { name: "unknown trunk start", timestamp: undefined },
    { name: "invalid trunk start", timestamp: "invalid" },
  ])("restores an in-memory fork of $name after module re-evaluation", async ({
    timestamp,
  }) => {
    let header = { type: "session", id: TRUNK_ID, timestamp };
    const manager = { getHeader: () => header };
    resolveSessionStart(manager, { forked: false });
    header = { type: "session", id: FORK_ID, timestamp: FORKED_AT };
    const facts = resolveSessionStart(manager, { forked: true });
    expect(facts).toEqual({
      startedAt: FORKED_AT,
      origin: {
        forked: true,
        forkedAt: FORKED_AT,
        trunkConversationId: TRUNK_ID,
        trunkStartedAt: timestamp === TRUNK_STARTED_AT ? timestamp : undefined,
      },
    });
    expect(Object.isFrozen(facts.origin)).toBe(true);
    const registry = (globalThis as Record<symbol, WeakMap<object, object>>)[
      Symbol.for("grafana.agento11y.pi.session-origin.v1")
    ]!;
    const stored = registry.get(manager)!;
    expect(Object.keys(stored).sort()).toEqual(["id", "origin", "startedAt"]);
    expect(Object.isFrozen(stored)).toBe(true);
    vi.resetModules();
    const reloaded = await import("./sessionOrigin.js");
    expect(reloaded.resolveSessionStart).not.toBe(resolveSessionStart);
    expect(reloaded.resolveSessionStart(manager, { forked: false })).toEqual(
      facts,
    );
  });

  it.each([
    undefined,
    "invalid",
  ])("does not invent a missing fork timestamp (%s)", (timestamp) => {
    let header = {
      type: "session",
      id: TRUNK_ID,
      timestamp: TRUNK_STARTED_AT as string | undefined,
    };
    const manager = { getHeader: () => header };
    resolveSessionStart(manager, { forked: false });
    header = { type: "session", id: FORK_ID, timestamp };
    expect(resolveSessionStart(manager, { forked: true })).toEqual({
      startedAt: undefined,
      origin: {
        forked: true,
        forkedAt: undefined,
        trunkConversationId: TRUNK_ID,
        trunkStartedAt: TRUNK_STARTED_AT,
      },
    });
  });

  it.each([
    false,
    true,
  ])("does not correlate independent managers (fork=%s)", (forked) => {
    resolveSessionStart({ getHeader: () => trunkHeader() }, { forked: false });
    const manager = {
      getHeader: () => ({ type: "session", id: FORK_ID, timestamp: FORKED_AT }),
    };
    const facts = resolveSessionStart(manager, { forked });
    expect(facts.origin.trunkConversationId).toBeUndefined();
    expect(facts.origin.forked).toBe(forked);
  });

  it("does not restore another manager's origin for the same session id", () => {
    const header = { type: "session", id: FORK_ID, timestamp: FORKED_AT };
    const first = { getHeader: () => header };
    const second = { getHeader: () => header };
    resolveSessionStart(first, { forked: true });
    expect(resolveSessionStart(second, { forked: false }).origin).toEqual({
      forked: false,
    });
    expect(resolveSessionStart(first, { forked: false }).origin.forked).toBe(
      true,
    );
  });

  it("does not reuse a snapshot after an unidentified session", () => {
    let header: ReturnType<typeof trunkHeader> | undefined = trunkHeader();
    const manager = { getHeader: () => header };
    resolveSessionStart(manager, { forked: false });
    header = undefined;
    resolveSessionStart(manager, { forked: false });
    header = { type: "session", id: FORK_ID, timestamp: FORKED_AT };
    expect(
      resolveSessionStart(manager, { forked: true }).origin.trunkConversationId,
    ).toBeUndefined();
  });

  it("does not make the same session its own trunk on a repeated fork start", () => {
    const manager = {
      getHeader: () => ({ type: "session", id: FORK_ID, timestamp: FORKED_AT }),
    };
    const first = resolveSessionStart(manager, { forked: true });
    expect(resolveSessionStart(manager, { forked: true })).toEqual(first);
    expect(first.origin.trunkConversationId).toBeUndefined();
  });

  it("uses only the immediate prior snapshot for another fork", () => {
    let header = { type: "session", id: TRUNK_ID, timestamp: TRUNK_STARTED_AT };
    const manager = { getHeader: () => header };
    resolveSessionStart(manager, { forked: false });
    header = { type: "session", id: FORK_ID, timestamp: FORKED_AT };
    resolveSessionStart(manager, { forked: true });
    header = {
      type: "session",
      id: "third",
      timestamp: "2020-01-01T00:02:00.000Z",
    };
    expect(resolveSessionStart(manager, { forked: true }).origin).toMatchObject(
      {
        trunkConversationId: FORK_ID,
        trunkStartedAt: FORKED_AT,
      },
    );
  });

  it("prefers persisted parentSession over an observed in-memory fork", () => {
    const trunk = writeSession("trunk.jsonl", trunkHeader());
    let header = {
      type: "session",
      id: "unrelated",
      timestamp: TRUNK_STARTED_AT,
    };
    const manager = { getHeader: () => header };
    resolveSessionStart(manager, { forked: false });
    header = forkHeader(trunk) as typeof header;
    expect(resolveSessionStart(manager, { forked: true }).origin).toMatchObject(
      {
        trunkConversationId: TRUNK_ID,
        trunkStartedAt: TRUNK_STARTED_AT,
      },
    );
  });

  it.each([
    undefined,
    null,
    {},
    { getHeader: () => ({ type: "session" }) },
  ])("suppresses ancestry without usable origin: %s", (manager) => {
    expect(resolveSessionStart(manager, { forked: false }).origin).toEqual({
      forked: true,
    });
  });
});
