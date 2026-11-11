import { afterAll, describe, expect, it } from 'vitest';
import { resumeCommand, resumeUnavailableMessage, shellQuote } from '../internal/local/web/src/resume';
import type { ResumeInfo } from '../internal/local/web/src/types';

// The viewer's tsconfig has no Node types (see raw.d.ts). The shell check loads
// Node modules through specifiers TypeScript does not resolve and types only
// the calls it makes.
interface SpawnResult {
  status: number | null;
  error?: Error;
  stdout: string;
  stderr: string;
}
interface NodeModules {
  spawnSync(
    command: string,
    args: string[],
    options: { cwd?: string; env?: Record<string, string>; encoding: 'utf8' },
  ): SpawnResult;
  chmodSync(path: string, mode: number): void;
  existsSync(path: string): boolean;
  mkdirSync(path: string, options?: { recursive: boolean }): void;
  mkdtempSync(prefix: string): string;
  readFileSync(path: string, encoding: 'utf8'): string;
  realpathSync(path: string): string;
  rmSync(path: string, options: { recursive?: boolean; force: boolean }): void;
  writeFileSync(path: string, data: string): void;
  tmpdir(): string;
  join(...parts: string[]): string;
}
const nodeModule = (name: string) => `node:${name}`;
const node: NodeModules = Object.assign(
  {},
  await import(nodeModule('child_process')),
  await import(nodeModule('fs')),
  await import(nodeModule('os')),
  await import(nodeModule('path')),
);
const { spawnSync, chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, realpathSync, rmSync, writeFileSync } =
  node;
const { tmpdir, join } = node;

const ID = '01a1b6bf-e6c8-7627-a64e-26fbc210f69f';

function ready(agent: 'claude-code' | 'pi', cwd: string, sessionID = ID): ResumeInfo {
  return { status: 'ready', agent, session_id: sessionID, cwd };
}

describe('shellQuote', () => {
  it.each([
    ['plain', '/work/repo', "'/work/repo'"],
    ['empty', '', "''"],
    ['spaces', '/my work/repo', "'/my work/repo'"],
    ['apostrophe', "/it's", "'/it'\\''s'"],
    ['leading apostrophe', "'a", "\\''a'"],
    ['backslash', '/a\\b', "'/a'\\\\'b'"],
    ['backslash then apostrophe', "\\'", "\\\\\\'"],
    ['substitution-looking text', '/$(touch x)`id`', "'/$(touch x)`id`'"],
    ['unicode', '/проект/日本 🎉', "'/проект/日本 🎉'"],
  ])('quotes %s', (_name, value, want) => {
    expect(shellQuote(value)).toBe(want);
  });

  it.each([
    ['newline', '/a\nb'],
    ['carriage return', '/a\rb'],
    ['tab', '/a\tb'],
    ['NUL', '/a\u0000b'],
    ['escape', '/a\u001b[31mb'],
    ['DEL', '/a\u007fb'],
    ['C1 next line', '/a\u0085b'],
    ['line separator', '/a\u2028b'],
    ['paragraph separator', '/a\u2029b'],
    ['lone surrogate', '/a\ud800b'],
  ])('rejects %s', (_name, value) => {
    expect(shellQuote(value)).toBeNull();
  });
});

describe('resumeCommand', () => {
  it('builds one line per supported agent', () => {
    expect(resumeCommand(ready('claude-code', '/work/repo'))).toBe(`cd '/work/repo' && claude --resume '${ID}'`);
    expect(resumeCommand(ready('pi', '/work/repo'))).toBe(`cd '/work/repo' && pi --session '${ID}'`);
  });

  it('returns null without a usable target', () => {
    expect(resumeCommand(undefined)).toBeNull();
    expect(resumeCommand({ status: 'unavailable', reason: 'missing_workspace' })).toBeNull();
    expect(resumeCommand(ready('pi', 'relative/dir'))).toBeNull();
    expect(resumeCommand(ready('pi', '/work\nrm -rf ~'))).toBeNull();
    expect(resumeCommand(ready('pi', '/work', `${ID}\n`))).toBeNull();
    for (const agent of ['codex', 'constructor', 'toString']) {
      expect(resumeCommand({ ...ready('pi', '/work'), agent } as unknown as ResumeInfo)).toBeNull();
    }
  });
});

describe('resumeUnavailableMessage', () => {
  it('explains a known reason and falls back for unknown ones', () => {
    expect(resumeUnavailableMessage('conflicting_workspace')).toBe(
      'Resume command unavailable: the main agent recorded more than one working directory, and none of them contains all the others.',
    );
    expect(resumeUnavailableMessage(undefined)).toMatch(/^Resume command unavailable: /);
    expect(resumeUnavailableMessage('toString' as never)).toBe(
      'Resume command unavailable: the recorded session metadata is incomplete.',
    );
  });
});

const SHELLS = [
  { name: 'bash', args: ['--noprofile', '--norc', '-c'] },
  { name: 'zsh', args: ['-f', '-c'] },
  { name: 'fish', args: ['--no-config', '-c'] },
].map((shell) => ({ ...shell, path: shellPath(shell.name) }));

// Resolved here because the commands run with a PATH that holds only the stubs
// and the system directories.
function shellPath(shell: string): string | null {
  const found = spawnSync('sh', ['-c', `command -v ${shell}`], { encoding: 'utf8' });
  return found.status === 0 ? found.stdout.trim() : null;
}

describe('generated commands in real shells', () => {
  const root = realpathSync(mkdtempSync(join(tmpdir(), 'resume-shell-')));
  const bin = join(root, 'bin');
  const out = join(root, 'out');
  mkdirSync(bin);
  // Each stub writes NUL-separated PWD, program name, and arguments.
  for (const program of ['claude', 'pi']) {
    const stub = join(bin, program);
    writeFileSync(stub, `#!/bin/sh\nprintf '%s\\0' "$PWD" "${program}" "$@" > "$RESUME_OUT"\n`);
    chmodSync(stub, 0o755);
  }
  afterAll(() => rmSync(root, { recursive: true, force: true }));

  const dirNames = [
    "it's a \\ dir",
    '$(touch pwned) `touch pwned` $HOME ~ * ? [a] {a,b} ; && | > !x %s',
    'проект 日本 🎉',
    "\\'\\\\''",
  ];
  const sessionID = "id 'with' \\ $(touch pwned) `x`";

  function run(shell: (typeof SHELLS)[number], command: string, cwd: string) {
    rmSync(out, { force: true });
    return spawnSync(shell.path as string, [...shell.args, command], {
      cwd,
      env: { PATH: `${bin}:/usr/bin:/bin`, HOME: root, RESUME_OUT: out },
      encoding: 'utf8',
    });
  }

  for (const shell of SHELLS) {
    it.skipIf(!shell.path).each(dirNames)(`${shell.name} delivers %s unchanged`, (name) => {
      const dir = join(root, 'dirs', name);
      mkdirSync(dir, { recursive: true });
      const scratch = mkdtempSync(join(root, 'scratch-'));
      for (const agent of ['claude-code', 'pi'] as const) {
        const command = resumeCommand(ready(agent, dir, sessionID));
        expect(command).not.toBeNull();
        const result = run(shell, command as string, scratch);
        expect(result.status, result.stderr).toBe(0);
        const fields = readFileSync(out, 'utf8').split('\0').slice(0, -1);
        expect(fields).toEqual(
          agent === 'pi' ? [dir, 'pi', '--session', sessionID] : [dir, 'claude', '--resume', sessionID],
        );
      }
      expect(existsSync(join(scratch, 'pwned'))).toBe(false);
      expect(existsSync(join(dir, 'pwned'))).toBe(false);
    });

    it.skipIf(!shell.path)(`${shell.name} does not start the agent when cd fails`, () => {
      const command = resumeCommand(ready('pi', join(root, 'missing dir')));
      const result = run(shell, command as string, root);
      expect(result.error).toBeUndefined();
      expect(result.status).not.toBe(0);
      expect(result.status).not.toBeNull();
      expect(existsSync(out)).toBe(false);
    });
  }
});
