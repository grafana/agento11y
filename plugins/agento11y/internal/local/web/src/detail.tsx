import { Markdown } from 'markdown-to-jsx';
import type { CSSProperties, MutableRefObject, ReactNode, TableHTMLAttributes } from 'react';
import { Fragment, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { durationBetweenSeconds, formatDuration, formatTime, formatTokens, NO_VALUE } from './formatters';
import { Notice } from './notices';
import { generationIDFromHash, isPlainLeftClick } from './routing';
import { agentColor, agentHosts, agentShort, HEADER_H, Icon, isSubagent, ModelPill } from './shell';
import type {
  ConversationDetail,
  ConversationSummary,
  Generation,
  Message,
  Part,
  PartKind,
  TokenBuckets,
  ToolCall,
  ToolResult,
} from './types';

// ============================================================
// Screen 2 — Conversation detail
// ============================================================

function partKind(part: Part): PartKind {
  return (
    part.kind ||
    (part.text
      ? 'text'
      : part.thinking
        ? 'thinking'
        : part.tool_call
          ? 'tool_call'
          : part.tool_result
            ? 'tool_result'
            : 'unknown')
  );
}

function messageParts(messages: Message[] | undefined): Part[] {
  const out: Part[] = [];
  for (const message of messages || []) {
    for (const part of message.parts || []) out.push(part);
  }
  return out;
}

function resultParts(messages: Message[] | undefined): ToolResult[] {
  return messageParts(messages)
    .filter(
      (part): part is Part & { tool_result: ToolResult } => partKind(part) === 'tool_result' && !!part.tool_result,
    )
    .map((part) => part.tool_result);
}

function outputCalls(gen: Generation | null | undefined): ToolCall[] {
  return messageParts(gen?.output || [])
    .filter((part): part is Part & { tool_call: ToolCall } => partKind(part) === 'tool_call' && !!part.tool_call)
    .map((part) => part.tool_call);
}

function resolveResult(
  gen: Generation | null | undefined,
  next: Generation | null | undefined,
  call: ToolCall,
  used: Set<ToolResult> = new Set(),
): ToolResult | null {
  const sameGeneration = resultParts(([] as Message[]).concat(gen?.output || [], gen?.input || []));
  const following = resultParts(next?.input || []);
  const available = (result: ToolResult) => !used.has(result);
  let result: ToolResult | null = null;
  if (call.id) {
    result =
      sameGeneration.find((item) => available(item) && item.tool_call_id === call.id) ||
      following.find((item) => available(item) && item.tool_call_id === call.id) ||
      null;
  } else {
    result =
      sameGeneration.find((item) => available(item) && item.name && item.name === call.name) ||
      following.find((item) => available(item) && item.name && item.name === call.name) ||
      null;
  }
  if (result) used.add(result);
  return result;
}

export function resultBody(result: ToolResult | null | undefined): string {
  if (!result) return '';
  if (result.content) return result.content;
  if (result.content_json == null) return '';
  if (typeof result.content_json === 'string') return result.content_json;
  try {
    return JSON.stringify(result.content_json, null, 2);
  } catch (_) {
    return String(result.content_json);
  }
}

// IDE integrations and agent harnesses prepend one or more complete
// XML-ish blocks, with or without attributes: <user_info>, <rules>, and
// pi's <skill name="..." location="...">. Scan only from the start so
// markup inside the prompt does not move the split point.
// scanPreambleBlocks walks the complete blocks at the head of the text and
// reports where they end and what they were called. Names come from this
// walk rather than from a search for tags, because a skill body is full of
// angle-bracket words that are not blocks.
interface PreambleScan {
  end: number;
  tags: string[];
}

function scanPreambleBlocks(source: string): PreambleScan {
  const tags: string[] = [];
  let cursor = 0;
  let end = 0;
  while (cursor < source.length) {
    while (cursor < source.length && /\s/.test(source[cursor] ?? '')) cursor++;
    const open = /^<([a-z_][a-z0-9_-]*)(?:\s[^>]*)?>/.exec(source.slice(cursor));
    if (!open) break;
    const close = `</${open[1] ?? ''}>`;
    const closeAt = source.indexOf(close, cursor + open[0].length);
    if (closeAt < 0) break;
    if (!tags.includes(open[1] ?? '')) tags.push(open[1] ?? '');
    cursor = closeAt + close.length;
    end = cursor;
  }
  return { end, tags };
}

interface PreambleSplit {
  preamble: string;
  prompt: string;
}

export function splitPreamble(text: string | null | undefined): PreambleSplit {
  const source = String(text || '');
  const scan = scanPreambleBlocks(source);
  let end = scan.end;
  if (scan.tags.length === 0) return { preamble: '', prompt: source };
  while (end < source.length && /\s/.test(source[end] ?? '')) end++;
  const prompt = source.slice(end);
  if (!prompt.trim()) return { preamble: '', prompt: source };
  return { preamble: source.slice(0, end), prompt };
}

// Agent prose is markdown, and it is model output, so it is rendered the
// way the Agent Observability app plugin renders its own: markdown-to-jsx
// into a React tree, never innerHTML, with raw HTML parsing off so markup
// in the text shows up as text. The overrides below are ported from that
// plugin's MarkdownPreview, and tests/transcript.test.ts covers each of them.

// BlockedElement drops an element instead of rendering it. Every tag that
// can load a remote resource or run script is mapped to it, so the viewer
// cannot be made to phone home by a session it displays.
export function BlockedElement() {
  return null;
}

const MARKDOWN_BLOCKED_TAGS = [
  'iframe',
  'video',
  'audio',
  'embed',
  'object',
  'source',
  'track',
  'base',
  'script',
  'svg',
  'math',
  'style',
  'link',
  'form',
  'textarea',
  'select',
  'button',
  'details',
  'dialog',
  'img',
] as const;

type MarkdownBlockedTag = (typeof MARKDOWN_BLOCKED_TAGS)[number];

// markdownURL returns the href to render, or undefined to render the link
// with none. Relative forms pass through; everything else has to parse as
// a URL in one of four schemes, which is what rejects javascript:, data:,
// and vbscript:. Tabs, newlines and backslashes go first, because browsers
// strip them before resolving a URL and they can otherwise hide a scheme.
export function markdownURL(input: string | null | undefined): string | undefined {
  if (!input) return undefined;
  const raw = String(input)
    .trim()
    .replace(/[\t\r\n\\]/g, '');
  if (!raw || raw.startsWith('//')) return undefined;
  if (/^[/#?]/.test(raw) || raw.startsWith('./') || raw.startsWith('../')) return raw;
  try {
    const parsed = new URL(raw);
    const allowed = ['http:', 'https:', 'mailto:', 'tel:'];
    return allowed.includes(parsed.protocol) ? parsed.toString() : undefined;
  } catch (_) {
    return undefined;
  }
}

interface SafeAnchorProps {
  children?: ReactNode;
  href?: string;
  title?: string;
}

export function SafeAnchor({ children, href, title }: SafeAnchorProps) {
  return (
    <a href={markdownURL(href)} title={title} target="_blank" rel="noopener noreferrer">
      {children}
    </a>
  );
}

interface ScrollableTableProps extends TableHTMLAttributes<HTMLTableElement> {
  children?: ReactNode;
}

function ScrollableTable({ children, ...props }: ScrollableTableProps) {
  return (
    <div style={{ overflowX: 'auto' }}>
      <table {...props}>{children}</table>
    </div>
  );
}

interface TaskListCheckboxProps {
  type?: string;
  checked?: boolean;
}

function TaskListCheckbox({ type, checked }: TaskListCheckboxProps) {
  if (type !== 'checkbox') return null;
  return <input type="checkbox" checked={Boolean(checked)} readOnly disabled />;
}

export const MARKDOWN_OPTIONS = {
  overrides: {
    ...(Object.fromEntries(MARKDOWN_BLOCKED_TAGS.map((tag) => [tag, { component: BlockedElement }])) as Record<
      MarkdownBlockedTag,
      { component: typeof BlockedElement }
    >),
    a: { component: SafeAnchor },
    table: { component: ScrollableTable },
    input: { component: TaskListCheckbox },
  },
  forceBlock: true,
  disableParsingRawHTML: true,
};

// CappedBlock keeps large argument and result payloads inside the row that
// owns them. The complete text remains available through the scroll area.
interface CappedBlockProps {
  children?: ReactNode;
  maxHeight?: number;
  preStyle?: CSSProperties;
}

function CappedBlock({ children, maxHeight = 180, preStyle }: CappedBlockProps) {
  return (
    <pre
      style={{
        maxHeight,
        overflow: 'auto',
        background: 'var(--bg-primary)',
        border: '1px solid var(--border-weak)',
        borderRadius: 8,
        padding: '8px 10px',
        margin: 0,
        fontFamily: 'var(--fontFamilyMonospace)',
        fontSize: 11.5,
        lineHeight: 1.6,
        color: 'var(--fg1)',
        whiteSpace: 'pre-wrap',
        wordBreak: 'break-word',
        ...(preStyle || {}),
      }}
    >
      {children}
    </pre>
  );
}

/** A tool call's recorded arguments: named arguments, or the raw string the host wrote. */
type ToolCallInput = string | Record<string, unknown> | null;

function toolCallArgPreview(input: ToolCallInput): string {
  if (!input) return '';
  if (typeof input === 'string') return input.length > 140 ? `${input.slice(0, 140)}…` : input;
  for (const key of ['command', 'file_path', 'path', 'pattern', 'query', 'url', 'cmd', 'name']) {
    if (input[key] != null && input[key] !== '') return String(input[key]).replace(/\s+/g, ' ');
  }
  try {
    const value = JSON.stringify(input);
    return value.length > 140 ? `${value.slice(0, 140)}…` : value;
  } catch (_) {
    return '';
  }
}

function firstUserText(step: Generation): string {
  for (const message of step.input || []) {
    if (message.role !== 'user') continue;
    for (const part of message.parts || []) {
      if (partKind(part) === 'text' && (part.text || '').trim()) return part.text ?? '';
    }
  }
  return '';
}

function leadingAssistantText(step: Generation): string {
  for (const message of step.output || []) {
    for (const part of message.parts || []) {
      if (partKind(part) === 'text') {
        const text = (part.text || '').trim();
        if (text) return text;
      }
    }
  }
  return '';
}

const tsMs = (value: string | null | undefined): number => {
  const time = value ? new Date(value).getTime() : 0;
  return Number.isFinite(time) ? time : 0;
};

/**
 * One agent run: the generations one agent produced in a row. start, end,
 * totalTokens, hasError and depth are filled in by buildSubagentForest once
 * the run has all of its generations, so they are absent while it collects.
 */
interface SubagentRunNode {
  id: string;
  agent?: string;
  gens: Generation[];
  start?: number;
  end?: number;
  totalTokens?: number;
  hasError?: boolean;
  depth?: number;
}

interface SubagentForest {
  runs: Map<string, SubagentRunNode>;
  spawnedBy: Map<string, SubagentRunNode[]>;
  topRuns: SubagentRunNode[];
  byId: Map<string, Generation>;
}

function buildSubagentForest(gens: Generation[] | null | undefined): SubagentForest {
  const byId = new Map<string, Generation>((gens || []).map((gen) => [gen.generation_id, gen]));
  const inConvParent = (gen: Generation) => {
    const parentID = (gen.parent_generation_ids || [])[0];
    return parentID && byId.has(parentID) ? byId.get(parentID) : null;
  };
  const runRootId = (gen: Generation) => {
    let current = gen;
    const seen = new Set<string>();
    for (;;) {
      if (seen.has(current.generation_id)) return current.generation_id;
      seen.add(current.generation_id);
      const parent = inConvParent(current);
      if (parent && (parent.agent_name || '') === (current.agent_name || '')) {
        current = parent;
        continue;
      }
      return current.generation_id;
    }
  };
  const runs = new Map<string, SubagentRunNode>();
  for (const gen of gens || []) {
    const rootID = runRootId(gen);
    let run = runs.get(rootID);
    if (!run) {
      run = {
        id: rootID,
        agent: (byId.get(rootID) || gen).agent_name,
        gens: [],
      };
      runs.set(rootID, run);
    }
    run.gens.push(gen);
  }
  const spawnedBy = new Map<string, SubagentRunNode[]>();
  const topRuns: SubagentRunNode[] = [];
  for (const run of runs.values()) {
    run.gens.sort((a: Generation, b: Generation) => tsMs(a.started_at) - tsMs(b.started_at));
    run.start = Math.min(...run.gens.map((gen) => tsMs(gen.started_at) || Infinity));
    run.end = Math.max(...run.gens.map((gen) => tsMs(gen.completed_at) || tsMs(gen.started_at) || 0));
    run.totalTokens = run.gens.reduce((sum, gen) => sum + (gen.total_tokens || 0), 0);
    run.hasError = run.gens.some((gen) => gen.call_error);
    const parent = inConvParent(byId.get(run.id) as Generation);
    if (parent && (parent.agent_name || '') !== (run.agent || '')) {
      if (!spawnedBy.has(parent.generation_id)) spawnedBy.set(parent.generation_id, []);
      spawnedBy.get(parent.generation_id)?.push(run);
    } else {
      topRuns.push(run);
    }
  }
  for (const children of spawnedBy.values()) children.sort((a, b) => (a.start ?? 0) - (b.start ?? 0));
  topRuns.sort((a, b) => (a.start ?? 0) - (b.start ?? 0));
  const depthSeen = new Set<string>();
  const setDepth = (run: SubagentRunNode, depth: number) => {
    if (depthSeen.has(run.id)) return;
    depthSeen.add(run.id);
    run.depth = depth;
    run.gens.forEach((gen) => {
      (spawnedBy.get(gen.generation_id) || []).forEach((child) => {
        setDepth(child, depth + 1);
      });
    });
  };
  topRuns.forEach((run) => {
    setDepth(run, 0);
  });
  return { runs, spawnedBy, topRuns, byId };
}

/** One generation in reading order, with the run it belongs to. */
interface ForestRow {
  gen: Generation;
  depth: number;
  run: SubagentRunNode;
  runPath: string[];
  isRunStart: boolean;
  /** 1-based position in the flattened forest, set after the walk. */
  n?: number;
}

function flattenForest(forest: SubagentForest): ForestRow[] {
  const out: ForestRow[] = [];
  const seen = new Set<string>();
  const visit = (run: SubagentRunNode, depth: number, path: string[]) => {
    if (seen.has(run.id)) return;
    seen.add(run.id);
    const runPath = path.concat(run.id);
    run.gens.forEach((gen, index) => {
      out.push({
        gen,
        depth,
        run,
        runPath,
        isRunStart: index === 0 && depth > 0,
      });
      (forest.spawnedBy.get(gen.generation_id) || []).forEach((child) => {
        visit(child, depth + 1, runPath);
      });
    });
  };
  forest.topRuns.forEach((run) => {
    visit(run, 0, []);
  });
  forest.runs.forEach((run) => {
    if (!seen.has(run.id)) visit(run, 0, []);
  });
  out.forEach((row, index) => {
    row.n = index + 1;
  });
  return out;
}

interface StepTokenWork {
  generated: number;
  ingested: number;
  work: number;
}

function stepTokenWork(gen: Generation | null | undefined): StepTokenWork {
  const buckets: Partial<TokenBuckets> = gen?.token_buckets || {};
  const generated = (buckets.output || 0) + (buckets.reasoning || 0);
  const ingested = (buckets.fresh_input || 0) + (buckets.cache_write || 0);
  return { generated, ingested, work: generated + ingested };
}

function mergedSpan(intervals: Array<[number, number]>): number {
  const sorted = intervals.filter((interval) => interval[1] > interval[0]).sort((a, b) => a[0] - b[0]);
  let total = 0;
  let currentStart = -1;
  let currentEnd = -1;
  for (const [start, end] of sorted) {
    if (start > currentEnd) {
      if (currentEnd > currentStart) total += currentEnd - currentStart;
      currentStart = start;
      currentEnd = end;
    } else {
      currentEnd = Math.max(currentEnd, end);
    }
  }
  if (currentEnd > currentStart) total += currentEnd - currentStart;
  return total;
}

function argumentBody(input: ToolCallInput): string {
  if (input == null) return '';
  if (typeof input === 'string') return input;
  try {
    return JSON.stringify(input, null, 2);
  } catch (_) {
    return String(input);
  }
}

/** One tool call and the result that answered it, as a transcript row. */
interface ToolCallRow {
  key: string;
  genId: string;
  id: string;
  name: string;
  input: ToolCallInput;
  result: ToolResult | null;
  failed: boolean;
}

export interface TranscriptWorkBlock {
  kind: 'work';
  id: string;
  genIds: string[];
  calls: ToolCallRow[];
  subruns: SubagentRunSummary[];
  durationSec: number;
}

interface TranscriptProseBlock {
  kind: 'prose';
  text: string;
  genId: string;
}

interface TranscriptReasoningBlock {
  kind: 'reasoning';
  id: string;
  text: string;
  genId: string;
  /** The model reasoned, but the host kept no text for it. */
  notRecorded: boolean;
}

interface TranscriptErrorBlock {
  kind: 'error';
  id: string;
  text: string;
  genId: string;
}

export type TranscriptBlock =
  | TranscriptWorkBlock
  | TranscriptProseBlock
  | TranscriptReasoningBlock
  | TranscriptErrorBlock;

/** One subagent run, flattened into the row the parent transcript shows. */
interface SubagentRunSummary {
  id: string;
  agent?: string;
  gens: Generation[];
  calls: ToolCallRow[];
  errors: TranscriptErrorBlock[];
  task: string;
  returned: string;
  durationSec: number;
  failedCount: number;
  childCount: number;
}

function summarizeSubagentRun(
  run: SubagentRunNode,
  forest: SubagentForest,
  nextByID: Map<string, Generation | null>,
  consumedResults: Set<ToolResult>,
): SubagentRunSummary {
  const calls: ToolCallRow[] = [];
  const errors: TranscriptErrorBlock[] = [];
  let returned = '';
  for (const gen of run.gens) {
    const blocks = generationTranscriptBlocks(gen, nextByID.get(gen.generation_id), [], consumedResults);
    for (const block of blocks) {
      if (block.kind === 'work') calls.push(...block.calls);
      if (block.kind === 'error') errors.push(block);
      if (block.kind === 'prose' && block.text.trim()) returned = block.text.trim();
    }
  }
  const children = run.gens.flatMap((gen) => forest.spawnedBy.get(gen.generation_id) || []);
  return {
    id: run.id,
    agent: run.agent,
    gens: run.gens,
    calls,
    errors,
    task: firstUserText(run.gens[0] as Generation) || leadingAssistantText(run.gens[0] as Generation) || 'Subagent run',
    returned,
    durationSec: Math.max(0, ((run.end ?? 0) - (run.start ?? 0)) / 1000),
    failedCount: calls.filter((call) => call.failed).length + errors.length,
    childCount: children.length,
  };
}

function generationTranscriptBlocks(
  gen: Generation,
  next: Generation | null | undefined,
  subruns: SubagentRunSummary[],
  consumedResults: Set<ToolResult> = new Set(),
): TranscriptBlock[] {
  const blocks: TranscriptBlock[] = [];
  let work: TranscriptWorkBlock | null = null;
  let sawReasoning = false;
  let reasoningIndex = 0;
  let callIndex = 0;
  // A generation times one model call, not each batch of tool calls it
  // emitted, so only its first work block takes the duration.
  let durationTaken = false;
  const closeWork = () => {
    work = null;
  };
  const ensureWork = () => {
    if (!work) {
      work = {
        kind: 'work',
        id: `work-${gen.generation_id}-${blocks.length}`,
        genIds: [gen.generation_id],
        calls: [],
        subruns: [],
        durationSec: durationTaken ? 0 : Math.max(0, Number(gen.duration_seconds) || 0),
      };
      durationTaken = true;
      blocks.push(work);
    }
    return work;
  };

  for (const message of gen.output || []) {
    for (const part of message.parts || []) {
      const kind = partKind(part);
      if (kind === 'tool_call' && part.tool_call) {
        const call = part.tool_call;
        const result = resolveResult(gen, next, call, consumedResults);
        const row: ToolCallRow = {
          key: `${gen.generation_id}:${callIndex++}`,
          genId: gen.generation_id,
          id: call.id || '',
          name: call.name || 'tool',
          input: call.input_json == null ? null : (call.input_json as ToolCallInput),
          result,
          failed: !!result?.is_error,
        };
        ensureWork().calls.push(row);
        continue;
      }
      if (kind === 'tool_result') continue;
      closeWork();
      if (kind === 'text' && (part.text || '').trim()) {
        blocks.push({
          kind: 'prose',
          text: part.text ?? '',
          genId: gen.generation_id,
        });
      } else if (kind === 'thinking' && (part.thinking || '').trim()) {
        sawReasoning = true;
        blocks.push({
          kind: 'reasoning',
          id: `${gen.generation_id}:reasoning:${reasoningIndex++}`,
          text: part.thinking ?? '',
          genId: gen.generation_id,
          notRecorded: false,
        });
      }
    }
  }

  if (gen.call_error) {
    closeWork();
    blocks.push({
      kind: 'error',
      id: `${gen.generation_id}:error`,
      text: gen.call_error,
      genId: gen.generation_id,
    });
  }
  if (gen.thinking_enabled && !sawReasoning) {
    blocks.unshift({
      kind: 'reasoning',
      id: `${gen.generation_id}:reasoning:0`,
      text: '',
      genId: gen.generation_id,
      notRecorded: true,
    });
  }
  if (subruns.length > 0) {
    let owner: TranscriptWorkBlock | null = null;
    for (let index = blocks.length - 1; index >= 0; index--) {
      if (blocks[index]?.kind === 'work') {
        owner = blocks[index] as TranscriptWorkBlock;
        break;
      }
    }
    if (!owner) owner = ensureWork();
    owner.subruns.push(...subruns);
  }
  return blocks;
}

function appendTranscriptBlocks(target: TranscriptBlock[], incoming: TranscriptBlock[]): void {
  for (const block of incoming) {
    const previous = target[target.length - 1];
    if (previous && previous.kind === 'work' && block.kind === 'work') {
      previous.calls.push(...block.calls);
      previous.subruns.push(...block.subruns);
      previous.genIds.push(...block.genIds);
      previous.durationSec += block.durationSec;
      continue;
    }
    target.push(block);
  }
}

/**
 * One exchange in the thread: the user message that opened it and every
 * generation and block that answered it. The measurements are added by
 * finishTurn, so a turn only carries them once it is closed.
 */
interface TranscriptTurn {
  index: number;
  startGenId: string;
  userText: string;
  userStartedAt: string;
  gens: Generation[];
  genIds: string[];
  blocks: TranscriptBlock[];
  start: number;
  end: number;
  durationSec: number;
  toolCount: number;
  failedCount: number;
  subrunCount: number;
}

type TranscriptTurnDraft = Omit<
  TranscriptTurn,
  'start' | 'end' | 'durationSec' | 'toolCount' | 'failedCount' | 'subrunCount'
> &
  Partial<TranscriptTurn>;

export function buildTranscript(steps: Generation[] | null | undefined): TranscriptTurn[] {
  const ordered = (steps || []).slice().sort((a, b) => tsMs(a.started_at) - tsMs(b.started_at));
  const forest = buildSubagentForest(ordered);
  const rows = flattenForest(forest);
  const nextByID = new Map<string, Generation | null>();
  for (const run of forest.runs.values()) {
    run.gens.forEach((gen, index) => {
      nextByID.set(gen.generation_id, run.gens[index + 1] || null);
    });
  }
  const previousTopLevelByAgent = new Map<string, Generation>();
  for (const gen of ordered) {
    if (isSubagent(gen.agent_name)) continue;
    const agent = gen.agent_name || '';
    const previous = previousTopLevelByAgent.get(agent);
    if (previous) nextByID.set(previous.generation_id, gen);
    previousTopLevelByAgent.set(agent, gen);
  }
  const consumedResults = new Set<ToolResult>();
  const subrunSummary = new Map<string, SubagentRunSummary>();
  const summarizeRun = (run: SubagentRunNode): SubagentRunSummary => {
    if (subrunSummary.has(run.id)) return subrunSummary.get(run.id) as SubagentRunSummary;
    const summary = summarizeSubagentRun(run, forest, nextByID, consumedResults);
    subrunSummary.set(run.id, summary);
    const children = run.gens.flatMap((gen) => forest.spawnedBy.get(gen.generation_id) || []);
    summary.failedCount += children.reduce((sum, child) => sum + summarizeRun(child).failedCount, 0);
    return summary;
  };
  for (const run of forest.runs.values()) {
    if ((run.depth ?? 0) > 0) summarizeRun(run);
  }

  const turns: TranscriptTurn[] = [];
  let current: TranscriptTurnDraft | null = null;
  const startTurn = (gen: Generation, userText: string): TranscriptTurnDraft => ({
    index: turns.length + 1,
    startGenId: gen.generation_id,
    userText,
    userStartedAt: gen.started_at,
    gens: [],
    genIds: [],
    blocks: [],
  });
  const finishTurn = (turn: TranscriptTurnDraft | null) => {
    if (!turn || turn.gens.length === 0) return;
    const starts = turn.gens.map((gen) => tsMs(gen.started_at)).filter(Boolean);
    const ends = turn.gens.map((gen) => tsMs(gen.completed_at) || tsMs(gen.started_at)).filter(Boolean);
    turn.start = starts.length ? Math.min(...starts) : 0;
    turn.end = ends.length ? Math.max(...ends) : turn.start;
    turn.durationSec = Math.max(0, (turn.end - turn.start) / 1000);
    turn.toolCount = turn.gens.reduce((sum, gen) => sum + outputCalls(gen).length, 0);
    turn.failedCount = turn.blocks.reduce((sum, block) => {
      if (block.kind === 'error') return sum + 1;
      if (block.kind !== 'work') return sum;
      const failedCalls = block.calls.filter((call) => call.failed).length;
      const failedSubruns = block.subruns.reduce((subrunSum, run) => subrunSum + run.failedCount, 0);
      return sum + failedCalls + failedSubruns;
    }, 0);
    turn.subrunCount = new Set(
      turn.gens
        .filter((gen) => isSubagent(gen.agent_name))
        .map((gen) => {
          const row = rows.find((candidate) => candidate.gen.generation_id === gen.generation_id);
          return row ? row.run.id : gen.generation_id;
        }),
    ).size;
    turns.push(turn as TranscriptTurn);
  };

  for (const row of rows) {
    const userText = row.depth === 0 ? firstUserText(row.gen) : '';
    if (row.depth === 0 && userText) {
      finishTurn(current);
      current = startTurn(row.gen, userText);
    } else if (!current) {
      current = startTurn(row.gen, userText);
    }
    current.gens.push(row.gen);
    current.genIds.push(row.gen.generation_id);
    if (row.depth === 0) {
      const subruns = (forest.spawnedBy.get(row.gen.generation_id) || [])
        .map((run) => subrunSummary.get(run.id))
        .filter(Boolean) as SubagentRunSummary[];
      appendTranscriptBlocks(
        current.blocks,
        generationTranscriptBlocks(row.gen, nextByID.get(row.gen.generation_id), subruns, consumedResults),
      );
    }
  }
  finishTurn(current);
  return turns;
}

/** The gap between two turns, and the turn that ended it. */
interface TranscriptIdleGap {
  durationMs: number;
  turn: TranscriptTurn;
}

interface ToolHistogramEntry {
  name: string;
  count: number;
}

interface TranscriptMetrics {
  startMs: number;
  endMs: number;
  wallMs: number;
  workingMs: number;
  idleMs: number;
  longestIdle: TranscriptIdleGap | null;
  usageAvailable: boolean;
  totalTokens: number;
  toolHistogram: ToolHistogramEntry[];
}

export function buildTranscriptMetrics(
  steps: Generation[] | null | undefined,
  turns: TranscriptTurn[] | null | undefined,
): TranscriptMetrics {
  const intervals: Array<[number, number]> = [];
  let startMs = Infinity;
  let endMs = -Infinity;
  const histogram = new Map<string, number>();
  let totalTokens = 0;
  let usageAvailable = false;
  for (const gen of steps || []) {
    const start = tsMs(gen.started_at);
    const end = tsMs(gen.completed_at) || start;
    if (start) startMs = Math.min(startMs, start);
    if (end) endMs = Math.max(endMs, end);
    if (start > 0 && end > start) intervals.push([start, end]);
    const tokens = Number(gen.total_tokens) || 0;
    totalTokens += tokens;
    if (tokens > 0) usageAvailable = true;
    for (const call of outputCalls(gen))
      histogram.set(call.name || 'tool', (histogram.get(call.name || 'tool') || 0) + 1);
  }
  if (!Number.isFinite(startMs)) startMs = 0;
  if (!Number.isFinite(endMs)) endMs = startMs;
  const wallMs = Math.max(0, endMs - startMs);
  const workingMs = mergedSpan(intervals);
  let longestIdle: TranscriptIdleGap | null = null;
  const chronological = (turns || [])
    .filter((turn) => turn.start > 0 && turn.end >= turn.start)
    .sort((a, b) => a.start - b.start);
  for (let index = 1; index < chronological.length; index++) {
    const durationMs = Math.max(0, (chronological[index]?.start ?? 0) - (chronological[index - 1]?.end ?? 0));
    if (durationMs > 0 && (!longestIdle || durationMs > longestIdle.durationMs)) {
      longestIdle = { durationMs, turn: chronological[index] as TranscriptTurn };
    }
  }
  return {
    startMs,
    endMs,
    wallMs,
    workingMs,
    idleMs: Math.max(0, wallMs - workingMs),
    longestIdle,
    usageAvailable,
    totalTokens,
    toolHistogram: [...histogram.entries()]
      .map(([name, count]) => ({ name, count }))
      .sort((a, b) => b.count - a.count || a.name.localeCompare(b.name)),
  };
}

// promptLine is the first line of what was asked, for the sticky header.
// The agent's answer is far longer than the prompt, so by the time you are
// reading it the question has scrolled away; the header carries it along.
function promptLine(turn: TranscriptTurn): string {
  const prompt = splitPreamble(turn.userText).prompt || '';
  const line =
    prompt
      .split('\n')
      .map((part) => part.trim())
      .find(Boolean) || '';
  return line.length > 120 ? `${line.slice(0, 120)}…` : line;
}

// The turn rule and the speaker labels stack into two sticky lines under
// the session bar, so which turn you are in, what you asked, and which
// block you are reading stay on screen however long the answer runs. Both
// rows are fixed-height, because the second one's offset is the first
// one's height.
const TURN_RULE_H = 32;
const SPEAKER_H = 26;

interface TurnRuleProps {
  turn: TranscriptTurn;
  slowest: boolean;
  first: boolean;
}

function TurnRule({ turn, slowest, first }: TurnRuleProps) {
  const asked = promptLine(turn);
  return (
    // A sticky box can only stick inside its own parent, so this row is a
    // direct child of the turn section rather than a wrapper's child, and
    // the gap above it is its own margin.
    <div
      style={{
        position: 'sticky',
        top: HEADER_H + 46,
        zIndex: 3,
        height: TURN_RULE_H,
        marginTop: first ? 0 : 20,
        display: 'flex',
        alignItems: 'center',
        gap: 10,
        background: 'var(--bg-canvas)',
      }}
    >
      <span
        style={{
          fontFamily: 'var(--fontFamilyMonospace)',
          fontSize: 11,
          letterSpacing: '0.1em',
          color: 'var(--fg3)',
          whiteSpace: 'nowrap',
        }}
      >
        TURN {turn.index}
      </span>
      {asked && (
        <span
          title={asked}
          style={{
            minWidth: 0,
            flex: '0 1 auto',
            color: 'var(--fg2)',
            fontSize: 12,
            overflow: 'hidden',
            textOverflow: 'ellipsis',
            whiteSpace: 'nowrap',
          }}
        >
          {asked}
        </span>
      )}
      <span style={{ flex: 1, height: 1, background: 'var(--border-weak)' }} />
      <span
        style={{
          fontFamily: 'var(--fontFamilyMonospace)',
          fontSize: 11,
          color: slowest ? 'var(--warning-text)' : 'var(--fg3)',
          whiteSpace: 'nowrap',
        }}
      >
        {formatDuration(turn.durationSec)}
        {slowest ? ' · slowest turn' : ` · ${turn.toolCount} ${turn.toolCount === 1 ? 'tool' : 'tools'}`}
      </span>
      {turn.failedCount > 0 && (
        <span
          style={{
            display: 'inline-flex',
            alignItems: 'center',
            gap: 5,
            fontFamily: 'var(--fontFamilyMonospace)',
            fontSize: 11,
            color: 'var(--error-text)',
            whiteSpace: 'nowrap',
          }}
        >
          <span
            style={{
              width: 6,
              height: 6,
              borderRadius: '50%',
              background: 'var(--error-text)',
            }}
          />
          {turn.failedCount} failed
        </span>
      )}
    </div>
  );
}

// Who is speaking is carried by one device: a labelled rule in that
// speaker's colour, pinned under the turn rule for as long as the block is
// on screen. The two colours are the brand orange and the timeline's blue,
// so neither collides with green for a passing call, red for a failure, or
// amber for the slowest turn.
const SPEAKERS = {
  you: {
    label: 'YOU',
    colour: 'var(--brand-orange-text)',
    rule: 'var(--speaker-you-rule)',
  },
  agent: {
    label: 'AGENT',
    colour: 'var(--agent-accent-text)',
    rule: 'var(--speaker-agent-rule)',
  },
};

type SpeakerKind = keyof typeof SPEAKERS;

interface SpeakerLabelProps {
  speaker: SpeakerKind;
  suffix?: string;
}

function SpeakerLabel({ speaker, suffix }: SpeakerLabelProps) {
  const { label, colour, rule } = SPEAKERS[speaker];
  return (
    <div
      style={{
        position: 'sticky',
        top: HEADER_H + 46 + TURN_RULE_H,
        zIndex: 2,
        height: SPEAKER_H,
        display: 'flex',
        alignItems: 'center',
        gap: 10,
        background: 'var(--bg-canvas)',
      }}
    >
      <span
        style={{
          fontFamily: 'var(--fontFamilyMonospace)',
          fontSize: 10,
          letterSpacing: '0.1em',
          color: colour,
          whiteSpace: 'nowrap',
        }}
      >
        {label}
      </span>
      {suffix && (
        <span
          style={{
            fontFamily: 'var(--fontFamilyMonospace)',
            fontSize: 11,
            color: 'var(--fg3)',
            whiteSpace: 'nowrap',
          }}
        >
          {suffix}
        </span>
      )}
      <span
        style={{
          flex: 1,
          height: 1,
          background: rule,
        }}
      />
    </div>
  );
}

interface PreambleChipProps {
  text: string;
}

function PreambleChip({ text }: PreambleChipProps) {
  const [open, setOpen] = useState(false);
  const tags = scanPreambleBlocks(String(text || '')).tags;
  const lineCount = text ? text.replace(/\s+$/, '').split('\n').length : 0;
  return (
    <div style={{ marginBottom: 10 }}>
      <button
        type="button"
        onClick={() => setOpen((value) => !value)}
        aria-expanded={open}
        style={{
          maxWidth: '100%',
          display: 'flex',
          alignItems: 'center',
          gap: 6,
          padding: '3px 9px',
          background: 'var(--preamble-chip-bg)',
          border: '1px solid var(--border-weak)',
          borderRadius: 2,
          color: 'var(--fg3)',
          cursor: 'pointer',
          fontFamily: 'var(--fontFamilyMonospace)',
          fontSize: 11,
        }}
      >
        <Icon name={open ? 'chevron' : 'cright'} size={10} />
        <span
          style={{
            overflow: 'hidden',
            textOverflow: 'ellipsis',
            whiteSpace: 'nowrap',
          }}
        >
          preamble{tags.length > 0 ? ` · ${tags.join(', ')}` : ''} · {lineCount} {lineCount === 1 ? 'line' : 'lines'}
        </span>
      </button>
      {open && (
        <pre
          style={{
            maxHeight: 220,
            overflow: 'auto',
            margin: '6px 0 0',
            padding: '8px 10px',
            background: 'var(--bg-primary)',
            border: '1px solid var(--border-weak)',
            borderRadius: 8,
            color: 'var(--fg2)',
            fontFamily: 'var(--fontFamilyMonospace)',
            fontSize: 11.5,
            lineHeight: 1.6,
            whiteSpace: 'pre-wrap',
            wordBreak: 'break-word',
          }}
        >
          {text}
        </pre>
      )}
    </div>
  );
}

interface UserMessageProps {
  turn: TranscriptTurn;
}

function UserMessage({ turn }: UserMessageProps) {
  const split = splitPreamble(turn.userText);
  const lineCount = split.prompt ? split.prompt.split('\n').length : 0;
  const clamp = lineCount > 6;
  const [full, setFull] = useState(false);
  return (
    <div style={{ paddingBottom: 4 }}>
      <SpeakerLabel speaker="you" suffix={formatTime(turn.userStartedAt)} />
      <div style={{ height: 4 }} />
      {split.preamble && <PreambleChip text={split.preamble} />}
      {split.prompt ? (
        <div
          style={{
            fontSize: 16,
            lineHeight: 1.55,
            color: 'var(--fg-max)',
            whiteSpace: 'pre-wrap',
            wordBreak: 'break-word',
            ...(clamp && !full
              ? {
                  display: '-webkit-box',
                  WebkitLineClamp: 4,
                  WebkitBoxOrient: 'vertical',
                  overflow: 'hidden',
                }
              : {}),
          }}
        >
          {split.prompt}
        </div>
      ) : (
        <div
          style={{
            color: 'var(--fg3)',
            fontSize: 12,
            fontFamily: 'var(--fontFamilyMonospace)',
          }}
        >
          No user message content captured.
        </div>
      )}
      {clamp && (
        <button
          type="button"
          onClick={() => setFull((value) => !value)}
          style={{
            marginTop: 7,
            padding: 0,
            background: 'transparent',
            border: 'none',
            color: 'var(--fg3)',
            cursor: 'pointer',
            fontSize: 11.5,
          }}
          onMouseEnter={(event) => (event.currentTarget.style.color = 'var(--fg1)')}
          onMouseLeave={(event) => (event.currentTarget.style.color = 'var(--fg3)')}
        >
          {full ? 'Show less' : `Show full message · ${lineCount} lines`}
        </button>
      )}
    </div>
  );
}

interface ProseBlockProps {
  text: string;
}

export function ProseBlock({ text }: ProseBlockProps) {
  // markdown-to-jsx resolves to the vendored global. If that script failed to
  // load, show the text rather than nothing: reading the transcript is the
  // whole point.
  if (!Markdown) {
    return (
      <div
        style={{
          fontSize: 14.5,
          lineHeight: 1.68,
          color: 'var(--fg1)',
          whiteSpace: 'pre-wrap',
          wordBreak: 'break-word',
          marginBottom: 12,
        }}
      >
        {text}
      </div>
    );
  }
  return (
    <div className="sigil-md" style={{ marginBottom: 12 }}>
      <Markdown options={MARKDOWN_OPTIONS}>{String(text || '')}</Markdown>
    </div>
  );
}

interface ReasoningBlockProps {
  block: TranscriptReasoningBlock;
  open: boolean;
  onToggle: () => void;
}

function ReasoningBlock({ block, open, onToggle }: ReasoningBlockProps) {
  if (block.notRecorded) {
    return (
      <div
        title="The model reasoned on this call, but the host did not persist the reasoning text."
        style={{
          display: 'inline-flex',
          alignItems: 'center',
          gap: 6,
          paddingBottom: 10,
          color: 'var(--fg3)',
          fontFamily: 'var(--fontFamilyMonospace)',
          fontSize: 11,
        }}
      >
        <Icon name="info" size={10} />
        reasoning, not recorded
      </div>
    );
  }
  return (
    <div>
      <button
        type="button"
        onClick={onToggle}
        aria-expanded={open}
        style={{
          display: 'inline-flex',
          alignItems: 'center',
          gap: 6,
          padding: '0 0 10px',
          background: 'transparent',
          border: 'none',
          color: 'var(--agent-accent-text)',
          cursor: 'pointer',
          fontFamily: 'var(--fontFamilyMonospace)',
          fontSize: 11,
        }}
      >
        <Icon name={open ? 'chevron' : 'cright'} size={10} />
        Reasoning
      </button>
      {open && (
        <div
          style={{
            borderLeft: '2px solid var(--viz-blue)',
            padding: '2px 0 2px 12px',
            marginBottom: 12,
            color: 'var(--fg2)',
            fontSize: 13,
            lineHeight: 1.6,
            fontStyle: 'italic',
            whiteSpace: 'pre-wrap',
            wordBreak: 'break-word',
          }}
        >
          {block.text}
        </div>
      )}
    </div>
  );
}

interface CallErrorBlockProps {
  block: TranscriptErrorBlock;
  compact?: boolean;
}

function CallErrorBlock({ block, compact = false }: CallErrorBlockProps) {
  return (
    <div
      role="alert"
      style={{
        marginBottom: compact ? 0 : 12,
        padding: compact ? '9px 10px' : '10px 12px',
        border: '1px solid var(--error-border)',
        borderRadius: 2,
        background: 'var(--tool-failed-bg)',
        color: 'var(--error-text)',
      }}
    >
      <div
        style={{
          marginBottom: 5,
          fontFamily: 'var(--fontFamilyMonospace)',
          fontSize: 10,
          letterSpacing: '0.08em',
        }}
      >
        MODEL CALL FAILED
      </div>
      <div
        style={{
          fontFamily: 'var(--fontFamilyMonospace)',
          fontSize: compact ? 10.5 : 11.5,
          lineHeight: 1.6,
          whiteSpace: 'pre-wrap',
          wordBreak: 'break-word',
        }}
      >
        {block.text}
      </div>
    </div>
  );
}

function SuccessGlyph() {
  return (
    <svg
      width={12}
      height={12}
      viewBox="0 0 24 24"
      fill="none"
      stroke="var(--viz-green)"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <title>Succeeded</title>
      <path d="m5 13 4 4L19 7" />
    </svg>
  );
}

function FailureGlyph() {
  return (
    <svg
      width={12}
      height={12}
      viewBox="0 0 24 24"
      fill="none"
      stroke="var(--error-text)"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <title>Failed</title>
      <path d="M6 6l12 12M18 6 6 18" />
    </svg>
  );
}

interface ToolRowProps {
  call: ToolCallRow;
  compact?: boolean;
}

function ToolRow({ call, compact = false }: ToolRowProps) {
  const body = resultBody(call.result);
  const resolved = !!call.result;
  const lineCount = resolved ? (body ? body.split('\n').length : 0) : 0;
  const [open, setOpen] = useState(call.failed);
  useEffect(() => {
    if (call.failed) setOpen(true);
  }, [call.failed]);
  const args = argumentBody(call.input);
  const statusLabel = !resolved
    ? ''
    : call.failed
      ? `error · ${lineCount} ${lineCount === 1 ? 'line' : 'lines'}`
      : `${lineCount} ${lineCount === 1 ? 'line' : 'lines'}`;
  const rowHeight = compact ? 28 : 30;
  return (
    <Fragment>
      <button
        type="button"
        onClick={() => setOpen((value) => !value)}
        aria-expanded={open}
        style={{
          width: '100%',
          display: 'grid',
          gridTemplateColumns: compact ? '14px 84px 1fr auto' : '14px 92px 1fr auto',
          alignItems: 'center',
          gap: compact ? 8 : 10,
          padding: compact ? '0 10px' : '0 12px',
          height: rowHeight,
          cursor: 'pointer',
          border: 'none',
          borderBottom: '1px solid var(--border-weak)',
          borderLeft: call.failed ? '2px solid var(--error-main)' : '2px solid transparent',
          background: call.failed ? 'var(--tool-failed-bg)' : 'transparent',
          textAlign: 'left',
          fontFamily: 'var(--fontFamilyMonospace)',
        }}
        onMouseEnter={(event) =>
          (event.currentTarget.style.background = call.failed ? 'var(--tool-failed-hover)' : 'var(--row-hover)')
        }
        onMouseLeave={(event) =>
          (event.currentTarget.style.background = call.failed ? 'var(--tool-failed-bg)' : 'transparent')
        }
      >
        <span
          style={{
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
          }}
        >
          {resolved ? call.failed ? <FailureGlyph /> : <SuccessGlyph /> : null}
        </span>
        <span
          style={{
            color: 'var(--fg1)',
            fontSize: compact ? 11 : 11.5,
            overflow: 'hidden',
            textOverflow: 'ellipsis',
            whiteSpace: 'nowrap',
          }}
        >
          {call.name}
        </span>
        <span
          style={{
            color: call.failed ? 'var(--error-text)' : 'var(--fg2)',
            fontSize: compact ? 11 : 11.5,
            overflow: 'hidden',
            textOverflow: 'ellipsis',
            whiteSpace: 'nowrap',
            minWidth: 0,
          }}
        >
          {toolCallArgPreview(call.input)}
        </span>
        <span
          style={{
            color: call.failed ? 'var(--error-text)' : 'var(--fg3)',
            fontSize: compact ? 10.5 : 11,
            whiteSpace: 'nowrap',
          }}
        >
          {statusLabel}
        </span>
      </button>
      {open && (
        <div
          style={{
            background: 'var(--bg-canvas)',
            padding: compact ? '8px 10px 10px 32px' : '10px 12px 12px 36px',
            borderBottom: '1px solid var(--border-weak)',
          }}
        >
          {args && (
            <div style={{ marginBottom: resolved ? 10 : 0 }}>
              <div
                style={{
                  marginBottom: 6,
                  fontFamily: 'var(--fontFamilyMonospace)',
                  fontSize: 10,
                  letterSpacing: '0.08em',
                  color: 'var(--fg3)',
                }}
              >
                ARGUMENTS
              </div>
              <CappedBlock>{args}</CappedBlock>
            </div>
          )}
          {resolved && (
            <div>
              <div
                style={{
                  marginBottom: 6,
                  fontFamily: 'var(--fontFamilyMonospace)',
                  fontSize: 10,
                  letterSpacing: '0.08em',
                  color: call.failed ? 'var(--error-text)' : 'var(--fg3)',
                }}
              >
                RESULT · {lineCount} {lineCount === 1 ? 'line' : 'lines'}
              </div>
              <CappedBlock preStyle={call.failed ? { color: 'var(--error-text)' } : undefined}>{body}</CappedBlock>
            </div>
          )}
        </div>
      )}
    </Fragment>
  );
}

interface SubagentRunProps {
  run: SubagentRunSummary;
}

function SubagentRun({ run }: SubagentRunProps) {
  const [open, setOpen] = useState(false);
  const color = agentColor(run.agent);
  const summary = String(run.task || '').replace(/\s+/g, ' ');
  return (
    <div style={{ borderBottom: '1px solid var(--border-weak)' }}>
      <button
        type="button"
        onClick={() => setOpen((value) => !value)}
        aria-expanded={open}
        style={{
          width: '100%',
          height: 30,
          padding: '0 12px',
          display: 'flex',
          alignItems: 'center',
          gap: 8,
          border: 'none',
          borderLeft: `2px solid ${color}`,
          background: 'var(--subagent-row-bg)',
          cursor: 'pointer',
          textAlign: 'left',
        }}
      >
        <Icon name={open ? 'chevron' : 'cright'} size={11} style={{ color: 'var(--fg3)' }} />
        <svg
          width={11}
          height={11}
          viewBox="0 0 24 24"
          fill="none"
          stroke={color}
          strokeWidth="2"
          aria-hidden="true"
          focusable="false"
        >
          <circle cx="12" cy="8" r="4" />
          <path d="M4 21a8 8 0 0 1 16 0" />
        </svg>
        <span
          style={{
            fontFamily: 'var(--fontFamilyMonospace)',
            fontSize: 11.5,
            color: 'var(--fg1)',
            whiteSpace: 'nowrap',
          }}
        >
          {agentShort(run.agent)}
        </span>
        <span
          style={{
            minWidth: 0,
            flex: 1,
            overflow: 'hidden',
            textOverflow: 'ellipsis',
            whiteSpace: 'nowrap',
            color: 'var(--fg3)',
            fontSize: 11.5,
          }}
        >
          subagent · {summary}
        </span>
        <span
          style={{
            fontFamily: 'var(--fontFamilyMonospace)',
            fontSize: 11,
            color: run.failedCount ? 'var(--error-text)' : 'var(--fg3)',
            whiteSpace: 'nowrap',
          }}
        >
          {run.calls.length} tools · {formatDuration(run.durationSec)}
        </span>
      </button>
      {open && (
        <div
          style={{
            borderLeft: `2px solid ${color}`,
            paddingLeft: 18,
            background: 'var(--bg-canvas)',
          }}
        >
          {run.calls.map((call) => (
            <ToolRow key={call.key} call={call} compact />
          ))}
          {run.errors.map((error) => (
            <CallErrorBlock key={error.id} block={error} compact />
          ))}
          {run.childCount > 0 && (
            <div
              style={{
                padding: '8px 10px',
                color: 'var(--fg3)',
                fontFamily: 'var(--fontFamilyMonospace)',
                fontSize: 10.5,
              }}
            >
              {run.childCount} nested {run.childCount === 1 ? 'run' : 'runs'}
            </div>
          )}
          {run.returned && (
            <div
              style={{
                padding: '9px 10px 11px',
                color: 'var(--fg2)',
                fontSize: 12.5,
                lineHeight: 1.5,
                whiteSpace: 'pre-wrap',
                wordBreak: 'break-word',
              }}
            >
              {run.returned}
            </div>
          )}
        </div>
      )}
    </div>
  );
}

interface WorkGroupProps {
  block: TranscriptWorkBlock;
  open: boolean;
  onToggle: () => void;
}

function WorkGroup({ block, open, onToggle }: WorkGroupProps) {
  const failedCount = block.calls.filter((call) => call.failed).length;
  const large = block.calls.length > 40;
  const subCount = block.subruns.length;
  return (
    <div
      style={{
        display: 'flex',
        flexDirection: 'column',
        gap: 4,
        marginBottom: 14,
      }}
    >
      <button
        type="button"
        onClick={onToggle}
        aria-expanded={open}
        style={{
          width: '100%',
          display: 'flex',
          alignItems: 'center',
          gap: 8,
          padding: '0 0 4px',
          border: 'none',
          background: 'transparent',
          cursor: 'pointer',
          textAlign: 'left',
        }}
      >
        <Icon name={open ? 'chevron' : 'cright'} size={11} style={{ color: 'var(--fg3)' }} />
        <span
          style={{
            fontFamily: 'var(--fontFamilyMonospace)',
            fontSize: 11,
            color: 'var(--fg2)',
            whiteSpace: 'nowrap',
          }}
        >
          {block.calls.length} {block.calls.length === 1 ? 'tool' : 'tools'}
          {large && !open ? ' - collapsed' : ''}
        </span>
        {block.durationSec > 0 && (
          <span
            style={{
              fontFamily: 'var(--fontFamilyMonospace)',
              fontSize: 11,
              color: 'var(--fg3)',
              whiteSpace: 'nowrap',
            }}
          >
            · {formatDuration(block.durationSec)}
          </span>
        )}
        {failedCount > 0 && (
          <span
            style={{
              fontFamily: 'var(--fontFamilyMonospace)',
              fontSize: 11,
              color: 'var(--error-text)',
              whiteSpace: 'nowrap',
            }}
          >
            · {failedCount} failed
          </span>
        )}
        {subCount > 0 && (
          <span
            style={{
              fontFamily: 'var(--fontFamilyMonospace)',
              fontSize: 11,
              color: 'var(--fg3)',
              whiteSpace: 'nowrap',
            }}
          >
            · {subCount} {subCount === 1 ? 'subagent' : 'subagents'}
          </span>
        )}
        <span
          style={{
            flex: 1,
            height: 1,
            background: 'var(--border-weak)',
          }}
        />
      </button>
      {open && (
        <div
          style={{
            border: '1px solid var(--border-weak)',
            borderRadius: 8,
            background: 'var(--bg-canvas)',
            overflow: 'hidden',
          }}
        >
          {block.calls.map((call) => (
            <ToolRow key={call.key} call={call} />
          ))}
          {block.subruns.map((run) => (
            <SubagentRun key={run.id} run={run} />
          ))}
        </div>
      )}
    </div>
  );
}

interface AgentBlockProps {
  turn: TranscriptTurn;
  openGroups: ReadonlySet<string>;
  toggleGroup: (id: string) => void;
  openReasoning: ReadonlySet<string>;
  toggleReasoning: (id: string) => void;
}

function AgentBlock({ turn, openGroups, toggleGroup, openReasoning, toggleReasoning }: AgentBlockProps) {
  return (
    <div style={{ marginTop: 10, paddingBottom: 2 }}>
      <SpeakerLabel speaker="agent" />
      <div style={{ height: 6 }} />
      {turn.blocks.length === 0 && (
        <div
          style={{
            color: 'var(--fg3)',
            fontSize: 12,
            fontFamily: 'var(--fontFamilyMonospace)',
            paddingBottom: 10,
          }}
        >
          No message content captured. Re-run with{' '}
          <code style={{ color: 'var(--fg1)' }}>AGENTO11Y_CONTENT_CAPTURE_MODE=full</code> to record prompts and
          responses.
        </div>
      )}
      {turn.blocks.map((block, index) => {
        if (block.kind === 'prose') return <ProseBlock key={`p${block.genId}-${index}`} text={block.text} />;
        if (block.kind === 'reasoning') {
          return (
            <ReasoningBlock
              key={block.id}
              block={block}
              open={openReasoning.has(block.id)}
              onToggle={() => toggleReasoning(block.id)}
            />
          );
        }
        if (block.kind === 'error') return <CallErrorBlock key={block.id} block={block} />;
        return (
          <WorkGroup
            key={block.id}
            block={block}
            open={openGroups.has(block.id)}
            onToggle={() => toggleGroup(block.id)}
          />
        );
      })}
    </div>
  );
}

// TurnPager is the thread's left rail: two dimmed controls that step to
// the turn before or after the one on screen. It sticks under the session
// bar so it stays reachable in a session with many turns.
interface TurnPagerProps {
  turns: TranscriptTurn[];
  activeTurn: number;
  onJump: (id: string) => void;
}

function TurnPager({ turns, activeTurn, onJump }: TurnPagerProps) {
  const index = Math.max(
    0,
    turns.findIndex((turn) => turn.index === activeTurn),
  );
  const steps = [
    {
      label: 'previous',
      rotate: 180,
      turn: index > 0 ? turns[index - 1] : null,
    },
    {
      label: 'next',
      rotate: 0,
      turn: index < turns.length - 1 ? turns[index + 1] : null,
    },
  ];
  return (
    <div
      style={{
        position: 'sticky',
        top: HEADER_H + 46 + 28,
        display: 'flex',
        flexDirection: 'column',
        gap: 8,
      }}
    >
      {steps.map((step) => (
        <button
          type="button"
          key={step.label}
          disabled={!step.turn}
          onClick={() => step.turn && onJump(step.turn.startGenId)}
          title={step.turn ? `Go to turn ${step.turn.index}` : `No ${step.label} turn`}
          style={{
            display: 'flex',
            alignItems: 'center',
            gap: 4,
            padding: '3px 0',
            background: 'transparent',
            border: 'none',
            textAlign: 'left',
            color: 'var(--fg3)',
            opacity: step.turn ? 1 : 0.35,
            cursor: step.turn ? 'pointer' : 'default',
            fontFamily: 'var(--fontFamilyMonospace)',
            fontSize: 10.5,
          }}
          onMouseEnter={(event) => {
            if (step.turn) event.currentTarget.style.color = 'var(--fg1)';
          }}
          onMouseLeave={(event) => {
            event.currentTarget.style.color = 'var(--fg3)';
          }}
        >
          <Icon
            name="chevron"
            size={12}
            style={{
              transform: `rotate(${step.rotate}deg)`,
              flex: 'none',
            }}
          />
          {step.label}
        </button>
      ))}
    </div>
  );
}

interface ConversationThreadProps {
  turns: TranscriptTurn[];
  jumpRef: MutableRefObject<(id: string) => void>;
}

function ConversationThread({ turns, jumpRef }: ConversationThreadProps) {
  const groups = useMemo(() => turns.flatMap((turn) => turn.blocks.filter((block) => block.kind === 'work')), [turns]);
  const [openGroups, setOpenGroups] = useState(
    () => new Set(groups.filter((group) => group.calls.length <= 40).map((group) => group.id)),
  );
  const [openReasoning, setOpenReasoning] = useState<Set<string>>(() => new Set());
  const [activeTurn, setActiveTurn] = useState(1);
  const [flashID, setFlashID] = useState<string | null>(null);
  const [hashGenID, setHashGenID] = useState(generationIDFromHash);
  const turnRefs = useRef<Record<string, HTMLElement | null>>({});
  const flashTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const knownGroups = useRef(new Set(groups.map((group) => group.id)));
  const handledHash = useRef('');
  const pendingHash = useRef('');

  useEffect(() => {
    const valid = new Set(groups.map((group) => group.id));
    const previousKnown = knownGroups.current;
    setOpenGroups((previous) => {
      const next = new Set([...previous].filter((id) => valid.has(id)));
      groups.forEach((group) => {
        if (group.calls.length <= 40 && !previousKnown.has(group.id)) next.add(group.id);
      });
      return next;
    });
    knownGroups.current = valid;
  }, [groups]);
  useEffect(
    () => () => {
      if (flashTimer.current) clearTimeout(flashTimer.current);
    },
    [],
  );

  const turnByGen = useMemo(() => {
    const map = new Map<string, TranscriptTurn>();
    turns.forEach((turn) => {
      turn.genIds.forEach((id) => {
        map.set(id, turn);
      });
    });
    return map;
  }, [turns]);
  const groupByGen = useMemo(() => {
    const map = new Map<string, string>();
    groups.forEach((group) => {
      group.genIds.forEach((id) => {
        map.set(id, group.id);
      });
      group.subruns.forEach((run) => {
        run.gens.forEach((gen) => {
          map.set(gen.generation_id, group.id);
        });
      });
    });
    return map;
  }, [groups]);

  const jumpTo = useCallback(
    (id: string) => {
      const turn = turnByGen.get(id);
      if (!turn) return;
      const groupID = groupByGen.get(id);
      if (groupID) setOpenGroups((previous) => (previous.has(groupID) ? previous : new Set(previous).add(groupID)));
      setActiveTurn(turn.index);
      setFlashID(null);
      requestAnimationFrame(() =>
        requestAnimationFrame(() => {
          const node = turnRefs.current[turn.startGenId];
          if (node) {
            const top = window.scrollY + node.getBoundingClientRect().top - (HEADER_H + 46 + 24);
            window.scrollTo({
              top: Math.max(0, top),
              behavior: 'smooth',
            });
          }
          setFlashID(turn.startGenId);
        }),
      );
      if (flashTimer.current) clearTimeout(flashTimer.current);
      flashTimer.current = setTimeout(() => setFlashID(null), 1400);
    },
    [groupByGen, turnByGen],
  );
  useEffect(() => {
    jumpRef.current = jumpTo;
    return () => {
      if (jumpRef.current === jumpTo) jumpRef.current = () => {};
    };
  }, [jumpRef, jumpTo]);

  useEffect(() => {
    const syncHash = () => {
      handledHash.current = '';
      pendingHash.current = '';
      setHashGenID(generationIDFromHash());
    };
    window.addEventListener('hashchange', syncHash);
    window.addEventListener('popstate', syncHash);
    return () => {
      window.removeEventListener('hashchange', syncHash);
      window.removeEventListener('popstate', syncHash);
    };
  }, []);
  useEffect(() => {
    if (!hashGenID) {
      handledHash.current = '';
      return;
    }
    if (handledHash.current === hashGenID || pendingHash.current === hashGenID || !turnByGen.has(hashGenID)) return;
    pendingHash.current = hashGenID;
    setTimeout(() => {
      pendingHash.current = '';
      handledHash.current = hashGenID;
      jumpTo(hashGenID);
    }, 0);
  }, [hashGenID, turnByGen, jumpTo]);

  // Track the turn under the session bar so the rail's previous/next
  // move relative to what the reader is looking at, not to the last jump.
  useEffect(() => {
    if (turns.length === 0) return undefined;
    let frame = 0;
    const measure = () => {
      frame = 0;
      let current = turns[0]?.index ?? 0;
      for (const turn of turns) {
        const node = turnRefs.current[turn.startGenId];
        if (!node) continue;
        if (node.getBoundingClientRect().top - (HEADER_H + 46 + 28) > 0) break;
        current = turn.index;
      }
      setActiveTurn(current);
    };
    const onScroll = () => {
      if (!frame) frame = requestAnimationFrame(measure);
    };
    measure();
    window.addEventListener('scroll', onScroll, { passive: true });
    window.addEventListener('resize', onScroll);
    return () => {
      if (frame) cancelAnimationFrame(frame);
      window.removeEventListener('scroll', onScroll);
      window.removeEventListener('resize', onScroll);
    };
  }, [turns]);

  const slowest = turns.reduce<TranscriptTurn | null>(
    (current, turn) => (!current || turn.durationSec > current.durationSec ? turn : current),
    null,
  );
  const toggleGroup = (id: string) =>
    setOpenGroups((previous) => {
      const next = new Set(previous);
      next.has(id) ? next.delete(id) : next.add(id);
      return next;
    });
  const toggleReasoning = (id: string) =>
    setOpenReasoning((previous) => {
      const next = new Set(previous);
      next.has(id) ? next.delete(id) : next.add(id);
      return next;
    });

  if (turns.length === 0) {
    return (
      <div
        style={{
          color: 'var(--fg2)',
          fontSize: 12,
          fontFamily: 'var(--fontFamilyMonospace)',
        }}
      >
        No turns recorded.
      </div>
    );
  }
  return (
    <div
      style={{
        display: 'flex',
        gap: 16,
        maxWidth: 968,
        margin: '0 auto',
      }}
    >
      <div style={{ width: 72, flex: 'none' }}>
        <TurnPager turns={turns} activeTurn={activeTurn} onJump={jumpTo} />
      </div>
      <div
        style={{
          flex: 1,
          minWidth: 0,
          maxWidth: 880,
          display: 'flex',
          flexDirection: 'column',
          gap: 8,
        }}
      >
        {turns.map((turn, index) => (
          <section
            key={turn.startGenId}
            ref={(node) => {
              turnRefs.current[turn.startGenId] = node;
            }}
            className={flashID === turn.startGenId ? 'sigil-step-flash' : undefined}
            style={{
              borderRadius: 8,
              outline: activeTurn === turn.index ? '1px solid transparent' : 'none',
            }}
          >
            <TurnRule turn={turn} slowest={!!slowest && slowest.startGenId === turn.startGenId} first={index === 0} />
            <UserMessage turn={turn} />
            <AgentBlock
              turn={turn}
              openGroups={openGroups}
              toggleGroup={toggleGroup}
              openReasoning={openReasoning}
              toggleReasoning={toggleReasoning}
            />
          </section>
        ))}
      </div>
    </div>
  );
}

interface PanelSectionProps {
  title: string;
  children?: ReactNode;
  aside?: ReactNode;
}

function PanelSection({ title, children, aside }: PanelSectionProps) {
  return (
    <section>
      <div
        style={{
          display: 'flex',
          alignItems: 'center',
          marginBottom: 10,
        }}
      >
        <span
          style={{
            fontFamily: 'var(--fontFamilyMonospace)',
            fontSize: 10,
            letterSpacing: '0.1em',
            color: 'var(--fg3)',
          }}
        >
          {title}
        </span>
        {aside && (
          <span
            style={{
              marginLeft: 'auto',
              fontFamily: 'var(--fontFamilyMonospace)',
              fontSize: 10.5,
              color: 'var(--fg3)',
            }}
          >
            {aside}
          </span>
        )}
      </div>
      {children}
    </section>
  );
}

interface TimelinePanelProps {
  turns: TranscriptTurn[];
  metrics: TranscriptMetrics;
  onJump: (id: string) => void;
}

function TimelinePanel({ turns, metrics, onJump }: TimelinePanelProps) {
  const span = Math.max(1, metrics.endMs - metrics.startMs);
  const slowest = turns.reduce<TranscriptTurn | null>(
    (current, turn) => (!current || turn.durationSec > current.durationSec ? turn : current),
    null,
  );
  return (
    <PanelSection title="TIMELINE" aside={`${formatDuration(metrics.idleMs / 1000)} idle`}>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 7 }}>
        {turns.map((turn) => {
          const left = Math.max(0, ((turn.start - metrics.startMs) / span) * 100);
          const width = Math.max(1.5, ((turn.end - turn.start) / span) * 100);
          const isSlow = slowest && slowest.startGenId === turn.startGenId;
          return (
            <button
              type="button"
              key={turn.startGenId}
              onClick={() => onJump(turn.startGenId)}
              title={`Jump to turn ${turn.index}`}
              style={{
                display: 'grid',
                gridTemplateColumns: '40px 1fr 52px',
                alignItems: 'center',
                gap: 8,
                width: '100%',
                padding: 0,
                border: 'none',
                background: 'transparent',
                cursor: 'pointer',
                textAlign: 'left',
              }}
            >
              <span
                style={{
                  fontFamily: 'var(--fontFamilyMonospace)',
                  fontSize: 10.5,
                  color: 'var(--fg3)',
                }}
              >
                T{turn.index}
              </span>
              <span
                style={{
                  position: 'relative',
                  height: 8,
                  background: 'var(--timeline-track)',
                  overflow: 'hidden',
                }}
              >
                <span
                  style={{
                    position: 'absolute',
                    left: `${left}%`,
                    width: `${Math.min(100 - left, width)}%`,
                    top: 0,
                    bottom: 0,
                    minWidth: 2,
                    background: isSlow ? 'var(--warning-main)' : 'var(--viz-blue)',
                  }}
                />
                {turn.failedCount > 0 && (
                  <span
                    style={{
                      position: 'absolute',
                      left: `${Math.min(98, left + Math.max(0, width - Math.min(width, 18)))}%`,
                      width: `${Math.min(width, 18)}%`,
                      top: 0,
                      bottom: 0,
                      minWidth: 2,
                      background: 'var(--error-main)',
                    }}
                  />
                )}
              </span>
              <span
                style={{
                  fontFamily: 'var(--fontFamilyMonospace)',
                  fontSize: 10.5,
                  color: 'var(--fg2)',
                  textAlign: 'right',
                }}
              >
                {formatDuration(turn.durationSec)}
              </span>
            </button>
          );
        })}
      </div>
      <div
        style={{
          margin: '10px 60px 0 48px',
          height: 13,
          borderTop: '1px solid var(--border-weak)',
          display: 'flex',
          justifyContent: 'space-between',
          paddingTop: 3,
        }}
      >
        <span
          style={{
            fontFamily: 'var(--fontFamilyMonospace)',
            fontSize: 9.5,
            color: 'var(--fg3)',
          }}
        >
          {metrics.startMs ? formatTime(new Date(metrics.startMs).toISOString()) : NO_VALUE}
        </span>
        <span
          style={{
            fontFamily: 'var(--fontFamilyMonospace)',
            fontSize: 9.5,
            color: 'var(--fg3)',
          }}
        >
          {metrics.endMs ? formatTime(new Date(metrics.endMs).toISOString()) : NO_VALUE}
        </span>
      </div>
      <div
        style={{
          display: 'flex',
          gap: 11,
          marginTop: 7,
          color: 'var(--fg3)',
          fontFamily: 'var(--fontFamilyMonospace)',
          fontSize: 10,
        }}
      >
        <span
          style={{
            display: 'inline-flex',
            alignItems: 'center',
            gap: 4,
          }}
        >
          <span
            style={{
              width: 6,
              height: 6,
              background: 'var(--viz-blue)',
            }}
          />
          turn
        </span>
        <span
          style={{
            display: 'inline-flex',
            alignItems: 'center',
            gap: 4,
          }}
        >
          <span
            style={{
              width: 6,
              height: 6,
              background: 'var(--warning-main)',
            }}
          />
          slowest
        </span>
        <span
          style={{
            display: 'inline-flex',
            alignItems: 'center',
            gap: 4,
          }}
        >
          <span
            style={{
              width: 6,
              height: 6,
              background: 'var(--error-main)',
            }}
          />
          failed
        </span>
      </div>
    </PanelSection>
  );
}

/** One row of the WORTH A LOOK panel: a turn worth jumping to, and why. */
interface WorthALookEntry {
  label: string;
  turn: TranscriptTurn;
  tone: 'error' | 'warning' | 'neutral';
}

interface WorthALookProps {
  steps: Generation[];
  turns: TranscriptTurn[];
  metrics: TranscriptMetrics;
  onJump: (id: string) => void;
}

function WorthALook({ steps, turns, metrics, onJump }: WorthALookProps) {
  if (turns.length === 0) return null;
  const slowest = turns.reduce<TranscriptTurn | null>(
    (current, turn) => (!current || turn.durationSec > current.durationSec ? turn : current),
    null,
  );
  const failed = turns.filter((turn) => turn.failedCount > 0);
  const turnByGen = new Map<string, TranscriptTurn>();
  turns.forEach((turn) => {
    turn.genIds.forEach((id) => {
      turnByGen.set(id, turn);
    });
  });
  const pickStep = (value: (gen: Generation) => number) =>
    (steps || []).reduce<Generation | null>(
      (current, gen) => (!current || value(gen) > value(current) ? gen : current),
      null,
    );
  const generated = metrics.usageAvailable ? pickStep((gen) => stepTokenWork(gen).generated) : null;
  const read = metrics.usageAvailable ? pickStep((gen) => stepTokenWork(gen).ingested) : null;
  const entries: WorthALookEntry[] = [];
  if (failed.length > 0)
    entries.push({
      label: failed.reduce((sum, turn) => sum + turn.failedCount, 0) === 1 ? 'failure' : 'failures',
      turn: failed[0] as TranscriptTurn,
      tone: 'error',
    });
  if (slowest) entries.push({ label: 'slowest turn', turn: slowest, tone: 'warning' });
  if (metrics.longestIdle && metrics.longestIdle.durationMs > 0)
    entries.push({
      label: `longest idle ${formatDuration(metrics.longestIdle.durationMs / 1000)}`,
      turn: metrics.longestIdle.turn,
      tone: 'neutral',
    });
  if (generated && turnByGen.has(generated.generation_id))
    entries.push({
      label: `most generated ${formatTokens(stepTokenWork(generated).generated)}`,
      turn: turnByGen.get(generated.generation_id) as TranscriptTurn,
      tone: 'neutral',
    });
  if (read && turnByGen.has(read.generation_id))
    entries.push({
      label: `most read ${formatTokens(stepTokenWork(read).ingested)}`,
      turn: turnByGen.get(read.generation_id) as TranscriptTurn,
      tone: 'neutral',
    });
  return (
    <PanelSection title="WORTH A LOOK">
      <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
        {entries.map((entry, index) => {
          const border =
            entry.tone === 'error'
              ? 'var(--error-border)'
              : entry.tone === 'warning'
                ? 'var(--warning-border)'
                : 'var(--border-medium)';
          const dot =
            entry.tone === 'error'
              ? 'var(--error-main)'
              : entry.tone === 'warning'
                ? 'var(--warning-main)'
                : 'var(--fg3)';
          const hover =
            entry.tone === 'error'
              ? 'var(--error-hover)'
              : entry.tone === 'warning'
                ? 'var(--warning-hover)'
                : 'var(--action-hover)';
          return (
            <button
              type="button"
              key={`${entry.label}-${index}`}
              onClick={() => onJump(entry.turn.startGenId)}
              style={{
                width: '100%',
                display: 'flex',
                alignItems: 'center',
                gap: 8,
                padding: '7px 10px',
                background: 'transparent',
                border: `1px solid ${border}`,
                borderRadius: 2,
                cursor: 'pointer',
                textAlign: 'left',
              }}
              onMouseEnter={(event) => (event.currentTarget.style.background = hover)}
              onMouseLeave={(event) => (event.currentTarget.style.background = 'transparent')}
            >
              <span
                style={{
                  width: 6,
                  height: 6,
                  borderRadius: '50%',
                  background: dot,
                  flexShrink: 0,
                }}
              />
              <span
                style={{
                  color: 'var(--fg1)',
                  fontSize: 12,
                  overflow: 'hidden',
                  textOverflow: 'ellipsis',
                  whiteSpace: 'nowrap',
                }}
              >
                {entry.label}
              </span>
              <span
                style={{
                  marginLeft: 'auto',
                  color: 'var(--fg3)',
                  fontFamily: 'var(--fontFamilyMonospace)',
                  fontSize: 11,
                }}
              >
                T{entry.turn.index}
              </span>
            </button>
          );
        })}
      </div>
    </PanelSection>
  );
}

export function missingUsageNotice(host: string): string {
  return host
    ? `No token usage was recorded for this ${host} session, so token counts and cost are unavailable.`
    : 'No token usage was recorded for this session, so token counts and cost are unavailable.';
}

/**
 * The row the detail screen was opened from: a list row from the daemon, or
 * the summary the app derives from the detail response, which leaves the
 * timestamps null when no generation carried one.
 */
interface DetailConversation extends Pick<ConversationSummary, 'id' | 'agents' | 'models'> {
  title?: string;
  started_at?: string | null;
  last_activity?: string | null;
}

interface MetricsPanelProps {
  conv: DetailConversation;
  steps: Generation[];
  turns: TranscriptTurn[];
  metrics: TranscriptMetrics;
  onJump: (id: string) => void;
}

function MetricsPanel({ conv, steps, turns, metrics, onJump }: MetricsPanelProps) {
  const maxToolCount = metrics.toolHistogram.reduce((max, item) => Math.max(max, item.count), 0) || 1;
  const host = agentHosts(conv.agents)[0] || '';
  const stats = [
    { value: formatDuration(metrics.wallMs / 1000), label: 'elapsed' },
    {
      value: formatDuration(metrics.workingMs / 1000),
      label: 'agent working',
    },
    {
      value: String(steps.length),
      label: steps.length === 1 ? 'call' : 'calls',
    },
    {
      value: metrics.usageAvailable ? formatTokens(metrics.totalTokens) : '—',
      label: 'tokens',
      muted: !metrics.usageAvailable,
    },
  ];
  return (
    <aside
      style={{
        width: 320,
        flex: 'none',
        position: 'sticky',
        top: HEADER_H + 46,
        alignSelf: 'flex-start',
        maxHeight: `calc(100vh - ${HEADER_H + 46}px)`,
        overflow: 'auto',
        borderLeft: '1px solid var(--border-weak)',
        background: 'var(--bg-primary)',
        padding: '20px 18px 40px',
        display: 'flex',
        flexDirection: 'column',
        gap: 22,
      }}
    >
      <PanelSection title="SESSION">
        <div
          style={{
            display: 'grid',
            gridTemplateColumns: '1fr 1fr',
            gap: 12,
          }}
        >
          {stats.map((stat) => (
            <div key={stat.label}>
              <div
                style={{
                  fontFamily: 'var(--fontFamilyMonospace)',
                  fontSize: 18,
                  color: stat.muted ? 'var(--fg3)' : 'var(--fg-max)',
                }}
              >
                {stat.value}
              </div>
              <div
                style={{
                  marginTop: 2,
                  color: 'var(--fg3)',
                  fontSize: 11,
                }}
              >
                {stat.label}
              </div>
            </div>
          ))}
        </div>
      </PanelSection>
      {!metrics.usageAvailable && (
        <div
          style={{
            display: 'flex',
            alignItems: 'flex-start',
            gap: 9,
            padding: '10px 12px',
            border: '1px solid var(--border-weak)',
            borderRadius: 8,
            background: 'var(--notice-info-bg)',
            color: 'var(--fg2)',
            fontSize: 12,
            lineHeight: 1.5,
          }}
        >
          <Icon name="info" size={14} style={{ color: 'var(--fg3)', marginTop: 2 }} />
          <span>{missingUsageNotice(host)}</span>
        </div>
      )}
      <TimelinePanel turns={turns} metrics={metrics} onJump={onJump} />
      <WorthALook steps={steps} turns={turns} metrics={metrics} onJump={onJump} />
      <PanelSection title="TOOLS USED">
        {metrics.toolHistogram.length === 0 ? (
          <div style={{ color: 'var(--fg3)', fontSize: 11.5 }}>No tools recorded.</div>
        ) : (
          <div
            style={{
              display: 'flex',
              flexDirection: 'column',
              gap: 8,
            }}
          >
            {metrics.toolHistogram.map((tool) => (
              <div
                key={tool.name}
                style={{
                  display: 'flex',
                  alignItems: 'center',
                  gap: 8,
                }}
              >
                <span
                  title={tool.name}
                  style={{
                    width: 88,
                    overflow: 'hidden',
                    textOverflow: 'ellipsis',
                    whiteSpace: 'nowrap',
                    color: 'var(--fg1)',
                    fontFamily: 'var(--fontFamilyMonospace)',
                    fontSize: 11.5,
                  }}
                >
                  {tool.name}
                </span>
                <span
                  style={{
                    position: 'relative',
                    flex: 1,
                    height: 6,
                    background: 'var(--histogram-track)',
                  }}
                >
                  <span
                    style={{
                      position: 'absolute',
                      inset: 0,
                      right: 'auto',
                      width: `${(tool.count / maxToolCount) * 100}%`,
                      background: 'var(--histogram-fill)',
                    }}
                  />
                </span>
                <span
                  style={{
                    width: 18,
                    textAlign: 'right',
                    color: 'var(--fg3)',
                    fontFamily: 'var(--fontFamilyMonospace)',
                    fontSize: 11.5,
                  }}
                >
                  {tool.count}
                </span>
              </div>
            ))}
          </div>
        )}
      </PanelSection>
    </aside>
  );
}

interface TraceDetailViewProps {
  conv: DetailConversation;
  detail: ConversationDetail | null;
  loading: boolean;
  error: string | null;
  backHref: string;
  backLabel: string;
  onBack: () => void;
}

export function TraceDetailView({ conv, detail, loading, error, backHref, backLabel, onBack }: TraceDetailViewProps) {
  const steps = useMemo(() => (detail ? detail.generations : []), [detail]);
  const turns = useMemo(() => buildTranscript(steps), [steps]);
  const metrics = useMemo(() => buildTranscriptMetrics(steps, turns), [steps, turns]);
  const jumpRef = useRef<(id: string) => void>(() => {});
  const wallSec =
    metrics.wallMs > 0 ? metrics.wallMs / 1000 : durationBetweenSeconds(conv.started_at, conv.last_activity);
  const buttonStyle: CSSProperties = {
    display: 'inline-flex',
    alignItems: 'center',
    gap: 6,
    padding: '0 11px',
    height: 28,
    background: 'transparent',
    color: 'var(--fg1)',
    border: '1px solid var(--border-medium)',
    borderRadius: 2,
    fontSize: 12,
    cursor: 'pointer',
    fontFamily: 'var(--fontFamily)',
    fontWeight: 500,
    whiteSpace: 'nowrap',
  };
  const onExport = () => {
    const blob = new Blob([JSON.stringify({ ...conv, generations: steps }, null, 2)], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement('a');
    anchor.href = url;
    anchor.download = `${conv.id}.json`;
    document.body.appendChild(anchor);
    anchor.click();
    anchor.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  };
  return (
    <div
      style={{
        display: 'flex',
        flexDirection: 'column',
        flex: 1,
        minHeight: 0,
        background: 'var(--bg-canvas)',
      }}
    >
      <div
        style={{
          display: 'flex',
          alignItems: 'center',
          gap: 10,
          height: 46,
          padding: '0 16px',
          borderBottom: '1px solid var(--border-weak)',
          background: 'var(--bg-primary)',
          position: 'sticky',
          top: HEADER_H,
          zIndex: 4,
          minWidth: 0,
        }}
      >
        <a
          href={backHref}
          aria-label={`Back to ${backLabel}`}
          onClick={(event) => {
            if (!isPlainLeftClick(event)) return;
            event.preventDefault();
            onBack();
          }}
          style={{
            fontSize: 13,
            color: 'var(--fg2)',
            textDecoration: 'none',
            whiteSpace: 'nowrap',
            flexShrink: 0,
            cursor: 'pointer',
          }}
          onMouseEnter={(event) => (event.currentTarget.style.color = 'var(--fg-max)')}
          onMouseLeave={(event) => (event.currentTarget.style.color = 'var(--fg2)')}
        >
          {backLabel}
        </a>
        <Icon name="cright" size={11} style={{ color: 'var(--fg3)', flexShrink: 0 }} />
        <span
          style={{
            fontFamily: 'var(--fontFamilyMonospace)',
            fontSize: 13,
            color: 'var(--fg-max)',
            whiteSpace: 'nowrap',
            overflow: 'hidden',
            textOverflow: 'ellipsis',
            minWidth: 0,
          }}
        >
          {conv.title || conv.id}
        </span>
        {(conv.models || []).map((model) => (
          <ModelPill key={model} name={model} />
        ))}
        <span
          style={{
            fontSize: 12,
            color: 'var(--fg3)',
            whiteSpace: 'nowrap',
          }}
        >
          {turns.length} {turns.length === 1 ? 'turn' : 'turns'} · {formatDuration(wallSec)}
        </span>
        <span style={{ flex: 1 }} />
        <button
          type="button"
          title="Download trace as JSON"
          onClick={onExport}
          style={buttonStyle}
          onMouseEnter={(event) => (event.currentTarget.style.background = 'var(--action-hover)')}
          onMouseLeave={(event) => (event.currentTarget.style.background = 'transparent')}
        >
          <Icon name="download" size={12} />
          Export JSON
        </button>
      </div>
      <div style={{ display: 'flex', alignItems: 'flex-start', gap: 0 }}>
        <main style={{ flex: 1, minWidth: 0, padding: '28px 32px 96px' }}>
          {error && (
            <Notice kind="error" title="Failed to load session">
              {error}
            </Notice>
          )}
          {!error && loading && (
            <div
              style={{
                color: 'var(--fg3)',
                fontFamily: 'var(--fontFamilyMonospace)',
                fontSize: 12,
              }}
            >
              Loading…
            </div>
          )}
          {!error && !loading && detail && <ConversationThread turns={turns} jumpRef={jumpRef} />}
        </main>
        {!error && !loading && detail && (
          <MetricsPanel
            conv={conv}
            steps={steps}
            turns={turns}
            metrics={metrics}
            onJump={(id) => jumpRef.current(id)}
          />
        )}
      </div>
    </div>
  );
}
