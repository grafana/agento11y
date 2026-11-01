import type { Message } from "@grafana/agento11y";

/**
 * Text that a redact guard removed from a session's outgoing requests.
 *
 * Native guards rewrite the request opencode sends to the model, but native
 * capture records from the session's own events, which keep the original text.
 * Capture redacts what it exports with these spans, so the recorded input,
 * system prompt and title match what the model received.
 *
 * The ledger keeps the removed spans rather than original-to-redacted pairs.
 * A span still matches when the same value turns up inside a longer string, a
 * shortened or generated title, or text a later guard redacted again, and
 * redacting with spans doesn't depend on the order they were recorded in.
 */
export interface GuardRedactions {
  /** Records the text each part of `redacted` removed from `sent` (index-aligned). */
  record(
    sessionID: string,
    sent: readonly Message[],
    redacted: readonly Message[],
  ): void;
  /** Redacts the session's removed spans from a string. */
  text(sessionID: string, value: string): string;
  /**
   * Redacts the session's removed spans from a title. A title can stop partway
   * through a value, so one that ends in the start of a span is redacted too.
   */
  title(sessionID: string, value: string): string;
  /** Redacts the session's removed spans from every text part. */
  messages(sessionID: string, messages: readonly Message[]): Message[];
  /**
   * Drops a session's spans. Capture calls this when it drops the session's
   * state, which is the only other copy of the original text, so the ledger
   * needs no size bound of its own and never forgets a span capture can still
   * export.
   */
  forget(sessionID: string): void;
}

/** The token a redact guard writes in place of each match. */
const REDACTION_TOKEN = "[REDACTED]";

// The shortest start of a span that redacts the end of a cut-off title.
// Anything shorter is too likely to match by coincidence.
const MIN_CUT_OFF_PREFIX = 4;

function textParts(message: Message | undefined): string[] {
  if (!message || typeof message !== "object") return [];
  if (typeof message.content === "string") return [message.content];
  return (message.parts ?? []).map((part) =>
    part.type === "text" ? part.text : "",
  );
}

/**
 * Returns the text a redact guard removed when it turned `before` into
 * `after`. The guard replaces each match with a fixed token, so the literal
 * text between the tokens in `after` lines up with `before`, and what `before`
 * holds between those literals is what the guard removed. Text changed any
 * other way, including to an empty string, counts as removed whole.
 */
export function removedSpans(before: string, after: string): string[] {
  if (before === after) return [];
  const literals = after.split(REDACTION_TOKEN);
  const head = literals[0] ?? "";
  const tail = literals[literals.length - 1] ?? "";
  if (
    literals.length === 1 ||
    head.length + tail.length > before.length ||
    !before.startsWith(head) ||
    !before.endsWith(tail)
  ) {
    return sensitivePieces([before]);
  }
  const end = before.length - tail.length;
  const spans: string[] = [];
  let pos = head.length;
  for (const literal of literals.slice(1, -1)) {
    const at = before.indexOf(literal, pos);
    if (at < 0 || at + literal.length > end) return sensitivePieces([before]);
    spans.push(before.slice(pos, at));
    pos = at + literal.length;
  }
  spans.push(before.slice(pos, end));
  return sensitivePieces(spans);
}

// A span can hold an earlier redaction's token when a second guard matched
// across it. Only the text around the token is sensitive.
function sensitivePieces(spans: string[]): string[] {
  return spans
    .flatMap((span) => span.split(REDACTION_TOKEN))
    .map((piece) => piece.trim())
    .filter((piece) => piece !== "");
}

/**
 * Replaces every occurrence of each span in `value` with the redaction token.
 * Overlapping occurrences merge into one, so a short span can't stop a longer
 * one from matching, whichever was recorded first.
 */
export function redactSpans(value: string, spans: Iterable<string>): string {
  const ranges: Array<[number, number]> = [];
  for (const span of spans) {
    if (span === "") continue;
    for (
      let at = value.indexOf(span);
      at >= 0;
      at = value.indexOf(span, at + 1)
    ) {
      ranges.push([at, at + span.length]);
    }
  }
  ranges.sort((a, b) => a[0] - b[0]);
  const merged: Array<[number, number]> = [];
  for (const [from, to] of ranges) {
    const last = merged[merged.length - 1];
    if (last && from <= last[1]) last[1] = Math.max(last[1], to);
    else merged.push([from, to]);
  }
  let out = "";
  let pos = 0;
  for (const [from, to] of merged) {
    out += value.slice(pos, from) + REDACTION_TOKEN;
    pos = to;
  }
  return out + value.slice(pos);
}

// Redacts the end of a title that stops partway through a span, ignoring a
// trailing ellipsis.
function redactCutOff(title: string, spans: Iterable<string>): string {
  const body = title.replace(/(?:…|\.\.\.)\s*$/u, "");
  let cut = body.length;
  for (const span of spans) {
    for (
      let n = Math.min(span.length - 1, body.length);
      n >= MIN_CUT_OFF_PREFIX;
      n--
    ) {
      if (body.endsWith(span.slice(0, n))) {
        cut = Math.min(cut, body.length - n);
        break;
      }
    }
  }
  return cut === body.length ? title : body.slice(0, cut) + REDACTION_TOKEN;
}

export function createGuardRedactions(): GuardRedactions {
  const sessions = new Map<string, Set<string>>();

  const text = (sessionID: string, value: string): string => {
    const spans = sessions.get(sessionID);
    return spans && value !== "" ? redactSpans(value, spans) : value;
  };

  return {
    record(sessionID, sent, redacted) {
      if (!sessionID || sent.length !== redacted.length) return;
      sent.forEach((message, index) => {
        const after = textParts(redacted[index]);
        textParts(message).forEach((original, part) => {
          const replacement = after[part];
          if (!original || replacement === undefined) return;
          const removed = removedSpans(original, replacement);
          if (removed.length === 0) return;
          let spans = sessions.get(sessionID);
          if (!spans) {
            spans = new Set();
            sessions.set(sessionID, spans);
          }
          for (const span of removed) spans.add(span);
        });
      });
    },
    text,
    title(sessionID, value) {
      const spans = sessions.get(sessionID);
      if (!spans || value === "") return value;
      return redactCutOff(redactSpans(value, spans), spans);
    },
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
    forget(sessionID) {
      sessions.delete(sessionID);
    },
  };
}
