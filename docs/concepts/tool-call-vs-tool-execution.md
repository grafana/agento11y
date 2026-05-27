# Tool Calls vs Tool Executions

Sigil distinguishes between two related but distinct concepts when instrumenting tool usage in AI agents. Understanding this distinction helps you choose the right instrumentation approach.

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

In the Sigil UI:
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

In the Sigil UI:
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

This allows Sigil to correlate the model's request with your execution timing.

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

See [Content Capture Modes](./content-capture-modes.md) for the full mode matrix, defaults, and resolution precedence.

## Summary

- **Tool Call**: Model's intent, part of generation messages, shows input/output in conversation view
- **Tool Execution**: Your code's runtime, separate span, shows in Tools tab with timing metrics

For most agent instrumentation, you'll use both: provider wrappers capture tool calls automatically, and you add tool executions around your handler code that processes those calls.
