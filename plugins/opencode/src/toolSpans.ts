import type {
  ContentCaptureMode,
  ToolExecutionKind,
  ToolExecutionRecorder,
  ToolExecutionResult,
  ToolExecutionStart,
} from "@grafana/agento11y";
import type { Redactor } from "./mappers.js";

export type ToolExecutionRecord = {
  sessionID: string;
  toolName: string;
  toolCallId: string;
  // undefined: unobserved; null: observed nonmatch; string: recognized skill.
  skillName?: string | null;
  executionKind?: ToolExecutionKind;
  verifiedSkillName?: string;
  startedAt: number;
  completedAt: number;
  input?: unknown;
  output?: unknown;
  isError?: boolean;
  error?: string;
};

/**
 * Emit agento11y tool execution spans for a set of completed tool records.
 * Errors thrown by the SDK are swallowed so a span failure cannot break
 * the plugin.
 *
 * @internal Exported for testing.
 */
export function emitToolSpans(
  factory: (start: ToolExecutionStart) => ToolExecutionRecorder,
  records: ToolExecutionRecord[],
  opts: {
    conversationId: string;
    conversationTitle?: string;
    agentName: string;
    agentVersion?: string;
    requestProvider: string;
    requestModel: string;
    contentCapture: ContentCaptureMode;
    redactor: Redactor;
    debugLog: (msg: string, ...args: unknown[]) => void;
  },
): void {
  if (records.length === 0) return;
  const includeContent = opts.contentCapture === "full";

  for (const record of records) {
    try {
      const rec = factory({
        toolName: record.toolName,
        toolCallId: record.toolCallId,
        toolType: "function",
        ...((record.executionKind || record.skillName) && {
          executionKind: record.executionKind ?? "load_skill",
        }),
        ...(record.skillName && { skillName: record.skillName }),
        conversationId: opts.conversationId,
        conversationTitle: opts.conversationTitle,
        agentName: opts.agentName,
        agentVersion: opts.agentVersion,
        requestProvider: opts.requestProvider,
        requestModel: opts.requestModel,
        startedAt: new Date(record.startedAt),
        contentCapture: opts.contentCapture,
      });

      if (record.isError) {
        rec.setCallError(new Error(record.error || "tool returned error"));
      }

      const end: ToolExecutionResult = {
        completedAt: new Date(record.completedAt),
        verifiedSkillName: record.verifiedSkillName,
      };

      if (includeContent && record.input !== undefined) {
        end.arguments = opts.redactor.redact(JSON.stringify(record.input));
      }
      if (includeContent && record.output !== undefined) {
        end.result = opts.redactor.redact(String(record.output));
      }

      rec.setResult(end);
      rec.end();
    } catch (err) {
      opts.debugLog(`tool span export failed for ${record.toolName}`, err);
    }
  }
}
