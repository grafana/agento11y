import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { TraceDetailView } from '../internal/local/web/src/detail';
import { ResumeAction } from '../internal/local/web/src/resume-action';
import { summaryFromDetail } from '../internal/local/web/src/routing';
import { HEADER_H } from '../internal/local/web/src/shell';
import type { ConversationDetail } from '../internal/local/web/src/types';

const ID = '01a1b6bf-e6c8-7627-a64e-26fbc210f69f';
const COMMAND = `cd '/work/repo' && pi --session '${ID}'`;
const COPIED = 'Command copied. Paste it into your terminal.';
const MANUAL = 'Could not copy. Select and copy the command below.';

interface Deferred {
  promise: Promise<void>;
  resolve: () => void;
  reject: (err: Error) => void;
}

function deferred(): Deferred {
  let resolve!: () => void;
  let reject!: (err: Error) => void;
  const promise = new Promise<void>((done, fail) => {
    resolve = done;
    reject = fail;
  });
  return { promise, resolve, reject };
}

function stubClipboard(clipboard: unknown) {
  Object.defineProperty(navigator, 'clipboard', { value: clipboard, configurable: true });
}

function clipboardWith(writeText: (text: string) => Promise<void>) {
  const spy = vi.fn(writeText);
  stubClipboard({ writeText: spy });
  return spy;
}

function renderAction(props: Partial<Parameters<typeof ResumeAction>[0]> = {}) {
  return render(<ResumeAction conversationId={ID} agent="pi" command={COMMAND} buttonStyle={{}} {...props} />);
}

function copyButton() {
  return screen.getByRole('button', { name: 'Copy resume command' }) as HTMLButtonElement;
}

function commandField() {
  return screen.getByRole('textbox', { name: 'Resume command' }) as HTMLTextAreaElement;
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  stubClipboard(undefined);
});

describe('ResumeAction clipboard', () => {
  it('copies the exact command and shows it for pasting', async () => {
    const writeText = clipboardWith(() => Promise.resolve());
    renderAction();

    await act(async () => {
      fireEvent.click(copyButton());
    });

    expect(writeText).toHaveBeenCalledTimes(1);
    expect(writeText).toHaveBeenCalledWith(COMMAND);
    expect(writeText.mock.calls[0]?.[0]?.endsWith('\n')).toBe(false);
    expect(screen.getByRole('status').textContent).toBe(COPIED);
    expect(commandField().value).toBe(COMMAND);
    expect(commandField().readOnly).toBe(true);
    expect(screen.getByText('Run on the original machine with your usual agent profile and sandbox.')).toBeTruthy();
    expect(screen.getByText('If Pi offers to fork, cancel to avoid creating a new session.')).toBeTruthy();
    expect(screen.queryByText(/resumed/i)).toBeNull();
  });

  it('omits the Pi fork note for Claude Code', async () => {
    clipboardWith(() => Promise.resolve());
    renderAction({ agent: 'claude-code', command: `cd '/w' && claude --resume '${ID}'` });
    await act(async () => {
      fireEvent.click(copyButton());
    });
    expect(screen.getByRole('status').textContent).toBe(COPIED);
    expect(screen.queryByText(/Pi offers to fork/)).toBeNull();
  });

  it.each([
    ['the clipboard API is missing', () => stubClipboard(undefined)],
    ['writeText is missing', () => stubClipboard({})],
    [
      'writeText throws synchronously',
      () =>
        clipboardWith(() => {
          throw new Error('denied');
        }),
    ],
    ['writeText rejects', () => clipboardWith(() => Promise.reject(new Error('denied')))],
  ])('falls back to manual copying when %s', async (_name, install) => {
    install();
    renderAction();

    await act(async () => {
      fireEvent.click(copyButton());
    });

    expect(screen.getByRole('status').textContent).toBe(MANUAL);
    expect(commandField().value).toBe(COMMAND);
    expect(document.activeElement).toBe(commandField());
  });

  it('ignores repeat clicks while the clipboard request is pending', async () => {
    const write = deferred();
    const writeText = clipboardWith(() => write.promise);
    renderAction();

    fireEvent.click(copyButton());
    fireEvent.click(copyButton());
    expect(writeText).toHaveBeenCalledTimes(1);
    expect(copyButton().getAttribute('aria-busy')).toBe('true');
    expect(screen.getByRole('status').textContent).toBe('');

    await act(async () => {
      write.resolve();
    });
    expect(screen.getByRole('status').textContent).toBe(COPIED);
    expect(copyButton().getAttribute('aria-busy')).toBeNull();
  });

  it('closes the panel with Escape or Dismiss and returns focus to the trigger', async () => {
    clipboardWith(() => Promise.reject(new Error('denied')));
    renderAction();
    const outer = vi.fn();
    window.addEventListener('keydown', outer);

    await act(async () => {
      fireEvent.click(copyButton());
    });
    expect(document.activeElement).toBe(commandField());
    fireEvent.keyDown(commandField(), { key: 'Escape' });
    expect(screen.queryByRole('region', { name: 'Resume command' })).toBeNull();
    expect(document.activeElement).toBe(copyButton());
    expect(outer).not.toHaveBeenCalled();

    await act(async () => {
      fireEvent.click(copyButton());
    });
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }));
    expect(screen.queryByRole('region', { name: 'Resume command' })).toBeNull();
    expect(document.activeElement).toBe(copyButton());

    // Without a panel, Escape belongs to the rest of the page.
    fireEvent.keyDown(copyButton(), { key: 'Escape' });
    expect(outer).toHaveBeenCalledTimes(1);
    window.removeEventListener('keydown', outer);
  });

  it('disables copying when there is no command', () => {
    const writeText = clipboardWith(() => Promise.resolve());
    renderAction({ command: null, describedBy: 'why' });
    expect(copyButton().disabled).toBe(true);
    expect(copyButton().getAttribute('aria-describedby')).toBe('why');
    fireEvent.click(copyButton());
    expect(writeText).not.toHaveBeenCalled();
  });
});

function sessionDetail(resume?: ConversationDetail['resume']): ConversationDetail {
  return {
    id: ID,
    title: 'Fix the parser',
    generations: [
      {
        generation_id: 'g1',
        agent_name: 'pi',
        model: 'claude-opus-4-7',
        started_at: '2026-05-21T10:00:00Z',
        completed_at: '2026-05-21T10:00:01Z',
        duration_seconds: 1,
        input_tokens: 10,
        output_tokens: 5,
        total_tokens: 15,
        token_buckets: { fresh_input: 10, cache_read: 0, cache_write: 0, output: 5, reasoning: 0 },
      },
    ],
    ...(resume ? { resume } : {}),
  };
}

function renderDetail(detail: ConversationDetail) {
  return render(
    <TraceDetailView
      conv={summaryFromDetail(detail, detail.id)}
      detail={detail}
      loading={false}
      error={null}
      backHref="/"
      backLabel="Sessions"
      onBack={() => {}}
    />,
  );
}

describe('TraceDetailView resume action', () => {
  it('sits immediately before Export JSON for a ready session', () => {
    renderDetail(sessionDetail({ status: 'ready', agent: 'pi', session_id: ID, cwd: '/work/repo' }));
    const copy = copyButton();
    const exportButton = screen.getByRole('button', { name: /Export JSON/ });
    expect(copy.disabled).toBe(false);
    expect(copy.closest('[role="presentation"]')?.nextElementSibling).toBe(exportButton);
  });

  it('explains a disabled action in visible text', () => {
    renderDetail(sessionDetail({ status: 'unavailable', reason: 'missing_workspace' }));
    const copy = copyButton();
    expect(copy.disabled).toBe(true);
    const describedBy = copy.getAttribute('aria-describedby');
    expect(describedBy).toBeTruthy();
    const note = document.getElementById(describedBy as string);
    expect(note?.textContent).toMatch(/^Resume command unavailable: /);
    expect(note?.closest('[title]')).toBeNull();
  });

  it('moves the sticky rows below the toolbar when its actions wrap', () => {
    let notify = () => {};
    vi.stubGlobal(
      'ResizeObserver',
      class {
        constructor(callback: () => void) {
          notify = callback;
        }
        observe() {}
        disconnect() {}
      },
    );
    renderDetail(sessionDetail({ status: 'ready', agent: 'pi', session_id: ID, cwd: '/work/repo' }));
    const panel = screen.getByRole('complementary');
    expect(panel.style.top).toBe(`${HEADER_H + 46}px`);

    vi.spyOn(HTMLElement.prototype, 'offsetHeight', 'get').mockReturnValue(82);
    act(() => notify());
    expect(panel.style.top).toBe(`${HEADER_H + 82}px`);
  });

  it('hides the action for sessions without resume information', () => {
    renderDetail(sessionDetail());
    expect(screen.queryByRole('button', { name: 'Copy resume command' })).toBeNull();
    expect(screen.queryByText(/Resume command unavailable/)).toBeNull();
    expect(screen.getByRole('button', { name: /Export JSON/ })).toBeTruthy();
  });

  it('exports the same JSON with or without resume information', async () => {
    const blobs: Blob[] = [];
    vi.spyOn(URL, 'createObjectURL').mockImplementation((blob) => {
      blobs.push(blob as Blob);
      return 'blob:test';
    });
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
    const plain = sessionDetail();

    for (const detail of [plain, sessionDetail({ status: 'ready', agent: 'pi', session_id: ID, cwd: '/work/repo' })]) {
      renderDetail(detail);
      fireEvent.click(screen.getByRole('button', { name: /Export JSON/ }));
      cleanup();
    }

    expect(blobs).toHaveLength(2);
    const [without, withResume] = await Promise.all(blobs.map((blob) => blob.text()));
    const conv = summaryFromDetail(plain, ID);
    expect(without).toBe(JSON.stringify({ ...conv, generations: plain.generations }, null, 2));
    expect(withResume).toBe(without);
  });
});
