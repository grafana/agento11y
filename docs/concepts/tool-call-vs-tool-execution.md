# Tool Calls vs Tool Executions

Agent Observability distinguishes between two related but distinct concepts when instrumenting tool usage in AI agents. Understanding this distinction helps you choose the right instrumentation approach.

## Overview

| Concept | What It Represents | Where It Appears | SDK Method |
|---------|-------------------|------------------|------------|
| **Tool Call** | The LLM's request to invoke a tool | Part of generation output messages | `MessagePart.toolCall(...)` |
| **Tool Execution** | Your code actually running the tool | Separate OTel span | `startToolExecution(...)` |

## Tool Call (Message Part)

A **tool call** is what the LLM outputs when it decides it wants to use a tool. It's part of the generation's message content—the model's **request** to invoke a tool.

```java
// Tool call appears in the generation output
Message output = new Message()
    .setRole(MessageRole.ASSISTANT)
    .setParts(List.of(
        MessagePart.toolCall(new ToolCall()
            .setId("call_abc123")
            .setName("weather")
            .setInputJson("{\"city\":\"Paris\"}".getBytes()))
    ));
```

In the Agent Observability UI:
- Shows as **"Call weather"** in the conversation view
- Input/output content is visible because it's stored in the generation payload
- **Not** listed in the Tools tab (it's embedded in the generation)

## Tool Execution (Span)

A **tool execution** is when your application code actually runs the tool. It creates a separate OTel span that tracks timing, errors, and optionally captures arguments/results.

```java
// Tool execution is your code running
try (ToolExecutionRecorder rec = client.startToolExecution(
        new ToolExecutionStart()
            .setToolName("weather")
            .setToolCallId("call_abc123"))) {
    
    String result = weatherService.getWeather("Paris");
    rec.setResult(result);
}
```

In the Agent Observability UI:
- Shows as **"Tool execute_tool weather"** in the flow view
- Listed in the Tools tab with timing metrics
- Attributes visible in span details

## When to Use Each

### Use Tool Calls When...

You're recording what the LLM requested as part of its generation output. Provider wrappers (OpenAI, Anthropic, etc.) handle this automatically when the model returns tool_use/function_call in its response.

### Use Tool Executions When...

You're instrumenting **your code** that runs in response to a tool call, or any tool-like operation that isn't an LLM-requested tool. Examples:

- **Agent tool handlers** — wrap your tool implementation code
- **RAG retrieval steps** — document lookups before generating a response
- **External service calls** — database queries, API calls made by the agent
- **Custom agent actions** — any function your agent framework executes

```java
// RAG retrieval example: your code, not an LLM-requested tool
try (ToolExecutionRecorder rec = client.startToolExecution(
        new ToolExecutionStart()
            .setToolName("document_retriever")
            .setToolDescription("Retrieves relevant documents for context"))) {
    
    List<Document> docs = vectorStore.similaritySearch(query, 5);
    rec.setResult(Map.of(
        "count", docs.size(),
        "source", "knowledge_base"
    ));
}
```

## Linking Tool Calls to Executions

When your tool execution corresponds to a specific LLM tool call, set `toolCallId` to correlate them:

```java
// LLM outputs a tool call with id "call_abc123"
// Your code executes it:
try (ToolExecutionRecorder rec = client.startToolExecution(
        new ToolExecutionStart()
            .setToolName("weather")
            .setToolCallId("call_abc123"))) {  // Links to the tool call
    // ...
}
```

This allows Agent Observability to correlate the model's request with your execution timing.

## Tracing skill loads, resource reads, and commands

The SDKs and Go `otelgenai` support the OpenTelemetry conventions for loading skills, reading skill resources, and running commands.
These conventions come from [GenAI #498](https://github.com/open-telemetry/semantic-conventions-genai/pull/498) and have Development status.
The global schema URL remains on the legacy schema because other signals still use it.
Each execution still creates one `INTERNAL` span with `gen_ai.operation.name=execute_tool`, without new metric labels.

Callers select an execution kind explicitly. The execution kind controls span naming and standard `gen_ai.skill.*` and `process.*` attributes.
The SDK does not emit the execution kind as an attribute.
`ToolType` still describes categories such as `function` or `extension`.
Unset or unknown execution kinds keep generic behavior and omit skill and process attributes, even when callers supply those fields.
The SDKs do not detect skills or infer execution kinds from tool names, arguments, paths, or results.
Generic convenience loops (`execute_tool_calls` / `executeToolCalls` and the .NET tool loop) remain generic and infer no exit codes.

| Execution kind | Use | Suffix after `execute_tool {tool}` |
| --- | --- | --- |
| `load_skill` | A dedicated tool loads skill instructions. | Skill name, if available. |
| `read_skill_resource` | A dedicated tool reads a skill resource. | Skill and resource names; resource alone if no skill; otherwise no suffix. |
| `command` | A tool executes commands, with or without a skill. | Skill and resource names; resource alone; executable name; otherwise no suffix, in that order. |

Resource reads and commands never append a skill name alone. Resource naming takes precedence over executable naming.
Names are trimmed; blank values are omitted. The SDKs do not resolve names or paths through filesystem or network lookups.

### API fields and applicability

Set `ExecutionKind` in Go and .NET, `execution_kind` in Python, `executionKind` in JS/TS, or `setExecutionKind(...)` in Java.
Go constants are `ToolExecutionKindLoadSkill`, `ToolExecutionKindReadSkillResource`, and `ToolExecutionKindCommand`.
Python exports `ToolExecutionKind.LOAD_SKILL`, `.READ_SKILL_RESOURCE`, and `.COMMAND`; JS/TS uses the string values above.
Go `otelgenai.Invocation` uses `ExecutionKind` and its own `ToolExecutionKind` constants.

| Go field | Python field | JS/TS field | Span attribute | Applies to |
| --- | --- | --- | --- | --- |
| `SkillName` | `skill_name` | `skillName` | `gen_ai.skill.name` | All three execution kinds |
| `SkillDescription` | `skill_description` | `skillDescription` | `gen_ai.skill.description` | All three execution kinds |
| `SkillSourceURI` | `skill_source_uri` | `skillSourceURI` | `gen_ai.skill.source.uri` | All three execution kinds |
| `SkillResourceName` | `skill_resource_name` | `skillResourceName` | `gen_ai.skill.resource.name` | Resource reads and commands |
| `ProcessExecutableName` | `process_executable_name` | `processExecutableName` | `process.executable.name` | Commands only |
| `ProcessExecutablePath` | `process_executable_path` | `processExecutablePath` | `process.executable.path` | Commands only |
| `ProcessExitCode` (completion) | `process_exit_code` (completion) | `processExitCode` (completion) | `process.exit.code` | Commands only |

`SkillDescription` describes the skill, not the tool's existing `ToolDescription`.
Only populate process fields for an actual, directly executed single process.
Do not guess an executable or aggregate an exit code across a pipeline or several commands.
Report the exit code at completion; zero means a reported success, while absent means unknown.
The nullable types are Go `*int64`, Python `int | None` (not `bool`), Java `Long`, and .NET `long?`.
JS/TS uses an optional safe integer. The SDKs do not map exit codes to errors, span status, or `error.type`.
Keep reporting execution errors through the existing recorder error API.
See the [compilable Go command example](../../go/agento11y/example_test.go).

Applicable structural tool, skill, resource, and executable names reach the sampler at span creation.
Exit codes arrive only at completion. Skill descriptions, source URIs, and executable paths never enter sampler inputs.
In Go `otelgenai`, end hooks cannot change the execution kind or applicable identity fields published at start.
Fields that were unknown at start can be supplied later.
Explicit raw attributes and hook-returned attributes retain override precedence, so exported attributes can differ from sampler inputs.

Skill descriptions, source URIs, and executable paths follow the [capture-mode privacy rules](./content-capture-modes.md#skill-and-process-attribute-privacy).
SDKs emit `gen_ai.skill.name`, not the legacy `agento11y.skill.name`.
A skill-name attribute alone does not identify a skill load: resource reads and commands use it too.

## Capturing Arguments and Results

By default, tool execution spans capture timing but not content. The core SDK client default is `NO_TOOL_CONTENT`, which keeps generation content but excludes tool arguments and results from spans. To include them, raise the mode to `FULL` for the tool execution:

```java
try (ToolExecutionRecorder rec = client.startToolExecution(
        new ToolExecutionStart()
            .setToolName("document_retriever")
            .setContentCapture(ContentCaptureMode.FULL))) {
    
    rec.setArguments(Map.of("query", searchQuery, "limit", 5));
    List<Document> docs = vectorStore.similaritySearch(searchQuery, 5);
    rec.setResult(Map.of("count", docs.size(), "ids", docs.stream().map(Document::getId).toList()));
}
```

With content capture enabled, arguments appear as `gen_ai.tool.call.arguments` and results as `gen_ai.tool.call.result` in span attributes.
The legacy `IncludeContent` / `include_content` / `includeContent` opt-in under `no_tool_content` remains unchanged.
It does not bypass reduced effective modes.

See [Content Capture Modes](./content-capture-modes.md) for the full mode matrix, defaults, and resolution precedence.

## Summary

- **Tool Call**: Model's intent, part of generation messages, shows input/output in conversation view
- **Tool Execution**: Your code's runtime, separate span, shows in Tools tab with timing metrics

For most agent instrumentation, you'll use both: provider wrappers capture tool calls automatically, and you add tool executions around your handler code that processes those calls.
