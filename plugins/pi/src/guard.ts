import type {
  Agento11yClient,
  HookEvaluateRequest,
  HookInput,
  Message,
} from "@grafana/agento11y";

export interface GuardArgs {
  client: Agento11yClient;
  agentName: string;
  agentVersion?: string;
  model: { provider: string; name: string };
  conversationId?: string;
  toolCallId: string;
  toolName: string;
  input: Record<string, unknown>;
  tags?: Record<string, string>;
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
 * Marks a deny that reports a failed guard evaluation rather than a policy
 * decision. The local daemon returns it when its chained Cloud hook call fails
 * and `GUARDS_FAIL_OPEN` is false, and its reason already explains that, so it
 * must not be wrapped by `formatPolicyDeny`.
 *
 * Mirrors `EvaluationFailureRuleID` in
 * `plugins/agento11y/internal/agents/guard/toolcall.go` and the same constant in
 * `plugins/opencode/src/guard.ts`. Keep the three in sync.
 */
const EVALUATION_FAILURE_RULE_ID = "__agento11y_guard_evaluation_failure";

/**
 * Fail-closed message used when the guard could not be evaluated (transport
 * failure or, on the Go side, missing credentials). Explicitly distinguishes
 * the infrastructure failure from a policy decision.
 *
 * Mirrors `formatEvalFailure` in the Go guard helper. Keep the wording aligned.
 */
export function formatEvalFailure(
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
  conversationId?: string;
  messages: Message[];
  signal?: AbortSignal;
  tags?: Record<string, string>;
  logger?: { warn: (msg: string) => void };
}

export type PreflightEvaluation =
  | { kind: "allow" }
  | { kind: "transform"; input: HookInput & { messages: Message[] } }
  | { kind: "deny" }
  | { kind: "invalid" }
  | { kind: "failure" };

export async function evaluatePreflight(
  args: PreflightTransformArgs,
): Promise<PreflightEvaluation> {
  try {
    const conversationId = args.conversationId?.trim();
    const response = await args.client.evaluateHook(
      {
        phase: "preflight",
        context: {
          agentName: args.agentName,
          agentVersion: args.agentVersion,
          ...(conversationId ? { conversationId } : {}),
          model: args.model,
          tags: args.tags,
        },
        input: { messages: args.messages },
      },
      {
        enabled: true,
        phases: ["preflight"],
        failOpen: false,
        strictTransforms: true,
        ...(args.signal ? { signal: args.signal } : {}),
      },
    );
    if (response.action === "deny")
      return {
        kind:
          response.ruleId === EVALUATION_FAILURE_RULE_ID ? "failure" : "deny",
      };
    if (!response.transformedInput) return { kind: "allow" };
    if (
      Object.keys(response.transformedInput).some(
        (key) => key !== "messages",
      ) ||
      !Array.isArray(response.transformedInput.messages) ||
      response.transformedInput.messages.length === 0
    )
      return { kind: "invalid" };
    return {
      kind: "transform",
      input: { messages: response.transformedInput.messages },
    };
  } catch (error) {
    return {
      kind:
        error instanceof Error && error.name === "HookTransformError"
          ? "invalid"
          : "failure",
    };
  }
}

/**
 * Evaluates the Agent Observability postflight hook for a tool call. Returns a block result
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
    const conversationId = args.conversationId?.trim();
    const req: HookEvaluateRequest = {
      phase: "postflight",
      context: {
        agentName: args.agentName,
        agentVersion: args.agentVersion,
        ...(conversationId ? { conversationId } : {}),
        model: args.model,
        tags: args.tags,
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
      // No rule produced this deny, and its reason already says so.
      if (resp.ruleId === EVALUATION_FAILURE_RULE_ID) {
        return {
          block: true,
          reason: resp.reason ?? formatEvalFailure(args.toolName, undefined),
        };
      }
      return {
        block: true,
        reason: formatPolicyDeny(args.toolName, resp.reason),
      };
    }
    const transform = extractToolCallTransform(
      resp.transformedInput?.output,
      args.toolCallId,
      args.toolName,
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
 * A unique exact ID takes precedence over names. Fallback requires one
 * compatible call: populated IDs and populated names must not conflict.
 * Returns `undefined` on any mismatch, ambiguity, or parse failure so the
 * caller can fall through to the original tool input.
 */
function extractToolCallTransform(
  output: Message[] | undefined,
  toolCallId: string | undefined,
  toolName: string | undefined,
  logger?: { warn: (msg: string) => void },
): Record<string, unknown> | undefined {
  if (!output || output.length === 0) return undefined;

  const all: { id?: string; name?: string; raw: unknown }[] = [];
  for (const msg of output) {
    if (!msg.parts) continue;
    for (const part of msg.parts) {
      if (part.type !== "tool_call") continue;
      const tc = part.toolCall;
      if (!tc) continue;
      all.push({ id: tc.id?.trim(), name: tc.name?.trim(), raw: tc.inputJSON });
    }
  }
  if (all.length === 0) return undefined;

  const id = toolCallId?.trim() ?? "";
  const name = toolName?.trim() ?? "";
  let matched = id ? all.filter((c) => c.id === id) : [];
  if (matched.length === 0) {
    matched = all.filter(
      (c) =>
        (!id || !c.id || c.id === id) &&
        (!name || !c.name || toolNamesEqual(c.name, name)),
    );
  }

  const label = id || name || "tool";
  const only = matched.length === 1 ? matched[0] : undefined;
  if (only) return parseTransformArgs(only.raw, label, logger);

  logger?.warn(`tool-call transform present but no part matched ${label}`);
  return undefined;
}

/** Same contract as Go `strings.EqualFold(strings.TrimSpace(a), b)`. */
function toolNamesEqual(candidate: string | undefined, want: string): boolean {
  return (
    (candidate ?? "").trim().localeCompare(want, undefined, {
      sensitivity: "accent",
    }) === 0
  );
}

function parseTransformArgs(
  raw: unknown,
  label: string,
  logger?: { warn: (msg: string) => void },
): Record<string, unknown> | undefined {
  if (typeof raw !== "string" || raw.length === 0) {
    logger?.warn(`tool-call transform for ${label} dropped: empty arguments`);
    return undefined;
  }
  try {
    const parsed = JSON.parse(raw) as unknown;
    if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
      logger?.warn(`tool-call transform for ${label} applied`);
      return parsed as Record<string, unknown>;
    }
    logger?.warn(
      `tool-call transform for ${label} dropped: arguments were not a JSON object`,
    );
    return undefined;
  } catch {
    logger?.warn(
      `tool-call transform for ${label} dropped: invalid JSON arguments`,
    );
    return undefined;
  }
}
