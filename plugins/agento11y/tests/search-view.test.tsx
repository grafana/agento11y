import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { PageHero } from '../internal/local/web/src/notices';
import { ConversationSearchPanel, searchHeroStats, searchResultCountLabel } from '../internal/local/web/src/search';
import type { SearchHit } from '../internal/local/web/src/types';

afterEach(cleanup);

function hit(index: number): SearchHit {
  return {
    id: `conv-${index}`,
    title: `Matching conversation ${index}`,
    agents: ['pi'],
    models: ['claude-opus-4-7'],
    last_activity: '2026-08-26T12:00:00Z',
    total_tokens: 100,
    calls: 1,
    status: 'ok',
    snippet: 'needle in the conversation',
    match_count: 1,
    token_buckets: { fresh_input: 50, cache_read: 0, cache_write: 0, output: 50, reasoning: 0 },
  };
}

function renderSearch(phase: 'loading' | 'done', hits: SearchHit[]) {
  render(
    <>
      <PageHero stats={searchHeroStats(phase, hits, 'fts')} />
      <ConversationSearchPanel
        query="needle"
        hits={hits}
        phase={phase}
        mode="fts"
        error={null}
        selectedIndex={-1}
        setSelectedIndex={vi.fn()}
        retry={vi.fn()}
        now={Date.parse('2026-08-26T13:00:00Z')}
        onOpen={vi.fn()}
      />
    </>,
  );
}

describe('conversation search states', () => {
  it('names the full scan and shows no result number while searching', () => {
    renderSearch('loading', [hit(0)]);

    expect(screen.getByText('Full scan')).toBeTruthy();
    expect(screen.queryByText('FTS')).toBeNull();
    expect(screen.getByText('Results').parentElement?.textContent).toBe('Results');
    expect(screen.queryByText('1 result')).toBeNull();
    expect(screen.getByText('Searching')).toBeTruthy();
  });

  it('shows zero only after a search completes with no matches', () => {
    renderSearch('done', []);

    expect(screen.getByText('Results').parentElement?.textContent).toBe('Results0');
    expect(screen.getByText('“needle”')).toBeTruthy();
  });

  it('shows a completed result count in the panel', () => {
    renderSearch('done', [hit(0)]);

    expect(screen.getByText('1 result')).toBeTruthy();
  });

  it('discloses when the returned hits reach the endpoint cap', () => {
    expect(searchResultCountLabel('done', 100)).toBe('100 results (capped)');
  });
});
