import type { ContentCaptureMode } from "@grafana/agento11y";
import { applyAgento11yDotenv } from "./agento11yDotenv.js";
import { parseTagPairs, resolveAutoTags, selectAutoTags } from "./autotag.js";
import { normalizeBaseEndpoint } from "./endpoint.js";
import { type LocalReceiver, resolveLocalReceiver } from "./local.js";
import { logger } from "./logger.js";

export type Agento11yAuthConfig =
  | {
      mode: "basic";
      basicUser: string;
      basicPassword: string;
      tenantId: string;
    }
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

export interface Agento11yPiConfig {
  endpoint: string;
  auth: Agento11yAuthConfig;
  agentName: string;
  agentVersion?: string;
  contentCapture: ContentCaptureMode;
  redactInputMessages: boolean;
  otlp?: OtlpConfig;
  guards: GuardsFeatureConfig;
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

export async function loadConfig(): Promise<Agento11yPiConfig | null> {
  // Read the shared agento11y dotenv file so plain `pi` and `agento11y pi --` resolve
  // credentials from the same place. Shell values in process.env always beat
  // config.env values, across both env-var spellings.
  applyAgento11yDotenv();
  // A saved AGENTO11Y_LOCAL=true means this session records to the machine,
  // whatever Cloud endpoint config.env also holds. Resolving the receiver can
  // fail; the caller reports that and captures nothing rather than falling
  // back to Cloud.
  if (!envBoolOr("LOCAL", false)) return resolveConfig();
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
 * Nothing here is written back to `process.env`. Pi reloads config on every
 * session start, so an override that leaked into the environment would follow
 * a later non-local session.
 */
export function resolveLocalConfig(receiver: LocalReceiver): Agento11yPiConfig {
  const tenantId =
    brandedEnv("AUTH_TENANT_ID")?.value ?? LOCAL_AUTH_PLACEHOLDER;
  const token = brandedEnv("AUTH_TOKEN")?.value ?? LOCAL_AUTH_PLACEHOLDER;
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
      headers: otlpHeaders(
        tenantId,
        brandedEnv("OTEL_AUTH_TOKEN")?.value ?? token,
      ),
    },
    local: true,
  };
}

export function resolveConfig(): Agento11yPiConfig | null {
  const endpoint = normalizeBaseEndpoint(brandedEnv("ENDPOINT")?.value ?? "");
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
  Agento11yPiConfig,
  "endpoint" | "auth" | "contentCapture" | "otlp"
> {
  return {
    agentName: brandedEnv("AGENT_NAME")?.value ?? "pi",
    agentVersion: brandedEnv("AGENT_VERSION")?.value,
    redactInputMessages: envBoolOr("REDACT_INPUT_MESSAGES", true),
    guards: resolveGuards(),
    autoTags: resolveAutoTagValues(),
  };
}

// resolveAutoTagValues reads AGENTO11Y_AUTO_CODING_AGENT_TAGS and the optional
// AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES allowlist, then resolves whatever they
// enable. The switch is off by default, so a session that has not opted in
// carries exactly the tags it carried before.
//
// The plugin builds one client per pi session, so these values freeze at
// session start: a checkout that changes mid-session keeps the metric label it
// started with. The per-generation `git.branch` tag (tags.ts) is resolved per
// turn and does follow the checkout.
function resolveAutoTagValues(): Record<string, string> | undefined {
  const { enabled } = selectAutoTags(brandedEnv, (message) =>
    logger.warn(message),
  );
  if (enabled.size === 0) return undefined;
  return resolveAutoTags(enabled, {
    cwd: process.cwd(),
    userId: brandedEnv("USER_ID")?.value,
    explicitTags: parseTagPairs(brandedEnv("TAGS")?.value),
  });
}

function resolveAuth(): Agento11yAuthConfig {
  const tenant = brandedEnv("AUTH_TENANT_ID")?.value ?? "";
  const token = brandedEnv("AUTH_TOKEN")?.value ?? "";
  if (tenant && token) {
    return {
      mode: "basic",
      basicUser: tenant,
      basicPassword: token,
      tenantId: tenant,
    };
  }
  return { mode: "none" };
}

function resolveOtlp(): OtlpConfig | undefined {
  // brandedEnv treats whitespace-only values as unset, so a blank branded
  // endpoint (either spelling) falls through to the standard
  // OTEL_EXPORTER_OTLP_ENDPOINT instead of suppressing it.
  const endpoint =
    brandedEnv("OTEL_EXPORTER_OTLP_ENDPOINT")?.value ??
    (env("OTEL_EXPORTER_OTLP_ENDPOINT") ?? "").trim();
  if (!endpoint) return undefined;

  const tenant = brandedEnv("AUTH_TENANT_ID")?.value ?? "";
  const token =
    brandedEnv("OTEL_AUTH_TOKEN")?.value ??
    brandedEnv("AUTH_TOKEN")?.value ??
    "";
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
  const resolved = brandedEnv("CONTENT_CAPTURE_MODE");
  if (resolved !== undefined) {
    return parseContentCaptureMode(resolved.value, resolved.key);
  }
  return "metadata_only";
}

function resolveGuards(): GuardsFeatureConfig {
  return {
    enabled: envBoolOr("GUARDS_ENABLED", false),
    timeoutMs: envPositiveIntOr("GUARDS_TIMEOUT_MS", 1500),
    failOpen: envBoolOr("GUARDS_FAIL_OPEN", true),
  };
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
  logger.warn(
    `unsupported contentCapture value "${value}" for ${key}, defaulting to metadata_only`,
  );
  return "metadata_only";
}

function env(key: string): string | undefined {
  const v = process.env[key];
  return v !== undefined && v !== "" ? v : undefined;
}

interface BrandedEnv {
  value: string;
  key: string;
}

// brandedEnv resolves one alias family from the process env: the first
// nonblank of AGENTO11Y_<suffix>, SIGIL_<suffix>. Blank or whitespace-only
// values count as unset. The returned key names the spelling the value came
// from so warnings can report what the user actually set. Selection happens
// before parsing: an invalid selected value never falls back to the other
// spelling.
function brandedEnv(suffix: string): BrandedEnv | undefined {
  for (const key of [`AGENTO11Y_${suffix}`, `SIGIL_${suffix}`]) {
    const value = (process.env[key] ?? "").trim();
    if (value !== "") return { value, key };
  }
  return undefined;
}

function envBoolOr(suffix: string, defaultValue: boolean): boolean {
  const resolved = brandedEnv(suffix);
  if (resolved === undefined) return defaultValue;
  const parsed = toBool(resolved.value);
  if (parsed === undefined) {
    logger.warn(
      `invalid boolean value for ${resolved.key}: "${resolved.value}" — using default ${defaultValue}`,
    );
    return defaultValue;
  }
  return parsed;
}

function envPositiveIntOr(suffix: string, defaultValue: number): number {
  // 0 is rejected: the SDK interprets timeoutMs <= 0 as "use built-in default
  // (15000ms)", which would silently override the plugin's documented 1500ms.
  const resolved = brandedEnv(suffix);
  if (resolved === undefined) return defaultValue;
  const n = Number(resolved.value);
  if (!Number.isFinite(n) || !Number.isInteger(n) || n <= 0) {
    logger.warn(
      `invalid integer value for ${resolved.key}: "${resolved.value}" — using default ${defaultValue}`,
    );
    return defaultValue;
  }
  return n;
}

function toBool(v: unknown): boolean | undefined {
  if (typeof v === "boolean") return v;
  if (typeof v !== "string") return undefined;

  const normalized = v.trim().toLowerCase();
  if (["1", "true", "yes", "on"].includes(normalized)) return true;
  if (["0", "false", "no", "off"].includes(normalized)) return false;

  return undefined;
}

// Re-exported so client.ts keeps one import for the config it reads and the
// path it appends to the endpoint.
export { EXPORT_PATH } from "./endpoint.js";
