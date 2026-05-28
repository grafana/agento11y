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
    if (resp.action === "deny") return denyResult(resp.reason);
    return undefined;
  } catch (err) {
    if (!args.failOpen) {
      return denyResult(`sigil guard evaluation failed: ${String(err)}`);
    }
    return undefined;
  }
}

export function denyResult(reason: string | undefined): GuardBlockResult {
  const trimmed = reason?.trim();
  return {
    block: true,
    reason:
      trimmed && trimmed.length > 0
        ? trimmed
        : "tool call denied by Sigil guard",
  };
}
