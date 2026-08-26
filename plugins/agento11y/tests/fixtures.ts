import type {
  ConversationMetricsAggregate,
  ConversationMetricsResponse,
  TokenBuckets,
} from '../internal/local/web/src/types';

export function response(body: unknown): Response {
  return {
    ok: true,
    status: 200,
    json: () => Promise.resolve(body),
    text: () => Promise.resolve(''),
  } as Response;
}

export const EMPTY_BUCKETS: TokenBuckets = {
  fresh_input: 0,
  cache_read: 0,
  cache_write: 0,
  output: 0,
  reasoning: 0,
};

export function aggregate(overrides: Partial<ConversationMetricsAggregate> = {}): ConversationMetricsAggregate {
  return {
    calls: 0,
    errored: 0,
    agents: 0,
    agent_hosts: [],
    workspaces: 0,
    token_buckets: { ...EMPTY_BUCKETS },
    token_buckets_by_model: {},
    models: [],
    ...overrides,
  };
}

// Sessions hides KPI tiles after loading if a metrics mock omits aggregate or
// matched_conversations.
export function metricsResponse(overrides: Partial<ConversationMetricsResponse> = {}): ConversationMetricsResponse {
  return {
    aggregate: aggregate(overrides.aggregate),
    conversations: [],
    matched_conversations: 0,
    ...overrides,
  };
}
