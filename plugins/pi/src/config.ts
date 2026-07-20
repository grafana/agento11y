import type { ContentCaptureMode } from "@grafana/agento11y";
import { applyAgento11yDotenv } from "./agento11yDotenv.js";
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
}

export async function loadConfig(): Promise<Agento11yPiConfig | null> {
  // Read the shared sigil dotenv file so plain `pi` and `agento11y pi --` resolve
  // credentials from the same place. Shell values in process.env always beat
  // config.env values, across both env-var spellings.
  applyAgento11yDotenv();
  return resolveConfig();
}

export function resolveConfig(): Agento11yPiConfig | null {
  const endpoint = normalizeBaseEndpoint(brandedEnv("ENDPOINT")?.value ?? "");
  if (!endpoint) return null;

  const agentName = brandedEnv("AGENT_NAME")?.value ?? "pi";
  const agentVersion = brandedEnv("AGENT_VERSION")?.value;

  const contentCapture = resolveContentCapture();
  const redactInputMessages = envBoolOr("REDACT_INPUT_MESSAGES", true);

  return {
    endpoint,
    auth: resolveAuth(),
    agentName,
    agentVersion,
    contentCapture,
    redactInputMessages,
    otlp: resolveOtlp(),
    guards: resolveGuards(),
  };
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

  const headers = parseOtelHeaders(env("OTEL_EXPORTER_OTLP_HEADERS") ?? "");
  const tenant = brandedEnv("AUTH_TENANT_ID")?.value ?? "";
  const token =
    brandedEnv("OTEL_AUTH_TOKEN")?.value ??
    brandedEnv("AUTH_TOKEN")?.value ??
    "";
  if (tenant && token && !hasAuthorizationHeader(headers)) {
    headers.Authorization = `Basic ${Buffer.from(`${tenant}:${token}`).toString("base64")}`;
  }
  return { endpoint, headers };
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

export const EXPORT_PATH = "/api/v1/generations:export";

/**
 * Normalize a Sigil endpoint to the bare API base URL. Accepts either the
 * base URL (`https://host` or `https://host/prefix`) or a full generations
 * export URL (`https://host/api/v1/generations:export`) — the latter is a
 * common copy-paste mistake. Trailing slashes are stripped. The export path
 * is reapplied in `client.ts` when constructing the generationExport URL.
 */
function normalizeBaseEndpoint(endpoint: string): string {
  if (!endpoint) return "";
  try {
    const url = new URL(endpoint);
    let pathname = url.pathname.replace(/\/+$/, "");
    if (pathname.endsWith(EXPORT_PATH)) {
      pathname = pathname.slice(0, pathname.length - EXPORT_PATH.length);
    }
    url.pathname = pathname;
    return url.toString().replace(/\/+$/, "");
  } catch {
    const trimmed = endpoint.replace(/\/+$/, "");
    return trimmed.endsWith(EXPORT_PATH)
      ? trimmed.slice(0, trimmed.length - EXPORT_PATH.length)
      : trimmed;
  }
}
