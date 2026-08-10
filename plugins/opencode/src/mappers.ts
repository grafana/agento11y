import type {
  ContentCaptureMode,
  GenerationResult,
  Message,
  ToolDefinition,
} from "@grafana/agento11y";
import type { Hooks } from "@opencode-ai/plugin";
import type { AssistantMessage, Part } from "@opencode-ai/sdk";

export type { GenerationResult };

/**
 * String-level redaction contract the mappers and hooks share. The
 * implementation comes from `@grafana/agento11y-core`; see `createRedactor` in
 * hooks.ts.
 */
export interface Redactor {
  redact(text: string): string;
  redactLightweight(text: string): string;
}

function includesMessageBodies(contentCapture: ContentCaptureMode): boolean {
  return contentCapture !== "metadata_only";
}

function includesToolBodies(contentCapture: ContentCaptureMode): boolean {
  return (
    contentCapture === "full" || contentCapture === "full_with_metadata_spans"
  );
}

/**
 * Map user-side parts to agento11y input messages. Nothing is redacted here:
 * user text is the user's own data and Agent Observability needs it for prompt
 * analysis when content capture allows it. A caller that ran a preflight redact
 * rule substitutes the rewritten text before calling this.
 */
export function mapInputMessages(
  parts: Part[],
  contentCapture: ContentCaptureMode = "full",
): Message[] {
  if (!includesMessageBodies(contentCapture)) return [];

  const messages: Message[] = [];
  for (const part of parts) {
    if (part.type === "text" && part.text.trim().length > 0) {
      messages.push({
        role: "user",
        parts: [{ type: "text", text: part.text }],
      });
    }
  }
  return messages;
}

/** Map assistant-side parts to agento11y output messages with redaction. */
export function mapOutputMessages(
  parts: Part[],
  redactor: Redactor,
  contentCapture: ContentCaptureMode = "full",
): Message[] {
  const messages: Message[] = [];
  const includeBodies = includesMessageBodies(contentCapture);
  const includeToolBodies = includesToolBodies(contentCapture);

  for (const part of parts) {
    switch (part.type) {
      case "text": {
        if (includeBodies) {
          const text = redactor.redactLightweight(part.text);
          if (text.trim().length > 0) {
            messages.push({
              role: "assistant",
              parts: [{ type: "text", text }],
            });
          }
        }
        break;
      }
      case "reasoning": {
        if (includeBodies) {
          const thinking = redactor.redactLightweight(part.text);
          if (thinking.trim().length > 0) {
            messages.push({
              role: "assistant",
              parts: [{ type: "thinking", thinking }],
            });
          }
        }
        break;
      }
      case "tool": {
        const { state } = part;
        if (state.status === "completed") {
          messages.push({
            role: "assistant",
            parts: [
              {
                type: "tool_call",
                toolCall: {
                  id: part.callID,
                  name: part.tool,
                  inputJSON: includeToolBodies
                    ? redactor.redact(JSON.stringify(state.input ?? {}))
                    : "",
                },
              },
            ],
          });
          messages.push({
            role: "tool",
            parts: [
              {
                type: "tool_result",
                toolResult: {
                  toolCallId: part.callID,
                  name: part.tool,
                  content: includeToolBodies
                    ? redactor.redact(state.output ?? "")
                    : "",
                },
              },
            ],
          });
        } else if (state.status === "error") {
          messages.push({
            role: "assistant",
            parts: [
              {
                type: "tool_call",
                toolCall: {
                  id: part.callID,
                  name: part.tool,
                  inputJSON: includeToolBodies
                    ? redactor.redact(JSON.stringify(state.input ?? {}))
                    : "",
                },
              },
            ],
          });
          messages.push({
            role: "tool",
            parts: [
              {
                type: "tool_result",
                toolResult: {
                  toolCallId: part.callID,
                  name: part.tool,
                  content: includeToolBodies
                    ? redactor.redact(state.error ?? "unknown error")
                    : "",
                  isError: true,
                },
              },
            ],
          });
        }
        break;
      }
    }
  }
  return messages;
}

/** Return the enabled tool names from legacy `UserMessage.tools` overrides. */
export function legacyToolOverrideNames(
  tools: Record<string, boolean> | undefined,
): string[] {
  if (!tools) return [];
  return Object.entries(tools)
    .filter(([, enabled]) => enabled)
    .map(([name]) => name);
}

/**
 * Name-only function tool definitions, deduplicated and sorted by name.
 * OpenCode does not expose tool descriptions or schemas to the plugin, so
 * this matches the claude-code plugin: the catalog builds up over time from
 * the tools each generation used.
 */
export function mapToolDefinitions(names: Iterable<string>): ToolDefinition[] {
  const uniq = new Set<string>();
  for (const name of names) {
    if (typeof name === "string" && name.length > 0) uniq.add(name);
  }
  return [...uniq].sort().map((name) => ({ name, type: "function" }));
}

/**
 * Token counts in opencode's shape, shared by `AssistantMessage.tokens` and
 * `StepFinishPart.tokens`. `input` excludes cached tokens and `output` excludes
 * reasoning tokens; `Session.getUsage` subtracts both upstream.
 */
export type OpencodeTokens = AssistantMessage["tokens"];

/**
 * Map an AssistantMessage + parts to an agento11y GenerationResult with content.
 *
 * `tokens` are the per-step counts summed over the whole message, which the
 * caller accumulates from `step-finish` parts. Omit them to use `msg.tokens`,
 * which on a multi-step message covers only the last step.
 */
export function mapGeneration(
  msg: AssistantMessage,
  userParts: Part[],
  assistantParts: Part[],
  redactor: Redactor,
  contentCapture: ContentCaptureMode = "full",
  tokens?: OpencodeTokens,
): GenerationResult {
  const usage = tokens ?? msg.tokens;
  return {
    input: mapInputMessages(userParts, contentCapture),
    output: mapOutputMessages(assistantParts, redactor, contentCapture),
    usage: {
      inputTokens: usage.input,
      outputTokens: usage.output,
      reasoningTokens: usage.reasoning,
      cacheReadInputTokens: usage.cache.read,
      cacheWriteInputTokens: usage.cache.write,
    },
    responseModel: msg.modelID,
    stopReason: msg.finish,
    completedAt: msg.time.completed ? new Date(msg.time.completed) : undefined,
    metadata: {
      cost: msg.cost,
    },
  };
}

/**
 * One entry of opencode's `experimental.chat.messages.transform` output. The
 * host requires both fields. They are optional here because opencode hands the
 * same array to every plugin in turn, so an earlier plugin can have replaced or
 * emptied an entry. An unrecognized shape becomes a placeholder, not a throw.
 */
export type OutgoingMessage = {
  info?: { id?: string; role?: string; sessionID?: string };
  parts?: Part[];
};

type HostOutgoingMessage = Parameters<
  NonNullable<Hooks["experimental.chat.messages.transform"]>
>[1]["messages"][number];

/**
 * Build-time pin on the host payload, never called. Every field of
 * `OutgoingMessage` is optional, so a host rename of `info` would still compile
 * and preflight would silently map every message to a placeholder. Reading both
 * fields off the host type turns that rename into a compile error.
 */
function _pinHostOutgoingShape(entry: HostOutgoingMessage): OutgoingMessage {
  return { info: entry.info, parts: entry.parts };
}

/** A text part of an outgoing message that preflight may rewrite. */
type TextSlot = { part: Extract<Part, { type: "text" }> };

/**
 * Map opencode's outgoing conversation (the messages it is about to convert
 * for the provider) to agento11y `Message[]` for a preflight hook evaluation.
 *
 * Emits one message per input entry, in order, so the redacted round-trip can
 * be aligned by index. A slot with nothing to evaluate becomes an empty
 * placeholder, because a shorter array discards the transform on write-back.
 *
 * Only text is forwarded, matching what `applyRedactedText` can write back:
 * - `reasoning` parts carry an opaque provider signature (`part.metadata`)
 *   that `MessageV2.toModelMessages` replays unchanged, so a rewrite would
 *   invalidate it. pi drops `thinking` for the same reason.
 * - Tool arguments are already evaluated postflight by `runToolCallGuard`, and
 *   tool result text cannot be written back without breaking slot alignment.
 * - `contentCapture` is not applied: the hook server holds this payload in
 *   memory and never stores it, so redacting it would defeat the transform.
 *
 * The opencode counterpart of `mapAgentMessagesForHook` in
 * `plugins/pi/src/mappers.ts`. The placeholder-slot contract is shared, the
 * covered part types are not.
 */
export function mapOutgoingMessagesForHook(
  messages: OutgoingMessage[],
): Message[] {
  return messages.map((msg) => {
    if (!msg) return { role: "unknown", parts: [] };
    const text = redactableTextSlots(msg)
      .map((slot) => slot.part.text)
      .join("\n");
    return {
      role: msg.info?.role || "unknown",
      parts: text.length > 0 ? [{ type: "text", text }] : [],
    };
  });
}

/**
 * The text parts of an outgoing message that preflight may rewrite, in `parts`
 * order.
 *
 * `ignored` user text is excluded because `MessageV2.toModelMessages` never
 * sends it. Empty text parts are excluded too: the same conversion drops an
 * empty user part, and an empty assistant part is the separator opencode keeps
 * between signed reasoning blocks. Writing into either would add text the
 * provider does not see today, and redact nothing.
 */
function redactableTextSlots(msg: OutgoingMessage): TextSlot[] {
  const role = msg.info?.role;
  if (role !== "user" && role !== "assistant") return [];
  const parts = msg.parts;
  if (!Array.isArray(parts)) return [];

  const slots: TextSlot[] = [];
  parts.forEach((part) => {
    if (!part || part.type !== "text") return;
    if (part.text.length === 0) return;
    if (role === "user" && part.ignored === true) return;
    slots.push({ part });
  });
  return slots;
}

/**
 * What a preflight transform rewrote: how many messages it touched, and the
 * new text of every part it wrote, keyed by `partTextKey`. The map includes
 * the parts a rewrite emptied, so a caller that replays it reproduces the
 * conversation the provider received rather than a longer one.
 */
export type AppliedRedaction = {
  messageCount: number;
  textByPart: Map<string, string>;
};

/**
 * Key for one part's rewritten text. Scoped by session so a stale entry cannot
 * outlive `session.deleted`, which clears a session's keys by prefix.
 */
export function partTextKey(sessionID: string, partID: string): string {
  return `${sessionID}\x00${partID}`;
}

/** One entry's planned rewrite: the new text for each targeted text slot. */
type MessagePlan = {
  index: number;
  entry: OutgoingMessage;
  parts: Part[];
  updates: Array<{ slot: TextSlot; text: string }>;
  needsClone: boolean;
};

/**
 * Write redacted text from a server-side preflight transform back into
 * opencode's outgoing messages, aligned by index. Returns the messages it
 * rewrote and the new text of each part, or `null` when the transform was
 * discarded whole. A server that changed nothing yields a count of `0` and an
 * empty map.
 *
 * Message count and order never change, and only text parts are touched. Every
 * entry is planned before anything is written, so a transform is never applied
 * halfway.
 *
 * `outgoing` is the live array opencode passes to the hook, so writes go
 * through it. Text parts are mutated in place to keep object identity. A frozen
 * part is cloned into a new array slot instead, because opencode freezes
 * `output.args` on newer versions and this payload could follow.
 *
 * Shares a name with `applyRedactedText` in `plugins/pi/src/mappers.ts` but not
 * the behavior: this one is all-or-nothing, handles frozen parts, and returns a
 * count. Both parse the same `transformed_input` encoding, so a wire change has
 * to reach both.
 */
export function applyRedactedText(
  outgoing: OutgoingMessage[],
  redacted: Message[],
): AppliedRedaction | null {
  if (outgoing.length !== redacted.length) return null;

  const plans: MessagePlan[] = [];
  for (let i = 0; i < outgoing.length; i++) {
    const entry = outgoing[i];
    const sig = redacted[i];
    if (!sig) return null;
    // A placeholder slot the forward mapper kept for alignment. Skipping it
    // keeps the redaction for the rest of the conversation.
    if (!entry) continue;

    const slots = redactableTextSlots(entry);
    const parts = entry.parts;
    if (slots.length === 0 || !Array.isArray(parts)) continue;

    const text = textFromAgento11yMessage(sig);
    // Writing an empty string back would delete content rather than redact it:
    // opencode drops an empty user text part, then drops the message once no
    // part survives, changing the message count this function preserves.
    if (text === null || text.length === 0) continue;
    if (text === slots.map((slot) => slot.part.text).join("\n")) continue;

    // The redacted text goes into the first slot and the rest are emptied. A
    // transform can add or remove newlines, so splitting it back across the
    // slots risks misrepresenting the message.
    plans.push({
      index: i,
      entry,
      parts,
      updates: slots.map((slot, position) => ({
        slot,
        text: position === 0 ? text : "",
      })),
      needsClone: slots.some((slot) => Object.isFrozen(slot.part)),
    });
  }

  if (plans.some((plan) => plan.needsClone) && Object.isFrozen(outgoing)) {
    // A frozen part needs its array slot reassigned, and the array is frozen
    // too. Discard rather than apply the transform to part of the turn.
    return null;
  }

  const textByPart = new Map<string, string>();
  for (const plan of plans) {
    for (const update of plan.updates) {
      const { id, sessionID } = update.slot.part;
      // A part without both ids cannot be matched at export time. It reaches
      // the provider redacted either way; only the export keeps the original.
      if (id && sessionID) {
        textByPart.set(partTextKey(sessionID, id), update.text);
      }
    }
    if (plan.needsClone) {
      // Keyed by part identity, not position, so a reorder between planning
      // and writing cannot put a replacement on the wrong part.
      const replacements = new Map<Part, string>(
        plan.updates.map((update) => [update.slot.part, update.text]),
      );
      const nextParts = plan.parts.map((part) => {
        const text = replacements.get(part);
        return text === undefined ? part : { ...part, text };
      });
      outgoing[plan.index] = { ...plan.entry, parts: nextParts };
      continue;
    }
    for (const update of plan.updates) {
      update.slot.part.text = update.text;
    }
  }
  return { messageCount: plans.length, textByPart };
}

/**
 * Join the text parts of a server-returned message, or null when it carries no
 * text. The `content` shorthand is accepted as well as typed parts: the Agent
 * Observability API emits typed parts, but both encodings round-trip.
 *
 * A renamed copy of `extractTextFromAgento11yMessage` in
 * `plugins/pi/src/mappers.ts`, parsing the same encoding; keep the two aligned.
 */
function textFromAgento11yMessage(msg: Message): string | null {
  if (typeof msg.content === "string") return msg.content;
  if (!msg.parts) return null;
  const texts: string[] = [];
  for (const part of msg.parts) {
    if (part.type === "text") texts.push(part.text);
  }
  return texts.length > 0 ? texts.join("\n") : null;
}

export function mapError(error: NonNullable<AssistantMessage["error"]>): Error {
  switch (error.name) {
    case "ProviderAuthError":
      return new Error("provider_auth");
    case "APIError":
      return new Error(`api_error: ${error.data.statusCode ?? "unknown"}`);
    case "MessageOutputLengthError":
      return new Error("output_length_exceeded");
    case "MessageAbortedError":
      return new Error("aborted");
    case "UnknownError":
      return new Error("unknown_error");
    default: {
      const _exhaustive: never = error;
      return new Error("unknown_error");
    }
  }
}
