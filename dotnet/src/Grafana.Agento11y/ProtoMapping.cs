using Google.Protobuf;
using Google.Protobuf.WellKnownTypes;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using Proto = Sigil.V1;

namespace Grafana.Sigil;

internal static class ProtoMapping
{
    public static Proto.Generation ToProto(Generation model)
    {
        var proto = new Proto.Generation
        {
            Id = model.Id,
            ConversationId = model.ConversationId,
            AgentName = model.AgentName,
            AgentVersion = model.AgentVersion,
            OperationName = model.OperationName,
            Mode = MapMode(model.Mode),
            TraceId = model.TraceId,
            SpanId = model.SpanId,
            Model = new Proto.ModelRef
            {
                Provider = model.Model.Provider,
                Name = model.Model.Name,
            },
            ResponseId = model.ResponseId,
            ResponseModel = model.ResponseModel,
            SystemPrompt = model.SystemPrompt,
            Usage = new Proto.TokenUsage
            {
                InputTokens = model.Usage.InputTokens,
                OutputTokens = model.Usage.OutputTokens,
                TotalTokens = model.Usage.TotalTokens,
                CacheReadInputTokens = model.Usage.CacheReadInputTokens,
                CacheWriteInputTokens = model.Usage.CacheWriteInputTokens,
                ReasoningTokens = model.Usage.ReasoningTokens,
            },
            StopReason = model.StopReason,
            CallError = model.CallError,
        };

        if (model.StartedAt.HasValue)
        {
            proto.StartedAt = Timestamp.FromDateTimeOffset(model.StartedAt.Value);
        }

        if (model.CompletedAt.HasValue)
        {
            proto.CompletedAt = Timestamp.FromDateTimeOffset(model.CompletedAt.Value);
        }

        if (model.MaxTokens.HasValue)
        {
            proto.MaxTokens = model.MaxTokens.Value;
        }

        if (model.Temperature.HasValue)
        {
            proto.Temperature = model.Temperature.Value;
        }

        if (model.TopP.HasValue)
        {
            proto.TopP = model.TopP.Value;
        }

        if (!string.IsNullOrWhiteSpace(model.ToolChoice))
        {
            proto.ToolChoice = model.ToolChoice;
        }

        if (model.ThinkingEnabled.HasValue)
        {
            proto.ThinkingEnabled = model.ThinkingEnabled.Value;
        }
        proto.ParentGenerationIds.AddRange(model.ParentGenerationIds);

        var trimmedEffectiveVersion = model.EffectiveVersion.Trim();
        if (trimmedEffectiveVersion.Length > 0)
        {
            proto.EffectiveVersion = CanonicalEffectiveVersion(trimmedEffectiveVersion);
        }

        foreach (var tag in model.Tags)
        {
            proto.Tags[tag.Key] = tag.Value;
        }

        if (model.Metadata.Count > 0)
        {
            var metadataJson = JsonSerializer.Serialize(model.Metadata);
            proto.Metadata = Struct.Parser.ParseJson(metadataJson);
        }

        proto.Input.AddRange(model.Input.Select(MapMessage));
        proto.Output.AddRange(model.Output.Select(MapMessage));
        proto.Tools.AddRange(model.Tools.Select(MapTool));
        proto.RawArtifacts.AddRange(model.Artifacts.Select(MapArtifact));

        return proto;
    }

    private static string CanonicalEffectiveVersion(string value)
    {
        using var sha = SHA256.Create();
        var bytes = sha.ComputeHash(Encoding.UTF8.GetBytes(value));
        return "sha256:" + BitConverter.ToString(bytes).Replace("-", "").ToLowerInvariant();
    }

    private static Proto.GenerationMode MapMode(GenerationMode? mode)
    {
        return mode switch
        {
            GenerationMode.Sync => Proto.GenerationMode.Sync,
            GenerationMode.Stream => Proto.GenerationMode.Stream,
            _ => Proto.GenerationMode.Unspecified,
        };
    }

    private static Proto.Message MapMessage(Message message)
    {
        var proto = new Proto.Message
        {
            Role = message.Role switch
            {
                MessageRole.User => Proto.MessageRole.User,
                MessageRole.Assistant => Proto.MessageRole.Assistant,
                MessageRole.Tool => Proto.MessageRole.Tool,
                _ => Proto.MessageRole.Unspecified,
            },
            Name = message.Name,
        };

        proto.Parts.AddRange(message.Parts.Select(MapPart));
        return proto;
    }

    private static Proto.Part MapPart(Part part)
    {
        var proto = new Proto.Part();
        if (!string.IsNullOrWhiteSpace(part.Metadata.ProviderType))
        {
            proto.Metadata = new Proto.PartMetadata
            {
                ProviderType = part.Metadata.ProviderType,
            };
        }

        switch (part.Kind)
        {
            case PartKind.Text:
                proto.Text = part.Text;
                break;
            case PartKind.Thinking:
                proto.Thinking = part.Thinking;
                break;
            case PartKind.ToolCall:
                if (part.ToolCall != null)
                {
                    proto.ToolCall = new Proto.ToolCall
                    {
                        Id = part.ToolCall.Id,
                        Name = part.ToolCall.Name,
                        InputJson = ByteString.CopyFrom(part.ToolCall.InputJson ?? []),
                    };
                }
                break;
            case PartKind.ToolResult:
                if (part.ToolResult != null)
                {
                    proto.ToolResult = new Proto.ToolResult
                    {
                        ToolCallId = part.ToolResult.ToolCallId,
                        Name = part.ToolResult.Name,
                        Content = part.ToolResult.Content,
                        ContentJson = ByteString.CopyFrom(part.ToolResult.ContentJson ?? []),
                        IsError = part.ToolResult.IsError,
                    };
                }
                break;
        }

        return proto;
    }

    private static Proto.ToolDefinition MapTool(ToolDefinition definition)
    {
        return new Proto.ToolDefinition
        {
            Name = definition.Name,
            Description = definition.Description,
            Type = definition.Type,
            InputSchemaJson = ByteString.CopyFrom(definition.InputSchemaJson ?? []),
        };
    }

    private static Proto.Artifact MapArtifact(Artifact artifact)
    {
        return new Proto.Artifact
        {
            Kind = artifact.Kind switch
            {
                ArtifactKind.Request => Proto.ArtifactKind.Request,
                ArtifactKind.Response => Proto.ArtifactKind.Response,
                ArtifactKind.Tools => Proto.ArtifactKind.Tools,
                ArtifactKind.ProviderEvent => Proto.ArtifactKind.ProviderEvent,
                _ => Proto.ArtifactKind.Unspecified,
            },
            Name = artifact.Name,
            ContentType = artifact.ContentType,
            Payload = ByteString.CopyFrom(artifact.Payload ?? []),
            RecordId = artifact.RecordId,
            Uri = artifact.Uri,
        };
    }
}
