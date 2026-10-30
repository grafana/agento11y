import { isDeepStrictEqual } from "node:util";
import type { Message, MessagePart } from "@grafana/agento11y";

type Path = (string | number)[];
interface Slot {
  message: number;
  part: number;
  field: Path;
  source: Path;
  json?: boolean;
}
export interface GuardProjection {
  messages: Message[];
  slots: Slot[];
}

export class GuardProjectionError extends Error {
  constructor() {
    super(
      "Agent Observability could not apply a required guard transformation.",
    );
  }
}
const refuse = (): never => {
  throw new GuardProjectionError();
};
const record = (value: unknown): Record<string, unknown> | undefined =>
  value !== null && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : undefined;

export function projectGuardMessages(
  native: readonly unknown[],
): GuardProjection {
  const messages: Message[] = [];
  const slots: Slot[] = [];
  native.forEach((value, index) => {
    const message = record(value);
    if (!message) return;
    const role = message.role;
    const parts: MessagePart[] = [];
    const add = (part: MessagePart, field: Path, path: Path, json = false) => {
      slots.push({
        message: messages.length,
        part: parts.length,
        field,
        source: [index, ...path],
        json,
      });
      parts.push(part);
    };
    const text = (value: unknown, path: Path) => {
      if (typeof value === "string" && value.length > 0)
        add({ type: "text", text: value }, ["text"], path);
    };
    const content = () => {
      if (typeof message.content === "string")
        text(message.content, ["content"]);
      else if (Array.isArray(message.content))
        message.content.forEach((value, ordinal) => {
          const part = record(value);
          if (!part) return;
          const path: Path = ["content", ordinal];
          if (
            part.type === "text" &&
            typeof part.text === "string" &&
            part.text.length > 0
          ) {
            if (role === "toolResult")
              add(
                {
                  type: "tool_result",
                  toolResult: {
                    toolCallId: message.toolCallId as string,
                    name: message.toolName as string,
                    // The hook wire represents false by omission.
                    ...(message.isError === true ? { isError: true } : {}),
                    content: part.text,
                  },
                },
                ["toolResult", "content"],
                [...path, "text"],
              );
            else text(part.text, [...path, "text"]);
          } else if (role === "assistant" && part.type === "toolCall") {
            add(
              {
                type: "tool_call",
                toolCall: {
                  id: part.id as string,
                  name: part.name as string,
                  inputJSON: JSON.stringify(part.arguments) ?? "{}",
                },
              },
              ["toolCall", "inputJSON"],
              [...path, "arguments"],
              true,
            );
          }
        });
    };
    switch (role) {
      case "system":
        content();
        for (const [key, value] of Object.entries(
          record(message.sections) ?? {},
        ))
          text(value, ["sections", key]);
        if (Array.isArray(message.toolsAdded))
          message.toolsAdded.forEach((value, ordinal) => {
            const tool = record(value);
            if (!tool) return;
            text(tool.description, ["toolsAdded", ordinal, "description"]);
            if (tool.parameters !== undefined)
              add(
                { type: "text", text: JSON.stringify(tool.parameters) },
                ["text"],
                ["toolsAdded", ordinal, "parameters"],
                true,
              );
          });
        break;
      case "user":
      case "assistant":
      case "toolResult":
      case "custom":
        content();
        break;
      case "bashExecution":
        if (message.excludeFromContext) break;
        text(message.command, ["command"]);
        text(message.output, ["output"]);
        if (message.truncated) text(message.fullOutputPath, ["fullOutputPath"]);
        break;
      case "compactionSummary":
      case "branchSummary":
        text(message.summary, ["summary"]);
        break;
    }
    if (parts.length > 0)
      messages.push({
        role:
          role === "assistant"
            ? "assistant"
            : role === "system"
              ? "system"
              : role === "toolResult"
                ? "tool"
                : "user",
        name: `pi:${index}`,
        parts,
      });
  });
  return { messages, slots };
}

function at(value: unknown, path: Path): unknown {
  let current = value;
  for (const key of path) {
    if (current === null || typeof current !== "object") return refuse();
    current = (current as Record<string | number, unknown>)[key];
  }
  return current;
}
function replace(value: unknown, path: Path, next: unknown): unknown {
  if (path.length === 0) return next;
  if (value === null || typeof value !== "object") return refuse();
  const [key, ...rest] = path;
  if (key === undefined) return refuse();
  const copy = Array.isArray(value) ? [...value] : { ...value };
  (copy as Record<string | number, unknown>)[key] = replace(
    at(value, [key]),
    rest,
    next,
  );
  return copy;
}
function comparable(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(comparable);
  if (value && typeof value === "object")
    return Object.fromEntries(
      Object.entries(value)
        .filter(([, value]) => value !== undefined)
        .map(([key, value]) => [key, comparable(value)]),
    );
  return value;
}

export function applyGuardTransform<T>(
  native: readonly T[],
  projection: GuardProjection,
  transformed: Message[],
): T[] {
  if (
    !isDeepStrictEqual(projectGuardMessages(native), projection) ||
    !Array.isArray(transformed) ||
    transformed.length !== projection.messages.length
  )
    return refuse();
  const normalized = transformed.map((message, index) => {
    if (!record(message)) return refuse();
    if (message.content === undefined) return message;
    const original = projection.messages[index]?.parts;
    if (
      message.parts !== undefined ||
      typeof message.content !== "string" ||
      original?.length !== 1 ||
      original[0]?.type !== "text"
    )
      return refuse();
    const { content, ...rest } = message;
    return { ...rest, parts: [{ type: "text", text: content }] };
  });
  let expected: unknown = projection.messages;
  const changes: { path: Path; value: unknown }[] = [];
  for (const slot of projection.slots) {
    const path = [slot.message, "parts", slot.part, ...slot.field];
    const value = at(normalized, path);
    // The hook wire drops empty strings. Absence cannot prove deletion.
    if (typeof value !== "string" || value.length === 0) return refuse();
    if (value === at(projection.messages, path)) continue;
    let replacement: unknown = value;
    if (slot.json) {
      try {
        replacement = JSON.parse(value);
      } catch {
        return refuse();
      }
      if (!record(replacement)) return refuse();
    }
    expected = replace(expected, path, value);
    changes.push({ path: slot.source, value: replacement });
  }
  if (!isDeepStrictEqual(comparable(expected), comparable(normalized)))
    return refuse();
  let result: unknown = native;
  for (const change of changes)
    result = replace(result, change.path, change.value);
  return result as T[];
}
