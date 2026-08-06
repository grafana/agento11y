import { asError, baseURLFromAPIEndpoint } from '../utils.js';
import { conflictError, notFoundError, TRANSPORT_PREFIX, transportError, validationError } from './errors.js';

/** Response bodies above this size are abandoned instead of buffered. */
export const MAX_RESPONSE_BYTES = 8 << 20;

const DEFAULT_MAX_RETRIES = 3;
const DEFAULT_INITIAL_BACKOFF_MS = 100;
const DEFAULT_MAX_BACKOFF_MS = 5_000;
const DEFAULT_TIMEOUT_MS = 30_000;

/**
 * Retry behavior for experiment and score requests.
 *
 * Retries cover request timeouts, connection errors, HTTP 429, and HTTP 5xx,
 * with exponential backoff bounded by `maxBackoffMs`. 4xx responses other than
 * 429 are not retried (they are caller errors), and neither is a 503 that reports
 * a backend capability gap rather than a transient outage.
 */
export interface ExperimentsRetryPolicy {
  /** Retries after the first attempt. The default of 3 means four total attempts. */
  maxRetries: number;
  initialBackoffMs: number;
  maxBackoffMs: number;
  /** Per-attempt request timeout. Each attempt gets its own budget. */
  timeoutMs: number;
}

export function resolveRetryPolicy(policy: Partial<ExperimentsRetryPolicy> | undefined): ExperimentsRetryPolicy {
  return {
    maxRetries: Math.max(0, policy?.maxRetries ?? DEFAULT_MAX_RETRIES),
    initialBackoffMs: Math.max(0, policy?.initialBackoffMs ?? DEFAULT_INITIAL_BACKOFF_MS),
    maxBackoffMs: Math.max(0, policy?.maxBackoffMs ?? DEFAULT_MAX_BACKOFF_MS),
    timeoutMs: policy?.timeoutMs !== undefined && policy.timeoutMs > 0 ? policy.timeoutMs : DEFAULT_TIMEOUT_MS,
  };
}

/** Connection and auth for the experiments routes. */
export interface ExperimentsConnection {
  /** The Agent Observability API endpoint. Absolute URL, `grpc://` URL, or `host:port`. */
  endpoint: string;
  insecure: boolean;
  headers?: Record<string, string>;
  retry?: Partial<ExperimentsRetryPolicy>;
  fetchImpl?: typeof fetch;
  /** Injected for tests so a backoff does not spend wall-clock time. */
  sleep?: (durationMs: number, signal?: AbortSignal) => Promise<void>;
}

/**
 * One experiments request.
 *
 * A request carries a JSON `body`, a raw `bytes` body, or neither. Passing both
 * is rejected as a validation error rather than resolved by precedence.
 */
export interface ExperimentsRequest {
  method: 'GET' | 'POST' | 'PATCH' | 'DELETE';
  /** Path with every dynamic segment already percent-encoded (see `routes.ts`). */
  path: string;
  query?: Record<string, string>;
  /** JSON request body. Must not be combined with `bytes`. */
  body?: unknown;
  /** Raw request body, used by artifact upload. Must not be combined with `body`. */
  bytes?: Uint8Array;
  contentType?: string;
  /** Names the operation in error messages, e.g. `experiment create`. */
  label: string;
  signal?: AbortSignal;
}

/**
 * Sends one experiments request and decodes its JSON response.
 *
 * Status mapping: 400 and 422 become validation errors, 404 not found, 409 a
 * classified conflict; none of those are retried. Transport failures, 429, and
 * 5xx are retried up to `retry.maxRetries` times. Everything else fails with the
 * transport prefix.
 *
 * A caller `signal` aborts the wait immediately and rejects with the signal's own
 * reason, so a cancellation is never reported as a timeout or transport failure.
 */
export async function requestExperimentsJSON(
  connection: ExperimentsConnection,
  request: ExperimentsRequest,
): Promise<unknown> {
  const policy = resolveRetryPolicy(connection.retry);
  const fetchImpl = connection.fetchImpl ?? fetch;
  const sleep = connection.sleep ?? sleepWithSignal;
  const url = buildURL(connection, request);
  const { headers, payload } = buildRequestInit(connection, request);

  let backoffMs = policy.initialBackoffMs;

  for (let attempt = 0; ; attempt++) {
    throwIfAborted(request.signal);

    const controller = new AbortController();
    const unlink = linkSignal(request.signal, controller);
    const timer = setTimeout(() => {
      controller.abort(new Error(`request timed out after ${policy.timeoutMs}ms`));
    }, policy.timeoutMs);

    let outcome: AttemptOutcome;
    try {
      outcome = await sendAttempt(fetchImpl, url, request, headers, payload, controller.signal);
    } catch (error) {
      // The caller's own abort ends the call with the caller's reason.
      throwIfAborted(request.signal);
      if (error instanceof ResponseTooLargeError) {
        throw transportError(`${request.label}: ${error.message}`);
      }
      outcome = { kind: 'failed', detail: asError(error).message, retryable: true };
    } finally {
      // The timeout and the caller link stay armed until the body has been read.
      // A server that sends headers and then stalls the body must still fail the
      // attempt instead of hanging the call.
      clearTimeout(timer);
      unlink();
    }

    if (outcome.kind === 'success') {
      return decodeSuccess(outcome.text, request.label);
    }
    if (outcome.kind === 'fatal') {
      throw outcome.error;
    }
    if (!outcome.retryable || attempt >= policy.maxRetries) {
      throw transportError(`${request.label}: ${outcome.detail}`);
    }

    if (backoffMs > 0) {
      await sleep(Math.min(backoffMs, policy.maxBackoffMs), request.signal);
      backoffMs = Math.min(backoffMs * 2, policy.maxBackoffMs);
    }
  }
}

type AttemptOutcome =
  | { kind: 'success'; text: string }
  | { kind: 'fatal'; error: Error }
  | { kind: 'failed'; detail: string; retryable: boolean };

/**
 * Runs one attempt and classifies its response.
 *
 * The body is read here, inside the caller's armed timeout, so a stalled body
 * cannot outlive the per-attempt budget.
 */
async function sendAttempt(
  fetchImpl: typeof fetch,
  url: string,
  request: ExperimentsRequest,
  headers: Record<string, string>,
  payload: BodyInit | undefined,
  signal: AbortSignal,
): Promise<AttemptOutcome> {
  const response = await fetchImpl(url, {
    method: request.method,
    headers,
    signal,
    ...(payload !== undefined ? { body: payload } : {}),
  });

  if (response.status >= 200 && response.status < 300) {
    return { kind: 'success', text: await readCappedText(response, request.label) };
  }

  const body = await readErrorBody(response);
  const detail = body.length > 0 ? body : String(response.status);
  if (response.status === 400 || response.status === 422) {
    return { kind: 'fatal', error: validationError(`${request.label}: ${detail}`) };
  }
  if (response.status === 404) {
    return { kind: 'fatal', error: notFoundError(`${request.label}: ${detail}`) };
  }
  if (response.status === 409) {
    return { kind: 'fatal', error: conflictError(`${request.label}: ${detail}`) };
  }
  return {
    kind: 'failed',
    detail: `status ${response.status}: ${body.length > 0 ? body : 'unexpected status'}`,
    retryable:
      !isCapabilityGap(response, body) &&
      (response.status === 429 || (response.status >= 500 && response.status < 600)),
  };
}

/** Resolves after `durationMs`, rejecting with the signal's reason if it aborts first. */
export function sleepWithSignal(durationMs: number, signal?: AbortSignal): Promise<void> {
  if (durationMs <= 0) {
    throwIfAborted(signal);
    return Promise.resolve();
  }
  return new Promise((resolve, reject) => {
    if (signal?.aborted === true) {
      reject(abortReason(signal));
      return;
    }
    const timer = setTimeout(() => {
      signal?.removeEventListener('abort', onAbort);
      resolve();
    }, durationMs);
    function onAbort(): void {
      clearTimeout(timer);
      reject(abortReason(signal));
    }
    signal?.addEventListener('abort', onAbort, { once: true });
  });
}

export function throwIfAborted(signal: AbortSignal | undefined): void {
  if (signal?.aborted === true) {
    throw abortReason(signal);
  }
}

function abortReason(signal: AbortSignal | undefined): unknown {
  const reason = signal?.reason;
  return reason !== undefined ? reason : new Error('aborted');
}

function linkSignal(signal: AbortSignal | undefined, controller: AbortController): () => void {
  if (signal === undefined) {
    return () => {};
  }
  const onAbort = (): void => {
    controller.abort(signal.reason);
  };
  signal.addEventListener('abort', onAbort, { once: true });
  return () => {
    signal.removeEventListener('abort', onAbort);
  };
}

function buildURL(connection: ExperimentsConnection, request: ExperimentsRequest): string {
  const base = baseURLFromAPIEndpoint(connection.endpoint, connection.insecure, TRANSPORT_PREFIX);
  const query = request.query;
  if (query === undefined || Object.keys(query).length === 0) {
    return `${base}${request.path}`;
  }
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) {
    params.set(key, value);
  }
  return `${base}${request.path}?${params.toString()}`;
}

function buildRequestInit(
  connection: ExperimentsConnection,
  request: ExperimentsRequest,
): { headers: Record<string, string>; payload: BodyInit | undefined } {
  const headers: Record<string, string> = { ...(connection.headers ?? {}) };
  if (request.bytes !== undefined && request.body !== undefined) {
    throw validationError(`${request.label}: a request carries either a JSON body or raw bytes, not both`);
  }
  if (request.bytes !== undefined) {
    headers['Content-Type'] = request.contentType ?? 'application/octet-stream';
    // A Uint8Array view is a valid fetch body; the DOM lib types only accept the
    // narrower ArrayBuffer-backed spelling.
    return { headers, payload: request.bytes as unknown as BodyInit };
  }
  if (request.body === undefined) {
    return { headers, payload: undefined };
  }
  if (!hasHeader(headers, 'content-type')) {
    headers['Content-Type'] = 'application/json';
  }
  return { headers, payload: JSON.stringify(request.body) };
}

function hasHeader(headers: Record<string, string>, name: string): boolean {
  return Object.keys(headers).some((key) => key.toLowerCase() === name);
}

function decodeSuccess(text: string, label: string): unknown {
  const trimmed = text.trim();
  if (trimmed.length === 0) {
    return {};
  }
  try {
    return JSON.parse(trimmed);
  } catch (error) {
    throw transportError(`${label}: invalid JSON response: ${asError(error).message}`);
  }
}

async function readErrorBody(response: Response): Promise<string> {
  try {
    return (await readCappedText(response, 'error body')).trim();
  } catch {
    return '';
  }
}

/** Signals the body cap so the retry loop reports it instead of retrying it. */
class ResponseTooLargeError extends Error {
  constructor(label: string) {
    super(`${label}: response too large`);
    this.name = 'ResponseTooLargeError';
  }
}

/**
 * Reads a response body, abandoning it once it passes `MAX_RESPONSE_BYTES`.
 *
 * On the streaming path reading stops at the cap and the stream is cancelled, so
 * a runaway body is never buffered whole. A runtime whose response has no
 * readable stream falls back to buffering the whole body and checking its size
 * afterwards.
 */
async function readCappedText(response: Response, label: string): Promise<string> {
  const body = response.body;
  if (body === null || typeof body.getReader !== 'function') {
    const text = await response.text();
    if (new TextEncoder().encode(text).byteLength > MAX_RESPONSE_BYTES) {
      throw new ResponseTooLargeError(label);
    }
    return text;
  }
  const reader = body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) {
        break;
      }
      if (value === undefined) {
        continue;
      }
      total += value.byteLength;
      if (total > MAX_RESPONSE_BYTES) {
        throw new ResponseTooLargeError(label);
      }
      chunks.push(value);
    }
  } finally {
    reader.releaseLock?.();
    if (total > MAX_RESPONSE_BYTES) {
      await body.cancel().catch(() => {});
    }
  }
  const joined = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    joined.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return new TextDecoder().decode(joined);
}

/**
 * Shape of the plain-text message the ingest control plane returns with a 503:
 * lowercase words ending in "is unavailable", such as "trial evaluation service
 * is unavailable". Proxy and load balancer 503 pages do not match it: they are
 * HTML, and their prose is capitalized and punctuated.
 */
const capabilityGapMessage = /^[a-z0-9][a-z0-9 ._-]* is unavailable$/;

/**
 * Whether a 503 means the backend cannot serve this route at all.
 *
 * Agent Observability answers 503 when the store behind a route does not
 * implement the feature, so every retry returns the same thing and only spends
 * the budget. The match stays narrow, and an unrecognized 503 still retries.
 */
function isCapabilityGap(response: Response, body: string): boolean {
  if (response.status !== 503) {
    return false;
  }
  const contentType = response.headers.get('content-type') ?? '';
  if (!contentType.trim().toLowerCase().startsWith('text/plain')) {
    return false;
  }
  return capabilityGapMessage.test(body.trim());
}
