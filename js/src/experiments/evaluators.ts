import type { TokenUsage } from '../types.js';
import type { Evaluator } from './models.js';

export const DEFAULT_LLM_JUDGE_PROMPT = `Grade the candidate output against the input and expected result.

Input:
{input}

Expected:
{expected}

Candidate output:
{output}

Return only JSON with this shape:
{"score": <number from 0 to 1>, "passed": <boolean>, "explanation": "<brief reason>"}
`;

/** A grader request and response that can be published as a generation. */
export interface GraderGeneration {
  input: string;
  output: string;
  modelProvider: string;
  modelName: string;
  agentName?: string;
  agentVersion?: string;
  operationName?: string;
  usage?: TokenUsage;
}

/** A local evaluator result ready to attach to a trial. */
export interface EvaluationResult {
  evaluator: Evaluator;
  value: number | boolean | string;
  passed: boolean;
  explanation?: string;
  scoreKey?: string;
  metadata?: Record<string, unknown>;
  grader?: GraderGeneration;
}

export interface EvaluateOutputInput {
  input: unknown;
  output: unknown;
  expected?: unknown;
}

/** Evaluator that scores caller-supplied output without a platform definition. */
export interface OutputEvaluator {
  readonly evaluator: Evaluator;
  evaluateOutput(input: EvaluateOutputInput): Promise<EvaluationResult> | EvaluationResult;
}

/** The parsed fields a judge response has to yield. */
export interface ParsedJudgeResult {
  score: number;
  passed: boolean;
  explanation: string;
}

export interface LLMJudgeOptions {
  evaluatorId: string;
  /** Receives the rendered prompt. May return a string or a provider response object. */
  invoke: (prompt: string) => Promise<unknown> | unknown;
  modelName: string;
  promptTemplate?: string;
  modelProvider?: string;
  version?: string;
  scoreKey?: string;
  passThreshold?: number;
  /** Replaces the default lenient JSON parsing. */
  parser?: (raw: string) => ParsedJudgeResult;
  /** Replaces the default token-usage extraction. */
  usageExtractor?: (response: unknown) => TokenUsage | undefined;
  agentName?: string;
  agentVersion?: string;
  operationName?: string;
}

/**
 * Provider-neutral LLM judge with structured-result parsing.
 *
 * `invoke` receives the rendered prompt and may return a string or an object with
 * a `content` field, which covers common provider and framework clients. No
 * platform evaluator or control-plane credential is required.
 */
export class LLMJudge implements OutputEvaluator {
  readonly evaluator: Evaluator;
  readonly scoreKey: string;
  readonly passThreshold: number;

  private readonly invoke: (prompt: string) => Promise<unknown> | unknown;
  private readonly modelName: string;
  private readonly modelProvider: string;
  private readonly promptTemplate: string;
  private readonly parser: ((raw: string) => ParsedJudgeResult) | undefined;
  private readonly usageExtractor: ((response: unknown) => TokenUsage | undefined) | undefined;
  private readonly agentName: string;
  private readonly agentVersion: string;
  private readonly operationName: string;

  constructor(options: LLMJudgeOptions) {
    if (options.evaluatorId.trim().length === 0) {
      throw new Error('evaluatorId is required');
    }
    if (typeof options.invoke !== 'function') {
      throw new Error('invoke must be callable');
    }
    if (options.modelName.trim().length === 0) {
      throw new Error('modelName is required');
    }
    const promptTemplate = options.promptTemplate ?? DEFAULT_LLM_JUDGE_PROMPT;
    if (promptTemplate.trim().length === 0) {
      throw new Error('promptTemplate is required');
    }
    const passThreshold = options.passThreshold ?? 0.5;
    if (!(passThreshold >= 0 && passThreshold <= 1)) {
      throw new Error('passThreshold must be between 0 and 1');
    }
    const version = options.version ?? '1';
    this.evaluator = { evaluatorId: options.evaluatorId, version, kind: 'llm_judge' };
    this.invoke = options.invoke;
    this.modelName = options.modelName;
    this.modelProvider = options.modelProvider ?? '';
    this.promptTemplate = promptTemplate;
    this.scoreKey = options.scoreKey ?? 'final';
    this.passThreshold = passThreshold;
    this.parser = options.parser;
    this.usageExtractor = options.usageExtractor;
    this.agentName = options.agentName ?? 'agento11y-llm-judge';
    this.agentVersion = options.agentVersion ?? version;
    this.operationName = options.operationName ?? 'llm-judge';
  }

  /** Grades explicit input and output values; it does not fetch a conversation. */
  async evaluateOutput(input: EvaluateOutputInput): Promise<EvaluationResult> {
    const prompt = renderPrompt(this.promptTemplate, input);
    const response = await this.invoke(prompt);
    const raw = responseText(response);
    const usage = this.usageExtractor !== undefined ? this.usageExtractor(response) : responseUsage(response);
    const parsed = this.parser !== undefined ? this.parser(raw) : this.parseDefault(raw);
    const metadata: Record<string, unknown> = { judge_model: this.modelName };
    if (this.modelProvider.length > 0) {
      metadata.judge_provider = this.modelProvider;
    }
    return {
      evaluator: this.evaluator,
      value: parsed.score,
      passed: parsed.passed,
      explanation: parsed.explanation,
      scoreKey: this.scoreKey,
      metadata,
      grader: {
        input: prompt,
        output: raw,
        modelProvider: this.modelProvider,
        modelName: this.modelName,
        agentName: this.agentName,
        agentVersion: this.agentVersion,
        operationName: this.operationName,
        ...(usage !== undefined ? { usage } : {}),
      },
    };
  }

  /**
   * Reads the last JSON object in the response that carries a numeric `score`.
   *
   * Judges tend to narrate before the verdict, so unrelated leading JSON is
   * skipped and the last scored object wins. A nested rubric score never replaces
   * a top-level one, because the top-level object is the outermost match.
   */
  private parseDefault(raw: string): ParsedJudgeResult {
    const objects = jsonObjects(raw);
    if (objects.length === 0) {
      throw new Error('LLM judge response did not contain a JSON object');
    }
    for (let index = objects.length - 1; index >= 0; index--) {
      const payload = objects[index];
      if (payload === undefined) {
        continue;
      }
      const rawScore = payload.score;
      const numeric = typeof rawScore === 'number' ? rawScore : Number(rawScore);
      if (rawScore === undefined || rawScore === null || typeof rawScore === 'boolean' || !Number.isFinite(numeric)) {
        continue;
      }
      const score = Math.max(0, Math.min(1, numeric));
      const passed = parsePassed(payload.passed ?? payload.pass, score, this.passThreshold);
      const explanation = String(payload.explanation ?? payload.reason ?? '').trim();
      return { score, passed, explanation };
    }
    throw new Error("LLM judge response requires a numeric 'score'");
  }
}

export interface RegexJudgeOptions {
  evaluatorId: string;
  pattern: string;
  version?: string;
  scoreKey?: string;
  flags?: string;
  fullMatch?: boolean;
  negate?: boolean;
  explanation?: string;
}

/** Deterministic regular-expression evaluator for candidate output. */
export class RegexJudge implements OutputEvaluator {
  readonly evaluator: Evaluator;
  readonly scoreKey: string;

  private readonly pattern: string;
  private readonly compiled: RegExp;
  private readonly fullMatch: boolean;
  private readonly negate: boolean;
  private readonly explanationOverride: string;

  constructor(options: RegexJudgeOptions) {
    if (options.evaluatorId.trim().length === 0) {
      throw new Error('evaluatorId is required');
    }
    if (options.pattern.length === 0) {
      throw new Error('pattern is required');
    }
    this.pattern = options.pattern;
    this.fullMatch = options.fullMatch ?? false;
    // Anchoring and the flag set are both fixed here, so the regex is compiled once
    // and reused for every evaluation, as Python's copy does. `g` is dropped
    // because `test` on a global regex advances `lastIndex` between calls.
    const source = this.fullMatch ? `^(?:${options.pattern})$` : options.pattern;
    this.compiled = new RegExp(source, (options.flags ?? '').replace(/g/g, ''));
    this.negate = options.negate ?? false;
    this.explanationOverride = options.explanation ?? '';
    this.scoreKey = options.scoreKey ?? 'regex_match';
    this.evaluator = {
      evaluatorId: options.evaluatorId,
      version: options.version ?? '1',
      kind: 'deterministic',
    };
  }

  evaluateOutput(input: EvaluateOutputInput): EvaluationResult {
    const text = String(input.output);
    const matched = this.compiled.test(text);
    const passed = this.negate ? !matched : matched;
    const explanation =
      this.explanationOverride.length > 0
        ? this.explanationOverride
        : passed
          ? `output ${this.negate ? 'did not match' : 'matched'} /${this.pattern}/`
          : `output ${this.negate ? 'matched excluded' : 'did not match'} /${this.pattern}/`;
    return {
      evaluator: this.evaluator,
      value: passed,
      passed,
      explanation,
      scoreKey: this.scoreKey,
      metadata: { pattern: this.pattern },
    };
  }
}

/** Replaces judge placeholders without treating JSON braces as formatting. */
function renderPrompt(template: string, values: EvaluateOutputInput): string {
  const replacements: Record<string, string> = {
    input: String(values.input),
    output: String(values.output),
    expected: String(values.expected),
  };
  return template.replace(/\{(input|output|expected)\}/g, (_match, key: string) => replacements[key] ?? '');
}

function responseText(response: unknown): string {
  const nested = field(response, 'content');
  const content = nested !== undefined ? nested : response;
  if (typeof content === 'string') {
    return content;
  }
  if (Array.isArray(content)) {
    const parts: string[] = [];
    for (const item of content) {
      if (typeof item === 'string') {
        parts.push(item);
      } else if (isRecordLike(item) && typeof (item as { text?: unknown }).text === 'string') {
        parts.push((item as { text: string }).text);
      }
    }
    if (parts.length > 0) {
      return parts.join('');
    }
  }
  if (isRecordLike(content)) {
    return JSON.stringify(content);
  }
  return String(content);
}

/** Extracts common SDK and framework token-usage shapes without owning them. */
function responseUsage(response: unknown): TokenUsage | undefined {
  const responseMetadata = field(response, 'response_metadata') ?? field(response, 'responseMetadata');
  const candidates = [
    field(response, 'usage_metadata') ?? field(response, 'usageMetadata'),
    field(response, 'usage'),
    field(responseMetadata, 'usage'),
    field(responseMetadata, 'token_usage') ?? field(responseMetadata, 'tokenUsage'),
  ];
  for (const candidate of candidates) {
    if (candidate === undefined || candidate === null) {
      continue;
    }
    const inputDetails = field(candidate, 'input_token_details') ?? field(candidate, 'inputTokenDetails');
    const outputDetails = field(candidate, 'output_token_details') ?? field(candidate, 'outputTokenDetails');
    const usage: TokenUsage = {
      inputTokens: firstInt(candidate, 'input_tokens', 'inputTokens', 'prompt_tokens', 'promptTokens'),
      outputTokens: firstInt(candidate, 'output_tokens', 'outputTokens', 'completion_tokens', 'completionTokens'),
      totalTokens: firstInt(candidate, 'total_tokens', 'totalTokens'),
      cacheReadInputTokens:
        firstInt(candidate, 'cache_read_input_tokens', 'cacheReadInputTokens', 'cache_read_tokens') ??
        firstInt(inputDetails, 'cache_read', 'cache_read_tokens'),
      // The field is cache_write_input_tokens, never cache_creation_input_tokens;
      // the provider spellings below are only read, not emitted.
      cacheWriteInputTokens:
        firstInt(
          candidate,
          'cache_write_input_tokens',
          'cacheWriteInputTokens',
          'cache_creation_input_tokens',
          'cache_creation_tokens',
        ) ?? firstInt(inputDetails, 'cache_write', 'cache_creation', 'cache_creation_tokens'),
      reasoningTokens:
        firstInt(candidate, 'reasoning_tokens', 'reasoningTokens') ??
        firstInt(outputDetails, 'reasoning', 'reasoning_tokens'),
    };
    if (Object.values(usage).some((value) => value !== undefined)) {
      return normalizeUsage(usage);
    }
  }
  return undefined;
}

function normalizeUsage(usage: TokenUsage): TokenUsage {
  const inputTokens = usage.inputTokens ?? 0;
  const outputTokens = usage.outputTokens ?? 0;
  return {
    inputTokens,
    outputTokens,
    totalTokens:
      usage.totalTokens !== undefined && usage.totalTokens > 0 ? usage.totalTokens : inputTokens + outputTokens,
    cacheReadInputTokens: usage.cacheReadInputTokens ?? 0,
    cacheWriteInputTokens: usage.cacheWriteInputTokens ?? 0,
    reasoningTokens: usage.reasoningTokens ?? 0,
  };
}

/**
 * Extracts complete JSON objects without merging unrelated brace ranges.
 *
 * A judge that emits two objects, or prose containing braces, would otherwise
 * produce one unparseable slice.
 */
export function jsonObjects(raw: string): Record<string, unknown>[] {
  const objects: Record<string, unknown>[] = [];
  let cursor = 0;
  while (cursor < raw.length) {
    const start = raw.indexOf('{', cursor);
    if (start < 0) {
      break;
    }
    const end = matchingBrace(raw, start);
    if (end < 0) {
      cursor = start + 1;
      continue;
    }
    try {
      const value = JSON.parse(raw.slice(start, end + 1));
      if (typeof value === 'object' && value !== null && !Array.isArray(value)) {
        objects.push(value as Record<string, unknown>);
      }
      cursor = end + 1;
    } catch {
      cursor = start + 1;
    }
  }
  return objects;
}

/** Finds the brace closing the object at `start`, skipping braces inside strings. */
function matchingBrace(raw: string, start: number): number {
  let depth = 0;
  let inString = false;
  let escaped = false;
  for (let index = start; index < raw.length; index++) {
    const char = raw[index];
    if (inString) {
      if (escaped) {
        escaped = false;
      } else if (char === '\\') {
        escaped = true;
      } else if (char === '"') {
        inString = false;
      }
      continue;
    }
    if (char === '"') {
      inString = true;
    } else if (char === '{') {
      depth++;
    } else if (char === '}') {
      depth--;
      if (depth === 0) {
        return index;
      }
    }
  }
  return -1;
}

function parsePassed(value: unknown, score: number, threshold: number): boolean {
  if (typeof value === 'boolean') {
    return value;
  }
  if (typeof value === 'string') {
    const normalized = value.trim().toLowerCase();
    if (['true', 'yes', '1', 'pass', 'passed'].includes(normalized)) {
      return true;
    }
    if (['false', 'no', '0', 'fail', 'failed'].includes(normalized)) {
      return false;
    }
  }
  return score >= threshold;
}

function field(value: unknown, name: string): unknown {
  if (value === null || value === undefined) {
    return undefined;
  }
  if (typeof value === 'object') {
    return (value as Record<string, unknown>)[name];
  }
  return undefined;
}

function firstInt(value: unknown, ...names: string[]): number | undefined {
  for (const name of names) {
    const raw = field(value, name);
    if (raw === undefined || raw === null || typeof raw === 'boolean') {
      continue;
    }
    const parsed = typeof raw === 'number' ? Math.trunc(raw) : Number.parseInt(String(raw), 10);
    if (Number.isFinite(parsed) && parsed >= 0) {
      return parsed;
    }
  }
  return undefined;
}

function isRecordLike(value: unknown): boolean {
  return typeof value === 'object' && value !== null;
}
