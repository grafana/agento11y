import { createPromptfooExtension } from '@grafana/agento11y/promptfoo';
import { ExperimentsClient, TestSuitesClient } from '@grafana/agento11y/experiments';

const local = process.env.AGENTO11Y_LOCAL_DEMO === 'true';

export const afterAll = createPromptfooExtension({
  experimentName: 'Promptfoo deterministic example',
  suiteId: 'promptfoo-example',
  suiteVersion: '1',
  primaryScoreKey: 'promptfoo',
  ...(local
    ? {
        client: new ExperimentsClient({
          endpoint: 'http://localhost:8080',
          ingestToken: 'local-development',
          grafanaUrl: 'http://localhost:3000',
        }),
        testSuitesClient: new TestSuitesClient({
          controlEndpoint: 'http://localhost:8080/api/v1/eval',
          serviceAccountToken: 'local-development',
        }),
        suitePublicationPolicy: 'subset',
      }
    : {}),
});
