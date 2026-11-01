import type {
  SessionBeforeCompactEvent,
  SessionBeforeTreeEvent,
  SessionBeforeTreeResult,
} from "@mariozechner/pi-coding-agent";
import { evaluatePreflight, type PreflightTransformArgs } from "./guard.js";
import {
  applyGuardTransform,
  projectGuardMessages,
} from "./guardProjection.js";

export interface SummaryGuardArgs
  extends Omit<PreflightTransformArgs, "messages" | "signal"> {
  failOpen: boolean;
}
type GuardResult = SessionBeforeTreeResult | undefined;
interface Binding {
  native: unknown;
  apply: (value: unknown) => void;
}

function textBinding(text: string, apply: (text: string) => void): Binding {
  return {
    native: { role: "user", content: text },
    apply: (value) => apply((value as { content: string }).content),
  };
}

function canAssign(target: object, key: string): boolean {
  const descriptor = Object.getOwnPropertyDescriptor(target, key);
  return descriptor
    ? descriptor.writable === true
    : Object.isExtensible(target);
}

async function protect(
  bindings: Binding[],
  commit: () => GuardResult,
  args: SummaryGuardArgs,
  signal: AbortSignal,
): Promise<GuardResult> {
  if (signal.aborted) return { cancel: true };
  try {
    const native = bindings.map((binding) => binding.native);
    const projection = projectGuardMessages(native);
    if (projection.messages.length === 0) return;
    const evaluation = await evaluatePreflight({
      ...args,
      messages: projection.messages,
      signal,
    });
    if (signal.aborted) return { cancel: true };
    if (evaluation.kind === "allow") return;
    if (evaluation.kind === "failure" && args.failOpen) {
      args.logger?.warn(
        "Agent Observability: summary guard evaluation failed; continuing with fail-open enabled.",
      );
      return;
    }
    if (evaluation.kind === "transform") {
      const transformed = applyGuardTransform(
        native,
        projection,
        evaluation.input.messages,
      );
      transformed.forEach((value, index) => {
        bindings[index]?.apply(value);
      });
      return commit();
    }
  } catch {
    if (signal.aborted) return { cancel: true };
    // Pi swallows session-before exceptions. Return cancellation instead.
  }
  args.logger?.warn(
    "Agent Observability: summary cancelled because its guard evaluation or required transformation could not be applied.",
  );
  return { cancel: true };
}

export async function guardCompaction(
  event: SessionBeforeCompactEvent,
  args: SummaryGuardArgs,
): Promise<GuardResult> {
  if (event.signal.aborted) return { cancel: true };
  try {
    const preparation = event.preparation;
    const messages = preparation.messagesToSummarize.slice();
    const prefix = preparation.turnPrefixMessages.slice();
    const fileOps = { ...preparation.fileOps };
    const fileLists = new Map<keyof typeof fileOps, string[]>();
    let previousSummary = preparation.previousSummary;
    let instructions = event.customInstructions;
    const bindings: Binding[] = [];
    for (const [original, candidate] of [
      [preparation.messagesToSummarize, messages],
      [preparation.turnPrefixMessages, prefix],
    ] as const) {
      original.forEach((native, index) => {
        bindings.push({
          native,
          apply: (value) => {
            candidate[index] = value as typeof native;
          },
        });
      });
    }
    if (previousSummary !== undefined)
      bindings.push(
        textBinding(previousSummary, (value) => {
          previousSummary = value;
        }),
      );
    for (const key of ["read", "written", "edited"] as const) {
      if (!(fileOps[key] instanceof Set)) throw new Error();
      const files = [...fileOps[key]];
      files.forEach((file, index) => {
        if (typeof file !== "string") throw new Error();
        bindings.push(
          textBinding(file, (value) => {
            files[index] = value;
          }),
        );
      });
      fileLists.set(key, files);
    }
    if (instructions !== undefined)
      bindings.push(
        textBinding(instructions, (value) => {
          instructions = value;
        }),
      );
    return await protect(
      bindings,
      () => {
        // Pi consumes instruction overrides only for tree summaries.
        if (instructions !== event.customInstructions) throw new Error();
        for (const [key, files] of fileLists) fileOps[key] = new Set(files);
        const updates = {
          messagesToSummarize: messages,
          turnPrefixMessages: prefix,
          ...(previousSummary !== undefined ? { previousSummary } : {}),
          fileOps,
        };
        if (Object.keys(updates).some((key) => !canAssign(preparation, key)))
          throw new Error();
        Object.assign(preparation, updates);
        return undefined;
      },
      args,
      event.signal,
    );
  } catch {
    args.logger?.warn(
      "Agent Observability: summary cancelled because its preparation could not be validated.",
    );
    return { cancel: true };
  }
}

export async function guardTreeSummary(
  event: SessionBeforeTreeEvent,
  args: SummaryGuardArgs,
): Promise<GuardResult> {
  if (event.signal.aborted) return { cancel: true };
  if (!event.preparation.userWantsSummary) return;
  try {
    const preparation = event.preparation;
    const entries = preparation.entriesToSummarize;
    const candidates = entries.slice();
    let instructions = preparation.customInstructions;
    const bindings: Binding[] = [];
    entries.forEach((entry, index) => {
      switch (entry.type) {
        case "message":
          bindings.push({
            native: entry.message,
            apply: (value) => {
              candidates[index] = {
                ...entry,
                message: value as typeof entry.message,
              };
            },
          });
          break;
        case "custom_message":
          bindings.push({
            native: { role: "custom", content: entry.content },
            apply: (value) => {
              candidates[index] = {
                ...entry,
                content: (value as { content: typeof entry.content }).content,
              };
            },
          });
          break;
        case "branch_summary":
        case "compaction": {
          bindings.push(
            textBinding(entry.summary, (value) => {
              candidates[index] = {
                ...candidates[index],
                summary: value,
              } as typeof entry;
            }),
          );
          if (
            entry.type !== "branch_summary" ||
            entry.fromHook ||
            !entry.details ||
            typeof entry.details !== "object"
          )
            break;
          const details = { ...entry.details } as Record<string, unknown>;
          for (const key of ["readFiles", "modifiedFiles"]) {
            if (!Array.isArray(details[key])) continue;
            const files: unknown[] = [...details[key]];
            details[key] = files;
            files.forEach((file, ordinal) => {
              if (typeof file !== "string") throw new Error();
              bindings.push(
                textBinding(file, (value) => {
                  files[ordinal] = value;
                }),
              );
            });
          }
          candidates[index] = { ...entry, details };
          break;
        }
      }
    });
    if (instructions !== undefined)
      bindings.push(
        textBinding(instructions, (value) => {
          instructions = value;
        }),
      );
    return await protect(
      bindings,
      () => {
        // Pi keeps the original array reference, but its entries belong to history.
        if (
          entries.length !== candidates.length ||
          candidates.some((_, index) => !canAssign(entries, String(index)))
        )
          throw new Error();
        candidates.forEach((entry, index) => {
          entries[index] = entry;
        });
        return instructions !== preparation.customInstructions
          ? { customInstructions: instructions }
          : undefined;
      },
      args,
      event.signal,
    );
  } catch {
    args.logger?.warn(
      "Agent Observability: summary cancelled because its preparation could not be validated.",
    );
    return { cancel: true };
  }
}
