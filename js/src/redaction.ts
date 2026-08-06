import { type EnvPair, envRedactInputMessages, envTrimmed } from './config.js';
import { emailPattern as generatedEmailPattern, tier1Patterns, tier2Patterns } from './redaction-patterns.generated.js';
import type { Agento11yLogger, GenerationSanitizer, Message, MessagePart } from './types.js';
import { cloneGeneration } from './utils.js';

/**
 * Secret redaction engine for agento11y content capture.
 *
 * The patterns come from the shared table in `redaction/patterns.json` through
 * `redaction-patterns.generated.ts`, so all SDKs and plugins redact the same
 * strings. Two tiers:
 *   - Tier 1: definite secret formats — used by both redact() and redactLightweight()
 *   - Tier 2: heuristic key/value patterns — used only by redact()
 *
 * To add a pattern, edit `redaction/patterns.json` and run
 * `mise run generate:redaction`.
 */

interface SecretPattern {
  id: string;
  regex: RegExp;
}

/** Options for the string-level redaction helpers. */
export interface SecretTextRedactionOptions {
  /**
   * Redact generic email addresses. Defaults to `true`, matching the generation
   * sanitizer and Go's `RedactSecretText`.
   */
  redactEmailAddresses?: boolean;
}

export interface SecretRedactionOptions {
  /**
   * Redact user input messages in addition to assistant/tool content.
   * Defaults to `false` to match the current opencode plugin behavior.
   */
  redactInputMessages?: boolean;
  /**
   * Redact generic email addresses.
   * Defaults to `true`. Set to `false` to opt out when company policy allows
   * email-like content.
   */
  redactEmailAddresses?: boolean;
}

const tier1Ids: readonly string[] = tier1Patterns.map((pattern) => pattern.id);

/**
 * Alternating every tier 1 pattern into one regex scans each input once instead
 * of once per pattern. Each pattern is wrapped in a capturing group; the matched
 * group index identifies which pattern fired. The generator rejects capturing
 * groups inside a tier 1 pattern, which would shift that mapping. Scanning once
 * is also what keeps this output identical to the other SDKs': with per-pattern
 * passes an earlier pattern can rewrite text a later one would have matched.
 */
const tier1Combined = new RegExp(tier1Patterns.map((pattern) => `(${pattern.source})`).join('|'), 'g');

const emailPattern: SecretPattern = {
  id: generatedEmailPattern.id,
  regex: new RegExp(generatedEmailPattern.source, generatedEmailPattern.flags),
};

const compiledTier2 = tier2Patterns.map((pattern) => ({
  regex: new RegExp(pattern.source, pattern.flags),
  replacement: pattern.replacement,
}));

class SecretRedactor {
  private readonly includeEmailAddresses: boolean;

  constructor(includeEmailAddresses: boolean) {
    this.includeEmailAddresses = includeEmailAddresses;
  }

  /** Full redaction: tier 1 + tier 2. Use for tool call args and tool results. */
  redact(text: string): string {
    return applyTier2Patterns(this.redactLightweight(text));
  }

  /** Lightweight redaction: tier 1 only. Use for assistant text and reasoning. */
  redactLightweight(text: string): string {
    let result = applyTier1(text);
    if (this.includeEmailAddresses) {
      result = applyPattern(result, emailPattern);
    }
    return result;
  }
}

/**
 * Redacts known secret formats from arbitrary text: tier 1, optional email, and
 * the heuristic tier 2 key/value patterns. Mirrors Go's `RedactSecretText` and
 * Python's `redact_secret_text`.
 *
 * The experiments surface calls this for score explanations, artifact text, and
 * telemetry strings.
 */
export function redactSecretText(text: string, options: SecretTextRedactionOptions = {}): string {
  if (text.length === 0) {
    return text;
  }
  return new SecretRedactor(options.redactEmailAddresses ?? true).redact(text);
}

/**
 * Redacts only the high-confidence formats and, when enabled, email addresses.
 * Use for assistant text and reasoning, where the tier 2 heuristics produce too
 * many false positives.
 */
export function redactSecretTextLightweight(text: string, options: SecretTextRedactionOptions = {}): string {
  return new SecretRedactor(options.redactEmailAddresses ?? true).redactLightweight(text);
}

/**
 * Build a generation sanitizer that redacts known secret formats.
 *
 * `redactInputMessages` resolves as: explicit option > `AGENTO11Y_REDACT_INPUT_MESSAGES`
 * (with `SIGIL_REDACT_INPUT_MESSAGES` fallback; accepts `1/0`, `true/false`,
 * `yes/no`, `on/off`, case-insensitive) > `false`. An unrecognised env value is
 * warned through `logger` and falls back to `false`, so a typo cannot silently
 * flip redaction.
 */
export function createSecretRedactionSanitizer(
  options: SecretRedactionOptions = {},
  env: Record<string, string | undefined> = readProcessEnv(),
  logger: Agento11yLogger = consoleLogger,
): GenerationSanitizer {
  const redactor = new SecretRedactor(options.redactEmailAddresses ?? true);
  const redactInputMessages = options.redactInputMessages ?? parseEnvBool(env, envRedactInputMessages, logger) ?? false;

  return (generation) => {
    const sanitized = cloneGeneration(generation);

    // A system prompt is assembled content, not model prose: it carries tool
    // definitions, environment dumps and pasted config, so it gets full
    // redaction like tool payloads do.
    if (sanitized.systemPrompt !== undefined) {
      sanitized.systemPrompt = redactor.redact(sanitized.systemPrompt);
    }
    if (sanitized.conversationTitle !== undefined) {
      sanitized.conversationTitle = redactor.redactLightweight(sanitized.conversationTitle);
    }
    if (sanitized.callError !== undefined) {
      sanitized.callError = redactor.redactLightweight(sanitized.callError);
    }

    for (const message of sanitized.input ?? []) {
      sanitizeMessage(
        message,
        redactor,
        message.role === 'user'
          ? redactInputMessages
            ? 'full'
            : 'none'
          : message.role === 'assistant'
            ? 'light'
            : message.role === 'tool'
              ? 'full'
              : 'none',
      );
    }
    for (const message of sanitized.output ?? []) {
      sanitizeMessage(
        message,
        redactor,
        message.role === 'assistant' ? 'light' : message.role === 'tool' ? 'full' : 'none',
      );
    }

    return sanitized;
  };
}

/**
 * Redacts secrets in every string reachable from `value`, rebuilding arrays and
 * objects. Matches Python's `redact_secret_value`.
 *
 * Every non-array object is walked, including a class instance and a
 * `null`-prototype object. Missing a secret matters more here than preserving an
 * exotic shape, and Python's `isinstance(item, dict)` covers subclasses the same
 * way. A Date, Map, Set, RegExp, Error, or typed array passes through unchanged.
 */
export function redactSecretValue<T>(value: T): T {
  if (typeof value === 'string') {
    return redactSecretText(value) as unknown as T;
  }
  if (Array.isArray(value)) {
    return value.map((entry) => redactSecretValue(entry)) as unknown as T;
  }
  if (isWalkableObject(value)) {
    const out: Record<string, unknown> = {};
    for (const [key, entry] of Object.entries(value)) {
      out[key] = redactSecretValue(entry);
    }
    return out as unknown as T;
  }
  return value;
}

/**
 * Whether `value` is an object whose own string properties should be walked.
 *
 * A built-in wrapper (Date, Map, Set, RegExp, Error, a typed array) is left alone:
 * rebuilding it as a plain object would change the payload's shape, and its
 * secrets, if any, are not in its own enumerable string properties.
 */
function isWalkableObject(value: unknown): value is Record<string, unknown> {
  if (typeof value !== 'object' || value === null) {
    return false;
  }
  if (value instanceof Date || value instanceof RegExp || value instanceof Error) {
    return false;
  }
  if (value instanceof Map || value instanceof Set || ArrayBuffer.isView(value)) {
    return false;
  }
  return true;
}

function sanitizeMessage(message: Message, redactor: SecretRedactor, defaultTextMode: 'none' | 'light' | 'full'): void {
  if (typeof message.content === 'string') {
    message.content = redactString(message.content, redactor, defaultTextMode);
  }
  for (const part of message.parts ?? []) {
    sanitizePart(part, redactor, defaultTextMode);
  }
}

function sanitizePart(part: MessagePart, redactor: SecretRedactor, defaultTextMode: 'none' | 'light' | 'full'): void {
  switch (part.type) {
    case 'text':
      part.text = redactString(part.text, redactor, defaultTextMode);
      break;
    case 'thinking':
      if (defaultTextMode !== 'none') {
        part.thinking = redactor.redactLightweight(part.thinking);
      }
      break;
    case 'tool_call':
      if (defaultTextMode !== 'none' && typeof part.toolCall.inputJSON === 'string') {
        part.toolCall.inputJSON = redactor.redact(part.toolCall.inputJSON);
      }
      break;
    case 'tool_result':
      if (defaultTextMode !== 'none') {
        if (typeof part.toolResult.content === 'string') {
          part.toolResult.content = redactor.redact(part.toolResult.content);
        }
        if (typeof part.toolResult.contentJSON === 'string') {
          part.toolResult.contentJSON = redactor.redact(part.toolResult.contentJSON);
        }
      }
      break;
  }
}

function redactString(value: string, redactor: SecretRedactor, mode: 'none' | 'light' | 'full'): string {
  switch (mode) {
    case 'full':
      return redactor.redact(value);
    case 'light':
      return redactor.redactLightweight(value);
    default:
      return value;
  }
}

function applyTier1(text: string): string {
  tier1Combined.lastIndex = 0;
  return text.replace(tier1Combined, (...args: unknown[]) => {
    // replace() passes the match, then one argument per capturing group. Exactly
    // one group of the alternation participates in a match.
    for (let group = 0; group < tier1Ids.length; group++) {
      if (args[group + 1] !== undefined) {
        return `[REDACTED:${tier1Ids[group]}]`;
      }
    }
    return String(args[0]);
  });
}

function applyPattern(text: string, pattern: SecretPattern): string {
  pattern.regex.lastIndex = 0;
  return text.replace(pattern.regex, `[REDACTED:${pattern.id}]`);
}

function applyTier2Patterns(text: string): string {
  let result = text;
  for (const pattern of compiledTier2) {
    pattern.regex.lastIndex = 0;
    result = result.replace(pattern.regex, pattern.replacement);
  }
  return result;
}

const TRUE_VALUES = new Set(['1', 'true', 'yes', 'on']);
const FALSE_VALUES = new Set(['0', 'false', 'no', 'off']);

function parseEnvBool(
  env: Record<string, string | undefined>,
  pair: EnvPair,
  logger: Agento11yLogger,
): boolean | undefined {
  const selected = envTrimmed(env, pair);
  if (selected === undefined) return undefined;
  const normalized = selected.value.toLowerCase();
  if (TRUE_VALUES.has(normalized)) return true;
  if (FALSE_VALUES.has(normalized)) return false;
  logger.warn?.(`agento11y: ignoring invalid ${selected.key}: ${selected.value}`);
  return undefined;
}

function readProcessEnv(): Record<string, string | undefined> {
  if (typeof process !== 'undefined' && process.env !== undefined) {
    return process.env;
  }
  return {};
}

const consoleLogger: Agento11yLogger = {
  warn(message: string, ...args: unknown[]) {
    console.warn(message, ...args);
  },
};
