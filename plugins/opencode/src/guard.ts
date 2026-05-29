import type { HookEvaluateRequest, SigilClient } from "@grafana/sigil-sdk-js";

export interface GuardArgs {
  client: SigilClient;
  agentName: string;
  agentVersion?: string;
  model: { provider: string; name: string };
  toolCallId?: string;
  toolName: string;
  input: unknown;
  failOpen: boolean;
}

export type GuardBlockResult = { block: true; reason: string };

/**
 * Instructs the model how to react to a guard deny verdict, so the reason is
 * not mistaken for a generic tool failure to retry or work around. Appended
 * by both the policy-deny and fail-closed formatters.
 *
 * Mirrors `guardBehaviorHint` in `plugins/sigil/internal/agents/guard/toolcall.go`.
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
 * when the server denies the call. On transport/timeout/serialization errors,
 * returns `undefined` (allow) when `failOpen` is true and a block result when
 * `failOpen` is false.
 */
export async function runToolCallGuard(
  args: GuardArgs,
): Promise<GuardBlockResult | undefined> {
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
    return undefined;
  } catch (err) {
    if (!args.failOpen) {
      return {
        block: true,
        reason: formatEvalFailure(args.toolName, String(err)),
      };
    }
    return undefined;
  }
}
