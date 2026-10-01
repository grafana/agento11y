# Promptfoo completed-result integration

`@grafana/agento11y/promptfoo` imports completed native Promptfoo runs. Promptfoo
continues to execute providers and assertions; the integration publishes the
finished result set as Agent Observability experiments.

## Install

```bash
npm install @grafana/agento11y promptfoo
```

Set `AGENTO11Y_ENDPOINT`, `AGENTO11Y_AUTH_TOKEN`, and, when required by your
stack, `AGENTO11Y_AUTH_TENANT_ID`. Set `AGENTO11Y_GRAFANA_URL` to print a deep
link to the experiment.

## Configure the extension

Create `agento11y-extension.mjs`:

```js
import { createPromptfooExtension } from '@grafana/agento11y/promptfoo';

export const afterAll = createPromptfooExtension({
  experimentName: 'Checkout agent regression',
  suiteId: 'checkout-regression',
  suiteVersion: '2026-09-09',
  primaryScoreKey: 'promptfoo',
});
```

Register only its `afterAll` hook in `promptfooconfig.yaml`:

```yaml
extensions:
  - file://agento11y-extension.mjs:afterAll
```

Each Promptfoo prompt/provider candidate becomes a separate Agent Observability
experiment run so candidate-level reports do not blend together. The same test
case id is retained across those runs for comparison. Within a run, each result
row becomes one trial. Promptfoo's aggregate `score` and `success` become the
trial's primary verdict under `primaryScoreKey`; every finite `namedScores`
entry becomes a diagnostic score. Repeated rows with the same case id become
attempts 1, 2, and so on.

Provider errors are published as operationally failed, unrated trials rather
than failed assertions. Native `latencyMs` is used as trial duration; absent
latency remains unknown.

Use `metadata.agento11y.test_case_id` in a Promptfoo test for a stable readable
case id:

```yaml
tests:
  - description: Refund policy
    vars:
      question: Can I return an opened item?
    metadata:
      agento11y.test_case_id: refund-policy
```

Without an explicit id, the adapter derives one from the case content so
reordering tests does not change identity.

To publish browsable cases, pass a `TestSuitesClient` and explicitly choose
`suitePublicationPolicy: 'subset'` to retain remote-only cases or `'replace'`
to prune them. Trial snapshots use the canonical cases returned by the server
and record digests when sanitization changed the executed case.

Provider IDs and display labels stay in Promptfoo metadata; they are not model
identities. Set `candidate: { modelProvider: 'openai', modelName: 'gpt-4o-mini' }`
when all candidates use the same model, or provide `agento11y.model_provider`
and `agento11y.model_name` in each result, test-case, or provider-response
metadata for mixed models. These fields identify both candidates and anchor
generations. Without explicit identity, candidate model fields stay unset and
anchor generations use the SDK's generic `eval` / `experiment` identity.

## Generation and conversation correlation

The adapter records Promptfoo's rendered input and provider output as one anchor
generation by default. If the target provider already uses Agent Observability
instrumentation, avoid duplicate generations:

```js
export default createPromptfooExtension({
  experimentName: 'Instrumented target',
  recordIO: false,
  resolveConversationId: (result) => result.response?.metadata?.conversationId,
  resolveGenerationId: (result) => result.response?.metadata?.generationId,
});
```

Without resolver functions, the adapter recognizes
`agento11y.conversation_id` and `agento11y.generation_id` in result, test-case,
or provider-response metadata. It also binds Promptfoo's `traceId` when present.

## Publish an existing result set

For programmatic Promptfoo use, call the lower-level function with the same
public `afterAll` context shape:

```ts
import { publishPromptfooResults } from '@grafana/agento11y/promptfoo';

const published = await publishPromptfooResults(context, {
  experimentName: 'Programmatic Promptfoo run',
});
for (const run of published.runs) console.log(run.url);
```

The adapter does not create or invoke Agent Observability stored evaluators.
Promptfoo performs grading locally (including any model-backed assertions) and
the adapter publishes the resulting scores.

See the [runnable example](../../../examples/experiments/promptfoo/README.md).
