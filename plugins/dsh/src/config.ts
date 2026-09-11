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

export interface Agento11yDshConfig {
  endpoint: string;
  auth: Agento11yAuthConfig;
  agentName: string;
  agentVersion?: string;
  contentCapture: ContentCaptureMode;
  redactInputMessages: boolean;
  otlp?: OtlpConfig;
  autoTags?: Record<string, string>;
  /** Internal destination marker; never passed to the SDK or exported. */
  local?: boolean;
}

export async function loadConfig(): Promise<Agento11yDshConfig | null> {
  applyAgento11yDotenv();
  // Local mode fails closed instead of falling back to Cloud.
  if (!envBoolOr("LOCAL", false)) return resolveConfig();
  return resolveLocalConfig(await resolveLocalReceiver());
}

// The receiver ignores credentials, but export paths reject empty values.
const LOCAL_AUTH_PLACEHOLDER = "local";

/**
 * Must match `plugins/agento11y/internal/local/env.go::LaunchEnv.Apply`:
 * receiver endpoints override Cloud, capture is full, and each missing
 * credential gets a placeholder. Keep overrides out of `process.env` because
 * one dsh process can serve later non-local sessions.
 */
export function resolveLocalConfig(
  receiver: LocalReceiver,
): Agento11yDshConfig {
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

export function resolveConfig(): Agento11yDshConfig | null {
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

function resolveSharedConfig(): Omit<
  Agento11yDshConfig,
  "endpoint" | "auth" | "contentCapture" | "otlp"
> {
  return {
    agentName: brandedEnv("AGENT_NAME")?.value ?? "dsh",
    agentVersion: brandedEnv("AGENT_VERSION")?.value,
    redactInputMessages: envBoolOr("REDACT_INPUT_MESSAGES", true),
    autoTags: resolveAutoTagValues(),
  };
}

// Auto-tags freeze at the first model call because config loads once per dsh
// process. Under `dsh web`, they describe the server cwd, not each session.
// Per-generation tags still use the session header.
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
  // A blank branded endpoint is unset, allowing the standard OTEL endpoint.
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
    `${key} has unsupported content capture mode "${value}"; using metadata_only`,
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

// Resolve the first nonblank AGENTO11Y_<suffix>, then SIGIL_<suffix>.
// Selection precedes parsing, so an invalid preferred value never falls back.
// Return the selected spelling for diagnostics.
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
      `invalid boolean value for ${resolved.key}: "${resolved.value}"; using default ${defaultValue}`,
    );
    return defaultValue;
  }
  return parsed;
}

function toBool(v: unknown): boolean | undefined {
  if (typeof v === "boolean") return v;
  if (typeof v !== "string") return undefined;

  const normalized = v.trim().toLowerCase();
  if (["1", "true", "yes", "on"].includes(normalized)) return true;
  if (["0", "false", "no", "off"].includes(normalized)) return false;

  return undefined;
}

export { EXPORT_PATH } from "./endpoint.js";
