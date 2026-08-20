import { randomUUID } from "node:crypto";
import type { Agento11yClient } from "@grafana/agento11y";
import { createAgento11yClient } from "./client.js";
import type { Agento11yDshConfig } from "./config.js";
import { loadConfig } from "./config.js";
import type {
  DshContext,
  DshFinishReason,
  DshGenerateOptions,
  DshSession,
  DshSessionStore,
  DshSessionTitleService,
  DshStreamChunk,
  DshTokenUsage,
  DshToolDispatchExecution,
  DshToolExecutionResult,
} from "./dsh.js";
import { resolveGitBranch } from "./git.js";
import { LocalReceiverError } from "./local.js";
import { logger } from "./logger.js";
import {
  agentNameFor,
  contentText,
  mapGenerationResult,
  mapGenerationStart,
  resolveConversationTitle,
} from "./mappers.js";
import {
  encodeAndRedactToolJSON,
  redactError,
  redactFullText,
} from "./redact.js";
import { StreamCapture } from "./streamCapture.js";
import { buildBuiltinTags } from "./tags.js";
import {
  createTelemetryProviders,
  type TelemetryProviders,
} from "./telemetry.js";

export const name = "agento11y";

interface CallLease {
  finish(work: Promise<unknown>): void;
}

interface CompletedStreamCapture {
  startedAt: number;
  completedAt: number;
  firstTokenAt?: number;
  outputBlocks: ReturnType<StreamCapture["blocks"]>;
  usage?: DshTokenUsage;
  finishReason?: DshFinishReason;
  error?: unknown;
}

interface SessionObservation {
  conversationId?: string;
  session?: DshSession;
  title?: string;
}

export function apply(ctx: DshContext): void {
  let client: Agento11yClient | null = null;
  let config: Agento11yDshConfig | null = null;
  let telemetry: TelemetryProviders | null = null;
  let initialization: Promise<boolean> | null = null;
  let closing = false;
  let shutdownPromise: Promise<void> | null = null;
  const pendingCalls = new Set<Promise<void>>();

  function admitCall(): CallLease | undefined {
    if (closing) return undefined;
    let resolve!: () => void;
    const done = new Promise<void>((doneResolve) => {
      resolve = doneResolve;
    });
    pendingCalls.add(done);
    void done.then(() => pendingCalls.delete(done));
    let finished = false;
    return {
      finish(work) {
        if (finished) return;
        finished = true;
        void work.then(resolve, resolve);
      },
    };
  }

  async function initialize(): Promise<boolean> {
    try {
      config = await loadConfig();
    } catch (err) {
      if (err instanceof LocalReceiverError) {
        logger.error("local capture unavailable", err);
        return false;
      }
      logger.error("config load failed", err);
      return false;
    }
    if (!config) return false;

    if (config.otlp) {
      try {
        telemetry = createTelemetryProviders(config.otlp, randomUUID());
      } catch (err) {
        logger.error("failed to create OTel providers", err);
      }
    }

    client = createAgento11yClient(config, {
      tracer: telemetry?.tracer,
      meter: telemetry?.meter,
    });
    if (!client) return false;

    logger.debug(
      `enabled, endpoint=${config.endpoint} auth=${config.auth.mode}${
        config.local ? " local=true" : ""
      }`,
    );
    return true;
  }

  function ensureClient(): Promise<boolean> {
    initialization ??= initialize().catch((err) => {
      logger.error("initialization failed", err);
      return false;
    });
    return initialization;
  }

  function sessionFor(sessionId: string | undefined): DshSession | undefined {
    if (!sessionId) return undefined;
    try {
      const store = ctx.get?.("sessions") as DshSessionStore | undefined;
      return store?.get?.(sessionId);
    } catch (err) {
      logger.debug("session lookup failed", err);
      return undefined;
    }
  }

  function titleFor(session: DshSession | undefined): string | undefined {
    if (!session) return undefined;
    try {
      const service = ctx.get?.("sessionTitle") as
        | DshSessionTitleService
        | undefined;
      return service?.get?.(session)?.title;
    } catch (err) {
      logger.debug("session title lookup failed", err);
      return undefined;
    }
  }

  function observeSession(
    conversationId: string | undefined,
    directSession: DshSession | undefined,
  ): SessionObservation {
    const session = directSession ?? sessionFor(conversationId);
    return { conversationId, session, title: titleFor(session) };
  }

  function refreshSession(observed: SessionObservation): SessionObservation {
    const latest = observeSession(observed.conversationId, observed.session);
    return { ...latest, title: latest.title ?? observed.title };
  }

  function sessionContext(
    observed: SessionObservation,
    settings: Agento11yDshConfig,
  ) {
    return {
      ...observed,
      conversationTitle: resolveConversationTitle({
        sessionTitle: observed.title,
        conversationId: observed.conversationId,
      }),
      agentName: agentNameFor(settings.agentName, observed.session),
    };
  }

  async function exportGeneration(
    request: DshGenerateOptions,
    ready: Promise<boolean>,
    capture: CompletedStreamCapture,
    observedSession: SessionObservation,
  ): Promise<void> {
    if (!(await ready) || !client || !config) return;
    const active = client;
    const settings = config;
    const call = sessionContext(observedSession, settings);
    const cwd = call.session?.header?.cwd ?? process.cwd();
    const seed = mapGenerationStart(request, {
      conversationId: call.conversationId,
      conversationTitle: call.conversationTitle,
      agentName: call.agentName,
      agentVersion: settings.agentVersion,
      startedAt: capture.startedAt,
      tags: buildBuiltinTags({ cwd, gitBranch: resolveGitBranch(cwd) }),
    });
    const result = mapGenerationResult({
      request,
      completedAt: capture.completedAt,
      outputBlocks: capture.outputBlocks,
      usage: capture.usage,
      finishReason: capture.finishReason,
    });

    const streaming = !request.purpose;
    const record = async (recorder: {
      setFirstTokenAt: (at: Date) => void;
      setResult: (r: typeof result) => void;
      setCallError: (err: Error) => void;
    }) => {
      if (streaming && capture.firstTokenAt !== undefined) {
        recorder.setFirstTokenAt(new Date(capture.firstTokenAt));
      }
      recorder.setResult(result);
      const callError = streamCallError(capture);
      if (callError) recorder.setCallError(callError);
    };

    if (streaming) await active.startStreamingGeneration(seed, record);
    else await active.startGeneration(seed, record);

    const providers = telemetry;
    if (providers) {
      try {
        await providers.forceFlush();
      } catch (err) {
        logger.debug("telemetry flush failed", err);
      }
    }
  }

  function streamCallError(capture: CompletedStreamCapture): Error | undefined {
    if (capture.error !== undefined) return redactError(capture.error);
    const kind = capture.finishReason?.kind;
    if (kind !== "error" && kind !== "aborted") return undefined;
    return redactError(
      capture.finishReason?.failure?.message ?? `model call ${kind}`,
    );
  }

  async function* wrapStream(
    request: DshGenerateOptions,
    next: () => AsyncIterable<DshStreamChunk>,
  ): AsyncGenerator<DshStreamChunk> {
    const lease = admitCall();
    if (!lease) {
      for await (const chunk of next()) yield chunk;
      return;
    }

    const ready = ensureClient();
    const startedAt = Date.now();
    const observedSession = observeSession(request.sessionId, undefined);
    const capture = new StreamCapture();
    let exhausted = false;
    let failure: unknown;
    try {
      for await (const chunk of next()) {
        try {
          capture.push(chunk);
        } catch (err) {
          logger.debug("chunk capture failed", err);
        }
        yield chunk;
      }
      exhausted = true;
    } catch (err) {
      failure = err;
      throw err;
    } finally {
      const aborted = request.signal?.aborted === true;
      let finishReason = capture.finish;
      if (aborted) {
        const message =
          request.signal?.reason instanceof Error
            ? request.signal.reason.message
            : request.signal?.reason === undefined
              ? "model call aborted"
              : String(request.signal.reason);
        finishReason = {
          kind: "aborted",
          failure: { message, code: "ABORTED" },
        };
        failure ??= request.signal?.reason ?? new Error(message);
      }
      const interrupted =
        aborted ||
        !exhausted ||
        finishReason?.kind === "aborted" ||
        finishReason?.kind === "error";
      const completed: CompletedStreamCapture = {
        startedAt,
        completedAt: Date.now(),
        firstTokenAt: capture.firstTokenAt,
        outputBlocks: interrupted
          ? capture.interruptedBlocks()
          : capture.blocks(),
        usage: capture.usage,
        finishReason,
        error: failure,
      };
      const completedSession = refreshSession(observedSession);
      lease.finish(
        exportGeneration(request, ready, completed, completedSession).catch(
          (err) => {
            logger.debug("generation export failed", err);
          },
        ),
      );
    }
  }

  async function recordToolExecution(
    exec: DshToolDispatchExecution,
    ready: Promise<boolean>,
    startedAt: number,
    completedAt: number,
    observedSession: SessionObservation,
    outcome: DshToolExecutionResult | undefined,
    failure: unknown,
  ): Promise<void> {
    if (!(await ready) || !client || !config) return;
    const settings = config;
    const call = sessionContext(observedSession, settings);
    const recorder = client.startToolExecution({
      toolName: exec.name,
      toolCallId: exec.callId,
      toolType: "function",
      conversationId: call.conversationId,
      conversationTitle: call.conversationTitle,
      agentName: call.agentName,
      agentVersion: settings.agentVersion,
      startedAt: new Date(startedAt),
      contentCapture: settings.contentCapture,
    });
    const end: { arguments?: unknown; result?: unknown; completedAt: Date } = {
      completedAt: new Date(completedAt),
    };
    if (settings.contentCapture === "full") {
      const encoded = encodeAndRedactToolJSON(exec.arguments);
      if (encoded !== undefined) end.arguments = encoded;
      const text = contentText(outcome?.content);
      if (text.length > 0) end.result = redactFullText(text);
    }
    if (failure !== undefined) {
      recorder.setCallError(redactError(failure));
    } else if (outcome?.isError) {
      recorder.setCallError(
        redactError(outcome.error?.message ?? "tool returned error"),
      );
    }
    recorder.setResult(end);
    recorder.end();
  }

  async function wrapToolExecute(
    exec: DshToolDispatchExecution,
    next: () => Promise<DshToolExecutionResult>,
  ): Promise<DshToolExecutionResult> {
    const lease = admitCall();
    if (!lease) return next();

    const ready = ensureClient();
    const startedAt = Date.now();
    const observedSession = observeSession(exec.agent?.id, exec.agent?.session);
    let outcome: DshToolExecutionResult | undefined;
    let failure: unknown;
    try {
      outcome = await next();
      return outcome;
    } catch (err) {
      failure = err;
      throw err;
    } finally {
      const completedAt = Date.now();
      const completedSession = refreshSession(observedSession);
      lease.finish(
        recordToolExecution(
          exec,
          ready,
          startedAt,
          completedAt,
          completedSession,
          outcome,
          failure,
        ).catch((err) => {
          logger.debug("tool execution record failed", err);
        }),
      );
    }
  }

  ctx.on("llm/stream", wrapStream);
  ctx.on("tools/execute", wrapToolExecute);
  ctx.effect?.(() => shutdown);

  function shutdown(): Promise<void> {
    shutdownPromise ??= shutdownOnce();
    return shutdownPromise;
  }

  async function shutdownOnce(): Promise<void> {
    closing = true;
    await Promise.allSettled([...pendingCalls]);
    const active = client;
    const providers = telemetry;
    client = null;
    config = null;
    telemetry = null;
    if (active) {
      try {
        await active.shutdown();
      } catch (err) {
        logger.error("client shutdown failed", err);
      }
    }
    if (providers) {
      try {
        await providers.shutdown();
      } catch (err) {
        logger.error("telemetry shutdown failed", err);
      }
    }
  }
}
