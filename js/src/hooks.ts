import { isSpanContextValid, context as otelContext, trace } from '@opentelemetry/api';
import { conversationIdFromContext } from './context.js';
import type {
  Agento11yLogger,
  HookEvaluateRequest,
  HookEvaluateResponse,
  HookEvaluation,
  HookInput,
  HooksConfig,
  Message,
  MessagePart,
  ToolCallPart,
  ToolDefinition,
  ToolResultPart,
} from './types.js';
import { asError, baseURLFromAPIEndpoint } from './utils.js';

const hooksEvaluatePath = '/api/v1/hooks:evaluate';
const hookTimeoutHeader = 'X-Agento11y-Hook-Timeout-Ms';

/**
 * Thrown by framework adapters when hook evaluation returns `action: 'deny'`.
 *
 * The error preserves the rule that triggered the deny and the per-rule
 * evaluation outcomes so callers can build user-facing error messages.
 */
export class HookDeniedError extends Error {
  readonly action = 'deny' as const;
  readonly reason: string;
  readonly ruleId?: string;
  readonly evaluations: HookEvaluation[];

  constructor(reason: string, ruleId?: string, evaluations: HookEvaluation[] = []) {
    super(formatDenyMessage(reason, ruleId));
    this.name = 'HookDeniedError';
    const normalized = reason?.trim() ?? '';
    this.reason = normalized.length > 0 ? normalized : 'request blocked by Agent Observability hook rule';
    this.ruleId = ruleId;
    this.evaluations = evaluations;
  }
}

/**
 * Sends a hook evaluation request to the Agent Observability API.
 *
 * `apiEndpoint` is the Agent Observability API base URL (without the `/api/v1/...` suffix).
 * `extraHeaders` is merged into the request — typically the same auth headers
 * the SDK uses for generation export.
 *
 * Returns `{ action: 'allow', evaluations: [] }` when the request fails and
 * `hooks.failOpen` is true.
 */
export async function evaluateHook(params: {
  apiEndpoint: string;
  insecure: boolean;
  extraHeaders: Record<string, string> | undefined;
  hooks: HooksConfig;
  request: HookEvaluateRequest;
  fetchImpl?: typeof fetch;
  /** Receives a warning whenever a failure is swallowed by `failOpen`. */
  logger?: Agento11yLogger;
}): Promise<HookEvaluateResponse> {
  const fetchImpl = params.fetchImpl ?? fetch;
  if (!params.hooks.enabled) {
    return allowResponse();
  }

  const phases = params.hooks.phases;
  if (phases.length > 0 && !phases.includes(params.request.phase)) {
    return allowResponse();
  }

  const timeoutMs = params.hooks.timeoutMs > 0 ? params.hooks.timeoutMs : 15_000;
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMs);

  try {
    const url = buildHooksEvaluateEndpoint(params.apiEndpoint, params.insecure);
    const body = serializeRequest(params.request);

    const response = await fetchImpl(url, {
      method: 'POST',
      signal: controller.signal,
      headers: {
        'content-type': 'application/json',
        [hookTimeoutHeader]: String(timeoutMs),
        ...(params.extraHeaders ?? {}),
      },
      body: JSON.stringify(body),
    });

    const responseText = (await response.text()).trim();
    if (!response.ok) {
      return failOpenOrThrow(
        params.hooks.failOpen,
        new Error(
          `agento11y hook evaluation failed: status ${response.status}: ${hookErrorText(responseText, response.status)}`,
        ),
        params.logger,
      );
    }
    if (responseText.length === 0) {
      return failOpenOrThrow(
        params.hooks.failOpen,
        new Error('agento11y hook evaluation failed: empty response payload'),
        params.logger,
      );
    }

    let payload: unknown;
    try {
      payload = JSON.parse(responseText);
    } catch (error) {
      return failOpenOrThrow(
        params.hooks.failOpen,
        new Error(`agento11y hook evaluation failed: invalid JSON response: ${asError(error).message}`),
        params.logger,
      );
    }

    return parseEvaluateResponse(payload);
  } catch (error) {
    return failOpenOrThrow(params.hooks.failOpen, hookTransportError(error), params.logger);
  } finally {
    clearTimeout(timer);
  }
}

/**
 * Labels every transport failure, including the `AbortError` a timeout raises, so
 * a fail-closed caller can tell a hook failure from any other thrown error. Go
 * wraps ErrHookTransportFailed and Python raises HookTransportError for the same
 * reason.
 */
function hookTransportError(error: unknown): Error {
  const converted = asError(error);
  if (converted.message.startsWith('agento11y hook evaluation failed')) {
    return converted;
  }
  const labelled = new Error(`agento11y hook evaluation failed: ${converted.message}`);
  labelled.cause = converted;
  return labelled;
}

function failOpenOrThrow(failOpen: boolean, error: Error, logger?: Agento11yLogger): HookEvaluateResponse {
  if (failOpen) {
    // Fail-open turns an evaluator outage into a silent allow, so record it.
    logger?.warn?.(`agento11y: hook evaluation failed, allowing request (failOpen): ${error.message}`);
    return allowResponse();
  }
  throw error;
}

function allowResponse(): HookEvaluateResponse {
  return { action: 'allow', evaluations: [] };
}

function formatDenyMessage(reason: string, ruleId: string | undefined): string {
  const trimmedReason = reason?.trim() ?? '';
  const baseReason = trimmedReason.length > 0 ? trimmedReason : 'request blocked by Agent Observability hook rule';
  if (ruleId !== undefined && ruleId.length > 0) {
    return `agento11y hook denied by rule ${ruleId}: ${baseReason}`;
  }
  return `agento11y hook denied: ${baseReason}`;
}

function buildHooksEvaluateEndpoint(endpoint: string, insecure: boolean): string {
  const baseURL = baseURLFromAPIEndpoint(endpoint, insecure, 'agento11y hook evaluation failed');
  return `${baseURL}${hooksEvaluatePath}`;
}

function serializeRequest(request: HookEvaluateRequest): Record<string, unknown> {
  const body: Record<string, unknown> = {
    phase: request.phase,
    context: serializeContext(request.context),
    input: serializeInput(request.input),
  };
  return body;
}

function serializeContext(hookContext: HookEvaluateRequest['context']): Record<string, unknown> {
  const out: Record<string, unknown> = {
    model: { provider: hookContext.model.provider, name: hookContext.model.name },
  };
  if (hookContext.agentName !== undefined && hookContext.agentName.length > 0) {
    out.agent_name = hookContext.agentName;
  }
  if (hookContext.agentVersion !== undefined && hookContext.agentVersion.length > 0) {
    out.agent_version = hookContext.agentVersion;
  }
  if (hookContext.tags !== undefined && Object.keys(hookContext.tags).length > 0) {
    out.tags = { ...hookContext.tags };
  }
  const conversationId = hookContext.conversationId ?? conversationIdFromContext();
  if (conversationId !== undefined && conversationId.length > 0) {
    out.conversation_id = conversationId;
  }
  const activeSpanContext = trace.getSpan(otelContext.active())?.spanContext();
  const spanContext =
    activeSpanContext !== undefined && isSpanContextValid(activeSpanContext) ? activeSpanContext : undefined;
  const traceId = hookContext.traceId ?? spanContext?.traceId;
  const spanId = hookContext.spanId ?? spanContext?.spanId;
  if (traceId !== undefined && traceId.length > 0) {
    out.trace_id = traceId;
  }
  if (spanId !== undefined && spanId.length > 0) {
    out.span_id = spanId;
  }
  return out;
}

function serializeInput(input: HookEvaluateRequest['input']): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  if (input.messages !== undefined && input.messages.length > 0) {
    out.messages = input.messages.map(serializeMessage);
  }
  if (input.tools !== undefined && input.tools.length > 0) {
    out.tools = input.tools.map(serializeTool);
  }
  if (input.systemPrompt !== undefined && input.systemPrompt.length > 0) {
    out.system_prompt = input.systemPrompt;
  }
  if (input.output !== undefined && input.output.length > 0) {
    out.output = input.output.map(serializeMessage);
  }
  if (input.conversationPreview !== undefined && input.conversationPreview.length > 0) {
    out.conversation_preview = input.conversationPreview;
  }
  return out;
}

/**
 * Maps an SDK message onto the hook wire shape.
 *
 * The public `MessagePart` union is camelCase and discriminated by `type`. The
 * server never reads `type`. It dispatches on a snake_case `kind`, and it
 * recovers a missing discriminator for text only.
 *
 * Passing an SDK object straight to `JSON.stringify` therefore sends every
 * thinking, tool-call, and tool-result part as an empty part. A tool-filter
 * guard sees no tool calls in it and allows the request. See
 * `conformance/hooks/README.md`.
 */
function serializeMessage(message: Message): Record<string, unknown> {
  const parts: Record<string, unknown>[] = [];
  for (const part of message.parts ?? []) {
    const serialized = serializeMessagePart(part);
    if (serialized !== undefined) {
      parts.push(serialized);
    }
  }
  // `content` is the text shorthand the SDK maps to a text part on export. Do
  // the same here so rules see the message body.
  if (parts.length === 0 && message.content !== undefined && message.content.length > 0) {
    parts.push({ kind: 'text', text: message.content });
  }
  const out: Record<string, unknown> = { role: message.role, parts };
  if (message.name !== undefined && message.name.length > 0) {
    out.name = message.name;
  }
  return out;
}

function serializeMessagePart(part: MessagePart): Record<string, unknown> | undefined {
  switch (part.type) {
    case 'text':
      return part.text.length > 0 ? { kind: 'text', text: part.text } : undefined;
    case 'thinking':
      return part.thinking.length > 0 ? { kind: 'thinking', thinking: part.thinking } : undefined;
    case 'tool_call': {
      const call: Record<string, unknown> = { name: part.toolCall.name };
      if (part.toolCall.id !== undefined && part.toolCall.id.length > 0) {
        call.id = part.toolCall.id;
      }
      if (part.toolCall.inputJSON !== undefined && part.toolCall.inputJSON.length > 0) {
        call.input_json = embeddedJSON(part.toolCall.inputJSON);
      }
      return { kind: 'tool_call', tool_call: call };
    }
    case 'tool_result': {
      const result: Record<string, unknown> = {};
      if (part.toolResult.toolCallId !== undefined && part.toolResult.toolCallId.length > 0) {
        result.tool_call_id = part.toolResult.toolCallId;
      }
      if (part.toolResult.name !== undefined && part.toolResult.name.length > 0) {
        result.name = part.toolResult.name;
      }
      if (part.toolResult.isError === true) {
        result.is_error = true;
      }
      if (part.toolResult.content !== undefined && part.toolResult.content.length > 0) {
        result.content = part.toolResult.content;
      }
      if (part.toolResult.contentJSON !== undefined && part.toolResult.contentJSON.length > 0) {
        result.content_json = embeddedJSON(part.toolResult.contentJSON);
      }
      return { kind: 'tool_result', tool_result: result };
    }
    default:
      return undefined;
  }
}

/**
 * Tool definitions travel base64 even though tool-call arguments do not: the
 * server decodes `input.tools` straight into its protobuf `ToolDefinition`,
 * whose `input_schema_json` is a bytes field. A schema under any other key is
 * ignored, and raw JSON under `input_schema_json` fails that decode, which makes
 * the server answer 400 for the whole evaluation.
 */
function serializeTool(tool: ToolDefinition): Record<string, unknown> {
  const out: Record<string, unknown> = { name: tool.name };
  if (tool.description !== undefined && tool.description.length > 0) {
    out.description = tool.description;
  }
  if (tool.type !== undefined && tool.type.length > 0) {
    out.type = tool.type;
  }
  if (tool.inputSchemaJSON !== undefined && tool.inputSchemaJSON.length > 0) {
    out.input_schema_json = base64FromUtf8(tool.inputSchemaJSON);
  }
  return out;
}

/**
 * Embeds a JSON payload the server reads as raw JSON. Text that does not parse
 * is sent as a JSON string, which keeps the body valid and leaves the value
 * visible to rules instead of dropping it. Go and Python do the same.
 */
function embeddedJSON(raw: string): unknown {
  try {
    return JSON.parse(raw);
  } catch {
    return raw;
  }
}

function base64FromUtf8(text: string): string {
  const g = globalThis as typeof globalThis & {
    Buffer?: { from(data: string, enc: string): { toString(enc: string): string } };
    btoa?: (data: string) => string;
  };
  if (g.Buffer !== undefined) {
    return g.Buffer.from(text, 'utf8').toString('base64');
  }
  const bytes = new TextEncoder().encode(text);
  let binary = '';
  for (const byte of bytes) {
    binary += String.fromCharCode(byte);
  }
  if (g.btoa !== undefined) {
    return g.btoa(binary);
  }
  throw new Error('agento11y hook evaluation failed: no base64 encoder available');
}

function parseEvaluateResponse(payload: unknown): HookEvaluateResponse {
  if (!isRecord(payload)) {
    throw new Error('agento11y hook evaluation failed: invalid response payload');
  }
  const action = payload.action === 'deny' ? 'deny' : 'allow';
  const ruleId = typeof payload.rule_id === 'string' ? payload.rule_id : undefined;
  const reason = typeof payload.reason === 'string' ? payload.reason : undefined;

  const rawEvaluations = Array.isArray(payload.evaluations) ? payload.evaluations : [];
  const evaluations: HookEvaluation[] = [];
  for (const entry of rawEvaluations) {
    if (!isRecord(entry)) {
      continue;
    }
    evaluations.push({
      ruleId: stringField(entry.rule_id),
      evaluatorId: stringField(entry.evaluator_id),
      evaluatorKind: stringField(entry.evaluator_kind),
      passed: entry.passed === true,
      latencyMs: numberField(entry.latency_ms),
      explanation: optionalStringField(entry.explanation),
      reason: optionalStringField(entry.reason),
    });
  }

  const transformedInput = parseTransformedInputPayload(payload);

  return { action, ruleId, reason, transformedInput, evaluations };
}

function parseTransformedInputPayload(payload: Record<string, unknown>): HookInput | undefined {
  const raw = payload.transformed_input;
  if (raw === undefined || raw === null) {
    return undefined;
  }
  if (!isRecord(raw)) {
    return undefined;
  }
  return parseHookInputWire(raw);
}

/** Parses `transformed_input` from the Agent Observability API (SDK-shaped JSON and Go/proto JSON encodings). */
function parseHookInputWire(raw: Record<string, unknown>): HookInput | undefined {
  const out: HookInput = {};
  const msgs = parseWireMessages(raw.messages);
  if (msgs !== undefined && msgs.length > 0) {
    out.messages = msgs;
  }
  const tools = parseWireTools(raw.tools);
  if (tools !== undefined && tools.length > 0) {
    out.tools = tools;
  }
  const output = parseWireMessages(raw.output);
  if (output !== undefined && output.length > 0) {
    out.output = output;
  }
  const sp = raw.system_prompt ?? raw.systemPrompt;
  if (typeof sp === 'string' && sp.length > 0) {
    out.systemPrompt = sp;
  }
  const cp = raw.conversation_preview ?? raw.conversationPreview;
  if (typeof cp === 'string' && cp.length > 0) {
    out.conversationPreview = cp;
  }
  if (
    out.messages === undefined &&
    out.tools === undefined &&
    out.systemPrompt === undefined &&
    out.output === undefined &&
    out.conversationPreview === undefined
  ) {
    return undefined;
  }
  return out;
}

function parseWireTools(raw: unknown): ToolDefinition[] | undefined {
  if (!Array.isArray(raw) || raw.length === 0) {
    return undefined;
  }
  const out: ToolDefinition[] = [];
  for (const item of raw) {
    if (!isRecord(item)) {
      continue;
    }
    const td = parseWireToolDefinition(item);
    if (td !== undefined) {
      out.push(td);
    }
  }
  return out.length > 0 ? out : undefined;
}

function parseWireToolDefinition(rec: Record<string, unknown>): ToolDefinition | undefined {
  const name = typeof rec.name === 'string' ? rec.name : '';
  if (name.length === 0) {
    return undefined;
  }
  const out: ToolDefinition = { name };
  if (typeof rec.description === 'string') {
    out.description = rec.description;
  }
  if (typeof rec.type === 'string') {
    out.type = rec.type;
  }
  const schemaKey = rec.input_schema_json ?? rec.inputSchemaJson ?? rec.inputSchemaJSON;
  if (typeof schemaKey === 'string' && schemaKey.length > 0) {
    out.inputSchemaJSON = decodeWireJSONPayload(schemaKey);
  }
  return out;
}

/**
 * Recovers a response-side JSON payload. The server marshals the protobuf bytes
 * fields `input_json`, `content_json`, and `input_schema_json` with Go's
 * `encoding/json`, so they arrive base64-encoded inside a JSON string.
 *
 * The result is always a JSON document: base64 that decodes to something else,
 * and a string that is neither base64 nor JSON, are kept as a JSON string so the
 * text survives. Go and Python apply the same rule; see
 * `conformance/hooks/README.md`.
 */
function decodeWireJSONPayload(value: string): string {
  const decoded = base64ToUtf8(value);
  if (decoded !== undefined) {
    return isJSONDocument(decoded) ? decoded : JSON.stringify(decoded);
  }
  if (isJSONDocument(value)) {
    return value;
  }
  return JSON.stringify(value);
}

function isJSONDocument(text: string): boolean {
  try {
    JSON.parse(text);
    return true;
  } catch {
    return false;
  }
}

/** Decodes strict base64, matching Go's `StdEncoding` and Python's `validate=True`. */
function base64ToUtf8(value: string): string | undefined {
  if (value.length === 0 || value.length % 4 !== 0 || !/^[A-Za-z0-9+/]+={0,2}$/.test(value)) {
    return undefined;
  }
  const g = globalThis as typeof globalThis & {
    Buffer?: { from(data: string, enc: string): { toString(enc: string): string } };
    atob?: (data: string) => string;
  };
  try {
    if (g.Buffer !== undefined) {
      return g.Buffer.from(value, 'base64').toString('utf8');
    }
    if (g.atob !== undefined) {
      const binary = g.atob(value);
      const bytes = Uint8Array.from(binary, (char) => char.charCodeAt(0));
      return new TextDecoder().decode(bytes);
    }
  } catch {
    return undefined;
  }
  return undefined;
}

function parseWireMessages(raw: unknown): Message[] | undefined {
  if (!Array.isArray(raw) || raw.length === 0) {
    return undefined;
  }
  const out: Message[] = [];
  for (const item of raw) {
    if (!isRecord(item)) {
      continue;
    }
    const m = parseWireMessage(item);
    if (m !== undefined) {
      out.push(m);
    }
  }
  return out.length > 0 ? out : undefined;
}

function parseWireMessage(rec: Record<string, unknown>): Message | undefined {
  const role = wireRoleToSdk(rec.role);
  const partsRaw = rec.parts;
  const parts: MessagePart[] = [];
  if (Array.isArray(partsRaw)) {
    for (const p of partsRaw) {
      if (!isRecord(p)) {
        continue;
      }
      const part = parseWireMessagePart(p);
      if (part !== undefined) {
        parts.push(part);
      }
    }
  }
  const name = typeof rec.name === 'string' ? rec.name : undefined;
  const content = typeof rec.content === 'string' ? rec.content : undefined;
  return {
    role,
    ...(name !== undefined ? { name } : {}),
    ...(content !== undefined ? { content } : {}),
    ...(parts.length > 0 ? { parts } : {}),
  };
}

function wireRoleToSdk(role: unknown): string {
  if (typeof role === 'string') {
    return role.toLowerCase();
  }
  if (role === 1) {
    return 'user';
  }
  if (role === 2) {
    return 'assistant';
  }
  if (role === 3) {
    return 'tool';
  }
  return 'user';
}

/** The part kinds the server dispatches on, and the only ones this SDK can hold. */
type WirePartKind = 'text' | 'thinking' | 'tool_call' | 'tool_result';

/** The payload fields of one wire part, read out of whichever shape carried them. */
interface WirePartFields {
  text?: string;
  thinking?: string;
  toolCall?: Record<string, unknown>;
  toolResult?: Record<string, unknown>;
}

function parseWireMessagePart(rec: Record<string, unknown>): MessagePart | undefined {
  const fields = wirePartFields(rec);
  const kind = wirePartKind(rec, fields);
  if (kind === undefined) {
    return undefined;
  }
  switch (kind) {
    case 'text':
      return fields.text !== undefined ? { type: 'text', text: fields.text } : undefined;
    case 'thinking':
      return fields.thinking !== undefined ? { type: 'thinking', thinking: fields.thinking } : undefined;
    case 'tool_call': {
      if (fields.toolCall === undefined) {
        return undefined;
      }
      const tc = parseWireToolCallPart(fields.toolCall);
      return tc !== undefined ? { type: 'tool_call', toolCall: tc } : undefined;
    }
    case 'tool_result': {
      if (fields.toolResult === undefined) {
        return undefined;
      }
      const tr = parseWireToolResultPart(fields.toolResult);
      return tr !== undefined ? { type: 'tool_result', toolResult: tr } : undefined;
    }
  }
}

/**
 * Reads the payload fields of one wire part.
 *
 * Three shapes reach this parser: the server's snake_case body, an echo of this
 * SDK's own camelCase part, and a protobuf `Part` marshalled with Go's
 * `encoding/json`, which nests the oneof under a capitalized `Payload` object. A
 * `Payload` that holds none of the four fields is ignored rather than treated as
 * an empty part, so the top-level fields still get their chance.
 */
function wirePartFields(rec: Record<string, unknown>): WirePartFields {
  const payload = rec.Payload ?? rec.payload;
  if (isRecord(payload)) {
    const nested = collectWirePartFields(payload.Text, payload.Thinking, payload.ToolCall, payload.ToolResult);
    if (Object.keys(nested).length > 0) {
      return nested;
    }
  }
  return collectWirePartFields(
    rec.text,
    rec.thinking,
    rec.tool_call ?? rec.toolCall,
    rec.tool_result ?? rec.toolResult,
  );
}

function collectWirePartFields(
  rawText: unknown,
  rawThinking: unknown,
  rawToolCall: unknown,
  rawToolResult: unknown,
): WirePartFields {
  const text = optionalStringField(rawText);
  const thinking = optionalStringField(rawThinking);
  return {
    ...(text !== undefined ? { text } : {}),
    ...(thinking !== undefined ? { thinking } : {}),
    ...(isRecord(rawToolCall) ? { toolCall: rawToolCall } : {}),
    ...(isRecord(rawToolResult) ? { toolResult: rawToolResult } : {}),
  };
}

/**
 * Resolves the kind to read a part as, and commits to it. A part that names its
 * kind is never rebuilt from another kind's field, so a `tool_call` that arrives
 * without its payload object is dropped rather than turned into the text that
 * happens to sit next to it. Go and Python resolve the kind the same way; see
 * `conformance/hooks/README.md`.
 */
function wirePartKind(rec: Record<string, unknown>, fields: WirePartFields): WirePartKind | undefined {
  const declared = optionalStringField(rec.kind) ?? optionalStringField(rec.type);
  if (declared !== undefined) {
    switch (declared) {
      case 'text':
      case 'thinking':
      case 'tool_call':
      case 'tool_result':
        return declared;
      default:
        // A kind this SDK does not know, which the server can only have sent as
        // text, so text is all there is to recover from it.
        return 'text';
    }
  }
  // A part without a discriminator is still recoverable from whichever payload
  // field is set, and dropping it would lose a transform the caller was asked to
  // apply. The server always sets `kind`, so this only covers a hand-written,
  // proto-JSON, or Go-marshalled body. Go and Python keep the same tolerance.
  if (fields.toolCall !== undefined) {
    return 'tool_call';
  }
  if (fields.toolResult !== undefined) {
    return 'tool_result';
  }
  if (fields.thinking !== undefined) {
    return 'thinking';
  }
  if (fields.text !== undefined) {
    return 'text';
  }
  return undefined;
}

function parseWireToolCallPart(rec: Record<string, unknown>): ToolCallPart | undefined {
  const name = typeof rec.name === 'string' ? rec.name : '';
  if (name.length === 0) {
    return undefined;
  }
  const out: ToolCallPart = { name };
  if (typeof rec.id === 'string') {
    out.id = rec.id;
  }
  const rawInput = rec.input_json ?? rec.inputJson ?? rec.inputJSON;
  if (typeof rawInput === 'string' && rawInput.length > 0) {
    out.inputJSON = decodeWireJSONPayload(rawInput);
  } else if (isRecord(rawInput) || Array.isArray(rawInput)) {
    out.inputJSON = JSON.stringify(rawInput);
  }
  return out;
}

function parseWireToolResultPart(rec: Record<string, unknown>): ToolResultPart | undefined {
  const out: ToolResultPart = {};
  if (typeof rec.tool_call_id === 'string') {
    out.toolCallId = rec.tool_call_id;
  } else if (typeof rec.toolCallId === 'string') {
    out.toolCallId = rec.toolCallId;
  }
  if (typeof rec.name === 'string') {
    out.name = rec.name;
  }
  if (typeof rec.content === 'string') {
    out.content = rec.content;
  }
  const rawCj = rec.content_json ?? rec.contentJson ?? rec.contentJSON;
  if (typeof rawCj === 'string' && rawCj.length > 0) {
    out.contentJSON = decodeWireJSONPayload(rawCj);
  } else if (isRecord(rawCj) || Array.isArray(rawCj)) {
    out.contentJSON = JSON.stringify(rawCj);
  }
  if (rec.is_error === true || rec.isError === true) {
    out.isError = true;
  }
  if (
    out.toolCallId === undefined &&
    out.name === undefined &&
    out.content === undefined &&
    out.contentJSON === undefined &&
    out.isError === undefined
  ) {
    return undefined;
  }
  return out;
}

function stringField(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

function optionalStringField(value: unknown): string | undefined {
  if (typeof value !== 'string' || value.length === 0) {
    return undefined;
  }
  return value;
}

function numberField(value: unknown): number {
  if (typeof value === 'number' && Number.isFinite(value)) {
    return value;
  }
  return 0;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function hookErrorText(body: string, status: number): string {
  if (body.length > 0) {
    return body;
  }
  return `HTTP ${status}`;
}
