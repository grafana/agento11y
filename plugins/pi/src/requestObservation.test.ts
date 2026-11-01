import type { ContentCaptureMode, Message } from "@grafana/agento11y";
import type { ExtensionContext } from "@mariozechner/pi-coding-agent";
import { describe, expect, it, vi } from "vitest";
import {
  RequestObservation,
  type RequestObservationManager,
} from "./requestObservation.js";

function user(content: unknown, timestamp = 1) {
  return { role: "user", content, timestamp };
}
function entry(id: string, message: ReturnType<typeof user>) {
  return { type: "message", id, message };
}
function projected(
  sourceEntry: ReturnType<typeof entry>,
  messages = [sourceEntry.message],
) {
  return { sourceEntry, messages };
}
function fixture(initial: ReturnType<typeof projected>[] = []) {
  let entries = initial;
  let sessionId = "session-a";
  const manager = {
    getSessionId: () => sessionId,
    getEntries: () => entries.map((item) => item.sourceEntry),
    buildSessionProjection: () => ({ entries }),
  };
  const observation = new RequestObservation();
  observation.reset(manager);
  return {
    manager,
    observation,
    set: (value: typeof entries) => {
      entries = value;
    },
    session: (value: string) => {
      sessionId = value;
    },
    consume: (mode: ContentCaptureMode = "full") =>
      observation.consume(manager, mode),
    observe: (messages: readonly unknown[]) =>
      observation.observe(manager, messages),
  };
}
function texts(messages: Message[]) {
  return messages.map((message) => message.parts);
}
function body(text: string) {
  return [{ type: "text", text }];
}

describe("RequestObservation", () => {
  it("accepts pi's structural readonly manager without a runtime pi dependency", () => {
    const compatible = (
      manager: ExtensionContext["sessionManager"],
    ): RequestObservationManager => manager;
    expect(compatible).toBeTypeOf("function");
  });

  it("baselines restored entry IDs, not their text or timestamp", () => {
    const old = entry("old", user("identical", 1));
    const f = fixture([projected(old)]);
    f.set([projected(old), projected(entry("new", user("identical", 2)))]);
    f.observe([user("restored transformed", 1), user("new transformed", 2)]);
    expect(texts(f.consume())).toEqual([body("new transformed")]);
    expect(f.observation.firstUserText).toBe("new transformed");
  });

  it("exports duplicate text as distinct occurrences when metadata is unique", () => {
    const f = fixture();
    f.set([
      projected(entry("a", user("same", 1))),
      projected(entry("b", user("same", 2))),
    ]);
    f.observe([user("same", 1), user("same", 2)]);
    expect(texts(f.consume())).toEqual([body("same"), body("same")]);
    expect(f.consume()).toEqual([]);
  });

  it("omits duplicate user timestamps even when bodies or order appear distinguishable", () => {
    const f = fixture();
    f.set([
      projected(entry("a", user("raw one"))),
      projected(entry("b", user("raw two"))),
    ]);
    f.observe([user("safe one"), user("safe two")]);
    expect(f.consume()).toEqual([]);
    expect(f.observation.firstUserText).toBeUndefined();
  });

  it("counts restored history when detecting timestamp ambiguity", () => {
    const old = projected(entry("old", user("old")));
    const f = fixture([old]);
    f.set([old, projected(entry("new", user("new")))]);
    f.observe([user("new")]);
    expect(f.consume()).toEqual([]);
    expect(f.observation.firstUserText).toBeUndefined();
  });

  it("omits duplicated observed metadata, rather than choosing a matching body", () => {
    const f = fixture();
    f.set([projected(entry("a", user("raw")))]);
    f.observe([user("raw"), user("safe")]);
    expect(f.consume()).toEqual([]);
  });

  it("captures an earlier handler's unique prefix from the observed request", () => {
    const f = fixture();
    f.set([projected(entry("a", user("persisted secret", 3)))]);
    f.observe([user("  earlier prefix: [REDACTED]  ", 3)]);
    expect(f.observation.firstUserText).toBe("earlier prefix: [REDACTED]");
    expect(texts(f.consume())).toEqual([
      body("  earlier prefix: [REDACTED]  "),
    ]);
  });

  it("uses the later persisted replacement occurrence, not an early message_end body", () => {
    const f = fixture();
    const earlyMessageEnd = user("early secret", 1);
    const persistedReplacement = entry(
      "finalized-entry",
      user("replacement secret", 50),
    );
    f.set([projected(persistedReplacement)]);
    f.observe([user("replacement safe", 50)]);
    expect(texts(f.consume())).toEqual([body("replacement safe")]);
    expect(f.observation.firstUserText).not.toBe(earlyMessageEnd.content);
    // Same occurrence with yet another body cannot become a second input.
    f.set([
      projected(entry("finalized-entry", user("another replacement", 51))),
    ]);
    f.observe([user("safe again", 51)]);
    expect(f.consume()).toEqual([]);
  });

  it("never uses persisted raw text when observation is missing", () => {
    const f = fixture();
    f.set([projected(entry("a", user("secret")))]);
    expect(f.consume()).toEqual([]);
    expect(f.observation.firstUserText).toBeUndefined();
    f.observe([user("secret")]);
    expect(f.consume()).toEqual([]);
  });

  it("does not correlate using matching text when timestamp or role differs", () => {
    const f = fixture();
    f.set([projected(entry("a", user("same", 10)))]);
    f.observe([
      user("same", 11),
      { role: "assistant", content: "same", timestamp: 10 },
    ]);
    expect(f.consume()).toEqual([]);
    expect(f.observation.firstUserText).toBeUndefined();
  });

  it("permits equal timestamps on different roles", () => {
    const f = fixture();
    f.set([projected(entry("a", user("raw")))]);
    f.observe([
      { role: "assistant", content: "not user", timestamp: 1 },
      user("safe"),
    ]);
    expect(texts(f.consume())).toEqual([body("safe")]);
  });

  it("omits out-of-order correspondence", () => {
    const f = fixture();
    f.set([
      projected(entry("a", user("one", 1))),
      projected(entry("b", user("two", 2))),
    ]);
    f.observe([user("safe two", 2), user("safe one", 1)]);
    expect(f.consume()).toEqual([]);
    expect(f.observation.firstUserText).toBeUndefined();
  });

  it("omits missing entries without recovering raw text or deriving a later title", () => {
    const f = fixture();
    f.set([
      projected(entry("a", user("secret", 1))),
      projected(entry("b", user("raw", 2))),
    ]);
    f.observe([user("safe second", 2)]);
    expect(texts(f.consume())).toEqual([body("safe second")]);
    expect(f.observation.firstUserText).toBeUndefined();
  });

  it("uses projected metadata rather than sourceEntry.message metadata", () => {
    const f = fixture();
    f.set([projected(entry("a", user("raw", 1)), [user("projected", 2)])]);
    f.observe([user("safe", 2)]);
    expect(texts(f.consume())).toEqual([body("safe")]);
  });

  it("ignores source entries whose projected contribution was omitted", () => {
    const f = fixture();
    f.set([projected(entry("a", user("secret")), [])]);
    f.observe([user("secret")]);
    expect(f.consume()).toEqual([]);
    expect(f.observation.firstUserText).toBeUndefined();
  });

  it("does not treat an expanded contribution as a unique occurrence", () => {
    const f = fixture();
    f.set([
      projected(entry("a", user("raw")), [user("one", 1), user("two", 2)]),
    ]);
    f.observe([user("one", 1), user("two", 2)]);
    expect(f.consume()).toEqual([]);
  });

  it("snapshots transformed text and reuses mapUserMessage multipart handling", () => {
    const f = fixture();
    f.set([projected(entry("a", user("raw")))]);
    const observed = user([
      { type: "text", text: "safe one" },
      { type: "image", data: "image-secret", mimeType: "image/png" },
      { type: "text", text: "safe two" },
    ]);
    f.observe([observed]);
    observed.content = "mutated secret";
    expect(texts(f.consume())).toEqual([body("safe one\nsafe two")]);
    expect(f.observation.firstUserText).toBe("safe one\nsafe two");
  });

  it.each([
    undefined,
    null,
    [],
    [{ type: "text", text: 123 }],
    [null],
  ])("omits empty or malformed observed content: %j", (content) => {
    const f = fixture();
    f.set([projected(entry("a", user("raw")))]);
    f.observe([user(content)]);
    expect(f.consume()).toEqual([]);
    expect(f.observation.firstUserText).toBeUndefined();
  });

  it.each([
    NaN,
    Infinity,
    undefined,
    "1",
  ])("requires finite native timestamp metadata: %s", (timestamp) => {
    const f = fixture();
    f.set([projected(entry("a", user("raw")))]);
    f.observe([{ role: "user", content: "raw", timestamp }]);
    expect(f.consume()).toEqual([]);
  });

  it.each([
    "full",
    "full_with_metadata_spans",
    "no_tool_content",
    "metadata_only",
  ] as const)("honors %s capture and still consumes the occurrence once", (mode) => {
    const f = fixture();
    f.set([projected(entry("a", user("raw")))]);
    f.observe([user("safe")]);
    expect(texts(f.consume(mode))).toEqual(
      mode === "metadata_only" ? [] : [body("safe")],
    );
    expect(f.consume()).toEqual([]);
  });

  it("keeps incremental input across repeated requests and tool continuation", () => {
    const f = fixture();
    const a = projected(entry("a", user("raw a", 1)));
    f.set([a]);
    f.observe([user("safe a", 1)]);
    f.observe([user("latest safe a", 1)]);
    expect(texts(f.consume())).toEqual([body("latest safe a")]);
    f.observe([
      user("safe a", 1),
      { role: "toolResult", timestamp: 2, content: [] },
    ]);
    expect(f.consume()).toEqual([]);
    f.set([a, projected(entry("b", user("raw b", 3)))]);
    f.observe([user("safe a", 1), user("safe b", 3)]);
    expect(texts(f.consume())).toEqual([body("safe b")]);
    expect(f.observation.firstUserText).toBe("latest safe a");
  });

  it.each([
    { name: "omitted user slot", messages: [] },
    { name: "duplicate user metadata", messages: [user("raw"), user("safe")] },
    { name: "changed timestamp", messages: [user("raw", 2)] },
    {
      name: "changed role",
      messages: [{ role: "assistant", content: "raw", timestamp: 1 }],
    },
  ])("drops a previous body after $name without raw fallback or replay", ({
    messages,
  }) => {
    const f = fixture();
    const first = projected(entry("a", user("raw")));
    f.set([first]);
    f.observe([user("previous safe observation")]);
    f.observe(messages);
    expect(f.consume()).toEqual([]);
    expect(f.observation.firstUserText).toBeUndefined();

    f.observe([user("later safe observation")]);
    expect(f.consume()).toEqual([]);
    f.set([first, projected(entry("b", user("raw next", 3)))]);
    f.observe([user("raw"), user("safe next", 3)]);
    expect(texts(f.consume())).toEqual([body("safe next")]);
    expect(f.observation.firstUserText).toBeUndefined();
  });

  it.each([
    {
      name: "later host text projection",
      later: [projected(entry("a", user("later projected raw")))],
      expected: [body("intermediate observed text")],
    },
    {
      name: "later metadata replacement",
      later: [projected(entry("a", user("later raw", 2)))],
      expected: [],
    },
    {
      name: "hidden projected contribution",
      later: [projected(entry("a", user("raw")), [])],
      expected: [],
    },
    {
      name: "expanded projected contribution",
      later: [
        projected(entry("a", user("raw")), [user("one"), user("two", 2)]),
      ],
      expected: [],
    },
    {
      name: "duplicate projected metadata",
      later: [
        projected(entry("a", user("raw"))),
        projected(entry("b", user("raw"))),
      ],
      expected: [],
    },
    {
      name: "duplicate source occurrence",
      later: [
        projected(entry("a", user("raw"))),
        projected(entry("a", user("raw", 2))),
      ],
      expected: [],
    },
  ])("keeps context observations intermediate, not final: $name", ({
    later,
    expected,
  }) => {
    const f = fixture();
    f.set([projected(entry("a", user("persisted raw")))]);
    const context = [user("intermediate observed text")];
    f.observe(context);
    // Later host mutations do not turn an observed context into final provider input.
    context[0]!.content = "later provider text";
    f.set(later);
    expect(texts(f.consume())).toEqual(expected);
    expect(f.observation.firstUserText).toBe(
      expected.length > 0 ? "intermediate observed text" : undefined,
    );
    expect(f.consume()).toEqual([]);
  });

  it("revalidates persisted ownership at consume, without looking up bodies", () => {
    const f = fixture();
    f.set([projected(entry("a", user("raw")))]);
    f.observe([user("safe")]);
    f.set([projected(entry("different-id", user("raw")))]);
    expect(f.consume()).toEqual([]);
    expect(f.observation.firstUserText).toBeUndefined();
  });

  it("uses getBranch only when projection is unsupported", () => {
    let branch: ReturnType<typeof entry>[] = [entry("old", user("history", 1))];
    const manager = { getBranch: () => branch };
    const observation = new RequestObservation();
    observation.reset(manager);
    branch = [...branch, entry("new", user("raw", 2))];
    observation.observe(manager, [user("history", 1), user("safe", 2)]);
    expect(texts(observation.consume(manager, "full"))).toEqual([body("safe")]);
    expect(observation.firstUserText).toBe("safe");

    const getBranch = vi.fn(() => {
      throw new Error("must not be called");
    });
    const supported = {
      buildSessionProjection: () => ({ entries: [] }),
      getBranch,
    };
    observation.reset(supported);
    observation.observe(supported, []);
    expect(observation.consume(supported, "full")).toEqual([]);
    expect(getBranch).not.toHaveBeenCalled();
  });

  it.each([
    {},
    { getEntries: () => [] },
    { buildSessionProjection: () => undefined },
    {
      buildSessionProjection: () => {
        throw new Error("unavailable");
      },
    },
    {
      getBranch: () => {
        throw new Error("unavailable");
      },
    },
  ])("fails closed when APIs are absent or unavailable: %j", (manager) => {
    const observation = new RequestObservation();
    observation.reset(manager);
    observation.observe(manager, [user("secret")]);
    expect(observation.consume(manager, "full")).toEqual([]);
    expect(observation.firstUserText).toBeUndefined();
  });

  it("does not downgrade a failed projection to raw branch text", () => {
    const getBranch = vi.fn(() => [entry("a", user("secret"))]);
    const manager = { buildSessionProjection: () => null, getBranch };
    const observation = new RequestObservation();
    observation.reset(manager);
    observation.observe(manager, [user("secret")]);
    expect(observation.consume(manager, "full")).toEqual([]);
    expect(getBranch).not.toHaveBeenCalled();
  });

  it("baselines persisted history outside the current compacted projection", () => {
    const f = fixture();
    const hidden = entry("hidden", user("old secret"));
    f.manager.getEntries = () => [hidden];
    f.observation.reset(f.manager);
    f.set([projected(hidden)]);
    f.observe([user("old observed")]);
    expect(f.consume()).toEqual([]);
    expect(f.observation.firstUserText).toBeUndefined();
  });

  it.each([
    "fork",
    "new session",
    "reload",
  ])("rebaselines %s and clears the previous title", (kind) => {
    const f = fixture();
    const inherited = projected(entry("same-id", user("old raw", 1)));
    f.set([inherited]);
    f.observe([user("old safe", 1)]);
    expect(texts(f.consume())).toEqual([body("old safe")]);
    if (kind !== "reload") f.session(kind);
    f.observation.reset(f.manager);
    f.observe([user("inherited", 1)]);
    expect(f.consume()).toEqual([]);
    expect(f.observation.firstUserText).toBeUndefined();
    f.set([inherited, projected(entry("new-id", user("new raw", 2)))]);
    f.observe([user("inherited", 1), user("new safe", 2)]);
    expect(texts(f.consume())).toEqual([body("new safe")]);
    expect(f.observation.firstUserText).toBe("new safe");
  });

  it("can export a reused entry ID in a genuinely new empty session", () => {
    const f = fixture();
    f.set([projected(entry("same-id", user("old raw")))]);
    f.observe([user("old safe")]);
    expect(texts(f.consume())).toEqual([body("old safe")]);
    f.session("new-session");
    f.set([]);
    f.observation.reset(f.manager);
    f.set([projected(entry("same-id", user("new raw")))]);
    f.observe([user("new safe")]);
    expect(texts(f.consume())).toEqual([body("new safe")]);
    expect(f.observation.firstUserText).toBe("new safe");
  });

  it("does not recover from a failed baseline by exporting restored entries", () => {
    const f = fixture();
    f.manager.getEntries = () => {
      throw new Error("baseline unavailable");
    };
    f.observation.reset(f.manager);
    f.set([projected(entry("restored", user("raw")))]);
    f.manager.getEntries = () => [entry("restored", user("raw"))];
    f.observe([user("restored observed")]);
    expect(f.consume()).toEqual([]);
    expect(f.observation.firstUserText).toBeUndefined();
  });

  it("omits a body if persisted metadata changes between observe and consume", () => {
    const f = fixture();
    f.set([projected(entry("a", user("raw", 1)))]);
    f.observe([user("safe", 1)]);
    f.set([projected(entry("a", user("replacement raw", 2)))]);
    expect(f.consume()).toEqual([]);
    expect(f.observation.firstUserText).toBeUndefined();
  });

  it("automatically baselines a changed session identity before observing", () => {
    const f = fixture();
    f.set([projected(entry("a", user("raw")))]);
    f.observe([user("safe")]);
    f.session("other-session");
    f.observe([user("inherited")]);
    expect(f.consume()).toEqual([]);
    expect(f.observation.firstUserText).toBeUndefined();
  });

  it("automatically baselines a replacement manager even without session ID APIs", () => {
    const observation = new RequestObservation();
    const first = { getBranch: () => [] };
    observation.reset(first);
    const second = { getBranch: () => [entry("a", user("inherited"))] };
    observation.observe(second, [user("inherited")]);
    expect(observation.consume(second, "full")).toEqual([]);
    expect(observation.firstUserText).toBeUndefined();
  });
});
