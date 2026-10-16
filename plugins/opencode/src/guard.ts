import {
  type Agento11yClient,
  type HookEvaluateRequest,
  type HookInput,
  HookTransformError,
  type Message,
} from "@grafana/agento11y";

// Strict diagnostics omit payloads, identities and exception details: SDK
// transport errors may contain response bodies or credentials.
const STRICT_TRANSFORM_FAILURE =
  "agento11y could not apply a required guard transform, so the operation was blocked as a safety measure.";
const STRICT_EVALUATION_FAILURE =
  "agento11y could not evaluate the guard, so the operation was blocked as a safety measure.";
const STRICT_EVALUATION_FAIL_OPEN =
  "agento11y could not evaluate the guard; fail-open allowed the operation.";

export interface GuardArgs {
  client: Agento11yClient;
  agentName: string;
  agentVersion?: string;
  model: { provider: string; name: string };
  conversationId?: string;
  toolCallId?: string;
  toolName: string;
  input: unknown;
  failOpen: boolean;
  /** Native opt-in; omitted/false preserves legacy behavior. */
  strictTransforms?: boolean;
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
    "agento11y could not evaluate the Grafana Agent Observability guard and blocked this message. Set AGENTO11Y_GUARDS_FAIL_OPEN=true in your shell or in ~/.config/agento11y/config.env. Restart the agent and resend.";
  const trimmed = detail?.trim();
  if (trimmed) {
    msg += ` Details: ${trimmed}`;
  }
  return msg;
}

export interface PromptGuardArgs {
  /** Native opt-in; omitted/false preserves legacy behavior. */
  strictTransforms?: boolean;
  client: Agento11yClient;
  agentName: string;
  agentVersion?: string;
  model: { provider: string; name: string };
  conversationId?: string;
  messages: Message[];
  failOpen: boolean;
  logger?: { warn: (msg: string) => void };
}

export type PromptGuardResult = { block: true; reason: string } | undefined;

/**
 * Evaluates the prompt the user just submitted against the Agent Observability
 * preflight hook, and reports whether the turn must be refused.
 *
 * For OpenCode v1, this is the only point where a plugin can refuse a turn
 * cleanly.
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
  let evaluating = false;
  try {
    const conversationId = args.conversationId?.trim();
    const req: HookEvaluateRequest = {
      phase: "preflight",
      context: {
        agentName: args.agentName,
        agentVersion: args.agentVersion,
        ...(conversationId ? { conversationId } : {}),
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
    // catch below, where `args.failOpen` decides. Strict mode also refuses
    // local serialization failures rather than treating them as outages.
    if (args.strictTransforms) JSON.stringify(req);
    evaluating = true;
    const resp = await args.client.evaluateHook(req, {
      enabled: true,
      phases: ["preflight"],
      failOpen: false,
      ...(args.strictTransforms ? { strictTransforms: true } : {}),
    });
    evaluating = false;
    if (resp.action !== "deny") return undefined;

    // The local daemon's own fail-closed deny already explains itself.
    if (resp.ruleId === EVALUATION_FAILURE_RULE_ID) {
      return {
        block: true,
        reason: args.strictTransforms
          ? STRICT_EVALUATION_FAILURE
          : (resp.reason ?? formatPromptEvalFailure(undefined)),
      };
    }
    return { block: true, reason: formatPromptDeny(resp.reason) };
  } catch (err) {
    if (args.strictTransforms) {
      const invalid = err instanceof HookTransformError || !evaluating;
      if (!invalid && args.failOpen) {
        args.logger?.warn(STRICT_EVALUATION_FAIL_OPEN);
        return undefined;
      }
      args.logger?.warn(
        invalid ? STRICT_TRANSFORM_FAILURE : STRICT_EVALUATION_FAILURE,
      );
      return {
        block: true,
        reason: invalid ? STRICT_TRANSFORM_FAILURE : STRICT_EVALUATION_FAILURE,
      };
    }
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
  /** Native opt-in; omitted/false preserves legacy behavior. */
  strictTransforms?: boolean;
  client: Agento11yClient;
  agentName: string;
  agentVersion?: string;
  model: { provider: string; name: string };
  conversationId?: string;
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
 * false. For OpenCode v1, refusing here is not free: opencode has already written
 * the assistant message row by the time it dispatches this hook
 * (`session/prompt.ts`), and it finalizes that row through `Effect.onInterrupt`,
 * which a thrown error does not trigger. The caller asks opencode to abort the
 * session so that finalizer runs, then throws so the turn stops whether the abort
 * arrived or not.
 *
 * For OpenCode v1, `runPromptGuard` covers the same rules at `chat.message`, where
 * nothing is written yet, so most denies never reach this point. What reaches it
 * is a deny on text the user did not just type: conversation history, an assistant
 * string generated during the turn, or a compaction dispatch.
 *
 * Mirrors `runPreflightTransform` in `plugins/pi/src/guard.ts`; keep the two
 * request shapes aligned.
 */
export async function runPreflightTransform(
  args: PreflightTransformArgs,
): Promise<PreflightTransformResult> {
  let evaluating = false;
  try {
    const conversationId = args.conversationId?.trim();
    const req: HookEvaluateRequest = {
      phase: "preflight",
      context: {
        agentName: args.agentName,
        agentVersion: args.agentVersion,
        ...(conversationId ? { conversationId } : {}),
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
    // below, which applies the caller's fail-open policy. Strict mode always
    // blocks invalid transforms and local serialization failures.
    if (args.strictTransforms) JSON.stringify(req);
    evaluating = true;
    const resp = await args.client.evaluateHook(req, {
      enabled: true,
      phases: ["preflight"],
      failOpen: false,
      ...(args.strictTransforms ? { strictTransforms: true } : {}),
    });
    evaluating = false;

    if (resp.action === "deny") {
      // The local daemon's own fail-closed deny already explains itself.
      if (resp.ruleId === EVALUATION_FAILURE_RULE_ID) {
        return {
          block: args.strictTransforms
            ? STRICT_EVALUATION_FAILURE
            : (resp.reason ?? formatConversationEvalFailure(undefined)),
        };
      }
      return { block: formatConversationDeny(resp.reason) };
    }

    const transformed = resp.transformedInput?.messages;
    if (
      args.strictTransforms &&
      resp.transformedInput !== undefined &&
      (!onlyFields(resp.transformedInput, ["messages"]) ||
        !Array.isArray(transformed) ||
        transformed.length === 0)
    ) {
      args.logger?.warn(STRICT_TRANSFORM_FAILURE);
      return { block: STRICT_TRANSFORM_FAILURE };
    }
    if (!transformed || transformed.length === 0) {
      return undefined;
    }
    return { messages: transformed };
  } catch (err) {
    if (args.strictTransforms) {
      const invalid = err instanceof HookTransformError || !evaluating;
      if (!invalid && args.failOpen) {
        args.logger?.warn(STRICT_EVALUATION_FAIL_OPEN);
        return undefined;
      }
      args.logger?.warn(
        invalid ? STRICT_TRANSFORM_FAILURE : STRICT_EVALUATION_FAILURE,
      );
      return {
        block: invalid ? STRICT_TRANSFORM_FAILURE : STRICT_EVALUATION_FAILURE,
      };
    }
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
 * redacted/sanitized the call's arguments. On transport/timeout
 * errors, returns `undefined` (allow) when `failOpen` is true and a block
 * result when `failOpen` is false.
 * Local serialization errors follow `failOpen` unless `strictTransforms` is true;
 * strict mode blocks them.
 */
export async function runToolCallGuard(args: GuardArgs): Promise<GuardResult> {
  let evaluating = false;
  try {
    const conversationId = args.conversationId?.trim();
    const req: HookEvaluateRequest = {
      phase: "postflight",
      context: {
        agentName: args.agentName,
        agentVersion: args.agentVersion,
        ...(conversationId ? { conversationId } : {}),
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

    if (args.strictTransforms) JSON.stringify(req);
    evaluating = true;
    const resp = await args.client.evaluateHook(req, {
      enabled: true,
      ...(args.strictTransforms
        ? { strictTransforms: true, phases: ["postflight"], failOpen: false }
        : {}),
    });
    evaluating = false;
    if (resp.action === "deny") {
      // No rule produced this deny, and its reason already says so.
      if (resp.ruleId === EVALUATION_FAILURE_RULE_ID) {
        return {
          block: true,
          reason: args.strictTransforms
            ? STRICT_EVALUATION_FAILURE
            : (resp.reason ?? formatEvalFailure(args.toolName, undefined)),
        };
      }
      return {
        block: true,
        reason: formatPolicyDeny(args.toolName, resp.reason),
      };
    }
    if (args.strictTransforms && resp.transformedInput !== undefined) {
      const transform = strictToolCallTransform(
        resp.transformedInput,
        args.toolCallId,
        args.toolName,
      );
      if (transform) return { transform };
      args.logger?.warn(STRICT_TRANSFORM_FAILURE);
      return { block: true, reason: STRICT_TRANSFORM_FAILURE };
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
    if (args.strictTransforms) {
      const invalid = err instanceof HookTransformError || !evaluating;
      if (!invalid && args.failOpen) {
        args.logger?.warn(STRICT_EVALUATION_FAIL_OPEN);
        return undefined;
      }
      args.logger?.warn(
        invalid ? STRICT_TRANSFORM_FAILURE : STRICT_EVALUATION_FAILURE,
      );
      return {
        block: true,
        reason: invalid ? STRICT_TRANSFORM_FAILURE : STRICT_EVALUATION_FAILURE,
      };
    }
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

function strictToolCallTransform(
  input: HookInput,
  toolCallId: string | undefined,
  toolName: string,
): Record<string, unknown> | undefined {
  if (
    !onlyFields(input, ["output"]) ||
    !Array.isArray(input.output) ||
    input.output.length !== 1
  )
    return undefined;
  const message = input.output[0];
  if (
    !message ||
    !onlyFields(message, ["role", "parts"]) ||
    message.role !== "assistant" ||
    message.parts?.length !== 1
  )
    return undefined;
  const part = message.parts[0];
  if (
    !part ||
    !onlyFields(part, ["type", "toolCall"]) ||
    part.type !== "tool_call"
  )
    return undefined;
  const call = part.toolCall;
  if (!onlyFields(call, ["id", "name", "inputJSON"])) return undefined;
  const id = toolCallId?.trim();
  const name = toolName.trim();
  const candidateID = call.id?.trim();
  const candidateName = call.name?.trim();
  if (id && candidateID && id !== candidateID) return undefined;
  if (name && candidateName && !toolNamesEqual(candidateName, name))
    return undefined;
  if (
    !(id && candidateID === id) &&
    !(name && candidateName && toolNamesEqual(candidateName, name))
  )
    return undefined;
  return parseTransformArgs(call.inputJSON, "tool");
}

function onlyFields(value: unknown, fields: string[]): boolean {
  return (
    value !== null &&
    typeof value === "object" &&
    !Array.isArray(value) &&
    Object.keys(value).every((key) => fields.includes(key))
  );
}

/**
 * Walks the server-returned `transformed_input.output` for the tool_call part
 * matching `toolCallId` and parses its `inputJSON` into an object. A unique
 * exact ID takes precedence over names. Fallback requires one compatible
 * call: populated IDs and populated names must not conflict.
 * Returns `undefined` on any mismatch, ambiguity, or parse failure so the
 * caller falls through to the original tool input.
 *
 * Mirrors `extractToolCallTransform` in `plugins/pi/src/guard.ts`; keep the two
 * aligned so both plugins consume the server transform identically.
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
      // Extraction only — the caller logs whether the transform was
      // actually applied, so a parse here is never mistaken for success.
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
