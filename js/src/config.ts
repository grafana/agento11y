import type {
  Agento11yLogger,
  Agento11ySdkConfig,
  Agento11ySdkConfigInput,
  ApiConfig,
  ContentCaptureMode,
  EmbeddingCaptureConfig,
  ExportAuthConfig,
  GenerationExportConfig,
  HooksConfig,
} from './types.js';

const tenantHeaderName = 'X-Scope-OrgID';
const authorizationHeaderName = 'Authorization';

const validAuthModes: ExportAuthConfig['mode'][] = ['none', 'tenant', 'bearer', 'basic'];

// EnvPair is one logical config field readable under the preferred
// AGENTO11Y_* name with a SIGIL_* legacy fallback. Selection happens before
// parsing: a nonblank preferred value always wins, even when it later fails
// validation, so stale legacy config cannot silently resurface.
export interface EnvPair {
  preferred: string;
  legacy: string;
}

function brandedPair(suffix: string): EnvPair {
  return { preferred: `AGENTO11Y_${suffix}`, legacy: `SIGIL_${suffix}` };
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

function defaultEnv(): Record<string, string | undefined> {
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
  if (endpoint !== undefined) generationExport.endpoint = endpoint.value;
  const protocol = envTrimmed(env, envProtocol);
  if (protocol !== undefined)
    generationExport.protocol = protocol.value.toLowerCase() as GenerationExportConfig['protocol'];
  const insecure = envTrimmed(env, envInsecure);
  if (insecure !== undefined) generationExport.insecure = parseBool(insecure.value);
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
  if (debug !== undefined) out.debug = parseBool(debug.value);

  return out;
}

function layerInputs(base: Agento11ySdkConfigInput, override: Agento11ySdkConfigInput): Agento11ySdkConfigInput {
  const out: Agento11ySdkConfigInput = { ...base, ...override };
  if (base.generationExport || override.generationExport) {
    const baseGE = base.generationExport ?? {};
    const overGE = override.generationExport ?? {};
    // Field-by-field so a partial auth from one layer doesn't clobber the other.
    const auth = mergeAuthInput(baseGE.auth, overGE.auth);
    out.generationExport = {
      ...baseGE,
      ...overGE,
      ...(auth !== undefined ? { auth } : {}),
      headers: overGE.headers !== undefined ? overGE.headers : baseGE.headers,
    };
  }
  if (base.api || override.api) {
    out.api = { ...(base.api ?? {}), ...(override.api ?? {}) };
  }
  if (base.embeddingCapture || override.embeddingCapture) {
    out.embeddingCapture = { ...(base.embeddingCapture ?? {}), ...(override.embeddingCapture ?? {}) };
  }
  if (base.hooks || override.hooks) {
    out.hooks = { ...(base.hooks ?? {}), ...(override.hooks ?? {}) };
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
export function envTrimmed(
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

function parseBool(raw: string): boolean {
  const v = raw.trim().toLowerCase();
  return v === '1' || v === 'true' || v === 'yes' || v === 'on';
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
function resolveHeadersWithAuth(
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
