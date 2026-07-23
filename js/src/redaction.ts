import { type EnvPair, envRedactInputMessages, envTrimmed } from './config.js';
import type { Agento11yLogger, GenerationSanitizer, Message, MessagePart } from './types.js';
import { cloneGeneration } from './utils.js';

/**
 * Secret redaction engine for agento11y content capture.
 *
 * ~20 high-confidence patterns hand-curated from Gitleaks
 * (https://github.com/gitleaks/gitleaks). Two tiers:
 *   - Tier 1: definite secret formats — used by both redact() and redactLightweight()
 *   - Tier 2: heuristic env patterns — used only by redact()
 *
 * Add more patterns when concrete unredacted secrets are observed.
 */

interface SecretPattern {
  id: string;
  regex: RegExp;
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

// --- Tier 1: High-confidence patterns (definite secret formats) ---
const tier1Patterns: SecretPattern[] = [
  { id: 'grafana-cloud-token', regex: /\bglc_[A-Za-z0-9_-]{20,}/g },
  { id: 'grafana-service-account-token', regex: /\bglsa_[A-Za-z0-9_-]{20,}/g },
  { id: 'aws-access-token', regex: /\b(?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z2-7]{16}\b/g },
  { id: 'github-pat', regex: /\bghp_[A-Za-z0-9_]{36,}/g },
  { id: 'github-oauth', regex: /\bgho_[A-Za-z0-9_]{36,}/g },
  { id: 'github-app-token', regex: /\bghs_[A-Za-z0-9_]{36,}/g },
  { id: 'github-fine-grained-pat', regex: /\bgithub_pat_[A-Za-z0-9_]{82}/g },
  { id: 'anthropic-api-key', regex: /\bsk-ant-api03-[a-zA-Z0-9_-]{93}AA/g },
  { id: 'anthropic-admin-key', regex: /\bsk-ant-admin01-[a-zA-Z0-9_-]{93}AA/g },
  { id: 'openai-api-key', regex: /\bsk-[a-zA-Z0-9]{20}T3BlbkFJ[a-zA-Z0-9]{20}/g },
  { id: 'openai-project-key', regex: /\bsk-proj-[a-zA-Z0-9_-]{40,}/g },
  { id: 'openai-svcacct-key', regex: /\bsk-svcacct-[a-zA-Z0-9_-]{40,}/g },
  { id: 'gcp-api-key', regex: /\bAIza[A-Za-z0-9_-]{35}/g },
  { id: 'private-key', regex: /-----BEGIN[A-Z ]*PRIVATE KEY-----[\s\S]*?-----END[A-Z ]*PRIVATE KEY-----/g },
  { id: 'connection-string', regex: /(?:postgres|mysql|mongodb|redis|amqp):\/\/[^\s'"]+@[^\s'"]+/g },
  { id: 'bearer-token', regex: /[Bb]earer\s+[A-Za-z0-9_.\-~+/]{20,}={0,3}/g },
  { id: 'slack-token', regex: /\bxox[bporas]-[A-Za-z0-9-]{10,}/g },
  { id: 'stripe-key', regex: /\b[sr]k_(?:live|test)_[A-Za-z0-9]{20,}/g },
  { id: 'sendgrid-api-key', regex: /\bSG\.[A-Za-z0-9_-]{22}\.[A-Za-z0-9_-]{43}/g },
  { id: 'twilio-api-key', regex: /\bSK[a-f0-9]{32}/g },
  { id: 'npm-token', regex: /\bnpm_[A-Za-z0-9]{36}/g },
  { id: 'pypi-token', regex: /\bpypi-[A-Za-z0-9_-]{50,}/g },
];

const emailPattern: SecretPattern = {
  id: 'email',
  regex: /\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b/gi,
};

// --- Tier 2: Heuristic patterns (env file values) ---
const tier2Patterns: SecretPattern[] = [
  {
    id: 'env-secret-value',
    regex: /((?:PASSWORD|SECRET|TOKEN|KEY|CREDENTIAL|API_KEY|PRIVATE_KEY|ACCESS_KEY)\s*[=:]\s*)(\S+)/gi,
  },
];

class SecretRedactor {
  private readonly includeEmailAddresses: boolean;

  constructor(includeEmailAddresses: boolean) {
    this.includeEmailAddresses = includeEmailAddresses;
  }

  /** Full redaction: tier 1 + tier 2. Use for tool call args and tool results. */
  redact(text: string): string {
    let result = applyPatterns(text, tier1Patterns);
    if (this.includeEmailAddresses) {
      result = applyPattern(result, emailPattern);
    }
    return applyTier2Patterns(result, tier2Patterns);
  }

  /** Lightweight redaction: tier 1 only. Use for assistant text and reasoning. */
  redactLightweight(text: string): string {
    let result = applyPatterns(text, tier1Patterns);
    if (this.includeEmailAddresses) {
      result = applyPattern(result, emailPattern);
    }
    return result;
  }
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

    if (sanitized.systemPrompt !== undefined) {
      sanitized.systemPrompt = redactor.redactLightweight(sanitized.systemPrompt);
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

function applyPatterns(text: string, patterns: SecretPattern[]): string {
  let result = text;
  for (const pattern of patterns) {
    result = applyPattern(result, pattern);
  }
  return result;
}

function applyPattern(text: string, pattern: SecretPattern): string {
  pattern.regex.lastIndex = 0;
  return text.replace(pattern.regex, `[REDACTED:${pattern.id}]`);
}

function applyTier2Patterns(text: string, patterns: SecretPattern[]): string {
  let result = text;
  for (const pattern of patterns) {
    pattern.regex.lastIndex = 0;
    result = result.replace(pattern.regex, `$1[REDACTED:${pattern.id}]`);
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
