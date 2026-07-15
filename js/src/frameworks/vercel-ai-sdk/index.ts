import type { SigilClient } from '../../client.js';
import { SigilVercelAiSdkInstrumentation } from './hooks.js';
import type { SigilVercelAiSdkOptions } from './types.js';

export { SigilVercelAiSdkInstrumentation } from './hooks.js';
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
  CallOptions,
  ConversationResolution,
  GenerateTextHooks,
  PrepareStepEvent,
  PrepareStepResult,
  SigilVercelAiSdkOptions,
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

export function createSigilVercelAiSdk(
  client: SigilClient,
  options: SigilVercelAiSdkOptions = {},
): SigilVercelAiSdkInstrumentation {
  return new SigilVercelAiSdkInstrumentation(client, options);
}
