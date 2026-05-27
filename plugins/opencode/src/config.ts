import type { ContentCaptureMode } from "@grafana/sigil-sdk-js";
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

export interface SigilOpencodeConfig {
  endpoint: string;
  auth: SigilAuthConfig;
  agentName: string;
  agentVersion?: string;
  contentCapture: ContentCaptureMode;
  debug: boolean;
}

export async function loadConfig(): Promise<SigilOpencodeConfig | null> {
  // Read the shared sigil dotenv file so the OpenCode plugin and every other
  // Sigil agent resolve credentials from the same place. Values in process.env
  // always win — applySigilDotenv only fills empty/whitespace entries.
  applySigilDotenv();
  return resolveConfig();
}

export function resolveConfig(): SigilOpencodeConfig | null {
  const endpoint = normalizeBaseEndpoint((env("SIGIL_ENDPOINT") ?? "").trim());
  if (!endpoint) return null;

  const configuredAgentName = (env("SIGIL_AGENT_NAME") ?? "opencode").trim();
  const agentName =
    configuredAgentName.length > 0 ? configuredAgentName : "opencode";

  const configuredAgentVersion = (env("SIGIL_AGENT_VERSION") ?? "").trim();
  const agentVersion =
    configuredAgentVersion.length > 0 ? configuredAgentVersion : undefined;

  const contentCapture = resolveContentCapture();
  const debug = envBool("SIGIL_DEBUG") ?? false;

  return {
    endpoint,
    auth: resolveAuth(),
    agentName,
    agentVersion,
    contentCapture,
    debug,
  };
}

function resolveAuth(): SigilAuthConfig {
  const tenant = (env("SIGIL_AUTH_TENANT_ID") ?? "").trim();
  const token = (env("SIGIL_AUTH_TOKEN") ?? "").trim();
  if (tenant && token) {
    return {
      mode: "basic",
      basicUser: tenant,
      basicPassword: token,
      tenantId: tenant,
    };
  }
  if (tenant) {
    return { mode: "tenant", tenantId: tenant };
  }
  if (token) {
    console.warn(
      "[sigil-opencode] SIGIL_AUTH_TOKEN is set but SIGIL_AUTH_TENANT_ID is missing — auth disabled",
    );
  }
  return { mode: "none" };
}

function resolveContentCapture(): ContentCaptureMode {
  const envVal = env("SIGIL_CONTENT_CAPTURE_MODE");
  if (envVal !== undefined) {
    return parseContentCaptureMode(envVal);
  }
  return "metadata_only";
}

const VALID_CAPTURE_MODES: ContentCaptureMode[] = [
  "full",
  "no_tool_content",
  "metadata_only",
];

function parseContentCaptureMode(value: string): ContentCaptureMode {
  const normalized = value.trim().toLowerCase();
  if (["1", "true", "yes", "on"].includes(normalized)) return "full";
  if (["0", "false", "no", "off"].includes(normalized)) return "metadata_only";
  if (VALID_CAPTURE_MODES.includes(normalized as ContentCaptureMode)) {
    return normalized as ContentCaptureMode;
  }
  console.warn(
    `[sigil-opencode] unsupported contentCapture value "${value}", defaulting to metadata_only`,
  );
  return "metadata_only";
}

function env(key: string): string | undefined {
  const v = process.env[key];
  return v !== undefined && v !== "" ? v : undefined;
}

function envBool(key: string): boolean | undefined {
  const v = env(key);
  return v !== undefined ? toBool(v) : undefined;
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
