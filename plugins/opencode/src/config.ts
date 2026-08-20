import type { ContentCaptureMode } from "@grafana/agento11y";
import { applyAgento11yDotenv } from "./agento11yDotenv.js";
import { parseTagPairs, resolveAutoTags, selectAutoTags } from "./autotag.js";
import { normalizeBaseEndpoint } from "./endpoint.js";
import { type LocalReceiver, resolveLocalReceiver } from "./local.js";

export type Agento11yAuthConfig =
  | {
      mode: "basic";
      basicUser: string;
      basicPassword: string;
      tenantId: string;
    }
  | { mode: "tenant"; tenantId: string }
  | { mode: "none" };

export interface OtlpConfig {
  endpoint: string;
  headers: Record<string, string>;
}

export interface GuardsFeatureConfig {
  enabled: boolean;
  timeoutMs: number;
  failOpen: boolean;
}

export interface Agento11yOpencodeConfig {
  endpoint: string;
  auth: Agento11yAuthConfig;
  agentName: string;
  agentVersion?: string;
  contentCapture: ContentCaptureMode;
  /**
   * Redact known secret formats from user prompt text before export. On by
   * default; `AGENTO11Y_REDACT_INPUT_MESSAGES=false` opts out. Assistant
   * text, thinking, tool arguments, and tool results are redacted under
   * either setting.
   */
  redactInputMessages: boolean;
  debug: boolean;
  guards?: GuardsFeatureConfig;
  otlp?: OtlpConfig;
  /**
   * Client tags resolved for AGENTO11Y_AUTO_CODING_AGENT_TAGS, or undefined
   * when the switch is off. Undefined and an empty map are the same to the SDK,
   * but undefined keeps the client config free of an empty `tags` object.
   */
  autoTags?: Record<string, string>;
  /**
   * True when this session records to the local receiver instead of Grafana
   * Cloud. index.ts reads it to name the destination; client.ts never passes
   * it to the SDK, so it is never part of an exported generation.
   */
  local?: boolean;
}

export async function loadConfig(): Promise<Agento11yOpencodeConfig | null> {
  // Read the shared agento11y dotenv file so the OpenCode plugin and every other
  // agento11y agent resolve credentials from the same place. Shell env values win
  // over file values for each alias family, and the winner is materialized
  // under both spellings; see applyAgento11yDotenv.
  applyAgento11yDotenv();
  // A saved AGENTO11Y_LOCAL=true means this session records to the machine,
  // whatever Cloud endpoint config.env also holds. Resolving the receiver can
  // fail; the caller reports that and captures nothing rather than falling
  // back to Cloud.
  if (!(brandedBool("LOCAL") ?? false)) return resolveConfig();
  return resolveLocalConfig(await resolveLocalReceiver());
}

// local.LaunchEnv fills a missing tenant or token with this stand-in. The
// receiver does not check credentials, but the export paths short-circuit on
// an empty one.
const LOCAL_AUTH_PLACEHOLDER = "local";

/**
 * Config for a session that records to `receiver`. Mirrors
 * `plugins/agento11y/internal/local/env.go::LaunchEnv.Apply`: the receiver
 * owns both endpoints whatever Cloud values are configured, content capture
 * is always full on this machine, and a missing tenant or token is filled
 * independently with the placeholder.
 *
 * Nothing here is written back to `process.env`, so a plugin instance that
 * loads later in the same process sees the configured values unchanged.
 */
export function resolveLocalConfig(
  receiver: LocalReceiver,
): Agento11yOpencodeConfig {
  const tenantId = brandedEnv("AUTH_TENANT_ID") ?? LOCAL_AUTH_PLACEHOLDER;
  const token = brandedEnv("AUTH_TOKEN") ?? LOCAL_AUTH_PLACEHOLDER;
  return {
    ...resolveSharedConfig(),
    endpoint: receiver.endpoint,
    auth: {
      mode: "basic",
      basicUser: tenantId,
      basicPassword: token,
      tenantId,
    },
    contentCapture: "full",
    otlp: {
      endpoint: receiver.otlpEndpoint,
      headers: otlpHeaders(tenantId, brandedEnv("OTEL_AUTH_TOKEN") ?? token),
    },
    local: true,
  };
}

export function resolveConfig(): Agento11yOpencodeConfig | null {
  const endpoint = normalizeBaseEndpoint(brandedEnv("ENDPOINT") ?? "");
  if (!endpoint) return null;

  return {
    ...resolveSharedConfig(),
    endpoint,
    auth: resolveAuth(),
    contentCapture: resolveContentCapture(),
    otlp: resolveOtlp(),
  };
}

// The settings a local session reads exactly as a Cloud one does. The two
// resolvers differ only in where the session goes, how it authenticates, and
// how much of it is captured.
function resolveSharedConfig(): Omit<
  Agento11yOpencodeConfig,
  "endpoint" | "auth" | "contentCapture" | "otlp"
> {
  return {
    agentName: brandedEnv("AGENT_NAME") ?? "opencode",
    agentVersion: brandedEnv("AGENT_VERSION"),
    // Redaction defaults to true so prompts are scrubbed without
    // configuration, matching the pi plugin. An unrecognised value keeps
    // redaction on, so a typo cannot silently disable it.
    redactInputMessages: brandedBool("REDACT_INPUT_MESSAGES") ?? true,
    debug: brandedBool("DEBUG") ?? false,
    guards: resolveGuards(),
  };
}

// resolveAutoTagValues reads AGENTO11Y_AUTO_CODING_AGENT_TAGS and the optional
// AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES allowlist, then resolves whatever they
// enable, for the project directory the caller passes. The switch is off by
// default, so a session that has not opted in carries exactly the tags it
// carried before.
//
// The caller resolves this rather than resolveConfig because the opencode
// server can run from a directory other than the project root; the plugin
// input names the project directory, and that is the checkout the repository
// and branch must come from.
//
// The plugin builds one client per opencode session, so these values freeze at
// session start: a checkout that changes mid-session keeps the metric label it
// started with. The per-generation `git.branch` tag (tags.ts) is resolved per
// turn and does follow the checkout.
export function resolveAutoTagValues(
  cwd: string,
): Record<string, string> | undefined {
  const { enabled } = selectAutoTags(lookupBrandedEnv, (message) =>
    console.warn(`[sigil-opencode] ${message}`),
  );
  if (enabled.size === 0) return undefined;
  return resolveAutoTags(enabled, {
    cwd,
    userId: brandedEnv("USER_ID"),
    explicitTags: parseTagPairs(brandedEnv("TAGS")),
  });
}

function resolveAuth(): Agento11yAuthConfig {
  const tenant = brandedEnv("AUTH_TENANT_ID") ?? "";
  const token = lookupBrandedEnv("AUTH_TOKEN");
  if (tenant && token) {
    return {
      mode: "basic",
      basicUser: tenant,
      basicPassword: token.value,
      tenantId: tenant,
    };
  }
  if (tenant) {
    return { mode: "tenant", tenantId: tenant };
  }
  if (token) {
    const tenantKey = token.key.replace(/AUTH_TOKEN$/, "AUTH_TENANT_ID");
    console.warn(
      `[sigil-opencode] ${token.key} is set but ${tenantKey} is missing — auth disabled`,
    );
  }
  return { mode: "none" };
}

function resolveOtlp(): OtlpConfig | undefined {
  const endpoint =
    brandedEnv("OTEL_EXPORTER_OTLP_ENDPOINT") ??
    (env("OTEL_EXPORTER_OTLP_ENDPOINT") ?? "").trim();
  if (!endpoint) return undefined;

  const tenant = brandedEnv("AUTH_TENANT_ID") ?? "";
  const token = brandedEnv("OTEL_AUTH_TOKEN") ?? brandedEnv("AUTH_TOKEN") ?? "";
  return { endpoint, headers: otlpHeaders(tenant, token) };
}

// Configured OTLP headers plus a basic Authorization header built from the
// resolved credentials, unless the user already supplied one.
function otlpHeaders(tenant: string, token: string): Record<string, string> {
  const headers = parseOtelHeaders(env("OTEL_EXPORTER_OTLP_HEADERS") ?? "");
  if (tenant && token && !hasAuthorizationHeader(headers)) {
    headers.Authorization = `Basic ${Buffer.from(`${tenant}:${token}`).toString("base64")}`;
  }
  return headers;
}

function parseOtelHeaders(raw: string): Record<string, string> {
  const headers: Record<string, string> = {};
  for (const pair of raw.split(",")) {
    const eq = pair.indexOf("=");
    if (eq <= 0) continue;
    const key = pair.slice(0, eq).trim();
    const value = pair.slice(eq + 1).trim();
    if (key && value) headers[key] = value;
  }
  return headers;
}

function hasAuthorizationHeader(headers: Record<string, string>): boolean {
  return Object.keys(headers).some(
    (key) => key.trim().toLowerCase() === "authorization",
  );
}

function resolveContentCapture(): ContentCaptureMode {
  const resolved = lookupBrandedEnv("CONTENT_CAPTURE_MODE");
  if (resolved !== undefined) {
    return parseContentCaptureMode(resolved.value, resolved.key);
  }
  return "metadata_only";
}

// Modes the parser passes through to the SDK verbatim. "default" is
// intentionally absent: it is collapsed to "metadata_only" in
// parseContentCaptureMode before the includes check, matching the canonical
// Go envconfig.ResolveContentMode. If "default" were listed here, removing
// the early-return would silently let the literal reach the SDK, which would
// then resolve it to "no_tool_content" via its client-level default.
const VALID_CAPTURE_MODES: ContentCaptureMode[] = [
  "full",
  "no_tool_content",
  "metadata_only",
  "full_with_metadata_spans",
];

function parseContentCaptureMode(
  value: string,
  key: string,
): ContentCaptureMode {
  const normalized = value.trim().toLowerCase();
  if (["1", "true", "yes", "on"].includes(normalized)) return "full";
  if (["0", "false", "no", "off"].includes(normalized)) return "metadata_only";
  // Resolve "default" inside the plugin: the SDK's client-level default would
  // otherwise map it to "no_tool_content", which differs from the Go binary.
  if (normalized === "default") return "metadata_only";
  if (VALID_CAPTURE_MODES.includes(normalized as ContentCaptureMode)) {
    return normalized as ContentCaptureMode;
  }
  console.warn(
    `[sigil-opencode] unsupported contentCapture value "${value}" for ${key}, defaulting to metadata_only`,
  );
  return "metadata_only";
}

function env(key: string): string | undefined {
  const v = process.env[key];
  return v !== undefined && v !== "" ? v : undefined;
}

interface BrandedValue {
  value: string;
  key: string;
}

// lookupBrandedEnv selects a branded variable's first nonblank spelling
// (preferred AGENTO11Y_<suffix>, then legacy SIGIL_<suffix>) and returns the
// trimmed value with the env-var name it came from, so warnings can name the
// key the user actually set. Blank or whitespace-only values are treated as
// unset. Selection happens before parsing: an invalid selected value never
// falls back to the other spelling.
function lookupBrandedEnv(suffix: string): BrandedValue | undefined {
  for (const key of [`AGENTO11Y_${suffix}`, `SIGIL_${suffix}`]) {
    const raw = process.env[key];
    if (raw !== undefined && raw.trim() !== "") {
      return { value: raw.trim(), key };
    }
  }
  return undefined;
}

function brandedEnv(suffix: string): string | undefined {
  return lookupBrandedEnv(suffix)?.value;
}

// brandedBool parses a boolean config key and warns when the value is outside
// the accepted set, so a typo is visible instead of silently falling back to
// the default. Matches brandedPositiveInt, the pi plugin, and envconfig.BoolValue.
function brandedBool(suffix: string): boolean | undefined {
  const found = lookupBrandedEnv(suffix);
  if (found === undefined) return undefined;
  const parsed = toBool(found.value);
  if (parsed === undefined) {
    console.warn(
      `[sigil-opencode] invalid boolean value for ${found.key}: "${found.value}" - using default`,
    );
  }
  return parsed;
}

export function resolveGuards(): GuardsFeatureConfig {
  return {
    enabled: brandedBool("GUARDS_ENABLED") ?? false,
    timeoutMs: brandedPositiveInt("GUARDS_TIMEOUT_MS") ?? 1500,
    failOpen: brandedBool("GUARDS_FAIL_OPEN") ?? true,
  };
}

function brandedPositiveInt(suffix: string): number | undefined {
  const found = lookupBrandedEnv(suffix);
  if (found === undefined) return undefined;
  const n = Number(found.value);
  if (Number.isInteger(n) && n > 0) return n;
  console.warn(
    `[sigil-opencode] invalid integer value for ${found.key}: "${found.value}" - using default`,
  );
  return undefined;
}

function toBool(v: unknown): boolean | undefined {
  if (typeof v === "boolean") return v;
  if (typeof v !== "string") return undefined;

  const normalized = v.trim().toLowerCase();
  if (["1", "true", "yes", "on"].includes(normalized)) return true;
  if (["0", "false", "no", "off"].includes(normalized)) return false;

  return undefined;
}

// Re-exported so client.ts and the config tests keep one import for the
// config they read and the endpoint helpers that go with it.
export { EXPORT_PATH, normalizeBaseEndpoint } from "./endpoint.js";
