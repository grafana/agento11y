using Google.GenAI.Types;
using System.Text.Json;
using SigilPart = Grafana.Sigil.Part;

namespace Grafana.Sigil.Gemini;

public static class GeminiGenerationMapper
{
    private const string ThinkingBudgetMetadataKey = "sigil.gen_ai.request.thinking.budget_tokens";
    private const string ThinkingLevelMetadataKey = "sigil.gen_ai.request.thinking.level";
    private const string ToolUsePromptTokensMetadataKey = "sigil.gen_ai.usage.tool_use_prompt_tokens";

    public static Generation FromRequestResponse(
        string model,
        IReadOnlyList<Content>? contents,
        GenerateContentConfig? config,
        GenerateContentResponse response,
        GeminiSigilOptions? options = null
    )
    {
        if (string.IsNullOrWhiteSpace(model))
        {
            throw new ArgumentException("model is required", nameof(model));
        }

        ArgumentNullException.ThrowIfNull(response);

        var request = BuildRequest(model, contents, config);
        return FromRequestResponseInternal(request, response, options);
    }

    public static Generation FromStream(
        string model,
        IReadOnlyList<Content>? contents,
        GenerateContentConfig? config,
        GeminiStreamSummary summary,
        GeminiSigilOptions? options = null
    )
    {
        if (string.IsNullOrWhiteSpace(model))
        {
            throw new ArgumentException("model is required", nameof(model));
        }

        ArgumentNullException.ThrowIfNull(summary);

        if (summary.Responses.Count == 0)
        {
            throw new ArgumentException("stream summary must contain at least one response", nameof(summary));
        }

        var request = BuildRequest(model, contents, config);
        return FromStreamInternal(request, summary, options);
    }

    public static EmbeddingResult EmbeddingFromResponse(
        string model,
        IReadOnlyList<Content>? contents,
        EmbedContentConfig? config,
        EmbedContentResponse? response
    )
    {
        _ = model;

        var result = new EmbeddingResult
        {
            InputCount = EmbeddingInputCount(contents),
            InputTexts = EmbeddingInputTexts(contents),
        };

        var requestedDimensions = config?.OutputDimensionality;
        if (response == null)
        {
            if (requestedDimensions.HasValue && requestedDimensions.Value > 0)
            {
                result.Dimensions = requestedDimensions.Value;
            }

            return result;
        }

        long inputTokens = 0;
        foreach (var embedding in response.Embeddings ?? [])
        {
            if (embedding == null)
            {
                continue;
            }

            var tokenCount = embedding.Statistics?.TokenCount;
            if (tokenCount.HasValue && tokenCount.Value > 0)
            {
                inputTokens += Convert.ToInt64(tokenCount.Value);
            }

            if (!result.Dimensions.HasValue && embedding.Values != null && embedding.Values.Count > 0)
            {
                result.Dimensions = embedding.Values.Count;
            }
        }

        result.InputTokens = inputTokens;
        if (!result.Dimensions.HasValue && requestedDimensions.HasValue && requestedDimensions.Value > 0)
        {
            result.Dimensions = requestedDimensions.Value;
        }

        return result;
    }

    private static Generation FromRequestResponseInternal(
        GenerateContentRequest request,
        GenerateContentResponse response,
        GeminiSigilOptions? options = null
    )
    {
        var effective = options ?? new GeminiSigilOptions();
        var modelName = ResolveModelName(request, effective);

        var output = MapCandidates(response.Candidates);
        var stopReason = ResolveStopReason(response.Candidates, preferLast: false);
        var usage = MapUsage(response.UsageMetadata);
        var tools = MapTools(request.Config);
        var maxTokens = ReadNullableLongProperty(request.Config, "MaxOutputTokens");
        var temperature = ReadNullableDoubleProperty(request.Config, "Temperature");
        var topP = ReadNullableDoubleProperty(request.Config, "TopP");
        var toolChoice = CanonicalToolChoice(ReadNestedProperty(request.Config, "ToolConfig", "FunctionCallingConfig", "Mode"));
        var thinkingEnabled = ReadNestedBool(request.Config, "ThinkingConfig", "IncludeThoughts");
        var thinkingBudget = ReadNullableLongProperty(request.Config, "ThinkingConfig", "ThinkingBudget");
        var thinkingLevel = ExtractThinkingLevel(request.Config);

        var metadata = ThinkingMetadata(effective.Metadata, thinkingBudget, thinkingLevel);
        metadata = MergeGeminiUsageMetadata(metadata, response.UsageMetadata);
        if (!string.IsNullOrWhiteSpace(response.ModelVersion))
        {
            metadata["model_version"] = response.ModelVersion;
        }

        var generation = new Generation
        {
            ConversationId = effective.ConversationId,
            AgentName = effective.AgentName,
            AgentVersion = effective.AgentVersion,
            Model = new ModelRef
            {
                Provider = effective.ProviderName,
                Name = modelName,
            },
            ResponseId = response.ResponseId ?? string.Empty,
            ResponseModel = response.ModelVersion ?? string.Empty,
            SystemPrompt = ExtractSystemPrompt(request.Config),
            MaxTokens = maxTokens,
            Temperature = temperature,
            TopP = topP,
            ToolChoice = toolChoice,
            ThinkingEnabled = thinkingEnabled,
            Input = MapContents(request.Contents),
            Output = output,
            Tools = tools,
            Usage = usage,
            StopReason = stopReason,
            Tags = new Dictionary<string, string>(effective.Tags, StringComparer.Ordinal),
            Metadata = metadata,
            Artifacts = BuildRequestResponseArtifacts(effective, request, response, tools),
            Mode = GenerationMode.Sync,
        };

        GenerationValidator.Validate(generation);
        return generation;
    }

    private static Generation FromStreamInternal(
        GenerateContentRequest request,
        GeminiStreamSummary summary,
        GeminiSigilOptions? options = null
    )
    {
        var effective = options ?? new GeminiSigilOptions();
        var modelName = ResolveModelName(request, effective);
        var tools = MapTools(request.Config);
        var maxTokens = ReadNullableLongProperty(request.Config, "MaxOutputTokens");
        var temperature = ReadNullableDoubleProperty(request.Config, "Temperature");
        var topP = ReadNullableDoubleProperty(request.Config, "TopP");
        var toolChoice = CanonicalToolChoice(ReadNestedProperty(request.Config, "ToolConfig", "FunctionCallingConfig", "Mode"));
        var thinkingEnabled = ReadNestedBool(request.Config, "ThinkingConfig", "IncludeThoughts");
        var thinkingBudget = ReadNullableLongProperty(request.Config, "ThinkingConfig", "ThinkingBudget");
        var thinkingLevel = ExtractThinkingLevel(request.Config);

        var output = new List<Message>();
        var responseId = string.Empty;
        var responseModel = string.Empty;
        var stopReason = string.Empty;
        var usage = new TokenUsage();
        GenerateContentResponseUsageMetadata? usageMetadata = null;

        foreach (var response in summary.Responses)
        {
            if (response == null)
            {
                continue;
            }

            if (!string.IsNullOrWhiteSpace(response.ResponseId))
            {
                responseId = response.ResponseId;
            }

            if (!string.IsNullOrWhiteSpace(response.ModelVersion))
            {
                responseModel = response.ModelVersion;
            }

            if (response.UsageMetadata != null)
            {
                usage = MapUsage(response.UsageMetadata);
                usageMetadata = response.UsageMetadata;
            }

            var responseStopReason = ResolveStopReason(response.Candidates, preferLast: true);
            if (!string.IsNullOrWhiteSpace(responseStopReason))
            {
                stopReason = responseStopReason;
            }

            output.AddRange(MapCandidates(response.Candidates));
        }

        var metadata = ThinkingMetadata(effective.Metadata, thinkingBudget, thinkingLevel);
        metadata = MergeGeminiUsageMetadata(metadata, usageMetadata);
        if (!string.IsNullOrWhiteSpace(responseModel))
        {
            metadata["model_version"] = responseModel;
        }

        var generation = new Generation
        {
            ConversationId = effective.ConversationId,
            AgentName = effective.AgentName,
            AgentVersion = effective.AgentVersion,
            Model = new ModelRef
            {
                Provider = effective.ProviderName,
                Name = modelName,
            },
            ResponseId = responseId,
            ResponseModel = responseModel,
            SystemPrompt = ExtractSystemPrompt(request.Config),
            MaxTokens = maxTokens,
            Temperature = temperature,
            TopP = topP,
            ToolChoice = toolChoice,
            ThinkingEnabled = thinkingEnabled,
            Input = MapContents(request.Contents),
            Output = output,
            Tools = tools,
            Usage = usage,
            StopReason = stopReason,
            Tags = new Dictionary<string, string>(effective.Tags, StringComparer.Ordinal),
            Metadata = metadata,
            Artifacts = BuildStreamArtifacts(effective, request, summary, tools),
            Mode = GenerationMode.Stream,
        };

        GenerationValidator.Validate(generation);
        return generation;
    }

    private static GenerateContentRequest BuildRequest(
        string model,
        IReadOnlyList<Content>? contents,
        GenerateContentConfig? config
    )
    {
        var mappedContents = new List<Content>();
        if (contents != null)
        {
            foreach (var content in contents)
            {
                if (content != null)
                {
                    mappedContents.Add(content);
                }
            }
        }

        return new GenerateContentRequest
        {
            Model = model,
            Contents = mappedContents,
            Config = config,
        };
    }

    private static int EmbeddingInputCount(IReadOnlyList<Content>? contents)
    {
        if (contents == null || contents.Count == 0)
        {
            return 0;
        }

        var count = 0;
        foreach (var content in contents)
        {
            if (content != null)
            {
                count++;
            }
        }

        return count;
    }

    private static List<string> EmbeddingInputTexts(IReadOnlyList<Content>? contents)
    {
        if (contents == null || contents.Count == 0)
        {
            return [];
        }

        var inputTexts = new List<string>(contents.Count);
        foreach (var content in contents)
        {
            var text = EmbeddingContentText(content);
            if (!string.IsNullOrWhiteSpace(text))
            {
                inputTexts.Add(text);
            }
        }

        return inputTexts;
    }

    private static string EmbeddingContentText(Content? content)
    {
        if (content?.Parts == null || content.Parts.Count == 0)
        {
            return string.Empty;
        }

        var chunks = new List<string>(content.Parts.Count);
        foreach (var part in content.Parts)
        {
            if (part != null && !string.IsNullOrWhiteSpace(part.Text))
            {
                chunks.Add(part.Text.Trim());
            }
        }

        return string.Join("\n", chunks);
    }

    private static List<Message> MapContents(List<Content>? contents)
    {
        if (contents == null || contents.Count == 0)
        {
            return [];
        }

        var mapped = new List<Message>(contents.Count + 2);
        foreach (var content in contents)
        {
            if (content == null)
            {
                continue;
            }

            var role = ParseRole(content.Role);
            var roleParts = new List<SigilPart>();
            var assistantParts = new List<SigilPart>();
            var toolParts = new List<SigilPart>();

            foreach (var part in content.Parts ?? [])
            {
                if (part == null)
                {
                    continue;
                }

                if (!string.IsNullOrWhiteSpace(part.Text))
                {
                    if (part.Thought == true && role == MessageRole.Assistant)
                    {
                        roleParts.Add(SigilPart.ThinkingPart(part.Text));
                    }
                    else
                    {
                        roleParts.Add(SigilPart.TextPart(part.Text));
                    }
                }

                if (part.FunctionCall != null)
                {
                    var mappedCall = SigilPart.ToolCallPart(new ToolCall
                    {
                        Id = part.FunctionCall.Id ?? string.Empty,
                        Name = part.FunctionCall.Name ?? string.Empty,
                        InputJson = SerializeJsonBytes(part.FunctionCall.Args),
                    });
                    mappedCall.Metadata.ProviderType = "function_call";

                    if (role == MessageRole.Assistant)
                    {
                        roleParts.Add(mappedCall);
                    }
                    else
                    {
                        assistantParts.Add(mappedCall);
                    }
                }

                if (part.FunctionResponse != null)
                {
                    var responsePayload = part.FunctionResponse.Response;
                    var contentText = ResolveFunctionResponseText(responsePayload);

                    var mappedResult = SigilPart.ToolResultPart(new ToolResult
                    {
                        ToolCallId = part.FunctionResponse.Id ?? string.Empty,
                        Name = part.FunctionResponse.Name ?? string.Empty,
                        Content = contentText,
                        ContentJson = SerializeJsonBytes(responsePayload),
                    });
                    mappedResult.Metadata.ProviderType = "function_response";
                    toolParts.Add(mappedResult);
                }
            }

            if (roleParts.Count > 0)
            {
                mapped.Add(new Message
                {
                    Role = role,
                    Parts = roleParts,
                });
            }

            if (assistantParts.Count > 0)
            {
                mapped.Add(new Message
                {
                    Role = MessageRole.Assistant,
                    Parts = assistantParts,
                });
            }

            if (toolParts.Count > 0)
            {
                mapped.Add(new Message
                {
                    Role = MessageRole.Tool,
                    Parts = toolParts,
                });
            }
        }

        return mapped;
    }

    private static List<Message> MapCandidates(List<Candidate>? candidates)
    {
        if (candidates == null || candidates.Count == 0)
        {
            return [];
        }

        var output = new List<Message>(candidates.Count);
        foreach (var candidate in candidates)
        {
            if (candidate?.Content == null)
            {
                continue;
            }

            output.AddRange(MapContents([candidate.Content]));
        }

        return output;
    }

    private static TokenUsage MapUsage(GenerateContentResponseUsageMetadata? usage)
    {
        if (usage == null)
        {
            return new TokenUsage();
        }

        var inputTokens = usage.PromptTokenCount ?? 0;
        var outputTokens = usage.CandidatesTokenCount ?? 0;
        var toolUsePromptTokens = usage.ToolUsePromptTokenCount ?? 0;
        var reasoningTokens = usage.ThoughtsTokenCount ?? 0;
        var totalTokens = usage.TotalTokenCount ?? (inputTokens + outputTokens + toolUsePromptTokens + reasoningTokens);

        return new TokenUsage
        {
            InputTokens = inputTokens,
            OutputTokens = outputTokens,
            TotalTokens = totalTokens,
            CacheReadInputTokens = usage.CachedContentTokenCount ?? 0,
            ReasoningTokens = reasoningTokens,
        };
    }

    private static Dictionary<string, object?> ThinkingMetadata(
        IReadOnlyDictionary<string, object?> metadata,
        long? thinkingBudget,
        string? thinkingLevel
    )
    {
        var outMetadata = new Dictionary<string, object?>(metadata, StringComparer.Ordinal);
        if (thinkingBudget.HasValue)
        {
            outMetadata[ThinkingBudgetMetadataKey] = thinkingBudget.Value;
        }
        if (!string.IsNullOrWhiteSpace(thinkingLevel))
        {
            outMetadata[ThinkingLevelMetadataKey] = thinkingLevel;
        }
        return outMetadata;
    }

    private static Dictionary<string, object?> MergeGeminiUsageMetadata(
        IReadOnlyDictionary<string, object?> metadata,
        GenerateContentResponseUsageMetadata? usage
    )
    {
        var outMetadata = new Dictionary<string, object?>(metadata, StringComparer.Ordinal);
        if (usage == null)
        {
            return outMetadata;
        }

        var toolUsePromptTokens = usage.ToolUsePromptTokenCount;
        if (toolUsePromptTokens.HasValue && toolUsePromptTokens.Value > 0)
        {
            outMetadata[ToolUsePromptTokensMetadataKey] = toolUsePromptTokens.Value;
        }

        return outMetadata;
    }

    private static string ExtractSystemPrompt(GenerateContentConfig? config)
    {
        if (config?.SystemInstruction?.Parts == null)
        {
            return string.Empty;
        }

        var chunks = new List<string>();
        foreach (var part in config.SystemInstruction.Parts)
        {
            if (!string.IsNullOrWhiteSpace(part?.Text))
            {
                chunks.Add(part.Text);
            }
        }

        return string.Join("\n\n", chunks);
    }

    private static List<ToolDefinition> MapTools(GenerateContentConfig? config)
    {
        if (config?.Tools == null || config.Tools.Count == 0)
        {
            return [];
        }

        var mapped = new List<ToolDefinition>();
        foreach (var tool in config.Tools)
        {
            if (tool?.FunctionDeclarations == null)
            {
                continue;
            }

            foreach (var declaration in tool.FunctionDeclarations)
            {
                if (string.IsNullOrWhiteSpace(declaration?.Name))
                {
                    continue;
                }

                byte[] schema = [];
                if (declaration.ParametersJsonSchema != null)
                {
                    schema = SerializeJsonBytes(declaration.ParametersJsonSchema);
                }
                else if (declaration.Parameters != null)
                {
                    schema = SerializeJsonBytes(declaration.Parameters);
                }

                mapped.Add(new ToolDefinition
                {
                    Name = declaration.Name,
                    Description = declaration.Description ?? string.Empty,
                    Type = "function",
                    InputSchemaJson = schema,
                });
            }
        }

        return mapped;
    }

    private static string ResolveStopReason(List<Candidate>? candidates, bool preferLast)
    {
        if (candidates == null || candidates.Count == 0)
        {
            return string.Empty;
        }

        IEnumerable<Candidate?> sequence = preferLast ? candidates.Reverse<Candidate?>() : candidates;
        foreach (var candidate in sequence)
        {
            var reason = candidate?.FinishReason?.ToString();
            if (!string.IsNullOrWhiteSpace(reason))
            {
                return NormalizeStopReason(reason!);
            }
        }

        return string.Empty;
    }

    private static string NormalizeStopReason(string reason)
    {
        var normalized = reason.Trim();
        if (normalized.Length == 0)
        {
            return string.Empty;
        }

        return normalized.ToUpperInvariant() switch
        {
            "STOP" => "STOP",
            "MAX_TOKENS" => "MAX_TOKENS",
            "SAFETY" => "SAFETY",
            "RECITATION" => "RECITATION",
            "OTHER" => "OTHER",
            "TOOL_CALL" => "TOOL_CALL",
            _ => normalized,
        };
    }

    private static string ResolveModelName(GenerateContentRequest request, GeminiSigilOptions options)
    {
        if (!string.IsNullOrWhiteSpace(options.ModelName))
        {
            return options.ModelName;
        }

        if (!string.IsNullOrWhiteSpace(request.Model))
        {
            return request.Model;
        }

        return "unknown";
    }

    private static string ResolveFunctionResponseText(Dictionary<string, object>? response)
    {
        if (response == null || response.Count == 0)
        {
            return string.Empty;
        }

        if (response.TryGetValue("output", out var output) && output is string text)
        {
            return text;
        }

        return JsonSerializer.Serialize(response);
    }

    private static MessageRole ParseRole(string? role)
    {
        return (role ?? string.Empty).Trim().ToLowerInvariant() switch
        {
            "assistant" => MessageRole.Assistant,
            "model" => MessageRole.Assistant,
            "tool" => MessageRole.Tool,
            _ => MessageRole.User,
        };
    }

    private static List<Artifact> BuildRequestResponseArtifacts(
        GeminiSigilOptions options,
        GenerateContentRequest request,
        GenerateContentResponse response,
        List<ToolDefinition> tools
    )
    {
        var artifacts = new List<Artifact>(3);

        if (options.IncludeRequestArtifact)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.Request, "gemini.generate_content.request", request));
        }

        if (options.IncludeResponseArtifact)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.Response, "gemini.generate_content.response", response));
        }

        if (options.IncludeToolsArtifact && tools.Count > 0)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.Tools, "gemini.generate_content.tools", tools));
        }

        return artifacts;
    }

    private static List<Artifact> BuildStreamArtifacts(
        GeminiSigilOptions options,
        GenerateContentRequest request,
        GeminiStreamSummary summary,
        List<ToolDefinition> tools
    )
    {
        var artifacts = new List<Artifact>(4);

        if (options.IncludeRequestArtifact)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.Request, "gemini.generate_content.request", request));
        }

        if (options.IncludeToolsArtifact && tools.Count > 0)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.Tools, "gemini.generate_content.tools", tools));
        }

        if (options.IncludeEventsArtifact)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.ProviderEvent, "gemini.generate_content.stream", summary.Responses));
        }

        if (options.IncludeResponseArtifact && summary.Responses.Count > 0)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.Response, "gemini.generate_content.response", summary.Responses[^1]));
        }

        return artifacts;
    }

    private static byte[] SerializeJsonBytes(object? value)
    {
        if (value == null)
        {
            return [];
        }

        return JsonSerializer.SerializeToUtf8Bytes(value);
    }

    private static object? ReadNestedProperty(object? source, params string[] path)
    {
        object? current = source;
        foreach (var segment in path)
        {
            if (current == null)
            {
                return null;
            }

            var property = current.GetType().GetProperty(segment);
            if (property == null)
            {
                return null;
            }

            current = property.GetValue(current);
        }

        return current;
    }

    private static long? ReadNullableLongProperty(object? source, params string[] path)
    {
        var value = ReadNestedProperty(source, path);
        if (value == null)
        {
            return null;
        }

        return value switch
        {
            long v => v,
            int v => v,
            short v => v,
            uint v => v,
            ulong v => (long)v,
            _ when long.TryParse(value.ToString(), out var parsed) => parsed,
            _ => null,
        };
    }

    private static double? ReadNullableDoubleProperty(object? source, params string[] path)
    {
        var value = ReadNestedProperty(source, path);
        if (value == null)
        {
            return null;
        }

        return value switch
        {
            double v => v,
            float v => v,
            decimal v => (double)v,
            long v => v,
            int v => v,
            _ when double.TryParse(value.ToString(), out var parsed) => parsed,
            _ => null,
        };
    }

    private static bool? ReadNestedBool(object? source, params string[] path)
    {
        var value = ReadNestedProperty(source, path);
        return value is bool boolValue ? boolValue : null;
    }

    private static string? ExtractThinkingLevel(GenerateContentConfig? config)
    {
        var level = ReadNestedProperty(config, "ThinkingConfig", "ThinkingLevel");
        if (level == null)
        {
            return null;
        }

        var normalized = level.ToString()?.Trim();
        if (string.IsNullOrWhiteSpace(normalized))
        {
            return null;
        }

        var canonical = normalized.ToLowerInvariant().Replace(" ", string.Empty);
        if (canonical.StartsWith("thinking_level_", StringComparison.Ordinal))
        {
            canonical = canonical["thinking_level_".Length..];
        }
        else if (canonical.StartsWith("thinkinglevel_", StringComparison.Ordinal))
        {
            canonical = canonical["thinkinglevel_".Length..];
        }
        else if (canonical.StartsWith("thinkinglevel", StringComparison.Ordinal))
        {
            canonical = canonical["thinkinglevel".Length..];
        }

        if (canonical == "unspecified")
        {
            return null;
        }

        return string.IsNullOrWhiteSpace(canonical) ? null : canonical;
    }

    private static string? CanonicalToolChoice(object? value)
    {
        if (value == null)
        {
            return null;
        }

        if (value is string text)
        {
            var normalized = text.Trim().ToLowerInvariant();
            return normalized.Length == 0 ? null : normalized;
        }

        if (value is Enum enumValue)
        {
            var normalized = enumValue.ToString().Trim().ToLowerInvariant();
            return normalized.Length == 0 ? null : normalized;
        }

        try
        {
            var element = JsonSerializer.SerializeToElement(value);
            return CanonicalJson(element);
        }
        catch
        {
            var normalized = value.ToString()?.Trim().ToLowerInvariant();
            return string.IsNullOrWhiteSpace(normalized) ? null : normalized;
        }
    }

    private static string CanonicalJson(JsonElement element)
    {
        using var stream = new MemoryStream();
        using var writer = new Utf8JsonWriter(stream);
        WriteCanonicalElement(writer, element);
        writer.Flush();
        return System.Text.Encoding.UTF8.GetString(stream.ToArray());
    }

    private static void WriteCanonicalElement(Utf8JsonWriter writer, JsonElement element)
    {
        switch (element.ValueKind)
        {
            case JsonValueKind.Object:
                writer.WriteStartObject();
                foreach (var property in element.EnumerateObject().OrderBy(property => property.Name, StringComparer.Ordinal))
                {
                    writer.WritePropertyName(property.Name);
                    WriteCanonicalElement(writer, property.Value);
                }

                writer.WriteEndObject();
                break;
            case JsonValueKind.Array:
                writer.WriteStartArray();
                foreach (var item in element.EnumerateArray())
                {
                    WriteCanonicalElement(writer, item);
                }

                writer.WriteEndArray();
                break;
            default:
                element.WriteTo(writer);
                break;
        }
    }
}
