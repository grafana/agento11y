import { createPromptfooExtension } from '@grafana/agento11y/promptfoo';

export const afterAll = createPromptfooExtension({
  experimentName: 'Promptfoo deterministic example',
  suiteId: 'promptfoo-example',
  suiteVersion: '1',
  primaryScoreKey: 'promptfoo',
});
