import { createServer } from "node:http";
import {
  Agento11yClient,
  type HookEvaluateRequest,
  type Message,
} from "@grafana/agento11y";
import type {
  SessionBeforeCompactEvent,
  SessionBeforeTreeEvent,
  SessionEntry,
} from "@mariozechner/pi-coding-agent";
import { trace } from "@opentelemetry/api";
import { describe, expect, it, vi } from "vitest";
import {
  guardCompaction,
  guardTreeSummary,
  type SummaryGuardArgs,
} from "./summaryGuards.js";

function guards(response?: (request: HookEvaluateRequest) => unknown) {
  const evaluateHook = vi.fn(
    async (request: HookEvaluateRequest, _overrides?: unknown) =>
      response?.(request) ?? { action: "allow" },
  );
  const warn = vi.fn();
  const args: SummaryGuardArgs = {
    client: { evaluateHook } as unknown as SummaryGuardArgs["client"],
    agentName: "pi",
    model: { provider: "test", name: "physical" },
    failOpen: false,
    logger: { warn },
  };
  return { args, evaluateHook, warn };
}
function redact(request: HookEvaluateRequest) {
  return {
    action: "allow",
    transformedInput: {
      messages: JSON.parse(
        JSON.stringify(request.input?.messages).replaceAll("Falcon", "[R]"),
      ),
    },
  };
}
function compaction(): SessionBeforeCompactEvent {
  return {
    type: "session_before_compact",
    reason: "manual",
    willRetry: false,
    signal: new AbortController().signal,
    branchEntries: [],
    customInstructions: "Keep the decisions",
    preparation: {
      firstKeptEntryId: "kept",
      tokensBefore: 400,
      isSplitTurn: true,
      settings: { enabled: true, reserveTokens: 100, keepRecentTokens: 50 },
      messagesToSummarize: [
        { role: "user", content: "Falcon user", timestamp: 1 },
      ],
      turnPrefixMessages: [
        { role: "user", content: "Falcon prefix", timestamp: 2 },
      ],
      previousSummary: "Falcon previous",
      fileOps: {
        read: new Set(["Falcon/read"]),
        written: new Set(["Falcon/written"]),
        edited: new Set(["Falcon/edited"]),
      },
    },
  };
}
function tree(): SessionBeforeTreeEvent {
  const base = {
    id: "entry",
    parentId: null,
    timestamp: "2026-01-01T00:00:00Z",
  };
  return {
    type: "session_before_tree",
    signal: new AbortController().signal,
    preparation: {
      targetId: "target",
      oldLeafId: "leaf",
      commonAncestorId: "common",
      userWantsSummary: true,
      customInstructions: "Falcon focus",
      replaceInstructions: true,
      label: "label",
      entriesToSummarize: [
        {
          ...base,
          id: "message",
          type: "message",
          message: { role: "user", content: "Falcon user", timestamp: 1 },
        },
        {
          ...base,
          id: "custom",
          type: "custom_message",
          customType: "custom",
          content: "Falcon custom",
          display: false,
          details: { opaque: "unchanged" },
        },
        {
          ...base,
          id: "branch",
          type: "branch_summary",
          fromId: "old",
          summary: "Falcon branch",
          details: {
            readFiles: ["Falcon/read"],
            modifiedFiles: ["Falcon/modified"],
            opaque: "unchanged",
          },
        },
        {
          ...base,
          id: "compaction",
          type: "compaction",
          firstKeptEntryId: "kept",
          summary: "Falcon compact",
          tokensBefore: 10,
          details: { opaque: "unchanged" },
        },
        {
          ...base,
          id: "ignored",
          type: "custom",
          customType: "opaque",
          data: { opaque: "Falcon ignored" },
        },
      ],
    },
  };
}

function runSummary(
  event: SessionBeforeCompactEvent | SessionBeforeTreeEvent,
  args: SummaryGuardArgs,
) {
  return event.type === "session_before_compact"
    ? guardCompaction(event, args)
    : guardTreeSummary(event, args);
}

async function within<T>(pending: Promise<T>): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      pending,
      new Promise<never>((_, reject) => {
        timer = setTimeout(
          () => reject(new Error("cancellation did not settle within 1s")),
          1000,
        );
      }),
    ]);
  } finally {
    clearTimeout(timer);
  }
}

describe("summary preparation guards", () => {
  for (const makeEvent of [compaction, tree]) {
    for (const failOpen of [true, false]) {
      it(`cancels stale ${makeEvent.name}, failOpen=${failOpen}, without evaluating`, async () => {
        const event = makeEvent();
        const controller = new AbortController();
        event.signal = controller.signal;
        controller.abort(new Error("private host reason"));
        const original = structuredClone(event.preparation);
        const { args, evaluateHook, warn } = guards();
        args.failOpen = failOpen;
        expect(await runSummary(event, args)).toEqual({ cancel: true });
        expect(evaluateHook).not.toHaveBeenCalled();
        expect(event.preparation).toEqual(original);
        expect(warn).not.toHaveBeenCalled();
      });

      it.each([
        "allow",
        "transform",
        "failure",
        "throw",
      ])(`cancels ${makeEvent.name} after abort racing with %s, failOpen=${failOpen}`, async (outcome) => {
        const event = makeEvent();
        const original = structuredClone(event.preparation);
        const controller = new AbortController();
        event.signal = controller.signal;
        const { args, warn } = guards((request) => {
          controller.abort(new Error("private host reason"));
          if (outcome === "throw") throw new Error("private transport error");
          if (outcome === "transform") return redact(request);
          return outcome === "failure"
            ? { action: "deny", ruleId: "__agento11y_guard_evaluation_failure" }
            : { action: "allow" };
        });
        args.failOpen = failOpen;
        expect(await runSummary(event, args)).toEqual({ cancel: true });
        expect(event.preparation).toEqual(original);
        expect(warn).not.toHaveBeenCalled();
      });

      it.each([
        "stale",
        "ongoing",
      ])(`cancels ${makeEvent.name} with real HTTP for %s abort, failOpen=${failOpen}`, async (stage) => {
        let hits = 0;
        let received!: () => void;
        let disconnected!: () => void;
        const arrival = new Promise<void>((resolve) => {
          received = resolve;
        });
        const closed = new Promise<void>((resolve) => {
          disconnected = resolve;
        });
        const server = createServer(async (req, res) => {
          hits++;
          for await (const _ of req) {
          }
          res.on("close", disconnected);
          received();
        });
        await new Promise<void>((resolve) =>
          server.listen(0, "127.0.0.1", resolve),
        );
        const address = server.address();
        if (!address || typeof address === "string")
          throw new Error("missing HTTP address");
        const client = new Agento11yClient({
          tracer: trace.getTracer("summary-cancellation-test"),
          api: { endpoint: `http://127.0.0.1:${address.port}` },
          generationExport: {
            protocol: "http",
            endpoint: "http://127.0.0.1:1/api/v1/generations:export",
            headers: {},
            flushIntervalMs: 0,
          },
          generationExporter: {
            exportGenerations: async () => ({ results: [] }),
            exportWorkflowSteps: async () => ({ results: [] }),
          },
          hooks: { enabled: true, timeoutMs: 15000, failOpen },
        });
        const controller = new AbortController();
        const event = makeEvent();
        event.signal = controller.signal;
        const original = structuredClone(event.preparation);
        const { args, warn } = guards();
        args.client = client;
        args.failOpen = failOpen;
        try {
          if (stage === "stale") controller.abort();
          const pending = runSummary(event, args);
          if (stage === "ongoing") {
            await within(arrival);
            controller.abort();
          }
          expect(await within(pending)).toEqual({ cancel: true });
          if (stage === "ongoing") await within(closed);
          expect(hits).toBe(stage === "stale" ? 0 : 1);
          expect(event.preparation).toEqual(original);
          expect(warn).not.toHaveBeenCalled();
        } finally {
          controller.abort();
          server.closeAllConnections();
          await new Promise<void>((resolve) => server.close(() => resolve()));
          await client.shutdown();
        }
      });
    }
  }

  it("updates the original compaction preparation with copies, preserving persisted content and split settings", async () => {
    const event = compaction();
    const preparation = event.preparation;
    const original = structuredClone(preparation);
    const messages = preparation.messagesToSummarize;
    const prefix = preparation.turnPrefixMessages;
    const fileOps = preparation.fileOps;
    const { args, evaluateHook } = guards(redact);
    expect(await guardCompaction(event, args)).toBeUndefined();
    expect(event.preparation).toBe(preparation);
    expect(preparation.messagesToSummarize[0]).toMatchObject({
      content: "[R] user",
    });
    expect(preparation.turnPrefixMessages[0]).toMatchObject({
      content: "[R] prefix",
    });
    expect(preparation.previousSummary).toBe("[R] previous");
    expect([...preparation.fileOps.read]).toEqual(["[R]/read"]);
    expect([...preparation.fileOps.written]).toEqual(["[R]/written"]);
    expect([...preparation.fileOps.edited]).toEqual(["[R]/edited"]);
    expect(messages).toEqual(original.messagesToSummarize);
    expect(prefix).toEqual(original.turnPrefixMessages);
    expect(fileOps).toEqual(original.fileOps);
    expect(preparation).toMatchObject({
      firstKeptEntryId: "kept",
      tokensBefore: 400,
      isSplitTurn: true,
      settings: original.settings,
    });
    expect(evaluateHook.mock.calls[0]?.[1]).toEqual({
      enabled: true,
      phases: ["preflight"],
      failOpen: false,
      strictTransforms: true,
      signal: event.signal,
    });
  });

  it("replaces tree elements, not the original array or nested persisted entries, and returns supported instructions", async () => {
    const event = tree();
    const entries = event.preparation.entriesToSummarize;
    const originals = entries.slice();
    const snapshot = structuredClone(entries);
    const { args } = guards(redact);
    expect(await guardTreeSummary(event, args)).toEqual({
      customInstructions: "[R] focus",
    });
    expect(event.preparation.entriesToSummarize).toBe(entries);
    expect(originals).toEqual(snapshot);
    expect(entries[0]).toMatchObject({
      id: "message",
      message: { content: "[R] user" },
    });
    expect(entries[1]).toMatchObject({
      content: "[R] custom",
      details: { opaque: "unchanged" },
      display: false,
    });
    expect(entries[2]).toMatchObject({
      summary: "[R] branch",
      details: {
        readFiles: ["[R]/read"],
        modifiedFiles: ["[R]/modified"],
        opaque: "unchanged",
      },
    });
    expect(entries[3]).toMatchObject({
      summary: "[R] compact",
      tokensBefore: 10,
      details: { opaque: "unchanged" },
    });
    expect(entries[4]).toBe(originals[4]);
    expect(event.preparation).toMatchObject({
      replaceInstructions: true,
      label: "label",
      targetId: "target",
      oldLeafId: "leaf",
      commonAncestorId: "common",
      customInstructions: "Falcon focus",
    });
  });

  it("cancels a required compaction instruction rewrite without committing other replacements, even with fail-open", async () => {
    const event = compaction();
    event.customInstructions = "Falcon instructions";
    const original = structuredClone(event.preparation);
    const { args } = guards(redact);
    args.failOpen = true;
    expect(await guardCompaction(event, args)).toEqual({ cancel: true });
    expect(event.preparation).toEqual(original);
    expect(event.customInstructions).toBe("Falcon instructions");
  });

  it.each([
    "allow",
    "navigation",
    "empty",
  ])("leaves preparation alone for %s", async (mode) => {
    const event = tree();
    if (mode === "navigation") event.preparation.userWantsSummary = false;
    if (mode === "empty") {
      event.preparation.entriesToSummarize = [];
      event.preparation.customInstructions = undefined;
    }
    const originals = event.preparation.entriesToSummarize.slice();
    const { args, evaluateHook } = guards();
    expect(await guardTreeSummary(event, args)).toBeUndefined();
    event.preparation.entriesToSummarize.forEach((entry, index) => {
      expect(entry).toBe(originals[index]);
    });
    expect(evaluateHook).toHaveBeenCalledTimes(mode === "allow" ? 1 : 0);
  });

  it.each([
    { outcome: "deny", failOpen: false, cancel: true },
    { outcome: "deny", failOpen: true, cancel: true },
    { outcome: "failure", failOpen: false, cancel: true },
    { outcome: "failure", failOpen: true, cancel: false },
    { outcome: "throw", failOpen: false, cancel: true },
    { outcome: "throw", failOpen: true, cancel: false },
    { outcome: "invalid", failOpen: true, cancel: true },
    { outcome: "invalid", failOpen: false, cancel: true },
  ])("returns explicit outcomes for $outcome / failOpen=$failOpen", async ({
    outcome,
    failOpen,
    cancel,
  }) => {
    const response = () => {
      if (outcome === "throw") throw new Error("Falcon secret failure");
      if (outcome === "invalid")
        return { action: "allow", transformedInput: { messages: [] } };
      return {
        action: "deny",
        ruleId:
          outcome === "failure"
            ? "__agento11y_guard_evaluation_failure"
            : "policy",
      };
    };
    for (const event of [compaction(), tree()]) {
      const original = structuredClone(event.preparation);
      const { args, warn } = guards(response);
      args.failOpen = failOpen;
      const result =
        event.type === "session_before_compact"
          ? await guardCompaction(event, args)
          : await guardTreeSummary(event, args);
      expect(result).toEqual(cancel ? { cancel: true } : undefined);
      expect(event.preparation).toEqual(original);
      expect(warn).toHaveBeenCalledOnce();
      expect(JSON.stringify(warn.mock.calls)).not.toContain("Falcon");
    }
  });

  it.each([
    "reordered",
    "identity",
    "extra-field",
    "late-empty",
    "invalid-json",
    "non-object-json",
  ])("cancels atomically for malformed %s transformations", async (kind) => {
    const event = compaction();
    event.preparation.messagesToSummarize.push({
      role: "assistant",
      content: [
        {
          type: "toolCall",
          id: "call",
          name: "read",
          arguments: { path: "Falcon/file" },
        },
      ],
      provider: "p",
      model: "m",
      api: "test",
      usage: {
        input: 0,
        output: 0,
        cacheRead: 0,
        cacheWrite: 0,
        totalTokens: 0,
        cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
      },
      stopReason: "toolUse",
      timestamp: 2,
    } as (typeof event.preparation.messagesToSummarize)[number]);
    const original = structuredClone(event.preparation);
    const { args } = guards((request) => {
      const response = redact(request);
      const messages: Message[] = response.transformedInput.messages;
      if (kind === "reordered") messages.reverse();
      if (kind === "identity" && messages[0]) messages[0].name = "not-pi";
      if (kind === "extra-field" && messages[0])
        Object.assign(messages[0], { unrelated: "added" });
      if (kind === "late-empty")
        messages[messages.length - 1] = {
          ...messages.at(-1),
          role: "user",
          parts: [{ type: "text", text: "" }],
        };
      if (kind.endsWith("json")) {
        const call = messages[1]?.parts?.[0];
        if (call?.type === "tool_call")
          call.toolCall.inputJSON = kind === "invalid-json" ? "{" : "[]";
      }
      return response;
    });
    args.failOpen = true;
    expect(await guardCompaction(event, args)).toEqual({ cancel: true });
    expect(event.preparation).toEqual(original);
  });

  it.each([
    "compaction",
    "tree",
  ])("rejects uncommittable %s before changing any host field", async (kind) => {
    const { args } = guards(redact);
    if (kind === "compaction") {
      const event = compaction();
      const original = structuredClone(event.preparation);
      Object.defineProperty(event.preparation, "fileOps", { writable: false });
      expect(await guardCompaction(event, args)).toEqual({ cancel: true });
      expect(event.preparation).toEqual(original);
    } else {
      const event = tree();
      const originals = event.preparation.entriesToSummarize.slice();
      Object.defineProperty(event.preparation.entriesToSummarize, "3", {
        writable: false,
      });
      expect(await guardTreeSummary(event, args)).toEqual({ cancel: true });
      event.preparation.entriesToSummarize.forEach((entry, index) => {
        expect(entry).toBe(originals[index]);
      });
    }
  });

  it("preserves opaque image/signature data while sanitizing text and argument JSON", async () => {
    const event = tree();
    const assistant = {
      role: "assistant",
      content: [
        { type: "text", text: "Falcon assistant" },
        {
          type: "thinking",
          thinking: "opaque Falcon",
          thinkingSignature: "signed Falcon",
        },
        {
          type: "toolCall",
          id: "call",
          name: "read",
          arguments: { path: "Falcon/path" },
        },
        { type: "image", data: "opaque Falcon", mimeType: "image/png" },
      ],
      timestamp: 3,
    };
    event.preparation.entriesToSummarize = [
      {
        type: "message",
        id: "a",
        parentId: null,
        timestamp: "time",
        message: assistant,
      } as SessionEntry,
    ];
    const original = structuredClone(assistant);
    const { args } = guards(redact);
    await guardTreeSummary(event, args);
    const entry = event.preparation.entriesToSummarize[0];
    expect(entry?.type === "message" ? entry.message : undefined).toMatchObject(
      {
        content: [
          { text: "[R] assistant" },
          original.content[1],
          { arguments: { path: "[R]/path" } },
          original.content[3],
        ],
      },
    );
    expect(assistant).toEqual(original);
  });
});
