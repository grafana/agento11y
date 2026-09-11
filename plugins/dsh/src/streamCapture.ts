import type {
  DshContentBlock,
  DshFinishReason,
  DshStreamChunk,
  DshTokenUsage,
} from "./dsh.js";

interface PartialBlock {
  blockType: string;
  text: string;
  toolCallId?: string;
  toolCallName?: string;
  toolCallArguments: string;
  block?: DshContentBlock;
}

function isTokenDelta(chunk: DshStreamChunk): boolean {
  switch (chunk.type) {
    case "text-delta":
    case "reasoning-delta":
      return chunk.text !== undefined && chunk.text !== "";
    case "tool-call-delta":
      return (
        (chunk.argumentsDelta !== undefined && chunk.argumentsDelta !== "") ||
        chunk.name !== undefined
      );
    default:
      return false;
  }
}

export class StreamCapture {
  private readonly partials = new Map<number, PartialBlock>();
  private readonly order: number[] = [];
  private readonly now: () => number;
  private firstTokenTime: number | undefined;
  private latestUsage: DshTokenUsage | undefined;
  private latestFinish: DshFinishReason | undefined;

  constructor(now: () => number = Date.now) {
    this.now = now;
  }

  push(chunk: DshStreamChunk, observedAt?: number): void {
    if (this.firstTokenTime === undefined && isTokenDelta(chunk)) {
      this.firstTokenTime = observedAt ?? this.now();
    }

    switch (chunk.type) {
      case "block-start": {
        if (
          chunk.index === undefined ||
          chunk.blockType === undefined ||
          this.partials.has(chunk.index)
        ) {
          return;
        }
        this.order.push(chunk.index);
        this.partials.set(chunk.index, {
          blockType: chunk.blockType,
          text: "",
          toolCallArguments: "",
        });
        return;
      }
      case "text-delta":
      case "reasoning-delta": {
        if (chunk.index === undefined) return;
        const partial = this.ensure(
          chunk.index,
          chunk.type === "text-delta" ? "text" : "reasoning",
        );
        if (partial.block !== undefined) return;
        partial.text += chunk.text ?? "";
        return;
      }
      case "tool-call-delta": {
        if (chunk.index === undefined) return;
        const partial = this.ensure(chunk.index, "tool-call");
        if (partial.block !== undefined) return;
        partial.toolCallId = chunk.id;
        if (chunk.name) partial.toolCallName = chunk.name;
        partial.toolCallArguments += chunk.argumentsDelta ?? "";
        return;
      }
      case "block-end": {
        if (chunk.index === undefined || chunk.block === undefined) return;
        const partial = this.ensure(chunk.index, chunk.block.type);
        // A completed dsh block is authoritative, and its first close wins.
        if (partial.block !== undefined) return;
        partial.block = chunk.block;
        return;
      }
      case "usage":
        this.latestUsage = chunk.usage;
        return;
      case "finish":
        this.latestFinish = chunk.reason;
        return;
      default:
        return;
    }
  }

  blocks(): DshContentBlock[] {
    const blocks = this.assembledBlocks();
    // dsh drops tool calls from max-token responses because they are unsafe to run.
    return this.latestFinish?.kind === "max-tokens"
      ? blocks.filter((block) => block.type !== "tool-call")
      : blocks;
  }

  interruptedBlocks(): DshContentBlock[] {
    const blocks: DshContentBlock[] = [];
    for (const index of this.order) {
      const partial = this.partials.get(index);
      if (partial === undefined) continue;
      const type = partial.block?.type ?? partial.blockType;
      if (type !== "text" && type !== "reasoning") continue;
      const block = this.assemble(partial, index);
      if (
        block !== undefined &&
        (block.type === "text" || block.type === "reasoning") &&
        (block.text ?? "").trim() !== ""
      ) {
        blocks.push(block);
      }
    }
    return blocks;
  }

  get firstTokenAt(): number | undefined {
    return this.firstTokenTime;
  }

  get usage(): DshTokenUsage | undefined {
    return this.latestUsage;
  }

  get finish(): DshFinishReason | undefined {
    return this.latestFinish;
  }

  private ensure(index: number, blockType: string): PartialBlock {
    let partial = this.partials.get(index);
    if (partial === undefined) {
      partial = { blockType, text: "", toolCallArguments: "" };
      this.partials.set(index, partial);
      this.order.push(index);
    }
    return partial;
  }

  private assembledBlocks(): DshContentBlock[] {
    const blocks: DshContentBlock[] = [];
    for (const index of this.order) {
      const partial = this.partials.get(index);
      if (partial === undefined) continue;
      const block = this.assemble(partial, index);
      if (block !== undefined) blocks.push(block);
    }
    return blocks;
  }

  private assemble(
    partial: PartialBlock,
    index: number,
  ): DshContentBlock | undefined {
    if (partial.block !== undefined) return partial.block;
    switch (partial.blockType) {
      case "text":
        return { type: "text", text: partial.text };
      case "reasoning":
        return { type: "reasoning", text: partial.text };
      case "tool-call":
        return {
          type: "tool-call",
          id: partial.toolCallId ?? `call-${index}`,
          name: partial.toolCallName ?? "",
          arguments: partial.toolCallArguments,
        };
      default:
        return undefined;
    }
  }
}
