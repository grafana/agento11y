import type {
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
import { asError } from './utils.js';

const hooksEvaluatePath = '/api/v1/hooks:evaluate';
const hookTimeoutHeader = 'X-Sigil-Hook-Timeout-Ms';

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
    this.reason = normalized.length > 0 ? normalized : 'request blocked by Sigil hook rule';
    this.ruleId = ruleId;
    this.evaluations = evaluations;
  }
}

/**
 * Sends a hook evaluation request to the Sigil API.
 *
 * `apiEndpoint` is the Sigil API base URL (without the `/api/v1/...` suffix).
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
          `sigil hook evaluation failed: status ${response.status}: ${hookErrorText(responseText, response.status)}`,
        ),
      );
    }
    if (responseText.length === 0) {
      return failOpenOrThrow(params.hooks.failOpen, new Error('sigil hook evaluation failed: empty response payload'));
    }

    let payload: unknown;
    try {
      payload = JSON.parse(responseText);
    } catch (error) {
      return failOpenOrThrow(
        params.hooks.failOpen,
        new Error(`sigil hook evaluation failed: invalid JSON response: ${asError(error).message}`),
      );
    }

    return parseEvaluateResponse(payload);
  } catch (error) {
    return failOpenOrThrow(params.hooks.failOpen, asError(error));
  } finally {
    clearTimeout(timer);
  }
}

function failOpenOrThrow(failOpen: boolean, error: Error): HookEvaluateResponse {
  if (failOpen) {
    return allowResponse();
  }
  throw error;
}

function allowResponse(): HookEvaluateResponse {
  return { action: 'allow', evaluations: [] };
}

function formatDenyMessage(reason: string, ruleId: string | undefined): string {
  const trimmedReason = reason?.trim() ?? '';
  const baseReason = trimmedReason.length > 0 ? trimmedReason : 'request blocked by Sigil hook rule';
  if (ruleId !== undefined && ruleId.length > 0) {
    return `sigil hook denied by rule ${ruleId}: ${baseReason}`;
  }
  return `sigil hook denied: ${baseReason}`;
}

function buildHooksEvaluateEndpoint(endpoint: string, insecure: boolean): string {
  const baseURL = baseURLFromAPIEndpoint(endpoint, insecure);
  return `${baseURL}${hooksEvaluatePath}`;
}

function baseURLFromAPIEndpoint(endpoint: string, insecure: boolean): string {
  const trimmed = endpoint.trim();
  if (trimmed.length === 0) {
    throw new Error('sigil hook evaluation failed: api endpoint is required');
  }

  if (trimmed.startsWith('http://') || trimmed.startsWith('https://')) {
    const parsed = new URL(trimmed);
    // Preserve a path prefix so prefix-mounted Sigil deployments
    // (https://host/sigil) route /api/v1/hooks:evaluate under the prefix.
    const path = parsed.pathname.replace(/\/+$/, '');
    return `${parsed.protocol}//${parsed.host}${path}`;
  }

  const withoutScheme = trimmed.startsWith('grpc://') ? trimmed.slice('grpc://'.length) : trimmed;
  const host = withoutScheme.split('/')[0]?.trim();
  if (host === undefined || host.length === 0) {
    throw new Error('sigil hook evaluation failed: api endpoint host is required');
  }
  return `${insecure ? 'http' : 'https'}://${host}`;
}

function serializeRequest(request: HookEvaluateRequest): Record<string, unknown> {
  const body: Record<string, unknown> = {
    phase: request.phase,
    context: serializeContext(request.context),
    input: serializeInput(request.input),
  };
  return body;
}

function serializeContext(context: HookEvaluateRequest['context']): Record<string, unknown> {
  const out: Record<string, unknown> = {
    model: { provider: context.model.provider, name: context.model.name },
  };
  if (context.agentName !== undefined && context.agentName.length > 0) {
    out.agent_name = context.agentName;
  }
  if (context.agentVersion !== undefined && context.agentVersion.length > 0) {
    out.agent_version = context.agentVersion;
  }
  if (context.tags !== undefined && Object.keys(context.tags).length > 0) {
    out.tags = { ...context.tags };
  }
  return out;
}

function serializeInput(input: HookEvaluateRequest['input']): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  if (input.messages !== undefined && input.messages.length > 0) {
    out.messages = input.messages.map(serializeMessage);
  }
  if (input.tools !== undefined && input.tools.length > 0) {
    out.tools = input.tools.map(serializeToolDefinition);
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

function serializeMessage(message: Message): Record<string, unknown> {
  const out: Record<string, unknown> = { role: message.role };
  if (message.name !== undefined && message.name.length > 0) {
    out.name = message.name;
  }
  const parts = message.parts?.map(serializeMessagePart) ?? [];
  if (parts.length > 0) {
    out.parts = parts;
  } else if (message.content !== undefined && message.content.length > 0) {
    out.parts = [{ kind: 'text', text: message.content }];
  }
  return out;
}

function serializeMessagePart(part: MessagePart): Record<string, unknown> {
  const metadata = serializePartMetadata(part.metadata);
  switch (part.type) {
    case 'text':
      return maybeWithMetadata({ kind: 'text', text: part.text }, metadata);
    case 'thinking':
      return maybeWithMetadata({ kind: 'thinking', thinking: part.thinking }, metadata);
    case 'tool_call': {
      const toolCall: Record<string, unknown> = {
        name: part.toolCall.name,
      };
      if (part.toolCall.id !== undefined && part.toolCall.id.length > 0) {
        toolCall.id = part.toolCall.id;
      }
      if (part.toolCall.inputJSON !== undefined && part.toolCall.inputJSON.length > 0) {
        toolCall.input_json = jsonPayload(part.toolCall.inputJSON);
      }
      return maybeWithMetadata({ kind: 'tool_call', tool_call: toolCall }, metadata);
    }
    case 'tool_result': {
      const toolResult: Record<string, unknown> = {};
      if (part.toolResult.toolCallId !== undefined && part.toolResult.toolCallId.length > 0) {
        toolResult.tool_call_id = part.toolResult.toolCallId;
      }
      if (part.toolResult.name !== undefined && part.toolResult.name.length > 0) {
        toolResult.name = part.toolResult.name;
      }
      if (part.toolResult.content !== undefined && part.toolResult.content.length > 0) {
        toolResult.content = part.toolResult.content;
      }
      if (part.toolResult.contentJSON !== undefined && part.toolResult.contentJSON.length > 0) {
        toolResult.content_json = jsonPayload(part.toolResult.contentJSON);
      }
      if (part.toolResult.isError === true) {
        toolResult.is_error = true;
      }
      return maybeWithMetadata({ kind: 'tool_result', tool_result: toolResult }, metadata);
    }
  }
}

function serializeToolDefinition(tool: ToolDefinition): Record<string, unknown> {
  const out: Record<string, unknown> = { name: tool.name };
  if (tool.description !== undefined && tool.description.length > 0) {
    out.description = tool.description;
  }
  if (tool.type !== undefined && tool.type.length > 0) {
    out.type = tool.type;
  }
  if (tool.inputSchemaJSON !== undefined && tool.inputSchemaJSON.length > 0) {
    out.input_schema = jsonPayload(tool.inputSchemaJSON);
  }
  return out;
}

function serializePartMetadata(metadata: MessagePart['metadata']): Record<string, unknown> | undefined {
  if (metadata?.providerType !== undefined && metadata.providerType.length > 0) {
    return { provider_type: metadata.providerType };
  }
  return undefined;
}

function maybeWithMetadata(
  part: Record<string, unknown>,
  metadata: Record<string, unknown> | undefined,
): Record<string, unknown> {
  if (metadata !== undefined) {
    part.metadata = metadata;
  }
  return part;
}

function jsonPayload(value: string): unknown {
  try {
    return JSON.parse(value);
  } catch {
    return value;
  }
}

function parseEvaluateResponse(payload: unknown): HookEvaluateResponse {
  if (!isRecord(payload)) {
    throw new Error('sigil hook evaluation failed: invalid response payload');
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

/** Parses `transformed_input` from the Sigil API (SDK-shaped JSON and Go/proto JSON encodings). */
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
    out.inputSchemaJSON = maybeDecodeGoProtoJSONBytes(schemaKey);
  }
  return out;
}

function maybeDecodeGoProtoJSONBytes(value: string): string {
  if (!/^[A-Za-z0-9+/]+=*$/.test(value) || value.length < 4) {
    return value;
  }
  try {
    const g = globalThis as typeof globalThis & {
      Buffer?: { from(data: string, enc: string): { toString(enc: string): string } };
    };
    if (g.Buffer !== undefined) {
      const text = g.Buffer.from(value, 'base64').toString('utf8');
      if (text.length > 0) {
        return text;
      }
    }
  } catch {
    /* ignore */
  }
  return value;
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

function parseWireMessagePart(rec: Record<string, unknown>): MessagePart | undefined {
  const typ = rec.type;
  if (typ === 'text' && typeof rec.text === 'string') {
    return { type: 'text', text: rec.text };
  }
  if (typ === 'thinking' && typeof rec.thinking === 'string') {
    return { type: 'thinking', thinking: rec.thinking };
  }
  if (typ === 'tool_call' && isRecord(rec.toolCall)) {
    const tc = parseWireToolCallPart(rec.toolCall);
    return tc !== undefined ? { type: 'tool_call', toolCall: tc } : undefined;
  }
  if (typ === 'tool_result' && isRecord(rec.toolResult)) {
    const tr = parseWireToolResultPart(rec.toolResult);
    return tr !== undefined ? { type: 'tool_result', toolResult: tr } : undefined;
  }

  const payload = rec.Payload ?? rec.payload;
  if (isRecord(payload)) {
    if (typeof payload.Text === 'string') {
      return { type: 'text', text: payload.Text };
    }
    if (typeof payload.Thinking === 'string') {
      return { type: 'thinking', thinking: payload.Thinking };
    }
    if (isRecord(payload.ToolCall)) {
      const tc = parseWireToolCallPart(payload.ToolCall);
      return tc !== undefined ? { type: 'tool_call', toolCall: tc } : undefined;
    }
    if (isRecord(payload.ToolResult)) {
      const tr = parseWireToolResultPart(payload.ToolResult);
      return tr !== undefined ? { type: 'tool_result', toolResult: tr } : undefined;
    }
  }

  const kind = typeof rec.kind === 'string' ? rec.kind : '';
  if (kind === 'text' && typeof rec.text === 'string') {
    return { type: 'text', text: rec.text };
  }
  if (kind === 'thinking' && typeof rec.thinking === 'string') {
    return { type: 'thinking', thinking: rec.thinking };
  }
  if (kind === 'tool_call') {
    const rawTc = rec.tool_call ?? rec.toolCall;
    if (isRecord(rawTc)) {
      const tc = parseWireToolCallPart(rawTc);
      return tc !== undefined ? { type: 'tool_call', toolCall: tc } : undefined;
    }
  }
  if (kind === 'tool_result') {
    const rawTr = rec.tool_result ?? rec.toolResult;
    if (isRecord(rawTr)) {
      const tr = parseWireToolResultPart(rawTr);
      return tr !== undefined ? { type: 'tool_result', toolResult: tr } : undefined;
    }
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
    out.inputJSON = maybeDecodeGoProtoJSONBytes(rawInput);
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
    out.contentJSON = maybeDecodeGoProtoJSONBytes(rawCj);
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
