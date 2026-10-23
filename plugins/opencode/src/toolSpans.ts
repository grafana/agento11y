import type {
  ContentCaptureMode,
  ToolExecutionRecorder,
  ToolExecutionStart,
} from "@grafana/agento11y";
import type { Redactor } from "./mappers.js";

export type ToolExecutionRecord = {
  sessionID: string;
  toolName: string;
  toolCallId: string;
  // undefined: unobserved; null: observed nonmatch; string: recognized skill.
  skillName?: string | null;
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
        ...(record.skillName && {
          executionKind: "load_skill",
          skillName: record.skillName,
        }),
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

      const end: {
        arguments?: unknown;
        result?: unknown;
        completedAt: Date;
      } = {
        completedAt: new Date(record.completedAt),
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
