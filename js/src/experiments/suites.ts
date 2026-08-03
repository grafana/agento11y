/**
 * Control-plane client for stored Agent Observability test suites.
 *
 * Experiment ingest writes runs, trials, scores, and generations with the ingest
 * token. Stored test suites live behind the Grafana plugin control plane, which
 * takes a service-account token instead, so this client keeps its own credential
 * and its own base URL.
 */

import { defaultEnv, resolveHeadersWithAuth } from '../config.js';
import {
  CONFLICT_PREFIX,
  ExperimentConflictError,
  NOT_FOUND_PREFIX,
  notFoundError,
  transportError,
  validationError,
} from './errors.js';
import { isRecord, normalizeCursor, str } from './models.js';
import type { ExperimentsRetryPolicy } from './transport.js';
import { requestExperimentsJSON } from './transport.js';
import type { TestCase, TestSuite } from './types.js';

const DEFAULT_CONTROL_PATH = '/api/plugins/grafana-agento11y-app/resources/eval';
const GRAFANA_APP_PATH = '/a/grafana-agento11y-app';
const PORTABILITY_METADATA_KEY = 'agento11y.sdk.portability';
const PORTABILITY_VERSION = 1;
/**
 * Control-plane requests get six total attempts, not the ingest transport's four:
 * a stored-suite push is a long interactive operation and a lost draft costs the
 * caller the whole push. Go uses the same budget; Python leaves its control-plane
 * client on the default four.
 */
const CONTROL_MAX_RETRIES = 5;
const DEFAULT_PAGE_LIMIT = 200;
const DEFAULT_MAX_PAGES = 100;

const ENV_GRAFANA_URL = 'AGENTO11Y_GRAFANA_URL';
const ENV_CONTROL_ENDPOINT = 'AGENTO11Y_CONTROL_ENDPOINT';
const ENV_SERVICE_ACCOUNT_TOKEN = 'AGENTO11Y_SERVICE_ACCOUNT_TOKEN';

export interface TestSuitesClientOptions {
  /** Grafana base URL. Falls back to `AGENTO11Y_GRAFANA_URL`. */
  grafanaUrl?: string;
  /** Control-plane base URL. Falls back to `AGENTO11Y_CONTROL_ENDPOINT`, then `grafanaUrl`. */
  controlEndpoint?: string;
  /** Grafana service-account token. Falls back to `AGENTO11Y_SERVICE_ACCOUNT_TOKEN`. */
  serviceAccountToken?: string;
  timeoutMs?: number;
  retry?: Partial<ExperimentsRetryPolicy>;
  fetchImpl?: typeof fetch;
  sleep?: (durationMs: number, signal?: AbortSignal) => Promise<void>;
  env?: Record<string, string | undefined>;
}

export interface ListOptions {
  limit?: number;
  maxPages?: number;
}

export interface PushSuiteOptions {
  publish?: boolean;
  changelog?: string;
  emptyDraft?: boolean;
  /** Deletes remote-only cases so the draft matches the local suite exactly. */
  prune?: boolean;
}

/** Summary returned by `pushSuite`. */
export interface PushedSuite {
  suiteId: string;
  suiteVersion: string;
  published: boolean;
  suite: TestSuite;
  remoteSuite: Record<string, unknown>;
  remoteVersion: Record<string, unknown>;
  prunedCaseIds: string[];
}

export class TestSuitesClient {
  readonly endpoint: string;
  readonly grafanaUrl: string;

  private readonly headers: Record<string, string>;
  private readonly retry: Partial<ExperimentsRetryPolicy>;
  private readonly fetchImpl: typeof fetch | undefined;
  private readonly sleep: ((durationMs: number, signal?: AbortSignal) => Promise<void>) | undefined;

  constructor(options: TestSuitesClientOptions = {}) {
    const env = options.env ?? defaultEnv();
    const grafana = (options.grafanaUrl ?? env[ENV_GRAFANA_URL] ?? '').trim();
    let endpoint = (options.controlEndpoint ?? env[ENV_CONTROL_ENDPOINT] ?? '').trim();
    if (endpoint.length === 0) {
      if (grafana.length === 0) {
        throw new Error('agento11y experiments: controlEndpoint is required (or set AGENTO11Y_CONTROL_ENDPOINT)');
      }
      endpoint = grafana;
    }
    const token = (options.serviceAccountToken ?? env[ENV_SERVICE_ACCOUNT_TOKEN] ?? '').trim();
    if (token.length === 0) {
      throw new Error(
        'agento11y experiments: serviceAccountToken is required (or set AGENTO11Y_SERVICE_ACCOUNT_TOKEN)',
      );
    }
    this.endpoint = normalizeControlEndpoint(endpoint);
    const parsed = new URL(this.endpoint);
    this.grafanaUrl = grafana.length > 0 ? grafana.replace(/\/+$/, '') : `${parsed.protocol}//${parsed.host}`;
    this.headers = {
      ...(resolveHeadersWithAuth(undefined, { mode: 'bearer', bearerToken: token }, 'agento11y test suites') ?? {}),
    };
    this.retry = {
      maxRetries: CONTROL_MAX_RETRIES,
      ...options.retry,
      ...(options.timeoutMs !== undefined ? { timeoutMs: options.timeoutMs } : {}),
    };
    this.fetchImpl = options.fetchImpl;
    this.sleep = options.sleep;
  }

  /** Lists every stored suite visible to the control-plane token. */
  async listSuites(options: ListOptions = {}): Promise<Record<string, unknown>[]> {
    return this.pageThrough('/test-suites', options, (items) => items, 'test suite list');
  }

  /** Fetches one stored suite record, including its version metadata. */
  async getSuite(suiteId: string): Promise<Record<string, unknown>> {
    const normalized = requireId(suiteId, 'suite_id');
    const body = await this.request('GET', `/test-suites/${encodeURIComponent(normalized)}`);
    return isRecord(body) ? body : {};
  }

  /** Lists every case in one exact stored suite version. */
  async listCases(suiteId: string, version: string, options: ListOptions = {}): Promise<TestCase[]> {
    const normalizedSuite = requireId(suiteId, 'suite_id');
    const normalizedVersion = requireId(version, 'version');
    return this.pageThrough(
      `/test-suites/${encodeURIComponent(normalizedSuite)}/versions/${encodeURIComponent(normalizedVersion)}/test-cases`,
      options,
      (items) => items.map(remoteCaseToLocal),
      'test case list',
    );
  }

  /** Pulls a stored suite version into the SDK's portable `TestSuite` shape. */
  async pullSuite(suiteId: string, version = 'latest_published'): Promise<TestSuite> {
    const remote = await this.getSuite(suiteId);
    const resolved = resolveVersion(remote, version);
    const cases = await this.listCases(suiteId, resolved);
    return {
      suiteId: str(remote.suite_id).length > 0 ? str(remote.suite_id) : suiteId,
      name: str(remote.name),
      version: resolved,
      description: str(remote.description),
      tags: Array.isArray(remote.tags) ? remote.tags.filter((tag): tag is string => typeof tag === 'string') : [],
      changelog: str(versionRecord(remote, resolved).changelog),
      testCases: cases,
    };
  }

  /**
   * Pushes local suite metadata and cases into a mutable stored draft.
   *
   * Local cases are upserted into the draft. Set `prune` to delete remote-only
   * cases and make the draft match the local suite exactly.
   */
  async pushSuite(suite: TestSuite, options: PushSuiteOptions = {}): Promise<PushedSuite> {
    const suiteId = requireId(suite.suiteId, 'suite_id');
    let remote = await this.ensureSuite(suite);
    await this.patchSuiteMetadata(suite, remote);
    remote = await this.getSuite(suiteId);
    const changelog = options.changelog ?? suite.changelog ?? '';
    let version = await this.ensureDraftVersion(suiteId, remote, changelog, options.emptyDraft ?? false);
    const versionId = str(version.version);
    if (versionId.length === 0) {
      throw transportError('test suite version: missing version');
    }

    for (const testCase of suite.testCases) {
      await this.upsertCase(suiteId, versionId, testCase);
    }

    const prunedCaseIds: string[] = [];
    if (options.prune === true) {
      const localIds = new Set(suite.testCases.map((testCase) => testCase.testCaseId));
      for (const remoteCase of await this.listCases(suiteId, versionId)) {
        if (!localIds.has(remoteCase.testCaseId)) {
          await this.deleteCase(suiteId, versionId, remoteCase.testCaseId);
          prunedCaseIds.push(remoteCase.testCaseId);
        }
      }
    }

    let published = false;
    if (options.publish === true) {
      const body = await this.request(
        'POST',
        `/test-suites/${encodeURIComponent(suiteId)}/versions/${encodeURIComponent(versionId)}:publish`,
      );
      version = isRecord(body) ? body : version;
      published = isRecord(body) ? body.published === true : true;
    }

    return {
      suiteId,
      suiteVersion: versionId,
      published,
      suite: {
        suiteId,
        name: suite.name !== undefined && suite.name.length > 0 ? suite.name : str(remote.name),
        version: versionId,
        description:
          suite.description !== undefined && suite.description.length > 0 ? suite.description : str(remote.description),
        tags: suite.tags !== undefined && suite.tags.length > 0 ? [...suite.tags] : remoteTags(remote),
        changelog,
        testCases: [...suite.testCases],
      },
      remoteSuite: remote,
      remoteVersion: version,
      prunedCaseIds,
    };
  }

  /** Resolves exact versions and the `latest_published`, `latest`, and `draft` aliases. */
  resolveVersion(suite: Record<string, unknown>, version: string): string {
    return resolveVersion(suite, version);
  }

  private async ensureSuite(suite: TestSuite): Promise<Record<string, unknown>> {
    try {
      return await this.getSuite(suite.suiteId);
    } catch (error) {
      if (!isNotFound(error)) {
        throw error;
      }
      const payload: Record<string, unknown> = {
        suite_id: suite.suiteId,
        name: suite.name !== undefined && suite.name.length > 0 ? suite.name : suite.suiteId,
      };
      if (suite.description !== undefined && suite.description.length > 0) {
        payload.description = suite.description;
      }
      if (suite.tags !== undefined && suite.tags.length > 0) {
        payload.tags = [...suite.tags];
      }
      const body = await this.request('POST', '/test-suites', { payload });
      return isRecord(body) ? body : {};
    }
  }

  private async patchSuiteMetadata(suite: TestSuite, remote: Record<string, unknown>): Promise<void> {
    const patch: Record<string, unknown> = {};
    if (suite.name !== undefined && suite.name.length > 0 && suite.name !== remote.name) {
      patch.name = suite.name;
    }
    if (suite.description !== undefined && suite.description.length > 0) {
      patch.description = suite.description;
    }
    if (suite.tags !== undefined && suite.tags.length > 0) {
      patch.tags = [...suite.tags];
    }
    if (Object.keys(patch).length > 0) {
      await this.request('PATCH', `/test-suites/${encodeURIComponent(suite.suiteId)}`, { payload: patch });
    }
  }

  private async ensureDraftVersion(
    suiteId: string,
    suite: Record<string, unknown>,
    changelog: string,
    emptyDraft: boolean,
  ): Promise<Record<string, unknown>> {
    const existing = draftVersion(versions(suite));
    if (existing !== undefined) {
      validateExistingDraftOptions(existing, changelog, emptyDraft);
      return existing;
    }
    const payload: Record<string, unknown> = {};
    if (changelog.length > 0) {
      payload.changelog = changelog;
    }
    if (emptyDraft) {
      payload.empty_draft = true;
    }
    try {
      const body = await this.request('POST', `/test-suites/${encodeURIComponent(suiteId)}/versions`, { payload });
      return isRecord(body) ? body : {};
    } catch (error) {
      if (!isConflict(error)) {
        throw error;
      }
      // Another writer opened the draft between the read and this create.
      const refreshed = await this.getSuite(suiteId);
      const draft = draftVersion(versions(refreshed));
      if (draft === undefined) {
        throw error;
      }
      validateExistingDraftOptions(draft, changelog, emptyDraft);
      return draft;
    }
  }

  private async upsertCase(suiteId: string, version: string, testCase: TestCase): Promise<void> {
    await this.request(
      'POST',
      `/test-suites/${encodeURIComponent(suiteId)}/versions/${encodeURIComponent(version)}/test-cases`,
      { payload: localCaseToRemote(testCase) },
    );
  }

  private async deleteCase(suiteId: string, version: string, testCaseId: string): Promise<void> {
    await this.request(
      'DELETE',
      `/test-suites/${encodeURIComponent(suiteId)}/versions/${encodeURIComponent(version)}/test-cases/${encodeURIComponent(testCaseId)}`,
    );
  }

  private async pageThrough<T>(
    path: string,
    options: ListOptions,
    map: (items: Record<string, unknown>[]) => T[],
    label: string,
  ): Promise<T[]> {
    const limit = options.limit !== undefined && options.limit > 0 ? options.limit : DEFAULT_PAGE_LIMIT;
    const maxPages = options.maxPages !== undefined && options.maxPages > 0 ? options.maxPages : DEFAULT_MAX_PAGES;
    const out: T[] = [];
    let cursor: string | undefined;
    for (let page = 0; page < maxPages; page++) {
      const query: Record<string, string> = { limit: String(limit) };
      if (cursor !== undefined) {
        query.cursor = cursor;
      }
      const body = await this.request('GET', path, { query });
      out.push(...map(items(body)));
      cursor = normalizeCursor(isRecord(body) ? body.next_cursor : undefined);
      if (cursor === undefined) {
        return out;
      }
    }
    throw transportError(`${label}: pagination did not terminate`);
  }

  private async request(
    method: 'GET' | 'POST' | 'PATCH' | 'DELETE',
    path: string,
    options: { payload?: Record<string, unknown>; query?: Record<string, string> } = {},
  ): Promise<unknown> {
    return requestExperimentsJSON(
      {
        // The normalized control endpoint already carries its own path prefix, so
        // it is passed through as the base URL rather than as an API endpoint.
        endpoint: this.endpoint,
        insecure: this.endpoint.startsWith('http://'),
        headers: this.headers,
        retry: this.retry,
        ...(this.fetchImpl !== undefined ? { fetchImpl: this.fetchImpl } : {}),
        ...(this.sleep !== undefined ? { sleep: this.sleep } : {}),
      },
      {
        method,
        path: path.startsWith('/') ? path : `/${path}`,
        ...(options.payload !== undefined ? { body: options.payload } : {}),
        ...(options.query !== undefined ? { query: options.query } : {}),
        label: 'test suite',
      },
    );
  }
}

/**
 * Normalizes a control endpoint onto the plugin resources path.
 *
 * A Grafana base URL, a URL that already points at the resources path, and a UI
 * app URL all resolve to the same endpoint, so a caller can paste whichever URL
 * they have.
 */
export function normalizeControlEndpoint(value: string): string {
  const raw = value.trim().replace(/\/+$/, '');
  let parsed: URL;
  try {
    parsed = new URL(raw);
  } catch {
    throw new Error('agento11y experiments: controlEndpoint must be an absolute URL');
  }
  if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
    throw new Error('agento11y experiments: controlEndpoint must be an absolute URL');
  }
  const path = parsed.pathname.replace(/\/+$/, '');
  let normalizedPath: string;
  if (path.endsWith(DEFAULT_CONTROL_PATH) || path.endsWith('/api/v1/eval')) {
    normalizedPath = path;
  } else {
    const appIndex = path.indexOf(GRAFANA_APP_PATH);
    const prefix = appIndex >= 0 ? path.slice(0, appIndex) : path;
    normalizedPath = prefix.replace(/\/+$/, '') + DEFAULT_CONTROL_PATH;
  }
  return `${parsed.protocol}//${parsed.host}${normalizedPath}`;
}

function resolveVersion(suite: Record<string, unknown>, version: string): string {
  const requested = version.trim();
  if (requested.length === 0) {
    throw validationError('test suite: version is required');
  }
  const all = versions(suite);
  let record: Record<string, unknown> | undefined;
  if (requested === 'latest_published') {
    record = latest(all.filter((item) => item.published === true));
    if (record === undefined) {
      throw notFoundError('test suite version: latest_published');
    }
  } else if (requested === 'latest') {
    record = latest(all);
    if (record === undefined) {
      throw notFoundError('test suite version: latest');
    }
  } else if (requested === 'draft') {
    record = draftVersion(all);
    if (record === undefined) {
      throw notFoundError('test suite version: draft');
    }
  } else {
    if (all.some((item) => str(item.version) === requested)) {
      return requested;
    }
    throw notFoundError(`test suite version: ${requested}`);
  }
  const resolved = str(record.version);
  if (resolved.length === 0) {
    throw notFoundError(`test suite version: ${requested}`);
  }
  return resolved;
}

function versions(suite: Record<string, unknown>): Record<string, unknown>[] {
  return Array.isArray(suite.versions) ? suite.versions.filter(isRecord) : [];
}

function versionRecord(suite: Record<string, unknown>, version: string): Record<string, unknown> {
  return versions(suite).find((item) => str(item.version) === version) ?? {};
}

function draftVersion(all: Record<string, unknown>[]): Record<string, unknown> | undefined {
  return all.find((item) => item.published !== true);
}

/** Orders `v<N>` versions numerically and everything else lexically below them. */
function latest(all: Record<string, unknown>[]): Record<string, unknown> | undefined {
  let best: Record<string, unknown> | undefined;
  let bestKey: [number, number, string] | undefined;
  for (const item of all) {
    const key = versionSortKey(str(item.version));
    if (bestKey === undefined || compareKeys(key, bestKey) > 0) {
      best = item;
      bestKey = key;
    }
  }
  return best;
}

function versionSortKey(value: string): [number, number, string] {
  const match = /^v(\d+)$/.exec(value.trim());
  if (match?.[1] !== undefined) {
    return [1, Number.parseInt(match[1], 10), value];
  }
  return [0, 0, value];
}

function compareKeys(a: [number, number, string], b: [number, number, string]): number {
  if (a[0] !== b[0]) {
    return a[0] - b[0];
  }
  if (a[1] !== b[1]) {
    return a[1] - b[1];
  }
  return a[2] < b[2] ? -1 : a[2] > b[2] ? 1 : 0;
}

/**
 * Rejects push options an already-open draft cannot satisfy.
 *
 * Both cases are recoverable: the caller publishes or discards the draft and
 * pushes again. They are classified `open_draft` so a caller branching on
 * `ExperimentConflictError.recoverable` reaches that path, which is what Go does.
 */
function validateExistingDraftOptions(draft: Record<string, unknown>, changelog: string, emptyDraft: boolean): void {
  const draftChangelog = str(draft.changelog);
  if (changelog.length > 0 && changelog !== draftChangelog) {
    throw new ExperimentConflictError(
      'agento11y test suite conflict: an existing draft cannot apply a different changelog',
      'open_draft',
    );
  }
  if (emptyDraft) {
    throw new ExperimentConflictError(
      'agento11y test suite conflict: empty_draft only applies when creating a new draft',
      'open_draft',
    );
  }
}

/** The remote suite's tags, used when the local suite leaves them unset. */
function remoteTags(remote: Record<string, unknown>): string[] {
  return Array.isArray(remote.tags) ? remote.tags.filter((tag): tag is string => typeof tag === 'string') : [];
}

/**
 * Maps a local case onto the remote shape.
 *
 * The control plane stores `input` and `expected` as objects, so a scalar is
 * wrapped as `{value: ...}` and the wrapping is recorded under the portability
 * metadata key. `pullSuite` unwraps exactly the fields that record names, so a
 * push-pull round trip returns the caller's original scalars.
 */
export function localCaseToRemote(testCase: TestCase): Record<string, unknown> {
  if (testCase.testCaseId.trim().length === 0) {
    throw validationError('test suite: test_case_id is required');
  }
  if (testCase.input === undefined || testCase.input === null) {
    throw validationError('test suite: input is required');
  }
  const metadata: Record<string, unknown> = { ...(testCase.metadata ?? {}) };
  const portability: Record<string, unknown> = { version: PORTABILITY_VERSION };
  if (testCase.weight !== undefined && testCase.weight !== 1) {
    portability.weight = testCase.weight;
  }
  const wrapped: string[] = [];
  const [remoteInput, inputWrapped] = ensureObject(testCase.input);
  if (inputWrapped) {
    wrapped.push('input');
  }
  let remoteExpected: Record<string, unknown> | undefined;
  if (testCase.expected !== undefined && testCase.expected !== null) {
    const [value, expectedWrapped] = ensureObject(testCase.expected);
    remoteExpected = value;
    if (expectedWrapped) {
      wrapped.push('expected');
    }
  }
  if (wrapped.length > 0) {
    portability.wrapped_fields = wrapped;
  }
  if (Object.keys(portability).length > 1) {
    metadata[PORTABILITY_METADATA_KEY] = portability;
  }

  const out: Record<string, unknown> = { test_case_id: testCase.testCaseId, input: remoteInput };
  if (testCase.name !== undefined && testCase.name.length > 0) {
    out.name = testCase.name;
  }
  if (testCase.description !== undefined && testCase.description.length > 0) {
    out.description = testCase.description;
  }
  if (testCase.tags !== undefined && testCase.tags.length > 0) {
    out.tags = [...testCase.tags];
  }
  if (testCase.category !== undefined && testCase.category.length > 0) {
    out.category = testCase.category;
  }
  if (remoteExpected !== undefined) {
    out.expected = remoteExpected;
  }
  if (Object.keys(metadata).length > 0) {
    out.metadata = metadata;
  }
  if (testCase.artifactRefs !== undefined && testCase.artifactRefs.length > 0) {
    out.artifact_refs = testCase.artifactRefs.map((ref) => ({ ...ref }));
  }
  return out;
}

export function remoteCaseToLocal(data: Record<string, unknown>): TestCase {
  const metadata: Record<string, unknown> = { ...(isRecord(data.metadata) ? data.metadata : {}) };
  const rawPortability = metadata[PORTABILITY_METADATA_KEY];
  let portability: Record<string, unknown> = {};
  if (isRecord(rawPortability) && rawPortability.version === PORTABILITY_VERSION) {
    portability = rawPortability;
    delete metadata[PORTABILITY_METADATA_KEY];
  }
  const weight = typeof portability.weight === 'number' ? portability.weight : 1;
  const wrapped = new Set(
    Array.isArray(portability.wrapped_fields)
      ? portability.wrapped_fields.filter((field): field is string => typeof field === 'string')
      : [],
  );
  return {
    testCaseId: str(data.test_case_id).length > 0 ? str(data.test_case_id) : str(data.id),
    name: str(data.name),
    description: str(data.description),
    tags: Array.isArray(data.tags) ? data.tags.filter((tag): tag is string => typeof tag === 'string') : [],
    category: str(data.category),
    input: wrapped.has('input') ? unwrapValue(data.input) : data.input,
    expected: wrapped.has('expected') ? unwrapValue(data.expected) : data.expected,
    weight,
    metadata,
    artifactRefs: Array.isArray(data.artifact_refs)
      ? data.artifact_refs.filter(isRecord).map((ref) => ({ ...ref }))
      : [],
  };
}

function ensureObject(value: unknown): [Record<string, unknown>, boolean] {
  if (isRecord(value)) {
    return [{ ...value }, false];
  }
  return [{ value }, true];
}

function unwrapValue(value: unknown): unknown {
  if (isRecord(value) && Object.keys(value).length === 1 && 'value' in value) {
    return value.value;
  }
  return value;
}

function items(body: unknown): Record<string, unknown>[] {
  if (!isRecord(body) || !Array.isArray(body.items)) {
    return [];
  }
  return body.items.filter(isRecord).map((item) => ({ ...item }));
}

function requireId(value: string, name: string): string {
  const normalized = value.trim();
  if (normalized.length === 0) {
    throw validationError(`test suite: ${name} is required`);
  }
  return normalized;
}

function isNotFound(error: unknown): boolean {
  return error instanceof Error && error.message.startsWith(NOT_FOUND_PREFIX);
}

function isConflict(error: unknown): boolean {
  return (
    error instanceof ExperimentConflictError || (error instanceof Error && error.message.startsWith(CONFLICT_PREFIX))
  );
}
