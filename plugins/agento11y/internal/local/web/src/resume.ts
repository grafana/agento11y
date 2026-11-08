import type { ResumeInfo, ResumeUnavailableReason } from './types';

// C0 and C1 controls, DEL, line and paragraph separators, and lone surrogates.
// A pasted line break would run the command early, and a lone surrogate
// cannot reach the terminal unchanged.
const REJECTED = /[\p{Cc}\p{Cs}\u2028\u2029]/u;

const PROGRAMS: Record<string, string> = {
  'claude-code': 'claude --resume',
  pi: 'pi --session',
};

/**
 * Quotes one argument for Bash, Zsh, and fish. Ordinary characters go in
 * single-quoted segments. Apostrophes and backslashes go outside them,
 * escaped, because fish treats a backslash inside single quotes as an escape.
 * Returns null for characters no quoting can paste safely.
 */
export function shellQuote(value: string): string | null {
  if (REJECTED.test(value)) return null;
  let out = '';
  let segment = '';
  const flush = () => {
    if (segment) out += `'${segment}'`;
    segment = '';
  };
  for (const ch of value) {
    if (ch === "'" || ch === '\\') {
      flush();
      out += `\\${ch}`;
    } else {
      segment += ch;
    }
  }
  flush();
  return out || "''";
}

/** Stays on one line, and && keeps a failed cd from starting the agent. */
export function resumeCommand(info: ResumeInfo | undefined): string | null {
  if (!info || info.status !== 'ready') return null;
  const program = Object.hasOwn(PROGRAMS, info.agent) ? PROGRAMS[info.agent] : undefined;
  if (!program || !info.cwd.startsWith('/')) return null;
  const cwd = shellQuote(info.cwd);
  const id = shellQuote(info.session_id);
  if (cwd == null || id == null) return null;
  return `cd ${cwd} && ${program} ${id}`;
}

const UNAVAILABLE: Record<ResumeUnavailableReason, string> = {
  unsupported_id: 'this session ID is not a full native session ID.',
  missing_workspace: 'the main agent recorded no working directory for this session.',
  invalid_workspace:
    'the recorded working directory does not start with /, or contains characters that cannot be pasted into a terminal safely.',
  conflicting_workspace:
    'the main agent recorded more than one working directory, and none of them contains all the others.',
  ambiguous_agent: 'records from more than one agent share this session ID.',
  missing_root: 'only subagent records were found for this session.',
};

export function resumeUnavailableMessage(reason: ResumeUnavailableReason | undefined): string {
  const detail =
    reason && Object.hasOwn(UNAVAILABLE, reason) ? UNAVAILABLE[reason] : 'the recorded session metadata is incomplete.';
  return `Resume command unavailable: ${detail}`;
}
