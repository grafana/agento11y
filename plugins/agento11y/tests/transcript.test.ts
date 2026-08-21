import { describe, expect, it } from 'vitest';
import {
  BlockedElement,
  buildTranscript,
  buildTranscriptMetrics,
  MARKDOWN_OPTIONS,
  markdownURL,
  missingUsageNotice,
  resultBody,
  SafeAnchor,
  splitPreamble,
  type TranscriptBlock,
  type TranscriptWorkBlock,
} from '../internal/local/web/src/detail';
import type { Generation, Message, MessageRole, Part } from '../internal/local/web/src/types';

// The transcript is derived, not stored: the daemon records generations, and
// the viewer folds them into turns, pairs each tool call with the result that
// answered it, and reports what failed. These scenarios are the ones the Go
// suite used to cover by running the viewer through Babel and a node vm, which
// is the harness this change removes.

const NO_TOKENS = { fresh_input: 0, cache_read: 0, cache_write: 0, output: 0, reasoning: 0 };

function message(role: MessageRole, parts: Part[]): Message {
  return { role, parts };
}

function callPart(id: string, name: string, input: Record<string, unknown> = {}): Part {
  return { kind: 'tool_call', tool_call: { id, name, input_json: input } };
}

function resultPart(id: string, name: string, content: string, isError = false): Part {
  return { kind: 'tool_result', tool_result: { tool_call_id: id, name, content_json: content, is_error: isError } };
}

function generation(id: string, input: Message[], output: Message[], overrides: Partial<Generation> = {}): Generation {
  const second = id === 'g1' ? '0' : '2';
  return {
    generation_id: id,
    agent_name: 'cursor',
    started_at: `2026-01-01T00:00:0${second}Z`,
    completed_at: `2026-01-01T00:00:0${id === 'g1' ? '1' : '3'}Z`,
    duration_seconds: 1,
    input,
    output,
    input_tokens: 0,
    output_tokens: 0,
    total_tokens: 0,
    token_buckets: { ...NO_TOKENS },
    parent_generation_ids: [],
    ...overrides,
  };
}

function userTurn(text = 'question'): Message[] {
  return [message('user', [{ kind: 'text', text }])];
}

function workBlocks(blocks: TranscriptBlock[]): TranscriptWorkBlock[] {
  return blocks.filter((block): block is TranscriptWorkBlock => block.kind === 'work');
}

function firstWork(blocks: TranscriptBlock[]): TranscriptWorkBlock {
  const block = workBlocks(blocks)[0];
  if (!block) throw new Error('no work block in the turn');
  return block;
}

function turnBlocks(steps: Generation[]): TranscriptBlock[] {
  const turns = buildTranscript(steps);
  const turn = turns[0];
  if (!turn) throw new Error('buildTranscript produced no turn');
  return turn.blocks;
}

describe('pairing a tool call with its result', () => {
  it('pairs a call and a result recorded in the same generation', () => {
    const same = generation('g1', userTurn(), [
      message('assistant', [callPart('', 'Read'), resultPart('', 'Read', 'line 1\nline 2')]),
    ]);
    const row = firstWork(turnBlocks([same])).calls[0];
    expect(resultBody(row?.result)).toBe('line 1\nline 2');
    expect(resultBody(row?.result).split('\n')).toHaveLength(2);
    expect(row?.failed).toBe(false);
  });

  it('pairs a call with a result that arrives in the next generation', () => {
    const first = generation('g1', userTurn(), [message('assistant', [callPart('call-1', 'Read')])]);
    const second = generation(
      'g2',
      [message('tool', [resultPart('call-1', 'Read', 'done')])],
      [message('assistant', [{ kind: 'text', text: 'answer' }])],
    );
    const row = firstWork(turnBlocks([first, second])).calls[0];
    expect(resultBody(row?.result)).toBe('done');
  });

  it('leaves a call unanswered rather than inventing a result', () => {
    const final = generation('g1', userTurn(), [message('assistant', [callPart('call-final', 'Read')])]);
    const row = firstWork(turnBlocks([final])).calls[0];
    expect(row?.result).toBeNull();
    expect(row?.failed).toBe(false);
  });

  it('keeps two calls of the same name in the order their results arrived', () => {
    const sameName = generation('g1', userTurn(), [
      message('assistant', [
        callPart('', 'Read'),
        callPart('', 'Read'),
        resultPart('', 'Read', 'first'),
        resultPart('', 'Read', 'second'),
      ]),
    ]);
    expect(firstWork(turnBlocks([sameName])).calls.map((call) => resultBody(call.result))).toEqual(['first', 'second']);
  });

  it('does not reuse one result for a call repeated across generations', () => {
    const first = generation('g1', userTurn(), [message('assistant', [callPart('', 'weather')])]);
    const second = generation(
      'g2',
      [message('tool', [resultPart('', 'weather', 'first result')])],
      [message('assistant', [callPart('', 'weather')])],
    );
    const third = generation(
      'g3',
      [message('tool', [resultPart('', 'weather', 'second result')])],
      [message('assistant', [{ kind: 'text', text: 'answer' }])],
      { started_at: '2026-01-01T00:00:04Z', completed_at: '2026-01-01T00:00:05Z' },
    );
    const blocks = turnBlocks([first, second, third]);
    expect(
      blocks.flatMap((block) => (block.kind === 'work' ? block.calls : [])).map((call) => resultBody(call.result)),
    ).toEqual(['first result', 'second result']);
  });

  it('keeps every call of a long generation', () => {
    const large = generation('g1', userTurn(), [
      message(
        'assistant',
        Array.from({ length: 41 }, (_, index) => callPart(`call-${index}`, 'Read')),
      ),
    ]);
    expect(firstWork(turnBlocks([large])).calls).toHaveLength(41);
  });
});

describe('block layout', () => {
  it('orders the blocks the way the parts were recorded', () => {
    const mixed = generation('g1', userTurn(), [
      message('assistant', [
        { kind: 'text', text: 'before' },
        { kind: 'thinking', thinking: 'reason' },
        callPart('a', 'Read'),
        callPart('b', 'Grep'),
        { kind: 'text', text: 'after' },
      ]),
    ]);
    expect(turnBlocks([mixed]).map((block) => block.kind)).toEqual(['prose', 'reasoning', 'work', 'prose']);
  });

  // A generation that interleaves prose and tool calls splits into several work
  // blocks. Only the first one carries the generation's duration, so a merge
  // with the next generation's work adds that generation's time and nothing
  // more.
  it('charges a generation duration to one work block only', () => {
    const split = generation(
      'g1',
      userTurn(),
      [
        message('assistant', [
          callPart('a', 'Read'),
          { kind: 'text', text: 'middle' },
          callPart('b', 'Grep'),
          { kind: 'text', text: '' },
          callPart('c', 'Glob'),
        ]),
      ],
      { duration_seconds: 4 },
    );
    expect(turnBlocks([split]).map((block) => block.kind)).toEqual(['work', 'prose', 'work']);
    expect(workBlocks(turnBlocks([split])).map((block) => block.durationSec)).toEqual([4, 0]);

    const next = generation(
      'g2',
      [message('tool', [resultPart('c', 'Glob', 'done')])],
      [message('assistant', [callPart('d', 'Read')])],
      { duration_seconds: 3 },
    );
    expect(workBlocks(turnBlocks([split, next])).map((block) => block.durationSec)).toEqual([4, 3]);
  });
});

describe('failure accounting', () => {
  it('counts a tool result marked as an error', () => {
    const failed = generation('g1', userTurn(), [
      message('assistant', [callPart('failed-call', 'Shell'), resultPart('failed-call', 'Shell', 'bad', true)]),
    ]);
    const turns = buildTranscript([failed]);
    expect(turns[0]?.failedCount).toBe(1);
    expect(firstWork(turnBlocks([failed])).calls[0]?.failed).toBe(true);
  });

  it('renders a call error as its own block', () => {
    const callErrorOnly = generation('g1', userTurn(), [], { call_error: 'provider unavailable' });
    const turns = buildTranscript([callErrorOnly]);
    expect(turns[0]?.failedCount).toBe(1);
    const blocks = turnBlocks([callErrorOnly]);
    expect(blocks.map((block) => block.kind)).toEqual(['error']);
    expect(blocks[0]?.kind === 'error' && blocks[0].text).toBe('provider unavailable');
  });

  it('keeps a successful call successful when the generation later failed', () => {
    const gen = generation(
      'g1',
      userTurn(),
      [message('assistant', [callPart('successful-call', 'Read'), resultPart('successful-call', 'Read', 'ok')])],
      { call_error: 'model call failed' },
    );
    const blocks = turnBlocks([gen]);
    const row = firstWork(blocks).calls[0];
    expect(row?.failed).toBe(false);
    expect(resultBody(row?.result)).toBe('ok');
    const errorBlock = blocks.find((block) => block.kind === 'error');
    expect(errorBlock?.kind === 'error' && errorBlock.text).toBe('model call failed');
    expect(buildTranscript([gen])[0]?.failedCount).toBe(1);
  });

  it('rolls a nested subagent failure up to the turn', () => {
    const parent = generation('g1', userTurn(), []);
    const child = generation('child', [], [], {
      agent_name: 'cursor/subagent',
      parent_generation_ids: ['g1'],
      started_at: '2026-01-01T00:00:02Z',
      completed_at: '2026-01-01T00:00:03Z',
    });
    const nested = generation('nested', [], [], {
      agent_name: 'cursor/nested',
      parent_generation_ids: ['child'],
      call_error: 'nested model call failed',
      started_at: '2026-01-01T00:00:04Z',
      completed_at: '2026-01-01T00:00:05Z',
    });
    const turns = buildTranscript([parent, child, nested]);
    expect(turns[0]?.failedCount).toBe(1);
    expect(firstWork(turnBlocks([parent, child, nested])).subruns[0]?.failedCount).toBe(1);
  });
});

describe('transcript metrics', () => {
  it('reports usage as unavailable when no generation recorded tokens', () => {
    const same = generation('g1', userTurn(), [
      message('assistant', [callPart('', 'Read'), resultPart('', 'Read', 'line 1\nline 2')]),
    ]);
    const turns = buildTranscript([same]);
    const metrics = buildTranscriptMetrics([same], turns);
    expect(metrics.usageAvailable).toBe(false);
    expect(metrics.totalTokens).toBe(0);
  });

  it('sums the tokens of every generation', () => {
    const first = generation('g1', userTurn(), [message('assistant', [callPart('call-1', 'Read')])], {
      total_tokens: 120,
    });
    const second = generation('g2', [message('tool', [resultPart('call-1', 'Read', 'done')])], [], {
      total_tokens: 30,
    });
    const metrics = buildTranscriptMetrics([first, second], buildTranscript([first, second]));
    expect(metrics.usageAvailable).toBe(true);
    expect(metrics.totalTokens).toBe(150);
  });

  it('names the host in the missing-usage notice, and omits it when unknown', () => {
    expect(missingUsageNotice('cursor')).toBe(
      'No token usage was recorded for this cursor session, so token counts and cost are unavailable.',
    );
    expect(missingUsageNotice('')).toBe(
      'No token usage was recorded for this session, so token counts and cost are unavailable.',
    );
  });
});

describe('splitting the harness preamble off a prompt', () => {
  it('splits complete blocks at the head and leaves markup inside the prompt alone', () => {
    expect(splitPreamble('<user_info>A</user_info>\n<rules>B</rules>\nCompare <old> and <new>.')).toEqual({
      preamble: '<user_info>A</user_info>\n<rules>B</rules>\n',
      prompt: 'Compare <old> and <new>.',
    });
  });

  it('keeps a message that is only preamble as the prompt', () => {
    const only = '<user_info>A</user_info>\n<rules>B</rules>';
    expect(splitPreamble(only)).toEqual({ preamble: '', prompt: only });
  });

  it('handles a block with attributes, which is how pi records a skill', () => {
    const skill = '<skill name="plan" location="/x/y/SKILL.md">body</skill>';
    expect(splitPreamble(`${skill}\nfix rebase issues`)).toEqual({
      preamble: `${skill}\n`,
      prompt: 'fix rebase issues',
    });
    expect(splitPreamble(skill)).toEqual({ preamble: '', prompt: skill });
  });
});

// Agent prose is model output rendered as markdown, so a link target and a raw
// HTML tag are both attacker-controlled as far as the viewer is concerned.
describe('markdown safety', () => {
  it('passes relative and allowed-scheme targets through', () => {
    for (const relative of ['/panel', './notes.md', '../up', '#anchor', '?q=1']) {
      expect(markdownURL(relative)).toBe(relative);
    }
    expect(markdownURL('https://grafana.com/')).toBe('https://grafana.com/');
    expect(markdownURL('mailto:a@b.c')).toBe('mailto:a@b.c');
  });

  it('renders no href at all for a scheme that can run script or carry a payload', () => {
    for (const blocked of [
      'javascript:alert(1)',
      'JaVaScRiPt:alert(1)',
      'java\tscript:alert(1)',
      'java\nscript:alert(1)',
      'data:text/html;base64,PHNjcmlwdD4=',
      'vbscript:msgbox(1)',
      'file:///etc/passwd',
      '//evil.example.com',
      '\\\\evil.example.com',
      '',
      '   ',
    ]) {
      expect(markdownURL(blocked), `must not render href ${JSON.stringify(blocked)}`).toBeUndefined();
    }
  });

  it('parses no raw HTML and drops every remote-loading tag', () => {
    expect(MARKDOWN_OPTIONS.disableParsingRawHTML).toBe(true);
    const overrides = MARKDOWN_OPTIONS.overrides as Record<string, { component: unknown }>;
    for (const tag of ['script', 'iframe', 'img', 'style', 'object', 'embed', 'form', 'svg']) {
      expect(overrides[tag]?.component, `${tag} must be blocked`).toBe(BlockedElement);
    }
    expect(MARKDOWN_OPTIONS.overrides.a.component).toBe(SafeAnchor);
  });
});
