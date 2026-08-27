// Loaders for the cross-language hook wire fixtures in `conformance/hooks/`.
//
// The preflight request is built from public SDK types so the conformance suite
// compares what a caller can actually produce against the shared contract. See
// `conformance/hooks/README.md` for the encoding rules.

import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
// js/test -> repository root
const fixtureDir = join(__dirname, '..', '..', 'conformance', 'hooks');

export const bashToolSchema =
  '{"type":"object","properties":{"command":{"type":"string","description":"Shell command to run."}},"required":["command"]}';

export const readFileToolSchema = '{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}';

export function loadPreflightRequest() {
  return JSON.parse(readFileSync(join(fixtureDir, 'request-preflight.json'), 'utf8'));
}

export function loadPostflightGuardRequest() {
  return JSON.parse(readFileSync(join(fixtureDir, 'request-postflight-guard.json'), 'utf8'));
}

export function loadResponses() {
  return JSON.parse(readFileSync(join(fixtureDir, 'responses.json'), 'utf8'));
}

/**
 * Reports structural differences as dotted JSON paths plus both values, so a
 * failure names the offending field instead of dumping two payloads.
 */
export function diffJson(got, want, path = '') {
  const label = path === '' ? '<root>' : path;
  if (Array.isArray(want)) {
    if (!Array.isArray(got)) {
      return [`${label}: got ${JSON.stringify(got)}, want an array`];
    }
    if (got.length !== want.length) {
      return [`${label}: got ${got.length} items, want ${want.length}`];
    }
    return want.flatMap((item, index) => diffJson(got[index], item, `${path}[${index}]`));
  }
  if (want !== null && typeof want === 'object') {
    if (got === null || typeof got !== 'object' || Array.isArray(got)) {
      return [`${label}: got ${JSON.stringify(got)}, want an object`];
    }
    const keys = [...new Set([...Object.keys(want), ...Object.keys(got)])].sort();
    const diffs = [];
    for (const key of keys) {
      const child = path === '' ? key : `${path}.${key}`;
      if (!(key in got)) {
        diffs.push(`${child}: missing, want ${JSON.stringify(want[key])}`);
      } else if (!(key in want)) {
        diffs.push(`${child}: unexpected ${JSON.stringify(got[key])}`);
      } else {
        diffs.push(...diffJson(got[key], want[key], child));
      }
    }
    return diffs;
  }
  if (got !== want) {
    return [`${label}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`];
  }
  return [];
}

/**
 * Builds `request-postflight-guard.json` from the public JS SDK types.
 *
 * The shipped guards evaluate a tool call under `input.output`, and the server's
 * tool filter scans that field before `input.messages`.
 */
export function postflightGuardRequest() {
  return {
    phase: 'postflight',
    context: {
      agentName: 'conformance-guard',
      agentVersion: '1.2.3',
      conversationId: 'conv-hooks-conformance',
      model: { provider: 'anthropic', name: 'claude-sonnet-4' },
    },
    input: {
      output: [
        {
          role: 'assistant',
          parts: [
            {
              type: 'tool_call',
              toolCall: { id: 'call-bash', name: 'Bash', inputJSON: '{"command":"rm -rf /tmp/cache"}' },
            },
          ],
        },
      ],
    },
  };
}

/** Builds `request-preflight.json` from the public JS SDK types. */
export function preflightRequest() {
  return {
    phase: 'preflight',
    context: {
      agentName: 'conformance-agent',
      agentVersion: '1.2.3',
      model: { provider: 'anthropic', name: 'claude-sonnet-4' },
      tags: { env: 'test', team: 'agent-observability' },
      conversationId: 'conv-hooks-conformance',
      traceId: '0123456789abcdef0123456789abcdef',
      spanId: '0123456789abcdef',
    },
    input: {
      systemPrompt: 'You are a careful assistant.',
      messages: [
        {
          role: 'user',
          parts: [{ type: 'text', text: 'Delete the cache directory under /tmp.' }],
        },
        {
          role: 'assistant',
          parts: [
            { type: 'thinking', thinking: 'The request is destructive, so inspect the directory first.' },
            {
              type: 'tool_call',
              toolCall: { id: 'call-read', name: 'read_file', inputJSON: '{"path":"/tmp/cache/manifest.json"}' },
            },
            {
              type: 'tool_call',
              toolCall: { id: 'call-bash', name: 'Bash', inputJSON: '{"command":"rm -rf /tmp/cache"}' },
            },
          ],
        },
        {
          role: 'tool',
          name: 'read_file',
          parts: [
            {
              type: 'tool_result',
              toolResult: {
                toolCallId: 'call-read',
                name: 'read_file',
                content: '3 entries',
                contentJSON: '{"entries":3}',
              },
            },
          ],
        },
        {
          role: 'tool',
          name: 'Bash',
          parts: [
            {
              type: 'tool_result',
              toolResult: {
                toolCallId: 'call-bash',
                name: 'Bash',
                isError: true,
                content: "rm: cannot remove '/tmp/cache': Permission denied",
                contentJSON: '{"exit_code":1}',
              },
            },
          ],
        },
      ],
      tools: [
        {
          name: 'Bash',
          description: 'Run a shell command.',
          type: 'function',
          inputSchemaJSON: bashToolSchema,
        },
        {
          name: 'read_file',
          description: 'Read a file from disk.',
          type: 'function',
          inputSchemaJSON: readFileToolSchema,
        },
      ],
      conversationPreview: 'user: Delete the cache directory under /tmp.',
    },
  };
}
