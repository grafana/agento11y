import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ConvRow, TokenChart, type TokenChartBucket } from '../internal/local/web/src/conversations';
import type { ConversationSummary, ModelPrices, TokenBucketKey } from '../internal/local/web/src/types';

afterEach(cleanup);

const NOW = Date.parse('2026-01-01T12:00:00Z');

function conversation(overrides: Partial<ConversationSummary> = {}): ConversationSummary {
  return {
    id: 'conv-1',
    title: 'Fix the flaky test',
    started_at: '2026-01-01T11:00:00Z',
    last_activity: '2026-01-01T11:30:00Z',
    calls: 3,
    input_tokens: 1_000_000,
    output_tokens: 0,
    total_tokens: 1_000_000,
    token_buckets: { fresh_input: 1_000_000, cache_read: 0, cache_write: 0, output: 0, reasoning: 0 },
    agents: ['claude'],
    models: ['claude-sonnet-4-5'],
    status: 'ok',
    workspace: '/Users/x/projects/agento11y',
    ...overrides,
  };
}

const prices: ModelPrices = { 'claude-sonnet-4-5': { input: 3, output: 15 } };

describe('ConvRow', () => {
  it('links to the conversation and opens it in place on a plain left click', () => {
    const onOpen = vi.fn();
    render(<ConvRow c={conversation()} now={NOW} onOpen={onOpen} prices={prices} />);

    const link = screen.getByRole('link');
    expect(link.getAttribute('href')).toBe('/conversations/conv-1');

    // The row is a real link so middle-click and open-in-new-tab work; only a
    // plain left click is intercepted for client-side routing.
    fireEvent.click(link, { button: 0 });
    expect(onOpen).toHaveBeenCalledTimes(1);
    expect(onOpen.mock.calls[0]?.[0]?.id).toBe('conv-1');

    onOpen.mockClear();
    fireEvent.click(link, { button: 0, metaKey: true });
    expect(onOpen).not.toHaveBeenCalled();
  });

  it('prices the row from the live catalog', () => {
    render(<ConvRow c={conversation()} now={NOW} onOpen={vi.fn()} prices={prices} />);
    // One million fresh input tokens at $3 per million.
    expect(screen.getByTitle(/estimated/i).textContent).toBe('$3');
  });

  it('shows no cost when the model is not in the catalog', () => {
    render(<ConvRow c={conversation({ models: ['unknown-model'] })} now={NOW} onOpen={vi.fn()} prices={prices} />);
    expect(screen.getByTitle(/estimated/i).textContent).not.toContain('$');
  });

  it('counts subagent steps only when there are some', () => {
    const { unmount } = render(<ConvRow c={conversation({ subagents: 1 })} now={NOW} onOpen={vi.fn()} prices={null} />);
    expect(screen.getByTitle('1 subagent step')).toBeTruthy();
    unmount();

    render(<ConvRow c={conversation({ subagents: 0 })} now={NOW} onOpen={vi.fn()} prices={null} />);
    expect(screen.queryByTitle(/subagent/)).toBeNull();
  });

  it('falls back to the id when the conversation has no title', () => {
    render(<ConvRow c={conversation({ title: '' })} now={NOW} onOpen={vi.fn()} prices={null} />);
    expect(screen.getAllByText('conv-1').length).toBeGreaterThan(0);
  });
});

function bucket(overrides: Partial<TokenChartBucket> = {}): TokenChartBucket {
  return {
    t: '11:00',
    start: NOW - 3_600_000,
    end: NOW,
    fresh_input: 100,
    cache_read: 50,
    cache_write: 0,
    output: 25,
    reasoning: 0,
    total: 175,
    ...overrides,
  };
}

function renderChart(props: Partial<React.ComponentProps<typeof TokenChart>> = {}) {
  const onToggleSeries = vi.fn();
  const onModelChange = vi.fn();
  render(
    <TokenChart
      data={[bucket()]}
      bucketLabel="1h"
      grandTotal={175}
      models={['claude-sonnet-4-5']}
      model=""
      onModelChange={onModelChange}
      hidden={new Set<TokenBucketKey>()}
      onToggleSeries={onToggleSeries}
      switcher={<span>range</span>}
      {...props}
    />,
  );
  return { onToggleSeries, onModelChange };
}

describe('TokenChart', () => {
  // A pure-Anthropic store records no reasoning tokens, and an always-zero
  // swatch reads as a series that exists and is empty.
  it('shows a legend entry only for a series that appears in the data', () => {
    renderChart();
    expect(screen.getByRole('button', { name: /Input/ })).toBeTruthy();
    expect(screen.queryByRole('button', { name: /Reasoning/ })).toBeNull();
  });

  it('shows the whole legend when there is no data at all', () => {
    renderChart({ data: [], grandTotal: 0 });
    expect(screen.getByRole('button', { name: /Reasoning/ })).toBeTruthy();
  });

  it('reports a legend click as a series toggle', () => {
    const { onToggleSeries } = renderChart();
    fireEvent.click(screen.getByRole('button', { name: /Input/ }));
    expect(onToggleSeries).toHaveBeenCalledWith('fresh_input');
  });

  it('offers to show a series that is hidden', () => {
    renderChart({ hidden: new Set<TokenBucketKey>(['fresh_input']) });
    expect(screen.getByTitle('Show Input')).toBeTruthy();
    expect(screen.getByTitle('Hide Cache read')).toBeTruthy();
  });
});
