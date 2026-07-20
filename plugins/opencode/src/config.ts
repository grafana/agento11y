import type { ContentCaptureMode } from "@grafana/agento11y";
import { applySigilDotenv } from "./sigilDotenv.js";

export type SigilAuthConfig =
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

export interface SigilOpencodeConfig {
  endpoint: string;
  auth: SigilAuthConfig;
  agentName: string;
  agentVersion?: string;
  contentCapture: ContentCaptureMode;
  debug: boolean;
  guards?: GuardsFeatureConfig;
  otlp?: OtlpConfig;
}

export async function loadConfig(): Promise<SigilOpencodeConfig | null> {
  // Read the shared sigil dotenv file so the OpenCode plugin and every other
  // Sigil agent resolve credentials from the same place. Shell env values win
  // over file values for each alias family, and the winner is materialized
  // under both spellings; see applySigilDotenv.
  applySigilDotenv();
  return resolveConfig();
}

export function resolveConfig(): SigilOpencodeConfig | null {
  const endpoint = normalizeBaseEndpoint(brandedEnv("ENDPOINT") ?? "");
  if (!endpoint) return null;

  const agentName = brandedEnv("AGENT_NAME") ?? "opencode";
  const agentVersion = brandedEnv("AGENT_VERSION");

  const contentCapture = resolveContentCapture();
  const debug = brandedBool("DEBUG") ?? false;

  return {
    endpoint,
    auth: resolveAuth(),
    agentName,
    agentVersion,
    contentCapture,
    debug,
    guards: resolveGuards(),
    otlp: resolveOtlp(),
  };
}

function resolveAuth(): SigilAuthConfig {
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

  const headers = parseOtelHeaders(env("OTEL_EXPORTER_OTLP_HEADERS") ?? "");
  const tenant = brandedEnv("AUTH_TENANT_ID") ?? "";
  const token = brandedEnv("OTEL_AUTH_TOKEN") ?? brandedEnv("AUTH_TOKEN") ?? "";
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

function brandedBool(suffix: string): boolean | undefined {
  const v = brandedEnv(suffix);
  return v !== undefined ? toBool(v) : undefined;
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

export const EXPORT_PATH = "/api/v1/generations:export";

/**
 * Normalize a Sigil endpoint to the bare API base URL. Accepts either the
 * base URL (`https://host` or `https://host/prefix`) or a full generations
 * export URL (`https://host/api/v1/generations:export`) — the latter is a
 * common copy-paste mistake. Trailing slashes are stripped. The export path
 * is reapplied in `client.ts` when constructing the generationExport URL.
 */
export function normalizeBaseEndpoint(endpoint: string): string {
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
