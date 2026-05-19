import { readFile } from "node:fs/promises";
import { homedir } from "node:os";
import { join } from "node:path";
import type { ContentCaptureMode } from "@grafana/sigil-sdk-js";

export type SigilAuthConfig =
  | { mode: "bearer"; bearerToken: string }
  | { mode: "tenant"; tenantId: string }
  | { mode: "basic"; user: string; password: string; tenantId: string }
  | { mode: "none" };

export interface OtlpConfig {
  endpoint: string;
  headers: Record<string, string>;
}

export interface RedactionConfig {
  enabled: boolean;
  redactInputMessages: boolean;
  redactEmailAddresses: boolean;
}

export interface SigilPiConfig {
  endpoint: string;
  auth: SigilAuthConfig;
  agentName: string;
  agentVersion?: string;
  contentCapture: ContentCaptureMode;
  debug: boolean;
  otlp?: OtlpConfig;
  redaction: RedactionConfig;
}

const CONFIG_PATH = join(homedir(), ".config", "sigil-pi", "config.json");

export async function loadConfig(): Promise<SigilPiConfig | null> {
  let fileConfig: Record<string, unknown> = {};
  try {
    const raw = await readFile(CONFIG_PATH, "utf-8");
    fileConfig = parseConfig(raw);
  } catch (err) {
    if (!isMissingFileError(err)) {
      console.warn(
        "[sigil-pi] failed to read config file, falling back to env vars:",
        err,
      );
    }
  }

  return resolveConfig(fileConfig);
}

export function resolveConfig(
  file: Record<string, unknown>,
): SigilPiConfig | null {
  // Canonical SIGIL_* env vars (shared with claude-code/codex/cursor) take
  // precedence over the legacy SIGIL_PI_* prefix so a single config.env can
  // drive every agent. The pi-specific names remain as a fallback so users
  // who already configured them keep working.
  const endpoint = ensureExportPath(
    (
      env("SIGIL_ENDPOINT") ??
      env("SIGIL_PI_ENDPOINT") ??
      asString(file.endpoint) ??
      ""
    ).trim(),
  );
  if (!endpoint) return null;

  const auth = resolveAuth(file);
  if (!auth) return null;

  const configuredAgentName = (
    env("SIGIL_AGENT_NAME") ??
    env("SIGIL_PI_AGENT_NAME") ??
    asString(file.agentName) ??
    "pi"
  ).trim();
  const agentName = configuredAgentName.length > 0 ? configuredAgentName : "pi";

  const configuredAgentVersion = (
    env("SIGIL_AGENT_VERSION") ??
    env("SIGIL_PI_AGENT_VERSION") ??
    asString(file.agentVersion) ??
    ""
  ).trim();
  const agentVersion =
    configuredAgentVersion.length > 0 ? configuredAgentVersion : undefined;

  const contentCapture = resolveContentCapture(file);

  const debug =
    envBool("SIGIL_DEBUG") ??
    envBool("SIGIL_PI_DEBUG") ??
    toBool(file.debug) ??
    false;

  const otlp = resolveOtlp(file);
  const redaction = resolveRedaction(file);

  return {
    endpoint,
    auth,
    agentName,
    agentVersion,
    contentCapture,
    debug,
    otlp,
    redaction,
  };
}

function resolveRedaction(file: Record<string, unknown>): RedactionConfig {
  const redactionObj = (file.redaction ?? {}) as Record<string, unknown>;

  const enabled =
    envBool("SIGIL_PI_REDACTION_ENABLED") ??
    toBool(redactionObj.enabled) ??
    true;

  const redactInputMessages =
    envBool("SIGIL_PI_REDACT_INPUT_MESSAGES") ??
    toBool(redactionObj.redactInputMessages) ??
    true;

  const redactEmailAddresses =
    envBool("SIGIL_PI_REDACT_EMAIL_ADDRESSES") ??
    toBool(redactionObj.redactEmailAddresses) ??
    true;

  return { enabled, redactInputMessages, redactEmailAddresses };
}

function resolveOtlp(file: Record<string, unknown>): OtlpConfig | undefined {
  const otlpObj = (file.otlp ?? {}) as Record<string, unknown>;

  // Endpoint precedence: canonical sigil override > standard OTel env var >
  // legacy SIGIL_PI_OTLP_ENDPOINT > file config.
  const endpoint = (
    env("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT") ??
    env("OTEL_EXPORTER_OTLP_ENDPOINT") ??
    env("SIGIL_PI_OTLP_ENDPOINT") ??
    asString(otlpObj.endpoint) ??
    ""
  ).trim();
  if (!endpoint) return undefined;

  const headers: Record<string, string> = {};

  // Support explicit headers object
  if (otlpObj.headers && typeof otlpObj.headers === "object") {
    for (const [k, v] of Object.entries(
      otlpObj.headers as Record<string, unknown>,
    )) {
      if (typeof v === "string") {
        headers[k] = resolveEnvVars(v);
      }
    }
  }

  // Support basic auth shorthand (Grafana Cloud pattern)
  const basicUser = resolveEnvVars(
    env("SIGIL_PI_OTLP_BASIC_USER") ?? asString(otlpObj.basicUser) ?? "",
  ).trim();
  const basicPassword = resolveEnvVars(
    env("SIGIL_PI_OTLP_BASIC_PASSWORD") ??
      asString(otlpObj.basicPassword) ??
      "",
  ).trim();

  if (basicUser && basicPassword && !headers.Authorization) {
    const encoded = Buffer.from(`${basicUser}:${basicPassword}`).toString(
      "base64",
    );
    headers.Authorization = `Basic ${encoded}`;
  }

  const bearerToken = resolveEnvVars(
    env("SIGIL_PI_OTLP_BEARER_TOKEN") ?? asString(otlpObj.bearerToken) ?? "",
  ).trim();
  if (bearerToken && !headers.Authorization) {
    headers.Authorization = `Bearer ${bearerToken}`;
  }

  // Final fallback: synthesise Basic auth from the canonical SIGIL_* creds,
  // matching the consolidated sigil binary's behaviour. SIGIL_OTEL_AUTH_TOKEN
  // overrides the auth token for OTel only (parity with sigil README).
  if (!headers.Authorization) {
    const canonicalTenant = (env("SIGIL_AUTH_TENANT_ID") ?? "").trim();
    const canonicalToken = (
      env("SIGIL_OTEL_AUTH_TOKEN") ??
      env("SIGIL_AUTH_TOKEN") ??
      ""
    ).trim();
    if (canonicalTenant && canonicalToken) {
      const encoded = Buffer.from(
        `${canonicalTenant}:${canonicalToken}`,
      ).toString("base64");
      headers.Authorization = `Basic ${encoded}`;
    }
  }

  return { endpoint, headers };
}

const VALID_CAPTURE_MODES: ContentCaptureMode[] = [
  "full",
  "no_tool_content",
  "metadata_only",
];

function resolveContentCapture(
  file: Record<string, unknown>,
): ContentCaptureMode {
  // Canonical name (shared with other sigil adapters) wins over the legacy
  // pi-specific one.
  const envVal =
    env("SIGIL_CONTENT_CAPTURE_MODE") ?? env("SIGIL_PI_CONTENT_CAPTURE");
  if (envVal !== undefined) {
    return parseContentCaptureMode(envVal);
  }
  if (file.contentCapture !== undefined) {
    if (typeof file.contentCapture === "boolean") {
      return file.contentCapture ? "full" : "metadata_only";
    }
    if (typeof file.contentCapture === "string") {
      return parseContentCaptureMode(file.contentCapture);
    }
  }
  return "metadata_only";
}

function parseContentCaptureMode(value: string): ContentCaptureMode {
  const normalized = value.trim().toLowerCase();
  if (["1", "true", "yes", "on"].includes(normalized)) return "full";
  if (["0", "false", "no", "off"].includes(normalized)) return "metadata_only";
  if (VALID_CAPTURE_MODES.includes(normalized as ContentCaptureMode)) {
    return normalized as ContentCaptureMode;
  }
  console.warn(
    `[sigil-pi] unsupported contentCapture value "${value}", defaulting to metadata_only`,
  );
  return "metadata_only";
}

function resolveAuth(file: Record<string, unknown>): SigilAuthConfig | null {
  const explicitMode =
    env("SIGIL_PI_AUTH_MODE") ??
    asString((file.auth as Record<string, unknown> | undefined)?.mode);

  // Canonical SIGIL_AUTH_TENANT_ID + SIGIL_AUTH_TOKEN pattern: when neither
  // the env nor the file picks an explicit auth mode, treat the canonical
  // pair as Grafana Cloud basic auth (tenant as user, token as password).
  // This matches the implicit contract used by claude-code/codex/cursor.
  if (explicitMode === undefined) {
    const canonicalTenant = (env("SIGIL_AUTH_TENANT_ID") ?? "").trim();
    const canonicalToken = (env("SIGIL_AUTH_TOKEN") ?? "").trim();
    if (canonicalTenant && canonicalToken) {
      return {
        mode: "basic",
        user: canonicalTenant,
        password: canonicalToken,
        tenantId: canonicalTenant,
      };
    }
  }

  const mode = (explicitMode ?? "none").trim().toLowerCase();

  const authObj = (file.auth ?? {}) as Record<string, unknown>;

  switch (mode) {
    case "bearer": {
      const token = resolveEnvVars(
        env("SIGIL_PI_BEARER_TOKEN") ?? asString(authObj.bearerToken) ?? "",
      ).trim();
      if (!token) {
        console.warn(
          "[sigil-pi] auth mode bearer requires bearerToken — disabling",
        );
        return null;
      }
      return { mode: "bearer", bearerToken: token };
    }
    case "tenant": {
      const tenantId = resolveEnvVars(
        env("SIGIL_PI_TENANT_ID") ?? asString(authObj.tenantId) ?? "",
      ).trim();
      if (!tenantId) {
        console.warn(
          "[sigil-pi] auth mode tenant requires tenantId — disabling",
        );
        return null;
      }
      return { mode: "tenant", tenantId };
    }
    case "basic": {
      const user = resolveEnvVars(
        env("SIGIL_PI_BASIC_USER") ?? asString(authObj.user) ?? "",
      ).trim();
      const password = resolveEnvVars(
        env("SIGIL_PI_BASIC_PASSWORD") ?? asString(authObj.password) ?? "",
      ).trim();

      if (!user || !password) {
        console.warn(
          "[sigil-pi] auth mode basic requires user and password — disabling",
        );
        return null;
      }

      const tenantId =
        resolveEnvVars(
          env("SIGIL_PI_TENANT_ID") ?? asString(authObj.tenantId) ?? "",
        ).trim() || user;

      return {
        mode: "basic",
        user,
        password,
        tenantId,
      };
    }
    case "none":
      return { mode: "none" };
    default:
      console.warn(`[sigil-pi] unsupported auth mode "${mode}" — disabling`);
      return null;
  }
}

export function resolveEnvVars(value: string): string {
  return value.replace(/\$\{(\w+)\}/g, (_match, name) => {
    const resolved = process.env[name as string];
    if (resolved === undefined) {
      console.warn(
        `[sigil-pi] env var \${${name}} is not set, resolving to empty string`,
      );
    }
    return resolved ?? "";
  });
}

function env(key: string): string | undefined {
  const v = process.env[key];
  return v !== undefined && v !== "" ? v : undefined;
}

function envBool(key: string): boolean | undefined {
  const v = env(key);
  return v !== undefined ? toBool(v) : undefined;
}

function asString(v: unknown): string | undefined {
  return typeof v === "string" ? v : undefined;
}

function toBool(v: unknown): boolean | undefined {
  if (typeof v === "boolean") return v;
  if (typeof v !== "string") return undefined;

  const normalized = v.trim().toLowerCase();
  if (["1", "true", "yes", "on"].includes(normalized)) return true;
  if (["0", "false", "no", "off"].includes(normalized)) return false;

  return undefined;
}

function parseConfig(raw: string): Record<string, unknown> {
  const parsed: unknown = JSON.parse(raw);
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new Error("config must be a JSON object");
  }
  return parsed as Record<string, unknown>;
}

const EXPORT_PATH = "/api/v1/generations:export";

/**
 * Normalize a Sigil endpoint by appending `/api/v1/generations:export` unless
 * the path already ends with it. URL parsing handles trailing slashes and
 * query strings; non-URL inputs fall through and let the SDK surface the
 * failure later.
 */
function ensureExportPath(endpoint: string): string {
  if (!endpoint) return "";
  let url: URL;
  try {
    url = new URL(endpoint);
  } catch {
    return endpoint.replace(/\/+$/, "") + EXPORT_PATH;
  }
  const cleanPath = url.pathname.replace(/\/+$/, "");
  if (cleanPath.endsWith(EXPORT_PATH)) return endpoint;
  url.pathname = cleanPath + EXPORT_PATH;
  return url.toString();
}

function isMissingFileError(err: unknown): boolean {
  return (
    typeof err === "object" &&
    err !== null &&
    "code" in err &&
    (err as { code?: string }).code === "ENOENT"
  );
}
