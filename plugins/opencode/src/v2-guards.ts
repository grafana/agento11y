import { isDeepStrictEqual } from "node:util";
import type { Agento11yClient, Message, MessagePart } from "@grafana/agento11y";
import type { Plugin } from "@opencode/plugin";
import type { PermissionEvaluation } from "@opencode/plugin/promise/permission";
import type { Registration } from "@opencode/plugin/promise/registration";
import type {
  SessionContext,
  SessionPrompt,
  SessionRequest,
} from "@opencode/plugin/promise/session";
import type { Agento11yOpencodeConfig } from "./config.js";
import {
  runPreflightTransform,
  runPromptGuard,
  runToolCallGuard,
} from "./guard.js";
import { nativeAgentName } from "./v2-mappers.js";

const TRANSFORM_REFUSAL =
  "agento11y could not safely apply a required guard transformation, so the operation was blocked.";

export class NativeGuardTransformError extends Error {
  constructor() {
    super(TRANSFORM_REFUSAL);
    this.name = "NativeGuardTransformError";
  }
}

type Path = readonly (string | number)[];
export interface NativeGuardSlot {
  readonly message: number;
  readonly part: number;
  readonly field?: Path;
  readonly source?: Path;
}
export interface NativeGuardProjection {
  readonly messages: Message[];
  readonly slots: readonly NativeGuardSlot[];
  readonly rewriteSafe: boolean;
}

function refuse(): never {
  throw new NativeGuardTransformError();
}

function json(value: unknown): string {
  try {
    const encoded = JSON.stringify(value);
    if (encoded === undefined) return refuse();
    return encoded;
  } catch {
    return refuse();
  }
}

// Provider metadata may bind a signature to the entire preceding conversation.
// Unknown native state is preserved, but does not establish rewrite safety.
function hasOpaqueState(value: unknown): boolean {
  if (!value || typeof value !== "object" || ArrayBuffer.isView(value))
    return false;
  return Object.entries(value).some(([key, child]) => {
    if (child === undefined || child === null) return false;
    if (
      /signature|encrypted|checkpoint|previous.?response|provider.?state/i.test(
        key,
      )
    )
      return true;
    if (
      ["native", "providerMetadata"].includes(key) &&
      typeof child === "object" &&
      Object.keys(child).length > 0
    )
      return true;
    return hasOpaqueState(child);
  });
}

export function nativePromptMessages(event: SessionPrompt): Message[] {
  return [{ role: "user", parts: [{ type: "text", text: event.prompt.text }] }];
}

export function nativePermissionInput(
  event: PermissionEvaluation,
): Record<string, unknown> {
  return {
    resources: event.resources,
    ...(event.metadata === undefined ? {} : { metadata: event.metadata }),
  };
}

/** Slots refer to the provider copy, never to stored session messages. */
export function projectNativeRequest(
  event: SessionRequest,
): NativeGuardProjection {
  const messages: Message[] = [];
  const slots: NativeGuardSlot[] = [];
  const add = (
    role: string,
    parts: { part: MessagePart; field?: Path; source?: Path }[],
  ) => {
    if (parts.length === 0) return;
    const message = messages.length;
    messages.push({ role, parts: parts.map((entry) => entry.part) });
    parts.forEach((entry, part) => {
      slots.push({ message, part, field: entry.field, source: entry.source });
    });
  };
  event.system.forEach((part, index) => {
    add("system", [
      {
        part: { type: "text", text: part.text },
        field: ["text"],
        source: ["system", index, "text"],
      },
    ]);
  });
  event.messages.forEach((message, index) => {
    const parts: { part: MessagePart; field?: Path; source?: Path }[] = [];
    message.content.forEach((part, ordinal) => {
      const source: Path = ["messages", index, "content", ordinal];
      switch (part.type) {
        case "text":
          parts.push({
            part: { type: "text", text: part.text },
            field: ["text"],
            source: [...source, "text"],
          });
          break;
        case "reasoning":
          parts.push({
            part: { type: "thinking", thinking: part.text },
            field: ["thinking"],
            source: [...source, "text"],
          });
          break;
        case "compaction":
          if (typeof part.text === "string")
            parts.push({ part: { type: "text", text: part.text } });
          break;
        case "tool-call":
          parts.push({
            part: {
              type: "tool_call",
              toolCall: {
                id: part.id,
                name: part.name,
                inputJSON: json(part.input),
              },
            },
          });
          break;
        case "tool-result": {
          const identity = {
            toolCallId: part.id,
            name: part.name,
            ...(part.result.type === "error" ? { isError: true } : {}),
          };
          const text = (value: string, path: Path) =>
            parts.push({
              part: {
                type: "tool_result",
                toolResult: { ...identity, content: value },
              },
              field: ["toolResult", "content"],
              source: path,
            });
          if (
            ["text", "error"].includes(part.result.type) &&
            typeof part.result.value === "string"
          ) {
            text(part.result.value, [...source, "result", "value"]);
          } else if (part.result.type === "content") {
            part.result.value.forEach((item, itemIndex) => {
              if (item.type === "text")
                text(item.text, [
                  ...source,
                  "result",
                  "value",
                  itemIndex,
                  "text",
                ]);
            });
          } else {
            parts.push({
              part: {
                type: "tool_result",
                toolResult: {
                  ...identity,
                  contentJSON: json(part.result.value),
                },
              },
            });
          }
          break;
        }
      }
    });
    add(message.role, parts);
  });
  return {
    messages,
    slots,
    rewriteSafe:
      !hasOpaqueState({
        system: event.system,
        messages: event.messages,
        options: event.options,
      }) &&
      !event.messages.some((message) =>
        message.content.some((part) => part.type === "compaction"),
      ),
  };
}

function at(value: unknown, path: Path): unknown {
  let current = value;
  for (const key of path) {
    if (current === null || typeof current !== "object") return refuse();
    current = (current as Record<string | number, unknown>)[key];
  }
  return current;
}

function replace(value: unknown, path: Path, text: string): unknown {
  if (path.length === 0) return text;
  if (value === null || typeof value !== "object") return refuse();
  const [key, ...rest] = path;
  const copy = Array.isArray(value) ? [...value] : { ...value };
  (copy as Record<string | number, unknown>)[key] = replace(
    at(value, [key]),
    rest,
    text,
  );
  return copy;
}

function comparable(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(comparable);
  if (value && typeof value === "object")
    return Object.fromEntries(
      Object.entries(value)
        .filter(([, child]) => child !== undefined)
        .map(([key, child]) => [key, comparable(child)]),
    );
  return value;
}

/** Validate every replacement before constructing either outgoing array. */
export function applyNativeTransform(
  event: SessionRequest,
  projection: NativeGuardProjection,
  transformed: Message[],
): Pick<SessionRequest, "system" | "messages"> {
  if (
    !Array.isArray(transformed) ||
    transformed.length !== projection.messages.length
  )
    return refuse();
  const normalized = transformed.map((message, index): Message => {
    if (!message || typeof message !== "object") return refuse();
    if (message.content === undefined) return message;
    const original = projection.messages[index].parts;
    if (
      message.parts !== undefined ||
      typeof message.content !== "string" ||
      original?.length !== 1 ||
      original[0].type !== "text"
    )
      return refuse();
    const { content, ...rest } = message;
    return { ...rest, parts: [{ type: "text", text: content }] };
  });
  let expected: unknown = projection.messages;
  const changes: { source: Path; text: string }[] = [];
  for (const slot of projection.slots) {
    if (!slot.field || !slot.source) continue;
    const path = [slot.message, "parts", slot.part, ...slot.field];
    const text = at(normalized, path);
    if (typeof text !== "string") return refuse();
    if (text === at(projection.messages, path)) continue;
    if (!projection.rewriteSafe) return refuse();
    expected = replace(expected, path, text);
    changes.push({ source: slot.source, text });
  }
  if (!isDeepStrictEqual(comparable(expected), comparable(normalized)))
    return refuse();
  let output: unknown = { system: event.system, messages: event.messages };
  for (const change of changes)
    output = replace(output, change.source, change.text);
  return output as Pick<SessionRequest, "system" | "messages">;
}

export interface NativeGuardsArgs {
  client: Agento11yClient;
  config: Agento11yOpencodeConfig;
  context: Pick<Plugin.Context, "app" | "session" | "tool" | "permission">;
}

interface GuardModel {
  provider: string;
  name: string;
}
interface SessionIdentity {
  agent?: string;
  model?: GuardModel;
}
const MAX_SESSION_IDENTITIES = 512;
const SESSION_LOOKUP_TIMEOUT_MS = 1000;

export function createNativeGuards({
  client,
  config,
  context,
}: NativeGuardsArgs) {
  const enabled = config.guards?.enabled === true;
  const common = {
    client,
    agentVersion: config.agentVersion || context.app.version,
    failOpen: config.guards?.failOpen ?? true,
    strictTransforms: true,
    logger: {
      warn: (message: string) =>
        console.warn(`[agento11y-opencode] ${message}`),
    },
  };
  const unknownModel = { provider: "unknown", name: "unknown" };
  // Prompt, tool and permission events carry no model, and prompts no agent.
  // Only the primary `context` request names the session's own agent and model.
  const identities = new Map<string, SessionIdentity>();
  const remember = (sessionID: string, identity: SessionIdentity) => {
    identities.delete(sessionID);
    identities.set(sessionID, identity);
    const [oldest] = identities.keys();
    if (identities.size > MAX_SESSION_IDENTITIES && oldest !== undefined)
      identities.delete(oldest);
  };
  const lookup = async (
    sessionID: string,
  ): Promise<SessionIdentity | undefined> => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    try {
      const session = await Promise.race([
        Promise.resolve().then(() => context.session.get({ sessionID })),
        new Promise<undefined>((resolve) => {
          timer = setTimeout(
            () => resolve(undefined),
            SESSION_LOOKUP_TIMEOUT_MS,
          );
        }),
      ]);
      return {
        agent: session?.agent,
        model: session?.model && {
          provider: session.model.providerID,
          name: session.model.id,
        },
      };
    } catch {
      return undefined;
    } finally {
      clearTimeout(timer);
    }
  };
  const identify = async (sessionID: string, agent?: string) => {
    const known = identities.get(sessionID) ?? (await lookup(sessionID));
    return {
      agentName: nativeAgentName(config.agentName, agent ?? known?.agent),
      model: known?.model ?? unknownModel,
    };
  };
  const request = async (
    event: SessionRequest & { readonly agent?: string },
  ) => {
    const projection = projectNativeRequest(event);
    const agentName = event.agent
      ? nativeAgentName(config.agentName, event.agent)
      : (await identify(event.sessionID)).agentName;
    const result = await runPreflightTransform({
      ...common,
      agentName,
      model: { provider: event.model.providerID, name: event.model.id },
      conversationId: event.sessionID,
      messages: projection.messages,
    });
    if (result?.block) throw new Error(result.block);
    if (!result?.messages) return;
    const output = applyNativeTransform(event, projection, result.messages);
    event.system = output.system;
    event.messages = output.messages;
  };

  return {
    forget(sessionID: string): void {
      identities.delete(sessionID);
    },
    async register(): Promise<() => Promise<void>> {
      if (!enabled) return async () => {};
      const registrations: Registration[] = [];
      const active = new Set<Promise<void>>();
      let closed = false;
      const guard =
        <T>(callback: (event: T) => Promise<void>) =>
        async (event: T) => {
          if (closed)
            throw new Error("agento11y native guards are shutting down");
          const task = callback(event);
          active.add(task);
          try {
            await task;
          } finally {
            active.delete(task);
          }
        };
      let cleanup: Promise<void> | undefined;
      const dispose = () =>
        (cleanup ??= (async () => {
          closed = true;
          const results = await Promise.allSettled(
            [...registrations]
              .reverse()
              .map((registration) =>
                Promise.resolve().then(() => registration.dispose()),
              ),
          );
          await Promise.allSettled([...active]);
          identities.clear();
          if (results.some((result) => result.status === "rejected"))
            throw new Error("agento11y native guard cleanup failed");
        })());
      try {
        registrations.push(
          await context.session.hook(
            "prompt",
            guard<SessionPrompt>(async (event) => {
              const result = await runPromptGuard({
                ...common,
                ...(await identify(event.sessionID)),
                conversationId: event.sessionID,
                messages: nativePromptMessages(event),
              });
              if (result?.block) throw new Error(result.reason);
            }),
          ),
        );
        registrations.push(
          await context.session.hook(
            "context",
            guard<SessionContext>(async (event) => {
              remember(event.sessionID, {
                agent: event.agent,
                model: {
                  provider: event.model.providerID,
                  name: event.model.id,
                },
              });
              await request(event);
            }),
          ),
        );
        for (const name of ["compaction", "title", "generate"] as const)
          registrations.push(await context.session.hook(name, guard(request)));
        registrations.push(
          await context.tool.hook(
            "execute.before",
            guard(async (event) => {
              const result = await runToolCallGuard({
                ...common,
                ...(await identify(event.sessionID, event.agent)),
                conversationId: event.sessionID,
                toolCallId: event.id,
                toolName: event.tool,
                input: event.input,
              });
              if (result && "block" in result) throw new Error(result.reason);
              if (result && "transform" in result)
                event.input = result.transform;
            }),
          ),
        );
        registrations.push(
          await context.permission.hook(
            "evaluate",
            guard<PermissionEvaluation>(async (event) => {
              const result = await runToolCallGuard({
                ...common,
                ...(await identify(event.sessionID, event.agent)),
                conversationId: event.sessionID,
                toolCallId: event.source?.id,
                toolName: event.action,
                input: nativePermissionInput(event),
              });
              if (!result) return;
              event.effect = "deny";
              event.message =
                "block" in result ? result.reason : TRANSFORM_REFUSAL;
            }),
          ),
        );
        return dispose;
      } catch {
        await dispose();
        throw new Error("agento11y native guard registration failed");
      }
    },
  };
}
