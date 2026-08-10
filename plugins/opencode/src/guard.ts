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
 * `plugins/pi/src/guard.ts`. Keep the three in sync.
 */
const EVALUATION_FAILURE_RULE_ID = "__agento11y_guard_evaluation_failure";

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
  let msg = `agento11y could not evaluate the Grafana Agent Observability guard for the "${toolName}" tool call, so it was blocked as a safety measure.`;
  const trimmed = detail?.trim();
  if (trimmed) {
    msg += ` Details: ${trimmed}`;
  }
  return `${msg}\n\n${GUARD_BEHAVIOR_HINT}`;
}

/**
 * Wraps a rule-authored reason for a denied prompt. Addressed to the user, not
 * the model: this verdict is delivered as a TUI toast and as the error that
 * aborts the turn, so it must not carry `GUARD_BEHAVIOR_HINT`, which instructs
 * an agent that is never reached.
 */
function formatPromptDeny(reason: string | undefined): string {
  let msg =
    "A Grafana Agent Observability policy blocked this message, so it was not sent to the model.";
  const trimmed = reason?.trim();
  if (trimmed) {
    msg += ` Reason: ${trimmed}`;
  }
  return msg;
}

/** Fail-closed message for a prompt whose guard evaluation could not run. */
function formatPromptEvalFailure(detail: string | undefined): string {
  let msg =
    "agento11y could not evaluate the Grafana Agent Observability guard for this message, so it was blocked as a safety measure. Set AGENTO11Y_GUARDS_FAIL_OPEN=true to send it anyway.";
  const trimmed = detail?.trim();
  if (trimmed) {
    msg += ` Details: ${trimmed}`;
  }
  return msg;
}

export interface PromptGuardArgs {
  client: Agento11yClient;
  agentName: string;
  agentVersion?: string;
  model: { provider: string; name: string };
  messages: Message[];
  failOpen: boolean;
  logger?: { warn: (msg: string) => void };
}

export type PromptGuardResult = { block: true; reason: string } | undefined;

/**
 * Evaluates the prompt the user just submitted against the Agent Observability
 * preflight hook, and reports whether the turn must be refused.
 *
 * This is the only point where opencode lets a plugin refuse a turn cleanly.
 * `chat.message` fires before the user message and its parts are persisted and
 * before the step loop starts (`session/prompt.ts`), so throwing from it stops
 * the turn without a provider call and without leaving a half-written assistant
 * message behind. `experimental.chat.messages.transform` runs after the
 * assistant message row exists, which is why the deny is enforced here and only
 * reported there. `runPreflightTransform` documents that side.
 *
 * A transform in the response is ignored on purpose. These parts are about to be
 * written to opencode's message store, so rewriting them would edit the user's
 * own saved message rather than what the provider receives. Redaction stays with
 * `runPreflightTransform`, which rewrites the provider-bound copy only.
 *
 * `failOpen` governs an evaluation that could not run, matching the postflight
 * tool-call path. A deny is always enforced.
 */
export async function runPromptGuard(
  args: PromptGuardArgs,
): Promise<PromptGuardResult> {
  try {
    const req: HookEvaluateRequest = {
      phase: "preflight",
      context: {
        agentName: args.agentName,
        agentVersion: args.agentVersion,
        model: {
          provider: args.model.provider || "unknown",
          name: args.model.name || "unknown",
        },
      },
      input: {
        messages: args.messages,
      },
    };

    // The phase override and `failOpen: false` are required for the same
    // reasons as in `runPreflightTransform`: the client pins `hooks.phases` to
    // `["postflight"]`, and the SDK's own fail-open would return a synthetic
    // `allow` that cannot be told from a real one. Errors are routed to the
    // catch below, where `args.failOpen` decides.
    const resp = await args.client.evaluateHook(req, {
      enabled: true,
      phases: ["preflight"],
      failOpen: false,
    });
    if (resp.action !== "deny") return undefined;

    // The local daemon's own fail-closed deny already explains itself.
    if (resp.ruleId === EVALUATION_FAILURE_RULE_ID) {
      return {
        block: true,
        reason: resp.reason ?? formatPromptEvalFailure(undefined),
      };
    }
    return { block: true, reason: formatPromptDeny(resp.reason) };
  } catch (err) {
    args.logger?.warn(`prompt guard eval failed: ${err}`);
    if (args.failOpen) return undefined;
    return { block: true, reason: formatPromptEvalFailure(String(err)) };
  }
}

/** Wraps a rule-authored reason for a denied conversation, for the user. */
function formatConversationDeny(reason: string | undefined): string {
  let msg =
    "A Grafana Agent Observability policy blocked this conversation, so the turn was stopped before it reached the model.";
  const trimmed = reason?.trim();
  if (trimmed) {
    msg += ` Reason: ${trimmed}`;
  }
  return msg;
}

/** Fail-closed message for a conversation whose evaluation could not run. */
function formatConversationEvalFailure(detail: string | undefined): string {
  let msg =
    "agento11y could not evaluate the Grafana Agent Observability guard for this conversation, so the turn was stopped as a safety measure. Set AGENTO11Y_GUARDS_FAIL_OPEN=true to let it through instead.";
  const trimmed = detail?.trim();
  if (trimmed) {
    msg += ` Details: ${trimmed}`;
  }
  return msg;
}

export interface PreflightTransformArgs {
  client: Agento11yClient;
  agentName: string;
  agentVersion?: string;
  model: { provider: string; name: string };
  messages: Message[];
  failOpen: boolean;
  logger?: { warn: (msg: string) => void };
}

export type PreflightTransformResult =
  | { messages?: Message[]; block?: string }
  | undefined;

/**
 * Evaluates the Agent Observability preflight hook against the outgoing
 * conversation. Returns the redacted messages from
 * `transformedInput.messages`, a reason to refuse the turn, or `undefined` when
 * there is nothing to do.
 *
 * A deny blocks, and so does an evaluation that could not run when `failOpen` is
 * false. Refusing here is not free: opencode has already written the assistant
 * message row by the time it dispatches this hook (`session/prompt.ts`), and it
 * finalizes that row through `Effect.onInterrupt`, which a thrown error does not
 * trigger. The caller asks opencode to abort the session so that finalizer runs,
 * then throws so the turn stops whether the abort arrived or not.
 *
 * `runPromptGuard` covers the same rules at `chat.message`, where nothing is
 * written yet, so most denies never reach this point. What reaches it is a deny
 * on text the user did not just type: conversation history, an assistant string
 * generated during the turn, or a compaction dispatch.
 *
 * Mirrors `runPreflightTransform` in `plugins/pi/src/guard.ts`; keep the two
 * request shapes aligned.
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
        model: {
          provider: args.model.provider || "unknown",
          name: args.model.name || "unknown",
        },
      },
      input: {
        messages: args.messages,
      },
    };

    // `createAgento11yClient` pins `hooks.phases` to `["postflight"]`, and
    // `evaluateHook` replaces that list wholesale, so without the override the
    // SDK answers `allow` without calling the server.
    //
    // `failOpen: false` makes a failure visible, not fatal. The SDK default
    // turns every error and timeout into a synthetic `allow`, which this
    // function cannot tell from a real one. Throwing routes them to the catch
    // below, which logs and returns `undefined`, so preflight still fails open.
    const resp = await args.client.evaluateHook(req, {
      enabled: true,
      phases: ["preflight"],
      failOpen: false,
    });

    if (resp.action === "deny") {
      // The local daemon's own fail-closed deny already explains itself.
      if (resp.ruleId === EVALUATION_FAILURE_RULE_ID) {
        return {
          block: resp.reason ?? formatConversationEvalFailure(undefined),
        };
      }
      return { block: formatConversationDeny(resp.reason) };
    }

    const transformed = resp.transformedInput?.messages;
    if (!transformed || transformed.length === 0) {
      return undefined;
    }
    return { messages: transformed };
  } catch (err) {
    args.logger?.warn(`preflight transform eval failed: ${err}`);
    if (args.failOpen) return undefined;
    return { block: formatConversationEvalFailure(String(err)) };
  }
}

/**
 * Fail-closed message used when the server's redacted arguments cannot be
 * written into the arguments opencode runs the tool with. Says the block came
 * from the plugin rather than from a rule: the server allowed the call, and
 * only the local rewrite failed.
 *
 * No Go counterpart: the shared binary returns the redacted arguments to its
 * host instead of writing them into a live object, so it has no such failure.
 */
export function formatTransformFailure(
  toolName: string,
  detail: string | undefined,
): string {
  let msg = `agento11y could not apply the redacted arguments that a Grafana Agent Observability guard returned for the "${toolName}" tool call, so it was blocked as a safety measure.`;
  const trimmed = detail?.trim();
  if (trimmed) {
    msg += ` Details: ${trimmed}`;
  }
  return `${msg}\n\n${GUARD_BEHAVIOR_HINT}`;
}

/**
 * Evaluates the Agent Observability postflight hook for a tool call. Returns a block result
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
