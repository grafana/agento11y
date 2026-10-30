import type { ContentCaptureMode, Message } from "@grafana/agento11y";
import {
  mapUserMessage,
  type PiUserMessage,
  userMessageText,
} from "./mappers.js";

export interface RequestObservationManager {
  getSessionId?: () => unknown;
  getEntries?: () => unknown;
  buildSessionProjection?: () => unknown;
  getBranch?: () => unknown;
}

type RecordLike = Record<string, unknown>;
interface Occurrence {
  id: string;
  key?: string;
}
interface Snapshot {
  sources: unknown[];
  messages: unknown[];
  occurrences: Occurrence[];
}
interface Pending {
  consumed: boolean;
  key?: string;
  text?: string;
}

function record(value: unknown): RecordLike | undefined {
  return value !== null && typeof value === "object"
    ? (value as RecordLike)
    : undefined;
}

function userEntryId(value: unknown): string | undefined {
  const entry = record(value);
  return entry?.type === "message" &&
    typeof entry.id === "string" &&
    record(entry.message)?.role === "user"
    ? entry.id
    : undefined;
}

/** Text changes must not change occurrence identity. */
function metadataKey(value: unknown): string | undefined {
  const message = record(value);
  return typeof message?.role === "string" &&
    typeof message.timestamp === "number" &&
    Number.isFinite(message.timestamp)
    ? JSON.stringify([message.role, message.timestamp])
    : undefined;
}

function snapshot(manager: RequestObservationManager): Snapshot | undefined {
  try {
    let entries: { sourceEntry: unknown; messages: unknown[] }[];
    if (typeof manager.buildSessionProjection === "function") {
      const projection = record(manager.buildSessionProjection());
      if (!Array.isArray(projection?.entries)) return undefined;
      entries = [];
      for (const value of projection.entries) {
        const entry = record(value);
        if (!entry || !Array.isArray(entry.messages)) return undefined;
        entries.push({
          sourceEntry: entry.sourceEntry,
          messages: entry.messages,
        });
      }
    } else if (typeof manager.getBranch === "function") {
      const branch = manager.getBranch();
      if (!Array.isArray(branch)) return undefined;
      entries = branch.map((sourceEntry) => ({
        sourceEntry,
        messages:
          record(sourceEntry)?.type === "message"
            ? [record(sourceEntry)?.message]
            : [],
      }));
    } else {
      return undefined;
    }
    return {
      sources: entries.map((entry) => entry.sourceEntry),
      messages: entries.flatMap((entry) => entry.messages),
      occurrences: entries.flatMap((entry) => {
        const id = userEntryId(entry.sourceEntry);
        if (id === undefined) return [];
        // A user entry must own exactly one projected user slot. Omitted or
        // expanded contributions cannot authorize a body.
        const message = entry.messages[0];
        const key =
          entry.messages.length === 1 && record(message)?.role === "user"
            ? metadataKey(message)
            : undefined;
        return [{ id, key }];
      }),
    };
  } catch {
    // Do not downgrade a broken projection to raw branch contents.
    return undefined;
  }
}

function positions(messages: readonly unknown[]): Map<string, number[]> {
  const result = new Map<string, number[]>();
  messages.forEach((message, index) => {
    const key = metadataKey(message);
    if (key === undefined) return;
    const indices = result.get(key) ?? [];
    indices.push(index);
    result.set(key, indices);
  });
  return result;
}

function observedText(value: unknown): string | undefined {
  const message = record(value);
  if (message?.role !== "user") return undefined;
  const content = message.content;
  if (typeof content !== "string") {
    if (!Array.isArray(content)) return undefined;
    for (const part of content) {
      const block = record(part);
      if (
        !block ||
        !(
          (block.type === "text" && typeof block.text === "string") ||
          (block.type === "image" &&
            typeof block.data === "string" &&
            typeof block.mimeType === "string")
        )
      ) {
        return undefined;
      }
    }
  }
  // Snapshot only observed text; never retain mutable hook objects or images.
  return userMessageText(message as unknown as PiUserMessage);
}

/** Export observed occurrence bodies, never restored-history text. */
export class RequestObservation {
  private manager?: RequestObservationManager;
  private sessionId: unknown;
  private enabled = false;
  private baseline = new Set<string>();
  private pending = new Map<string, Pending>();
  private firstId?: string;

  /** First new occurrence's safe observed text, never a restored-history title. */
  get firstUserText(): string | undefined {
    const text =
      this.firstId === undefined
        ? undefined
        : this.pending.get(this.firstId)?.text;
    return text?.trim() || undefined;
  }

  reset(manager: RequestObservationManager): void {
    this.manager = manager;
    this.sessionId = this.identity(manager);
    this.enabled = false;
    this.baseline.clear();
    this.pending.clear();
    this.firstId = undefined;
    const current = snapshot(manager);
    if (!current) return;
    try {
      const persisted =
        typeof manager.getEntries === "function"
          ? manager.getEntries()
          : current.sources;
      if (!Array.isArray(persisted)) return;
      for (const source of [...persisted, ...current.sources]) {
        const id = userEntryId(source);
        if (id !== undefined) this.baseline.add(id);
      }
      this.enabled = true;
    } catch {
      // A failed baseline must not turn restored history into new input later.
    }
  }

  observe(
    manager: RequestObservationManager,
    messages: readonly unknown[],
  ): void {
    const current = this.current(manager);
    for (const pending of this.pending.values()) {
      if (!pending.consumed) {
        pending.key = undefined;
        pending.text = undefined;
      }
    }
    if (!current) return;
    this.discover(current);
    const projected = positions(current.messages);
    const observed = positions(messages);
    const matches: { key: string; source: number; target: number }[] = [];
    for (const [key, indices] of projected) {
      const targets = observed.get(key);
      const source = indices[0];
      const target = targets?.[0];
      if (
        indices.length === 1 &&
        targets?.length === 1 &&
        source !== undefined &&
        target !== undefined
      ) {
        matches.push({ key, source, target });
      }
    }
    matches.sort((a, b) => a.source - b.source);
    // A reorder invalidates correspondence; matching text cannot rescue it.
    let previousTarget = -1;
    for (const match of matches) {
      if (match.target <= previousTarget) return;
      previousTarget = match.target;
    }
    const matched = new Map(matches.map((match) => [match.key, match.target]));
    const ids = current.occurrences.map((occurrence) => occurrence.id);
    for (const occurrence of current.occurrences) {
      const pending = this.pending.get(occurrence.id);
      if (!pending || pending.consumed || occurrence.key === undefined)
        continue;
      if (ids.indexOf(occurrence.id) !== ids.lastIndexOf(occurrence.id))
        continue;
      const index = matched.get(occurrence.key);
      if (index === undefined) continue;
      pending.key = occurrence.key;
      pending.text = observedText(messages[index]);
    }
  }

  consume(
    manager: RequestObservationManager,
    mode: ContentCaptureMode,
  ): Message[] {
    const current = this.current(manager);
    if (current) this.discover(current);
    const projected = positions(current?.messages ?? []);
    const result: Message[] = [];
    for (const [id, pending] of this.pending) {
      if (pending.consumed) continue;
      const occurrences =
        current?.occurrences.filter((entry) => entry.id === id) ?? [];
      const occurrence = occurrences[0];
      if (
        occurrences.length !== 1 ||
        pending.key === undefined ||
        occurrence?.key !== pending.key ||
        projected.get(pending.key)?.length !== 1
      ) {
        pending.text = undefined;
      }
      pending.consumed = true;
      if (pending.text === undefined) continue;
      const mapped = mapUserMessage(
        { role: "user", content: pending.text, timestamp: 0 },
        mode,
      );
      if (mapped) result.push(mapped);
    }
    return result;
  }

  private identity(manager: RequestObservationManager): unknown {
    try {
      return manager.getSessionId?.();
    } catch {
      return undefined;
    }
  }

  private current(manager: RequestObservationManager): Snapshot | undefined {
    if (this.manager !== manager || this.sessionId !== this.identity(manager)) {
      this.reset(manager);
    }
    return this.enabled ? snapshot(manager) : undefined;
  }

  private discover(current: Snapshot): void {
    for (const { id } of current.occurrences) {
      if (this.baseline.has(id) || this.pending.has(id)) continue;
      this.pending.set(id, { consumed: false });
      this.firstId ??= id;
    }
  }
}
