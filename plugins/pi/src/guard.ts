import type {
  Agento11yClient,
  HookEvaluateRequest,
  Message,
} from "@grafana/agento11y";

export interface GuardArgs {
  client: Agento11yClient;
  agentName: string;
  agentVersion?: string;
  model: { provider: string; name: string };
  toolCallId: string;
  toolName: string;
  input: Record<string, unknown>;
  failOpen: boolean;
  logger?: { warn: (msg: string) => void };
}

export type GuardBlockResult = { block: true; reason: string };

export type GuardTransformResult = { transform: Record<string, unknown> };

export type GuardResult = GuardBlockResult | GuardTransformResult | undefined;

/**
 * Instructs the model how to react to a guard deny verdict, so the reason is
 * not mistaken for a generic tool failure to retry or work around. Appended
 * by both the policy-deny and fail-closed formatters.
 *
 * Mirrors `guardBehaviorHint` in `plugins/agento11y/internal/agents/guard/toolcall.go`.
 * Keep the two in sync.
 */
const GUARD_BEHAVIOR_HINT =
  "Stop and tell the user this tool call was blocked, then wait for their direction before taking any other action.";

/**
 * Wraps a rule-authored reason (which may be empty) into a self-explanatory
 * message naming the Grafana Agent Observability source, the blocked tool, and
 * the expected agent behavior.
 *
 * Mirrors `formatPolicyDeny` in the Go guard helper. Keep the wording aligned.
 */
function formatPolicyDeny(
  toolName: string,
  reason: string | undefined,
): string {
  let msg = `A Grafana Agent Observability policy blocked the "${toolName}" tool call, so it was not run.`;
  const trimmed = reason?.trim();
  if (trimmed) {
    msg += ` Reason: ${trimmed}`;
  }
  return `${msg}\n\n${GUARD_BEHAVIOR_HINT}`;
}

/**
 * Fail-closed message used when the guard could not be evaluated (transport
 * failure or, on the Go side, missing credentials). Explicitly distinguishes
 * the infrastructure failure from a policy decision.
 *
 * Mirrors `formatEvalFailure` in the Go guard helper. Keep the wording aligned.
 */
function formatEvalFailure(
  toolName: string,
  detail: string | undefined,
): string {
  let msg = `agento11y could not evaluate the Grafana Agent Observability guard for the "${toolName}" tool call, so it was blocked as a safety measure.`;
  const trimmed = detail?.trim();
  if (trimmed) {
    msg += ` Details: ${trimmed}`;
  }
  return `${msg}\n\n${GUARD_BEHAVIOR_HINT}`;
}

export interface PreflightTransformArgs {
  client: Agento11yClient;
  agentName: string;
  agentVersion?: string;
  model: { provider: string; name: string };
  messages: Message[];
  logger?: { warn: (msg: string) => void };
}

export type PreflightTransformResult = { messages: Message[] } | undefined;

/**
 * Evaluates the Sigil preflight hook against the outgoing conversation and
 * returns the redacted messages from `transformedInput.messages`, or
 * `undefined` when no usable transform was applied or evaluation failed.
 *
 * Always fails open: pi's `ContextEventResult` has no `block` field, so a
 * preflight deny cannot be enforced at this seam, and any eval error or
 * timeout forwards the original messages. `SIGIL_GUARDS_FAIL_OPEN` only
 * governs the postflight tool-call block decision, so it has no effect here.
 */
export async function runPreflightTransform(
  args: PreflightTransformArgs,
): Promise<PreflightTransformResult> {
  try {
    const req: HookEvaluateRequest = {
      phase: "preflight",
      context: {
        agentName: args.agentName,
        agentVersion: args.agentVersion,
        model: args.model,
      },
      input: {
        messages: args.messages,
      },
    };

    const resp = await args.client.evaluateHook(req, {
      enabled: true,
      phases: ["preflight"],
    });

    const transformed = resp.transformedInput?.messages;
    if (!transformed || transformed.length === 0) {
      return undefined;
    }
    return { messages: transformed };
  } catch (err) {
    args.logger?.warn(`preflight transform eval failed: ${err}`);
    return undefined;
  }
}

/**
 * Evaluates the Sigil postflight hook for a tool call. Returns a block result
 * when the server denies the call. On transport/timeout/serialization errors,
 * returns `undefined` (allow) when `failOpen` is true and a block result when
 * `failOpen` is false.
 *
 * Pi treats handler exceptions as blocks (fail-safe), so we catch every error
 * here and translate it into one of the two outcomes above instead of letting
 * it propagate.
 */
export async function runToolCallGuard(args: GuardArgs): Promise<GuardResult> {
  try {
    const req: HookEvaluateRequest = {
      phase: "postflight",
      context: {
        agentName: args.agentName,
        agentVersion: args.agentVersion,
        model: args.model,
      },
      input: {
        output: [
          {
            role: "assistant",
            parts: [
              {
                type: "tool_call",
                toolCall: {
                  id: args.toolCallId,
                  name: args.toolName,
                  inputJSON: JSON.stringify(args.input),
                },
              },
            ],
          },
        ],
      },
    };

    const resp = await args.client.evaluateHook(req, { enabled: true });
    if (resp.action === "deny") {
      return {
        block: true,
        reason: formatPolicyDeny(args.toolName, resp.reason),
      };
    }
    const transform = extractToolCallTransform(
      resp.transformedInput?.output,
      args.toolCallId,
      args.logger,
    );
    if (transform) {
      return { transform };
    }
    return undefined;
  } catch (err) {
    args.logger?.warn(`guard eval failed: ${err}`);
    if (!args.failOpen) {
      return {
        block: true,
        reason: formatEvalFailure(args.toolName, String(err)),
      };
    }
    return undefined;
  }
}

/**
 * Walks the server-returned `transformed_input.output` for the tool_call
 * part matching `toolCallId` and parses its `inputJSON` into an object.
 * Returns `undefined` on any mismatch or parse failure so the caller can
 * fall through to the original tool input unchanged.
 */
function extractToolCallTransform(
  output: Message[] | undefined,
  toolCallId: string,
  logger?: { warn: (msg: string) => void },
): Record<string, unknown> | undefined {
  if (!output || output.length === 0) return undefined;
  for (const msg of output) {
    if (!msg.parts) continue;
    for (const part of msg.parts) {
      if (part.type !== "tool_call") continue;
      const tc = part.toolCall;
      if (!tc || tc.id !== toolCallId) continue;
      // A matching tool_call whose args we cannot parse means the server sent
      // a transform we cannot apply; log it.
      const raw = tc.inputJSON;
      if (typeof raw !== "string" || raw.length === 0) {
        logger?.warn(
          `tool-call transform for ${toolCallId} dropped: empty arguments`,
        );
        return undefined;
      }
      try {
        const parsed = JSON.parse(raw) as unknown;
        if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
          logger?.warn(`tool-call transform for ${toolCallId} applied`);
          return parsed as Record<string, unknown>;
        }
        logger?.warn(
          `tool-call transform for ${toolCallId} dropped: arguments were not a JSON object`,
        );
        return undefined;
      } catch {
        logger?.warn(
          `tool-call transform for ${toolCallId} dropped: invalid JSON arguments`,
        );
        return undefined;
      }
    }
  }
  // A transform was present in the response but none of its tool_call parts
  // matched this call's id, so the original input is left unchanged. Worth a
  // line because it is otherwise indistinguishable from a plain allow.
  logger?.warn(`tool-call transform present but no part matched ${toolCallId}`);
  return undefined;
}
