using System.Text.Json;

namespace Grafana.Agento11y;

public enum GenerationMode
{
    Sync,
    Stream
}

public enum MessageRole
{
    User,
    Assistant,
    Tool
}

public enum PartKind
{
    Text,
    Thinking,
    ToolCall,
    ToolResult
}

public enum ArtifactKind
{
    Request,
    Response,
    Tools,
    ProviderEvent
}

public enum ConversationRatingValue
{
    Good,
    Bad
}

public enum ContentCaptureMode
{
    Default = 0,
    Full = 1,
    NoToolContent = 2,
    MetadataOnly = 3,
    /// <summary>
    /// Splits the proto and span paths for generation content. Proto export
    /// keeps full content; the OTel span omits <c>agento11y.conversation.title</c>,
    /// <c>gen_ai.tool.call.arguments</c>, <c>gen_ai.tool.call.result</c>, and
    /// <c>gen_ai.embeddings.input_texts</c>.
    /// </summary>
    FullWithMetadataSpans = 4,
}

public static class ContentCaptureModeExtensions
{
    private const string ValueFull = "full";
    private const string ValueNoToolContent = "no_tool_content";
    private const string ValueMetadataOnly = "metadata_only";
    private const string ValueFullWithMetadataSpans = "full_with_metadata_spans";

    public static string ToMetadataValue(this ContentCaptureMode mode)
    {
        return mode switch
        {
            ContentCaptureMode.Full => ValueFull,
            ContentCaptureMode.NoToolContent => ValueNoToolContent,
            ContentCaptureMode.MetadataOnly => ValueMetadataOnly,
            ContentCaptureMode.FullWithMetadataSpans => ValueFullWithMetadataSpans,
            _ => string.Empty,
        };
    }
}

public sealed class ModelRef
{
    public string Provider { get; set; } = string.Empty;
    public string Name { get; set; } = string.Empty;
}

public sealed class ToolDefinition
{
    public string Name { get; set; } = string.Empty;
    public string Description { get; set; } = string.Empty;
    public string Type { get; set; } = string.Empty;
    public byte[] InputSchemaJson { get; set; } = [];
}

public sealed class TokenUsage
{
    public long InputTokens { get; set; }
    public long OutputTokens { get; set; }
    public long TotalTokens { get; set; }
    public long CacheReadInputTokens { get; set; }
    public long CacheWriteInputTokens { get; set; }
    public long ReasoningTokens { get; set; }

    public TokenUsage Normalize()
    {
        if (TotalTokens != 0)
        {
            return Clone();
        }

        var clone = Clone();
        clone.TotalTokens = clone.InputTokens + clone.OutputTokens;
        return clone;
    }

    public TokenUsage Clone()
    {
        return new TokenUsage
        {
            InputTokens = InputTokens,
            OutputTokens = OutputTokens,
            TotalTokens = TotalTokens,
            CacheReadInputTokens = CacheReadInputTokens,
            CacheWriteInputTokens = CacheWriteInputTokens,
            ReasoningTokens = ReasoningTokens,
        };
    }
}

public sealed class PartMetadata
{
    public string ProviderType { get; set; } = string.Empty;
}

public sealed class ToolCall
{
    public string Id { get; set; } = string.Empty;
    public string Name { get; set; } = string.Empty;
    public byte[] InputJson { get; set; } = [];
}

public sealed class ToolResult
{
    public string ToolCallId { get; set; } = string.Empty;
    public string Name { get; set; } = string.Empty;
    public string Content { get; set; } = string.Empty;
    public byte[] ContentJson { get; set; } = [];
    public bool IsError { get; set; }
}

public sealed class Part
{
    public PartKind Kind { get; set; }
    public string Text { get; set; } = string.Empty;
    public string Thinking { get; set; } = string.Empty;
    public ToolCall? ToolCall { get; set; }
    public ToolResult? ToolResult { get; set; }
    public PartMetadata Metadata { get; set; } = new();

    public static Part TextPart(string text)
    {
        return new Part { Kind = PartKind.Text, Text = text };
    }

    public static Part ThinkingPart(string thinking)
    {
        return new Part { Kind = PartKind.Thinking, Thinking = thinking };
    }

    public static Part ToolCallPart(ToolCall toolCall)
    {
        return new Part { Kind = PartKind.ToolCall, ToolCall = toolCall };
    }

    public static Part ToolResultPart(ToolResult toolResult)
    {
        return new Part { Kind = PartKind.ToolResult, ToolResult = toolResult };
    }
}

public sealed class Message
{
    public MessageRole Role { get; set; }
    public string Name { get; set; } = string.Empty;
    public List<Part> Parts { get; set; } = [];

    public static Message UserTextMessage(string text)
    {
        return new Message { Role = MessageRole.User, Parts = [Part.TextPart(text)] };
    }

    public static Message AssistantTextMessage(string text)
    {
        return new Message { Role = MessageRole.Assistant, Parts = [Part.TextPart(text)] };
    }

    public static Message ToolResultMessage(string toolCallId, object? content)
    {
        byte[] payload = [];
        if (content != null)
        {
            payload = JsonSerializer.SerializeToUtf8Bytes(content);
        }

        return new Message
        {
            Role = MessageRole.Tool,
            Parts =
            [
                Part.ToolResultPart(new ToolResult
                {
                    ToolCallId = toolCallId,
                    ContentJson = payload,
                }),
            ],
        };
    }
}

public sealed class Artifact
{
    public ArtifactKind Kind { get; set; }
    public string Name { get; set; } = string.Empty;
    public string ContentType { get; set; } = string.Empty;
    public byte[] Payload { get; set; } = [];
    public string RecordId { get; set; } = string.Empty;
    public string Uri { get; set; } = string.Empty;

    public static Artifact JsonArtifact(ArtifactKind kind, string name, object value)
    {
        return new Artifact
        {
            Kind = kind,
            Name = name,
            ContentType = "application/json",
            Payload = JsonSerializer.SerializeToUtf8Bytes(value),
        };
    }
}

public sealed class GenerationStart
{
    public string Id { get; set; } = string.Empty;
    public string ConversationId { get; set; } = string.Empty;
    public string ConversationTitle { get; set; } = string.Empty;
    public string UserId { get; set; } = string.Empty;
    public string AgentName { get; set; } = string.Empty;
    public string AgentVersion { get; set; } = string.Empty;
    public GenerationMode? Mode { get; set; }
    public string OperationName { get; set; } = string.Empty;
    public ModelRef Model { get; set; } = new();
    public string SystemPrompt { get; set; } = string.Empty;
    public long? MaxTokens { get; set; }
    public double? Temperature { get; set; }
    public double? TopP { get; set; }
    public string? ToolChoice { get; set; }
    public bool? ThinkingEnabled { get; set; }
    public List<string> ParentGenerationIds { get; set; } = [];
    public string EffectiveVersion { get; set; } = string.Empty;
    public List<ToolDefinition> Tools { get; set; } = [];
    public Dictionary<string, string> Tags { get; set; } = new(StringComparer.Ordinal);
    public Dictionary<string, object?> Metadata { get; set; } = new(StringComparer.Ordinal);
    public DateTimeOffset? StartedAt { get; set; }
    public ContentCaptureMode ContentCapture { get; set; } = ContentCaptureMode.Default;
}

public sealed class EmbeddingStart
{
    public ModelRef Model { get; set; } = new();
    public string AgentName { get; set; } = string.Empty;
    public string AgentVersion { get; set; } = string.Empty;
    public long? Dimensions { get; set; }
    public string EncodingFormat { get; set; } = string.Empty;
    public Dictionary<string, string> Tags { get; set; } = new(StringComparer.Ordinal);
    public Dictionary<string, object?> Metadata { get; set; } = new(StringComparer.Ordinal);
    public DateTimeOffset? StartedAt { get; set; }
}

public sealed class EmbeddingResult
{
    public int InputCount { get; set; }
    public long InputTokens { get; set; }
    public List<string> InputTexts { get; set; } = [];
    public string ResponseModel { get; set; } = string.Empty;
    public long? Dimensions { get; set; }
}

public sealed class Generation
{
    public string Id { get; set; } = string.Empty;
    public string ConversationId { get; set; } = string.Empty;
    public string ConversationTitle { get; set; } = string.Empty;
    public string UserId { get; set; } = string.Empty;
    public string AgentName { get; set; } = string.Empty;
    public string AgentVersion { get; set; } = string.Empty;
    public GenerationMode? Mode { get; set; }
    public string OperationName { get; set; } = string.Empty;
    public string TraceId { get; set; } = string.Empty;
    public string SpanId { get; set; } = string.Empty;
    public ModelRef Model { get; set; } = new();
    public string ResponseId { get; set; } = string.Empty;
    public string ResponseModel { get; set; } = string.Empty;
    public string SystemPrompt { get; set; } = string.Empty;
    public long? MaxTokens { get; set; }
    public double? Temperature { get; set; }
    public double? TopP { get; set; }
    public string? ToolChoice { get; set; }
    public bool? ThinkingEnabled { get; set; }
    public List<string> ParentGenerationIds { get; set; } = [];
    /// <summary>See <see cref="GenerationStart.EffectiveVersion"/>.</summary>
    public string EffectiveVersion { get; set; } = string.Empty;
    public List<Message> Input { get; set; } = [];
    public List<Message> Output { get; set; } = [];
    public List<ToolDefinition> Tools { get; set; } = [];
    public TokenUsage Usage { get; set; } = new();
    public string StopReason { get; set; } = string.Empty;
    public DateTimeOffset? StartedAt { get; set; }
    public DateTimeOffset? CompletedAt { get; set; }
    public Dictionary<string, string> Tags { get; set; } = new(StringComparer.Ordinal);
    public Dictionary<string, object?> Metadata { get; set; } = new(StringComparer.Ordinal);
    public List<Artifact> Artifacts { get; set; } = [];
    public string CallError { get; set; } = string.Empty;
}

public sealed class ToolExecutionStart
{
    public string ToolName { get; set; } = string.Empty;
    public string ToolCallId { get; set; } = string.Empty;
    public string ToolType { get; set; } = string.Empty;
    public string ToolDescription { get; set; } = string.Empty;
    public string ConversationId { get; set; } = string.Empty;
    public string ConversationTitle { get; set; } = string.Empty;
    public string AgentName { get; set; } = string.Empty;
    public string AgentVersion { get; set; } = string.Empty;
    /// <summary>The model that requested the tool call (e.g. "gpt-5").</summary>
    public string RequestModel { get; set; } = string.Empty;
    /// <summary>The provider that served the model (e.g. "openai").</summary>
    public string RequestProvider { get; set; } = string.Empty;
    /// <summary>Deprecated: Use ContentCapture instead.</summary>
    [Obsolete("Use ContentCapture instead. ContentCapture takes precedence when set to Full or MetadataOnly; with NoToolContent (or when unset) IncludeContent still controls whether tool content is captured.")]
    public bool IncludeContent { get; set; }
    public ContentCaptureMode ContentCapture { get; set; } = ContentCaptureMode.Default;
    public DateTimeOffset? StartedAt { get; set; }
}

public sealed class ToolExecutionEnd
{
    public object? Arguments { get; set; }
    public object? Result { get; set; }
    public DateTimeOffset? CompletedAt { get; set; }
}

/// <summary>Options for <see cref="Agento11yClient.ExecuteToolCalls"/>.</summary>
public sealed class ExecuteToolCallsOptions
{
    public string ConversationId { get; set; } = string.Empty;
    public string ConversationTitle { get; set; } = string.Empty;
    public string AgentName { get; set; } = string.Empty;
    public string AgentVersion { get; set; } = string.Empty;
    public ContentCaptureMode ContentCapture { get; set; } = ContentCaptureMode.Default;
    public string RequestModel { get; set; } = string.Empty;
    public string RequestProvider { get; set; } = string.Empty;
    public string ToolType { get; set; } = "function";

    /// <summary>Reserved for forward compatibility; not applied to tool spans in this release.</summary>
    public Dictionary<string, string> Tags { get; set; } = new(StringComparer.Ordinal);
}

public sealed class ExportGenerationResult
{
    public string GenerationId { get; set; } = string.Empty;
    public bool Accepted { get; set; }
    public string Error { get; set; } = string.Empty;
}

public sealed class ExportGenerationsRequest
{
    public List<Generation> Generations { get; set; } = [];
}

public sealed class ExportGenerationsResponse
{
    public List<ExportGenerationResult> Results { get; set; } = [];
}

public sealed class SubmitConversationRatingRequest
{
    public string RatingId { get; set; } = string.Empty;
    public ConversationRatingValue Rating { get; set; } = ConversationRatingValue.Good;
    public string Comment { get; set; } = string.Empty;
    public Dictionary<string, object?> Metadata { get; set; } = new(StringComparer.Ordinal);
    public string GenerationId { get; set; } = string.Empty;
    public string RaterId { get; set; } = string.Empty;
    public string Source { get; set; } = string.Empty;
}

public sealed class ConversationRating
{
    public string RatingId { get; set; } = string.Empty;
    public string ConversationId { get; set; } = string.Empty;
    public ConversationRatingValue Rating { get; set; } = ConversationRatingValue.Good;
    public string Comment { get; set; } = string.Empty;
    public Dictionary<string, object?> Metadata { get; set; } = new(StringComparer.Ordinal);
    public string GenerationId { get; set; } = string.Empty;
    public string RaterId { get; set; } = string.Empty;
    public string Source { get; set; } = string.Empty;
    public DateTimeOffset CreatedAt { get; set; }
}

public sealed class ConversationRatingSummary
{
    public int TotalCount { get; set; }
    public int GoodCount { get; set; }
    public int BadCount { get; set; }
    public ConversationRatingValue? LatestRating { get; set; }
    public DateTimeOffset LatestRatedAt { get; set; }
    public DateTimeOffset? LatestBadAt { get; set; }
    public bool HasBadRating { get; set; }
}

public sealed class SubmitConversationRatingResponse
{
    public ConversationRating Rating { get; set; } = new();
    public ConversationRatingSummary Summary { get; set; } = new();
}
