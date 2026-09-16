import {
  redactSecretText,
  redactSecretTextLightweight,
} from "@grafana/agento11y";

const JSON_REDACTION_MARKER = '"[REDACTED:json]"';

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

/** Preserve valid JSON when redacting encoded tool payloads. */
export function redactToolJSON(raw: string): string {
  const redacted = redactSecretText(raw);
  if (!isJSON(raw) || isJSON(redacted)) return redacted;
  return JSON_REDACTION_MARKER;
}

export function encodeAndRedactToolJSON(value: unknown): string | undefined {
  try {
    const encoded = JSON.stringify(value);
    return encoded === undefined ? undefined : redactToolJSON(encoded);
  } catch {
    return undefined;
  }
}

function isJSON(value: string): boolean {
  try {
    JSON.parse(value);
    return true;
  } catch {
    return false;
  }
}
