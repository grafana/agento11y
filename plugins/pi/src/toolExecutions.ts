import { redactSecretText } from "@grafana/agento11y";
import type {
  PiAssistantMessage,
  PiToolResult,
  ToolTiming,
} from "./mappers.js";
import { toolResultText } from "./mappers.js";
import { matchSkillRead, type SkillIdentity } from "./skills.js";

export interface ToolObservation extends ToolTiming {
  occurrence: number;
  parentToolCallId?: string;
  malformedParent?: boolean;
  duplicateId?: boolean;
  proposalJSON?: string;
  guardInputJSON?: string;
  postExecutionInputJSON?: string;
  postExecutionInputSafe: boolean;
  resultText?: string;
  durationMs?: number;
  proposalSafe: boolean;
  guardTransformed?: boolean;
  guardTransformJSON?: string;
  skillInput?: { ref: WeakRef<object>; cwd: string };
}

type ToolIdentity = {
  toolCallId: string;
  toolName?: string;
  parentToolCallId?: unknown;
};

function encode(value: unknown): string | undefined {
  try {
    return JSON.stringify(value);
  } catch {
    return undefined;
  }
}

function resultText(result: unknown): string | undefined {
  if (!result || typeof result !== "object") return undefined;
  const content = (result as { content?: unknown }).content;
  if (!Array.isArray(content)) return undefined;
  return toolResultText(
    content.filter(
      (part) => part?.type === "text" && typeof part.text === "string",
    ),
  );
}

/** One instance owns one turn. Call IDs are not identities outside that turn. */
export class ToolExecutions {
  readonly observations: ToolObservation[] = [];
  private active = new Set<ToolObservation>();

  reset(): void {
    this.observations.length = 0;
    this.active.clear();
  }

  start(
    event: ToolIdentity & { toolName: string; args?: unknown },
    now = Date.now(),
  ): void {
    const parent = event.parentToolCallId;
    const observation: ToolObservation = {
      occurrence: this.observations.length,
      toolCallId: event.toolCallId,
      toolName: event.toolName,
      ...(typeof parent === "string" && parent.length > 0
        ? { parentToolCallId: parent }
        : parent === undefined
          ? {}
          : { malformedParent: true }),
      proposalJSON: encode(event.args),
      proposalSafe: false,
      postExecutionInputSafe: false,
      startedAt: now,
      completedAt: now,
      isError: false,
    };
    for (const previous of this.observations) {
      if (previous.toolCallId === observation.toolCallId) {
        previous.duplicateId = true;
        observation.duplicateId = true;
      }
    }
    this.observations.push(observation);
    this.active.add(observation);
  }

  private current(event: ToolIdentity): ToolObservation | undefined {
    const matches = [...this.active].filter(
      (item) =>
        item.toolCallId === event.toolCallId &&
        (event.toolName === undefined || item.toolName === event.toolName) &&
        (event.parentToolCallId === undefined ||
          (typeof event.parentToolCallId === "string" &&
          event.parentToolCallId.length > 0
            ? item.parentToolCallId === event.parentToolCallId
            : item.malformedParent)),
    );
    return matches.length === 1 ? matches[0] : undefined;
  }

  guardInput(
    event: ToolIdentity & { input: unknown },
    skills: SkillIdentity[],
    cwd: string,
    afterTransform = false,
  ): void {
    const item = this.current(event);
    if (!item) return;
    if (!afterTransform) item.guardInputJSON = encode(event.input);
    item.skillName = matchSkillRead(skills, item.toolName, event.input, cwd);
    // Later handlers can rewrite and block without emitting tool_result.
    item.skillInput =
      item.toolName === "read" &&
      typeof event.input === "object" &&
      event.input !== null
        ? { ref: new WeakRef(event.input), cwd }
        : undefined;
  }

  guardChanged(event: ToolIdentity, transform?: unknown): void {
    const item = this.current(event);
    if (!item) return;
    item.guardTransformed = true;
    item.guardTransformJSON = encode(transform);
  }

  resultInput(
    event: ToolIdentity & { input: unknown },
    skills: SkillIdentity[],
    cwd: string,
  ): void {
    const item = this.current(event);
    if (!item) return;
    const observed = encode(event.input);
    item.postExecutionInputJSON = observed;
    item.postExecutionInputSafe =
      observed !== undefined &&
      (!item.guardTransformed || observed === item.guardTransformJSON);
    item.proposalSafe =
      !item.guardTransformed &&
      item.proposalJSON !== undefined &&
      item.proposalJSON === item.guardInputJSON &&
      item.proposalJSON === item.postExecutionInputJSON;
    item.skillName = matchSkillRead(skills, item.toolName, event.input, cwd);
    item.skillInput = undefined;
  }

  end(
    event: ToolIdentity & {
      isError: boolean;
      result?: unknown;
      durationMs?: number;
    },
    skills: SkillIdentity[],
    now = Date.now(),
  ): void {
    const item = this.current(event);
    if (!item) return;
    this.active.delete(item);
    if (item.skillInput) {
      const input = item.skillInput.ref.deref();
      if (input !== undefined) {
        item.skillName = matchSkillRead(
          skills,
          item.toolName,
          input,
          item.skillInput.cwd,
        );
      }
      item.skillInput = undefined;
    }
    item.completedAt = now;
    item.isError = event.isError;
    item.resultText = resultText(event.result);
    if (
      typeof event.durationMs === "number" &&
      Number.isFinite(event.durationMs) &&
      event.durationMs >= 0
    ) {
      item.durationMs = event.durationMs;
    }
  }

  completed(): ToolObservation[] {
    return this.observations.filter((item) => !this.active.has(item));
  }

  private unique(id: string, name: string): ToolObservation | undefined {
    const matches = this.observations.filter((item) => item.toolCallId === id);
    return matches.length === 1 && matches[0]?.toolName === name
      ? matches[0]
      : undefined;
  }

  safeMessage(message: PiAssistantMessage): PiAssistantMessage {
    const counts = new Map<string, number>();
    for (const block of message.content) {
      if (block.type === "toolCall")
        counts.set(block.id, (counts.get(block.id) ?? 0) + 1);
    }
    return {
      ...message,
      content: message.content.map((block) => {
        if (block.type !== "toolCall") return block;
        const item = this.unique(block.id, block.name);
        // Prepared arguments cannot establish what the model proposed.
        const proposal =
          counts.get(block.id) === 1 &&
          item?.proposalSafe &&
          encode(block.arguments) === item.proposalJSON
            ? item.proposalJSON
            : undefined;
        return {
          ...block,
          arguments: proposal === undefined ? undefined : JSON.parse(proposal),
        };
      }),
    };
  }

  safeResults(results: PiToolResult[]): PiToolResult[] {
    // message_end replacements settle after tool_execution_end.
    return results.map((result) => ({
      ...result,
      content: [
        {
          type: "text",
          text: redactSecretText(toolResultText(result.content)),
        },
      ],
    }));
  }
}

/** Ambiguous IDs and invalid edges stay attached to the generation. */
export function toolParents(
  observations: ToolObservation[],
): Map<ToolObservation, ToolObservation> {
  const byId = new Map<string, ToolObservation[]>();
  for (const item of observations) {
    const group = byId.get(item.toolCallId) ?? [];
    group.push(item);
    byId.set(item.toolCallId, group);
  }
  const cyclic = new Set<ToolObservation>();
  for (const origin of observations) {
    const path: ToolObservation[] = [];
    let current: ToolObservation | undefined = origin;
    while (current) {
      const repeated = path.indexOf(current);
      if (repeated >= 0) {
        for (const item of path.slice(repeated)) cyclic.add(item);
        break;
      }
      path.push(current);
      const candidates: ToolObservation[] | undefined = current.parentToolCallId
        ? byId.get(current.parentToolCallId)
        : undefined;
      current = candidates?.length === 1 ? candidates[0] : undefined;
    }
  }
  const parents = new Map<ToolObservation, ToolObservation>();
  for (const item of observations) {
    if (
      cyclic.has(item) ||
      item.duplicateId ||
      item.malformedParent ||
      item.parentToolCallId === undefined
    )
      continue;
    const candidates = byId.get(item.parentToolCallId);
    const parent = candidates?.length === 1 ? candidates[0] : undefined;
    if (
      parent &&
      !parent.duplicateId &&
      parent.occurrence < item.occurrence &&
      byId.get(item.toolCallId)?.length === 1
    ) {
      parents.set(item, parent);
    }
  }
  return parents;
}
