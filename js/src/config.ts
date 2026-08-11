import type {
  Agento11yLogger,
  Agento11ySdkConfig,
  Agento11ySdkConfigInput,
  ApiConfig,
  ContentCaptureMode,
  EmbeddingCaptureConfig,
  ExportAuthConfig,
  GenerationExportConfig,
  HookPhase,
  HooksConfig,
} from './types.js';

const tenantHeaderName = 'X-Scope-OrgID';
const authorizationHeaderName = 'Authorization';

const validAuthModes: ExportAuthConfig['mode'][] = ['none', 'tenant', 'bearer', 'basic'];

// EnvPair is one logical config field readable under the preferred
// AGENTO11Y_* name with a SIGIL_* legacy fallback. Selection happens before
// parsing: a nonblank preferred value always wins, even when it later fails
// validation, so stale legacy config cannot silently resurface.
interface EnvPair {
  preferred: string;
  legacy: string;
}

function brandedPair(suffix: string): EnvPair {
  return { preferred: `AGENTO11Y_${suffix}`, legacy: `SIGIL_${suffix}` };
}

// preferredOnlyPair is a variable with no legacy spelling. envTrimmed skips a
// lookup miss, so the empty legacy name never resolves.
function preferredOnlyPair(suffix: string): EnvPair {
  return { preferred: `AGENTO11Y_${suffix}`, legacy: '' };
}

// canonical env-var names: preferred AGENTO11Y_* with SIGIL_* fallback.
const envEndpoint = brandedPair('ENDPOINT');
const envProtocol = brandedPair('PROTOCOL');
const envInsecure = brandedPair('INSECURE');
const envHeaders = brandedPair('HEADERS');
const envAuthMode = brandedPair('AUTH_MODE');
const envAuthTenantId = brandedPair('AUTH_TENANT_ID');
const envAuthToken = brandedPair('AUTH_TOKEN');
const envAgentName = brandedPair('AGENT_NAME');
const envAgentVersion = brandedPair('AGENT_VERSION');
const envUserId = brandedPair('USER_ID');
// The two TAGS spellings are never merged; the selected value is used whole.
const envTags = brandedPair('TAGS');
const envContentCaptureMode = brandedPair('CONTENT_CAPTURE_MODE');
const envDebug = brandedPair('DEBUG');
export const envRedactInputMessages = brandedPair('REDACT_INPUT_MESSAGES');
// Hooks configuration is preferred-only: these names have no installed base
// under SIGIL_*, and the Python SDK already ignores that prefix everywhere.
const envHooksEnabled = preferredOnlyPair('HOOKS_ENABLED');
const envHooksPhases = preferredOnlyPair('HOOKS_PHASES');
const envHooksTimeoutMs = preferredOnlyPair('HOOKS_TIMEOUT_MS');
const envHooksFailOpen = preferredOnlyPair('HOOKS_FAIL_OPEN');

const defaultExportAuthConfig: ExportAuthConfig = {
  mode: 'none',
};

export const defaultGenerationExportConfig: GenerationExportConfig = {
  protocol: 'http',
  endpoint: 'http://localhost:8080',
  auth: defaultExportAuthConfig,
  insecure: false,
  batchSize: 100,
  flushIntervalMs: 1_000,
  queueSize: 2_000,
  maxRetries: 5,
  initialBackoffMs: 100,
  maxBackoffMs: 5_000,
  payloadMaxBytes: 4 << 20,
};

export const defaultAPIConfig: ApiConfig = {
  endpoint: 'http://localhost:8080',
};

export const defaultEmbeddingCaptureConfig: EmbeddingCaptureConfig = {
  captureInput: false,
  maxInputItems: 20,
  maxTextLength: 1024,
};

export const defaultHooksConfig: HooksConfig = {
  enabled: false,
  phases: ['preflight'],
  timeoutMs: 15_000,
  failOpen: true,
};

export const defaultLogger: Agento11yLogger = {
  debug(message: string, ...args: unknown[]) {
    console.debug(message, ...args);
  },
  warn(message: string, ...args: unknown[]) {
    console.warn(message, ...args);
  },
  error(message: string, ...args: unknown[]) {
    console.error(message, ...args);
  },
};

export const defaultContentCaptureMode: ContentCaptureMode = 'default';

export function defaultConfig(): Agento11ySdkConfig {
  return {
    generationExport: cloneGenerationExportConfig(defaultGenerationExportConfig),
    api: cloneAPIConfig(defaultAPIConfig),
    embeddingCapture: cloneEmbeddingCaptureConfig(defaultEmbeddingCaptureConfig),
    hooks: cloneHooksConfig(defaultHooksConfig),
    contentCapture: defaultContentCaptureMode,
  };
}

/**
 * Build a Agento11ySdkConfig from canonical AGENTO11Y_* environment variables
 * (with SIGIL_* fallbacks).
 *
 * Most callers should use `new Agento11yClient()` (env reading is automatic).
 * Use `configFromEnv()` for tests, debugging, or advanced layering.
 */
export function configFromEnv(env: Record<string, string | undefined> = defaultEnv()): Agento11ySdkConfig {
  return mergeConfig({}, env);
}

export function mergeConfig(
  config: Agento11ySdkConfigInput,
  env: Record<string, string | undefined> = defaultEnv(),
): Agento11ySdkConfig {
  // Layer env values under user-provided fields. The user-provided field wins
  // when defined; env fills in undefined fields; defaults fill the rest.
  // Malformed env values are logged and skipped — one typo cannot discard the
  // rest of the env layer (matches Go and Python SDK behavior).
  const envCfg = envOverrides(env, config.logger ?? defaultLogger);
  const overlaid = layerInputs(envCfg, config);

  return {
    generationExport: mergeGenerationExportConfig(overlaid.generationExport),
    api: mergeAPIConfig(overlaid.api),
    embeddingCapture: mergeEmbeddingCaptureConfig(overlaid.embeddingCapture),
    hooks: mergeHooksConfig(overlaid.hooks),
    contentCapture: overlaid.contentCapture ?? defaultContentCaptureMode,
    contentCaptureResolver: overlaid.contentCaptureResolver,
    generationSanitizer: overlaid.generationSanitizer,
    generationExporter: overlaid.generationExporter,
    tracer: overlaid.tracer,
    meter: overlaid.meter,
    logger: overlaid.logger,
    now: overlaid.now,
    sleep: overlaid.sleep,
    agentName: overlaid.agentName,
    agentVersion: overlaid.agentVersion,
    userId: overlaid.userId,
    tags: overlaid.tags ? { ...overlaid.tags } : undefined,
    debug: overlaid.debug,
  };
}

export function defaultEnv(): Record<string, string | undefined> {
  // Edge runtimes (for example Cloudflare Workers) may not define `process`.
  // Fall back to an empty env object so default config resolution stays safe.
  if (typeof process !== 'undefined' && process.env !== undefined) {
    return process.env;
  }
  return {};
}

function envOverrides(env: Record<string, string | undefined>, logger: Agento11yLogger): Agento11ySdkConfigInput {
  const out: Agento11ySdkConfigInput = {};

  const generationExport: Partial<GenerationExportConfig> = {};
  const auth: Partial<ExportAuthConfig> = {};

  const endpoint = envTrimmed(env, envEndpoint);
  if (endpoint !== undefined) {
    generationExport.endpoint = endpoint.value;
    // Hook evaluation posts to api.endpoint. One endpoint variable feeds both,
    // so env-enabled hooks reach the configured server instead of localhost. A
    // caller-supplied api.endpoint still wins through layerInputs. Matches the
    // Go and Python SDKs.
    out.api = { endpoint: endpoint.value };
  }
  const protocol = envTrimmed(env, envProtocol);
  if (protocol !== undefined)
    generationExport.protocol = protocol.value.toLowerCase() as GenerationExportConfig['protocol'];
  const insecure = envTrimmed(env, envInsecure);
  if (insecure !== undefined) generationExport.insecure = parseTruthy(insecure.value);
  const headers = envTrimmed(env, envHeaders);
  if (headers !== undefined) generationExport.headers = parseCsvKv(headers.value);

  const authMode = envTrimmed(env, envAuthMode);
  if (authMode !== undefined) {
    const normalized = authMode.value.toLowerCase();
    if (validAuthModes.includes(normalized as ExportAuthConfig['mode'])) {
      auth.mode = normalized as ExportAuthConfig['mode'];
    } else {
      logger.warn?.(`agento11y: ignoring invalid ${authMode.key}: ${authMode.value}`);
    }
  }
  const tenantId = envTrimmed(env, envAuthTenantId);
  if (tenantId !== undefined) auth.tenantId = tenantId.value;
  // Set both fields; resolveHeadersWithAuth uses only the one matching the
  // final mode. Lets env's token fill a caller-supplied mode without env
  // declaring an AUTH_MODE.
  const token = envTrimmed(env, envAuthToken);
  if (token !== undefined) {
    auth.bearerToken = token.value;
    auth.basicPassword = token.value;
  }
  if (auth.mode === 'basic' && !auth.basicUser && auth.tenantId) {
    auth.basicUser = auth.tenantId;
  }

  if (Object.keys(auth).length > 0) {
    generationExport.auth = { mode: auth.mode ?? 'none', ...auth } as ExportAuthConfig;
  }
  if (Object.keys(generationExport).length > 0) out.generationExport = generationExport;

  const agentName = envTrimmed(env, envAgentName);
  if (agentName !== undefined) out.agentName = agentName.value;
  const agentVersion = envTrimmed(env, envAgentVersion);
  if (agentVersion !== undefined) out.agentVersion = agentVersion.value;
  const userId = envTrimmed(env, envUserId);
  if (userId !== undefined) out.userId = userId.value;
  const tags = envTrimmed(env, envTags);
  if (tags !== undefined) out.tags = parseCsvKv(tags.value);
  const ccm = envTrimmed(env, envContentCaptureMode);
  if (ccm !== undefined) {
    const normalized = ccm.value.toLowerCase();
    if (['full', 'no_tool_content', 'metadata_only', 'full_with_metadata_spans'].includes(normalized)) {
      out.contentCapture = normalized as ContentCaptureMode;
    } else {
      logger.warn?.(`agento11y: ignoring invalid ${ccm.key}: ${ccm.value}`);
    }
  }
  const debug = envTrimmed(env, envDebug);
  if (debug !== undefined) out.debug = parseTruthy(debug.value);

  // Hooks. One variable per field; layerInputs keeps caller values on top. The
  // timeout is in milliseconds because that is the unit of the wire header.
  const hooks: Partial<HooksConfig> = {};
  const hooksEnabled = parseEnvBool(env, envHooksEnabled, logger);
  if (hooksEnabled !== undefined) hooks.enabled = hooksEnabled;
  const hooksPhases = hookPhasesFromEnv(env, logger);
  if (hooksPhases !== undefined) hooks.phases = hooksPhases;
  const hooksTimeoutMs = envParsed(env, envHooksTimeoutMs, parseHookTimeoutMs, logger);
  if (hooksTimeoutMs !== undefined) hooks.timeoutMs = hooksTimeoutMs;
  const hooksFailOpen = parseEnvBool(env, envHooksFailOpen, logger);
  if (hooksFailOpen !== undefined) hooks.failOpen = hooksFailOpen;
  if (Object.keys(hooks).length > 0) out.hooks = hooks;

  return out;
}

/**
 * Drops keys whose value is `undefined`.
 *
 * `{ ...base, ...override }` keeps an own key that holds `undefined`, so
 * `hooks: { enabled: opts.enableHooks }` with an unset option would erase the
 * env layer instead of leaving it alone. Go and Python read the same shape as
 * "not set", so this keeps the three SDKs on one precedence rule.
 */
function definedOnly<T extends object>(obj: T): T {
  const out: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(obj)) {
    if (value !== undefined) out[key] = value;
  }
  return out as T;
}

function layerInputs(base: Agento11ySdkConfigInput, override: Agento11ySdkConfigInput): Agento11ySdkConfigInput {
  const out: Agento11ySdkConfigInput = { ...base, ...definedOnly(override) };
  if (base.generationExport || override.generationExport) {
    const baseGE = base.generationExport ?? {};
    const overGE = definedOnly(override.generationExport ?? {});
    // Field-by-field so a partial auth from one layer doesn't clobber the other.
    const auth = mergeAuthInput(baseGE.auth, overGE.auth);
    out.generationExport = {
      ...baseGE,
      ...overGE,
      ...(auth !== undefined ? { auth } : {}),
    };
  }
  if (base.api || override.api) {
    out.api = { ...(base.api ?? {}), ...definedOnly(override.api ?? {}) };
  }
  if (base.embeddingCapture || override.embeddingCapture) {
    out.embeddingCapture = { ...(base.embeddingCapture ?? {}), ...definedOnly(override.embeddingCapture ?? {}) };
  }
  if (base.hooks || override.hooks) {
    out.hooks = { ...(base.hooks ?? {}), ...definedOnly(override.hooks ?? {}) };
  }
  if (base.tags || override.tags) {
    out.tags = { ...(base.tags ?? {}), ...(override.tags ?? {}) };
  }
  return out;
}

function mergeAuthInput(
  base: ExportAuthConfig | undefined,
  override: ExportAuthConfig | undefined,
): ExportAuthConfig | undefined {
  if (base === undefined && override === undefined) return undefined;
  return {
    mode: override?.mode ?? base?.mode ?? 'none',
    tenantId: override?.tenantId ?? base?.tenantId,
    bearerToken: override?.bearerToken ?? base?.bearerToken,
    basicUser: override?.basicUser ?? base?.basicUser,
    basicPassword: override?.basicPassword ?? base?.basicPassword,
  };
}

// envTrimmed selects the pair's first nonblank value (preferred, then legacy)
// and returns it with the env-var name it came from, so warnings can name the
// key the user actually set.
function envTrimmed(
  env: Record<string, string | undefined>,
  pair: EnvPair,
): { value: string; key: string } | undefined {
  for (const key of [pair.preferred, pair.legacy]) {
    const raw = env[key];
    if (raw === undefined) continue;
    const value = raw.trim();
    if (value.length === 0) continue;
    return { value, key };
  }
  return undefined;
}

/**
 * Parses the SDK's accepted truthy spellings: `1`, `true`, `yes`, `on`, in any
 * casing and with surrounding whitespace. Every other value, including an empty
 * string, is false.
 *
 * Go (`ExperimentalFeaturesEnabled`) and Python (`_TRUTHY`) accept the same set.
 * `redaction.ts` keeps its own list because it also has to tell a false value from
 * a typo; every caller that only needs true or false uses this one.
 */
export function parseTruthy(raw: string): boolean {
  const v = raw.trim().toLowerCase();
  return v === '1' || v === 'true' || v === 'yes' || v === 'on';
}

const trueValues = new Set(['1', 'true', 'yes', 'on']);
const falseValues = new Set(['0', 'false', 'no', 'off']);
const validHookPhases: HookPhase[] = ['preflight', 'postflight'];
// Bounds AGENTO11Y_HOOKS_TIMEOUT_MS. It is the largest value the server
// honours; above it the server falls back to its own budget, so a larger client
// deadline would not be respected anyway. Go and Python use the same ceiling.
const maxHookTimeoutMs = 119_999;

/**
 * Parses a boolean, returning `undefined` for anything it does not recognise.
 *
 * Used where a typo must not read as false: `AGENTO11Y_HOOKS_FAIL_OPEN`
 * defaults to true, so a lenient parse would quietly switch a deployment to
 * fail-closed.
 */
function parseStrictBool(raw: string): boolean | undefined {
  const normalized = raw.trim().toLowerCase();
  if (trueValues.has(normalized)) return true;
  if (falseValues.has(normalized)) return false;
  return undefined;
}

/**
 * Parses `AGENTO11Y_HOOKS_TIMEOUT_MS`.
 *
 * Rejects non-integers, zero and negatives because hook evaluation reads a
 * non-positive timeout as "use the default", and anything above
 * `maxHookTimeoutMs` because the server would not honour it. The digit test also
 * rejects the underscore grouping Python's `int()` would accept, so the same
 * value means the same thing in every SDK.
 */
function parseHookTimeoutMs(raw: string): number | undefined {
  const trimmed = raw.trim();
  if (!/^[+-]?\d+$/.test(trimmed)) return undefined;
  const value = Number(trimmed);
  if (!Number.isSafeInteger(value) || value <= 0 || value > maxHookTimeoutMs) return undefined;
  return value;
}

/**
 * Parses a comma-separated hook phase list. Entries are trimmed, lowercased and
 * deduplicated in first-seen order.
 *
 * An unknown entry is dropped and reported while the recognised entries still
 * apply. Rejecting the whole list instead would fall back to the default
 * `['preflight']`, so a typo in `'postflight,bogus'` would start preflight
 * enforcement the operator never asked for and skip the phase they did.
 */
function parsePhases(raw: string): { phases: HookPhase[]; unknown: string[] } {
  const phases: HookPhase[] = [];
  const unknown: string[] = [];
  for (const part of raw.split(',')) {
    const phase = part.trim().toLowerCase() as HookPhase;
    if (phase.length === 0) continue;
    if (!validHookPhases.includes(phase)) {
      unknown.push(phase);
      continue;
    }
    if (!phases.includes(phase)) phases.push(phase);
  }
  return { phases, unknown };
}

/** Reads `AGENTO11Y_HOOKS_PHASES`, warning about entries it drops. */
function hookPhasesFromEnv(env: Record<string, string | undefined>, logger: Agento11yLogger): HookPhase[] | undefined {
  const selected = envTrimmed(env, envHooksPhases);
  if (selected === undefined) return undefined;
  const { phases, unknown } = parsePhases(selected.value);
  if (unknown.length > 0) {
    logger.warn?.(`agento11y: ignoring unknown ${selected.key} entries: ${unknown.join(',')}`);
  } else if (phases.length === 0) {
    logger.warn?.(`agento11y: ignoring invalid ${selected.key}: ${selected.value}`);
  }
  return phases.length > 0 ? phases : undefined;
}

/**
 * Reads a variable and parses it, warning under the key that supplied the value
 * when the parse fails. A rejected value leaves the field unset, so one typo
 * cannot discard the rest of the env layer.
 */
function envParsed<T>(
  env: Record<string, string | undefined>,
  pair: EnvPair,
  parse: (raw: string) => T | undefined,
  logger: Agento11yLogger,
): T | undefined {
  const selected = envTrimmed(env, pair);
  if (selected === undefined) return undefined;
  const value = parse(selected.value);
  if (value === undefined) {
    logger.warn?.(`agento11y: ignoring invalid ${selected.key}: ${selected.value}`);
  }
  return value;
}

/** Reads a strict boolean variable, warning and returning `undefined` on a typo. */
export function parseEnvBool(
  env: Record<string, string | undefined>,
  pair: EnvPair,
  logger: Agento11yLogger,
): boolean | undefined {
  return envParsed(env, pair, parseStrictBool, logger);
}

function parseCsvKv(raw: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const part of raw.split(',')) {
    const trimmed = part.trim();
    if (trimmed.length === 0) continue;
    const idx = trimmed.indexOf('=');
    if (idx <= 0) continue;
    const key = trimmed.slice(0, idx).trim();
    const value = trimmed.slice(idx + 1).trim();
    if (key.length > 0) out[key] = value;
  }
  return out;
}

function mergeGenerationExportConfig(config: Partial<GenerationExportConfig> | undefined): GenerationExportConfig {
  const auth = mergeAuthConfig(config?.auth);
  const headers = config?.headers !== undefined ? { ...config.headers } : undefined;
  const merged: GenerationExportConfig = {
    ...defaultGenerationExportConfig,
    ...config,
    auth,
    headers,
  };
  merged.headers = resolveHeadersWithAuth(merged.headers, merged.auth, 'generation export');
  return merged;
}

function mergeAPIConfig(config: Partial<ApiConfig> | undefined): ApiConfig {
  return {
    ...defaultAPIConfig,
    ...config,
  };
}

function mergeEmbeddingCaptureConfig(config: Partial<EmbeddingCaptureConfig> | undefined): EmbeddingCaptureConfig {
  return {
    ...defaultEmbeddingCaptureConfig,
    ...config,
  };
}

function mergeHooksConfig(config: Partial<HooksConfig> | undefined): HooksConfig {
  return {
    enabled: config?.enabled ?? defaultHooksConfig.enabled,
    phases:
      Array.isArray(config?.phases) && config.phases.length > 0 ? [...config.phases] : [...defaultHooksConfig.phases],
    timeoutMs: config?.timeoutMs ?? defaultHooksConfig.timeoutMs,
    failOpen: config?.failOpen ?? defaultHooksConfig.failOpen,
  };
}

function mergeAuthConfig(config: ExportAuthConfig | undefined): ExportAuthConfig {
  return {
    ...defaultExportAuthConfig,
    ...config,
  };
}

// resolveHeadersWithAuth builds the auth-related headers for the given mode.
// Mode-irrelevant fields (e.g. tenantId on a bearer-mode config) are silently
// ignored — env layering can populate any field independently of mode, and
// rejecting cross-mode mixes only forced extra cleanup upstream. Callers who
// want strict validation should check their AuthConfig before constructing
// the client.
export function resolveHeadersWithAuth(
  headers: Record<string, string> | undefined,
  auth: ExportAuthConfig,
  label: string,
): Record<string, string> | undefined {
  const mode = (auth.mode ?? 'none').trim().toLowerCase();
  const tenantId = auth.tenantId?.trim() ?? '';
  const bearerToken = auth.bearerToken?.trim() ?? '';
  const out = headers ? { ...headers } : undefined;

  if (mode === 'none') {
    return out;
  }

  if (mode === 'tenant') {
    if (tenantId.length === 0) {
      throw new Error(`${label} auth mode "tenant" requires tenantId`);
    }
    if (hasHeaderKey(out, tenantHeaderName)) {
      return out;
    }
    return {
      ...(out ?? {}),
      [tenantHeaderName]: tenantId,
    };
  }

  if (mode === 'bearer') {
    if (bearerToken.length === 0) {
      throw new Error(`${label} auth mode "bearer" requires bearerToken`);
    }
    if (hasHeaderKey(out, authorizationHeaderName)) {
      return out;
    }
    return {
      ...(out ?? {}),
      [authorizationHeaderName]: formatBearerTokenValue(bearerToken),
    };
  }

  if (mode === 'basic') {
    const password = auth.basicPassword?.trim() ?? '';
    if (password.length === 0) {
      throw new Error(`${label} auth mode "basic" requires basicPassword`);
    }
    let user = auth.basicUser?.trim() ?? '';
    if (user.length === 0) {
      user = tenantId;
    }
    if (user.length === 0) {
      throw new Error(`${label} auth mode "basic" requires basicUser or tenantId`);
    }
    const result: Record<string, string> = { ...(out ?? {}) };
    if (!hasHeaderKey(result, authorizationHeaderName)) {
      const encoded = new TextEncoder().encode(`${user}:${password}`);
      result[authorizationHeaderName] = `Basic ${btoa(String.fromCharCode(...encoded))}`;
    }
    if (tenantId.length > 0 && !hasHeaderKey(result, tenantHeaderName)) {
      result[tenantHeaderName] = tenantId;
    }
    return result;
  }

  throw new Error(`unsupported ${label} auth mode: ${auth.mode}`);
}

function hasHeaderKey(headers: Record<string, string> | undefined, key: string): boolean {
  if (headers === undefined) {
    return false;
  }
  const target = key.toLowerCase();
  return Object.keys(headers).some((existing) => existing.toLowerCase() === target);
}

function formatBearerTokenValue(token: string): string {
  const value = token.trim();
  if (value.toLowerCase().startsWith('bearer ')) {
    return `Bearer ${value.slice(7).trim()}`;
  }
  return `Bearer ${value}`;
}

function cloneGenerationExportConfig(config: GenerationExportConfig): GenerationExportConfig {
  return {
    ...config,
    auth: { ...config.auth },
    headers: config.headers ? { ...config.headers } : undefined,
  };
}

function cloneAPIConfig(config: ApiConfig): ApiConfig {
  return {
    ...config,
  };
}

function cloneEmbeddingCaptureConfig(config: EmbeddingCaptureConfig): EmbeddingCaptureConfig {
  return {
    ...config,
  };
}

function cloneHooksConfig(config: HooksConfig): HooksConfig {
  return {
    ...config,
    phases: [...config.phases],
  };
}
