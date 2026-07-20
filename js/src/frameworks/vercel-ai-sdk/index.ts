import type { Agento11yClient } from '../../client.js';
import { Agento11yVercelAiSdkInstrumentation } from './hooks.js';
import type { Agento11yVercelAiSdkOptions } from './types.js';

export { Agento11yVercelAiSdkInstrumentation } from './hooks.js';
export {
  buildFrameworkMetadata,
  buildFrameworkTags,
  extractOutputSchemaTool,
  fallbackConversationId,
  frameworkIdentity,
  isTextChunk,
  mapInputMessages,
  mapModelFromStepStart,
  mapResponseFromStepFinish,
  mapStepOutput,
  mapUsageFromStepFinish,
  normalizeMetadata,
  parseToolCallFinish,
  parseToolCallStart,
  resolveConversationId,
} from './mapping.js';
export type {
  Agento11yVercelAiSdkOptions,
  CallOptions,
  ConversationResolution,
  GenerateTextHooks,
  PrepareStepEvent,
  PrepareStepResult,
  StepFinishEvent,
  StepOutputMapping,
  StepStartEvent,
  StreamChunkEvent,
  StreamErrorEvent,
  StreamTextHooks,
  ToolCallFinishEvent,
  ToolCallStartEvent,
  VercelAiSdkModelMessage,
} from './types.js';

export function createAgento11yVercelAiSdk(
  client: Agento11yClient,
  options: Agento11yVercelAiSdkOptions = {},
): Agento11yVercelAiSdkInstrumentation {
  return new Agento11yVercelAiSdkInstrumentation(client, options);
}
