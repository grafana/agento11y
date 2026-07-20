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
  toolCallId?: string;
  toolName: string;
  input: unknown;
  failOpen: boolean;
  logger?: { warn: (msg: string) => void };
}

export type GuardBlockResult = { block: true; reason: string };

/**
 * A postflight transform the server applied to the tool call's arguments. The
 * caller replaces the tool input with this redacted/sanitized argument set
 * before the tool runs.
 */
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
 * message naming the Grafana AI Observability source, the blocked tool, and
 * the expected agent behavior.
 *
 * Mirrors `formatPolicyDeny` in the Go guard helper. Keep the wording aligned.
 */
function formatPolicyDeny(
  toolName: string,
  reason: string | undefined,
): string {
  let msg = `A Grafana AI Observability policy blocked the "${toolName}" tool call, so it was not run.`;
  const trimmed = reason?.trim();
  if (trimmed) {
    msg += ` Reason: ${trimmed}`;
  }
  return `${msg}\n\n${GUARD_BEHAVIOR_HINT}`;
}

/**
 * Fail-closed message used when the guard evaluation request fails. Explicitly
 * distinguishes the infrastructure failure from a policy decision.
 *
 * Mirrors `formatEvalFailure` in the Go guard helper. Keep the wording aligned.
 */
function formatEvalFailure(
  toolName: string,
  detail: string | undefined,
): string {
  let msg = `Sigil could not evaluate the Grafana AI Observability guard for the "${toolName}" tool call, so it was blocked as a safety measure.`;
  const trimmed = detail?.trim();
  if (trimmed) {
    msg += ` Details: ${trimmed}`;
  }
  return `${msg}\n\n${GUARD_BEHAVIOR_HINT}`;
}

/**
 * Evaluates the Sigil postflight hook for a tool call. Returns a block result
 * when the server denies the call, or a transform result when the server
 * redacted/sanitized the call's arguments. On transport/timeout/serialization
 * errors, returns `undefined` (allow) when `failOpen` is true and a block
 * result when `failOpen` is false.
 */
export async function runToolCallGuard(args: GuardArgs): Promise<GuardResult> {
  try {
    const req: HookEvaluateRequest = {
      phase: "postflight",
      context: {
        agentName: args.agentName,
        agentVersion: args.agentVersion,
        model: {
          provider: args.model.provider || "unknown",
          name: args.model.name || "unknown",
        },
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
                  inputJSON: JSON.stringify(args.input ?? {}),
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
 * Walks the server-returned `transformed_input.output` for the tool_call part
 * matching `toolCallId` and parses its `inputJSON` into an object. Returns
 * `undefined` on any mismatch or parse failure so the caller falls through to
 * the original tool input unchanged.
 *
 * Mirrors `extractToolCallTransform` in `plugins/pi/src/guard.ts`; keep the two
 * aligned so both plugins consume the server transform identically.
 */
function extractToolCallTransform(
  output: Message[] | undefined,
  toolCallId: string | undefined,
  logger?: { warn: (msg: string) => void },
): Record<string, unknown> | undefined {
  if (!output || output.length === 0 || !toolCallId) return undefined;
  for (const msg of output) {
    if (!msg.parts) continue;
    for (const part of msg.parts) {
      if (part.type !== "tool_call") continue;
      const tc = part.toolCall;
      if (!tc || tc.id !== toolCallId) continue;
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
          // Extraction only — the caller logs whether the transform was
          // actually applied, so a parse here is never mistaken for success.
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
  logger?.warn(`tool-call transform present but no part matched ${toolCallId}`);
  return undefined;
}
