import {
  redactSecretText,
  redactSecretTextLightweight,
} from "@grafana/agento11y-core";
import type { Redactor } from "./mappers.js";

/**
 * Builds the redactor the mappers and hooks share.
 *
 * opencode redacts inside its own mappers rather than through the SDK's
 * generation sanitizer, so it calls the shared string helpers from
 * `@grafana/agento11y-core`, which carry the patterns generated from
 * `redaction/patterns.json`.
 *
 * Email redaction is off: agent transcripts routinely carry commit authors and
 * reviewer addresses, and redacting them costs more context than it protects.
 * See `redaction/README.md`.
 */
export function createRedactor(): Redactor {
  return {
    redact: (text: string) =>
      redactSecretText(text, { redactEmailAddresses: false }),
    redactLightweight: (text: string) =>
      redactSecretTextLightweight(text, { redactEmailAddresses: false }),
  };
}
