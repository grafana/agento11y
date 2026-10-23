import type { Message } from "@grafana/agento11y";

/**
 * Text that a redact guard replaced in a session's outgoing requests, keyed by
 * the original text.
 *
 * Native guards rewrite the request opencode sends to the model, but native
 * capture records from the session's own events, which keep the original text.
 * Capture applies these replacements to what it exports, so the recorded input,
 * system prompt and title match what the model received.
 */
export interface GuardRedactions {
  /** Records every text part `redacted` changed from `sent` (index-aligned). */
  record(
    sessionID: string,
    sent: readonly Message[],
    redacted: readonly Message[],
  ): void;
  /** Applies the session's replacements to a string. */
  text(sessionID: string, value: string): string;
  /** Applies the session's replacements to every text part. */
  messages(sessionID: string, messages: readonly Message[]): Message[];
}

// Both bounds keep a long-running host from growing the ledger without limit.
const MAX_SESSIONS = 512;
const MAX_PAIRS_PER_SESSION = 256;

function textParts(message: Message | undefined): string[] {
  if (!message || typeof message !== "object") return [];
  if (typeof message.content === "string") return [message.content];
  return (message.parts ?? []).map((part) =>
    part.type === "text" ? part.text : "",
  );
}

export function createGuardRedactions(): GuardRedactions {
  const sessions = new Map<string, Map<string, string>>();

  const pairsFor = (sessionID: string): Map<string, string> => {
    let pairs = sessions.get(sessionID);
    if (pairs) {
      // Refresh recency so an active session outlives idle ones.
      sessions.delete(sessionID);
    } else {
      pairs = new Map();
    }
    sessions.set(sessionID, pairs);
    while (sessions.size > MAX_SESSIONS) {
      const oldest = sessions.keys().next().value;
      if (oldest === undefined) break;
      sessions.delete(oldest);
    }
    return pairs;
  };

  const text = (sessionID: string, value: string): string => {
    const pairs = sessions.get(sessionID);
    if (!pairs || value === "") return value;
    const exact = pairs.get(value);
    if (exact !== undefined) return exact;
    // The system prompt joins several instruction parts, so a part can sit
    // inside a longer recorded string.
    let out = value;
    for (const [before, after] of pairs) {
      if (out.includes(before)) out = out.split(before).join(after);
    }
    return out;
  };

  return {
    record(sessionID, sent, redacted) {
      if (!sessionID || sent.length !== redacted.length) return;
      let pairs: Map<string, string> | undefined;
      sent.forEach((message, index) => {
        const before = textParts(message);
        const after = textParts(redacted[index]);
        before.forEach((original, part) => {
          const replacement = after[part];
          if (
            !original ||
            replacement === undefined ||
            replacement === original
          )
            return;
          pairs ??= pairsFor(sessionID);
          pairs.delete(original);
          pairs.set(original, replacement);
          while (pairs.size > MAX_PAIRS_PER_SESSION) {
            const oldest = pairs.keys().next().value;
            if (oldest === undefined) break;
            pairs.delete(oldest);
          }
        });
      });
    },
    text,
    messages(sessionID, messages) {
      if (!sessions.has(sessionID)) return [...messages];
      return messages.map((message) => {
        if (typeof message.content === "string") {
          const content = text(sessionID, message.content);
          return content === message.content
            ? message
            : { ...message, content };
        }
        if (!message.parts) return message;
        let changed = false;
        const parts = message.parts.map((part) => {
          if (part.type !== "text") return part;
          const redacted = text(sessionID, part.text);
          if (redacted === part.text) return part;
          changed = true;
          return { ...part, text: redacted };
        });
        return changed ? { ...message, parts } : message;
      });
    },
  };
}
