import {
  redactSecretText,
  redactSecretTextLightweight,
} from "@grafana/agento11y";

const JSON_REDACTION_MARKER = '"[REDACTED:json]"';
export const MAX_TOOL_INPUT_BYTES = 4096;
const MAX_TOOL_RESULT_BYTES = 1 << 20;

export function redactTitle(text: string): string {
  return redactSecretTextLightweight(text);
}

export function redactError(error: unknown): Error {
  const message = error instanceof Error ? error.message : String(error);
  return new Error(redactSecretTextLightweight(message));
}

export function redactFullText(text: string): string {
  return redactSecretText(text);
}

export function redactToolResult(text: string): string {
  return clipText(redactFullText(text), MAX_TOOL_RESULT_BYTES);
}

export function redactToolJSON(raw: string, maxBytes?: number): string {
  const redacted = redactSecretText(raw);
  if (isJSON(raw) && !isJSON(redacted)) return JSON_REDACTION_MARKER;
  if (maxBytes !== undefined && Buffer.byteLength(redacted) > maxBytes) {
    return JSON.stringify(clipText(redacted, maxBytes));
  }
  return redacted;
}

export function encodeAndRedactToolJSON(
  value: unknown,
  maxBytes?: number,
): string | undefined {
  try {
    const encoded = JSON.stringify(value);
    return encoded === undefined
      ? undefined
      : redactToolJSON(encoded, maxBytes);
  } catch {
    return undefined;
  }
}

// The SDK measures JSON-encoded bytes, including control-character escapes.
function clipText(text: string, limit: number): string {
  let bytes = 0;
  let end = 0;
  for (const character of text) {
    const size = Buffer.byteLength(JSON.stringify(character)) - 2;
    if (bytes + size > limit) return `${text.slice(0, end)} [truncated]`;
    bytes += size;
    end += character.length;
  }
  return text;
}

function isJSON(value: string): boolean {
  try {
    JSON.parse(value);
    return true;
  } catch {
    return false;
  }
}
