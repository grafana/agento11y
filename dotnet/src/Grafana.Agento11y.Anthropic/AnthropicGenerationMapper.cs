using Anthropic.Models.Messages;
using System.Text;
using System.Text.Json;
using AnthropicMessage = Anthropic.Models.Messages.Message;

namespace Grafana.Sigil.Anthropic;

public static class AnthropicGenerationMapper
{
    private const string ThinkingBudgetMetadataKey = "sigil.gen_ai.request.thinking.budget_tokens";
    private const string ServerToolUseWebSearchRequestsMetadataKey = "sigil.gen_ai.usage.server_tool_use.web_search_requests";
    private const string ServerToolUseWebFetchRequestsMetadataKey = "sigil.gen_ai.usage.server_tool_use.web_fetch_requests";
    private const string ServerToolUseTotalRequestsMetadataKey = "sigil.gen_ai.usage.server_tool_use.total_requests";

    public static Generation FromRequestResponse(
        MessageCreateParams request,
        AnthropicMessage response,
        AnthropicSigilOptions? options = null
    )
    {
        ArgumentNullException.ThrowIfNull(request);
        ArgumentNullException.ThrowIfNull(response);

        var effective = options ?? new AnthropicSigilOptions();
        var requestJson = NormalizeRequestJson(SerializeJson(request));
        var responseJson = SerializeJson(response);

        var requestModel = ResolveModelName(requestJson, effective);
        var responseModel = FirstNonEmpty(ReadString(responseJson, "model"), requestModel);
        var stopReason = ReadString(responseJson, "stop_reason");
        var requestMaxTokens = ReadNullableLong(requestJson, "max_tokens");
        var requestTemperature = ReadNullableDouble(requestJson, "temperature");
        var requestTopP = ReadNullableDouble(requestJson, "top_p");
        var requestToolChoice = CanonicalToolChoice(ReadObject(requestJson, "tool_choice"));
        var requestThinking = ReadObject(requestJson, "thinking");
        var requestThinkingEnabled = ResolveThinkingEnabled(requestThinking);
        var requestThinkingBudget = ResolveThinkingBudget(requestThinking);

        var input = MapRequestMessages(requestJson);
        var output = MapResponseContent(responseJson);
        var systemPrompt = ExtractSystemPrompt(requestJson);
        var tools = MapTools(requestJson);
        var metadata = MetadataWithThinkingBudget(effective.Metadata, requestThinkingBudget);
        metadata = MergeServerToolUsageMetadata(metadata, ReadObject(responseJson, "usage"));

        var generation = new Generation
        {
            ConversationId = effective.ConversationId,
            AgentName = effective.AgentName,
            AgentVersion = effective.AgentVersion,
            Model = new ModelRef
            {
                Provider = effective.ProviderName,
                Name = requestModel,
            },
            ResponseId = ReadString(responseJson, "id"),
            ResponseModel = responseModel,
            SystemPrompt = systemPrompt,
            MaxTokens = requestMaxTokens,
            Temperature = requestTemperature,
            TopP = requestTopP,
            ToolChoice = requestToolChoice,
            ThinkingEnabled = requestThinkingEnabled,
            Input = input,
            Output = output,
            Tools = tools,
            Usage = MapUsage(ReadObject(responseJson, "usage")),
            StopReason = stopReason,
            Tags = new Dictionary<string, string>(effective.Tags, StringComparer.Ordinal),
            Metadata = metadata,
            Artifacts = BuildRequestResponseArtifacts(effective, requestJson, responseJson, tools),
            Mode = GenerationMode.Sync,
        };

        GenerationValidator.Validate(generation);
        return generation;
    }

    public static Generation FromStream(
        MessageCreateParams request,
        AnthropicStreamSummary summary,
        AnthropicSigilOptions? options = null
    )
    {
        ArgumentNullException.ThrowIfNull(request);
        ArgumentNullException.ThrowIfNull(summary);

        if (summary.FinalMessage != null)
        {
            var finalGeneration = FromRequestResponse(request, summary.FinalMessage, options);
            finalGeneration.Mode = GenerationMode.Stream;
            return AppendEventsArtifact(finalGeneration, summary, options);
        }

        if (summary.Events.Count == 0)
        {
            throw new ArgumentException("stream summary must contain events or a final message", nameof(summary));
        }

        var effective = options ?? new AnthropicSigilOptions();
        var requestJson = NormalizeRequestJson(SerializeJson(request));
        var requestModel = ResolveModelName(requestJson, effective);
        var requestMaxTokens = ReadNullableLong(requestJson, "max_tokens");
        var requestTemperature = ReadNullableDouble(requestJson, "temperature");
        var requestTopP = ReadNullableDouble(requestJson, "top_p");
        var requestToolChoice = CanonicalToolChoice(ReadObject(requestJson, "tool_choice"));
        var requestThinking = ReadObject(requestJson, "thinking");
        var requestThinkingEnabled = ResolveThinkingEnabled(requestThinking);
        var requestThinkingBudget = ResolveThinkingBudget(requestThinking);

        var input = MapRequestMessages(requestJson);
        var systemPrompt = ExtractSystemPrompt(requestJson);
        var tools = MapTools(requestJson);
        var metadata = MetadataWithThinkingBudget(effective.Metadata, requestThinkingBudget);

        var responseId = string.Empty;
        var responseModel = requestModel;
        var stopReason = string.Empty;
        var usage = new TokenUsage();
        var usageMetadata = new JsonElement();

        var assistantParts = new List<Part>();
        var toolParts = new List<Part>();

        var streamBlocks = new Dictionary<long, StreamBlock>();

        foreach (var streamEvent in summary.Events)
        {
            var json = SerializeEvent(streamEvent);
            var eventType = ReadString(json, "type");

            switch (eventType)
            {
                case "message_start":
                    {
                        var message = ReadObject(json, "message");
                        responseId = FirstNonEmpty(ReadString(message, "id"), responseId);
                        responseModel = FirstNonEmpty(ReadString(message, "model"), responseModel);

                        var messageParts = MapResponseContent(message);
                        foreach (var mapped in messageParts)
                        {
                            if (mapped.Role == MessageRole.Tool)
                            {
                                toolParts.AddRange(mapped.Parts);
                                continue;
                            }

                            assistantParts.AddRange(mapped.Parts);
                        }

                        break;
                    }
                case "content_block_start":
                    {
                        var index = ReadLong(json, "index");
                        var block = ReadObject(json, "content_block");
                        streamBlocks[index] = StreamBlock.FromStart(index, block);
                        break;
                    }
                case "content_block_delta":
                    {
                        var index = ReadLong(json, "index");
                        var delta = ReadObject(json, "delta");
                        if (!streamBlocks.TryGetValue(index, out var block))
                        {
                            block = StreamBlock.FromStart(index, new JsonElement());
                            streamBlocks[index] = block;
                        }

                        block.ApplyDelta(delta);
                        break;
                    }
                case "content_block_stop":
                    {
                        var index = ReadLong(json, "index");
                        if (!streamBlocks.TryGetValue(index, out var block))
                        {
                            break;
                        }

                        var (part, isToolResult) = block.ToPart();
                        if (part != null)
                        {
                            if (isToolResult)
                            {
                                toolParts.Add(part);
                            }
                            else
                            {
                                assistantParts.Add(part);
                            }
                        }

                        streamBlocks.Remove(index);
                        break;
                    }
                case "message_delta":
                    {
                        var delta = ReadObject(json, "delta");
                        stopReason = FirstNonEmpty(ReadString(delta, "stop_reason"), stopReason);
                        var eventUsage = ReadObject(json, "usage");
                        usage = MapUsage(eventUsage);
                        if (eventUsage.ValueKind == JsonValueKind.Object)
                        {
                            usageMetadata = eventUsage;
                        }
                        break;
                    }
            }
        }

        metadata = MergeServerToolUsageMetadata(metadata, usageMetadata);

        foreach (var block in streamBlocks.Values.OrderBy(block => block.Index))
        {
            var (part, isToolResult) = block.ToPart();
            if (part == null)
            {
                continue;
            }

            if (isToolResult)
            {
                toolParts.Add(part);
            }
            else
            {
                assistantParts.Add(part);
            }
        }

        var output = new List<Message>();
        if (assistantParts.Count > 0)
        {
            output.Add(new Message
            {
                Role = MessageRole.Assistant,
                Parts = assistantParts,
            });
        }

        if (toolParts.Count > 0)
        {
            output.Add(new Message
            {
                Role = MessageRole.Tool,
                Parts = toolParts,
            });
        }

        var generation = new Generation
        {
            ConversationId = effective.ConversationId,
            AgentName = effective.AgentName,
            AgentVersion = effective.AgentVersion,
            Model = new ModelRef
            {
                Provider = effective.ProviderName,
                Name = requestModel,
            },
            ResponseId = responseId,
            ResponseModel = responseModel,
            SystemPrompt = systemPrompt,
            MaxTokens = requestMaxTokens,
            Temperature = requestTemperature,
            TopP = requestTopP,
            ToolChoice = requestToolChoice,
            ThinkingEnabled = requestThinkingEnabled,
            Input = input,
            Output = output,
            Tools = tools,
            Usage = usage,
            StopReason = stopReason,
            Tags = new Dictionary<string, string>(effective.Tags, StringComparer.Ordinal),
            Metadata = metadata,
            Artifacts = BuildStreamArtifacts(effective, requestJson, tools, summary),
            Mode = GenerationMode.Stream,
        };

        GenerationValidator.Validate(generation);
        return generation;
    }

    private static List<Message> MapRequestMessages(JsonElement requestJson)
    {
        if (!requestJson.TryGetProperty("messages", out var messages) || messages.ValueKind != JsonValueKind.Array)
        {
            return [];
        }

        var mapped = new List<Message>();
        foreach (var message in messages.EnumerateArray())
        {
            var role = ParseRole(ReadString(message, "role"));
            var roleParts = new List<Part>();
            var toolParts = new List<Part>();

            if (message.TryGetProperty("content", out var content))
            {
                if (content.ValueKind == JsonValueKind.String)
                {
                    var text = content.GetString();
                    if (!string.IsNullOrWhiteSpace(text))
                    {
                        roleParts.Add(Part.TextPart(text));
                    }
                }
                else if (content.ValueKind == JsonValueKind.Array)
                {
                    foreach (var block in content.EnumerateArray())
                    {
                        var (part, isToolResult) = MapBlock(block);
                        if (part == null)
                        {
                            continue;
                        }

                        if (isToolResult)
                        {
                            toolParts.Add(part);
                        }
                        else
                        {
                            roleParts.Add(part);
                        }
                    }
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

    private static List<Message> MapResponseContent(JsonElement responseJson)
    {
        if (!responseJson.TryGetProperty("content", out var content) || content.ValueKind != JsonValueKind.Array)
        {
            return [];
        }

        var assistantParts = new List<Part>();
        var toolParts = new List<Part>();

        foreach (var block in content.EnumerateArray())
        {
            var (part, isToolResult) = MapBlock(block);
            if (part == null)
            {
                continue;
            }

            if (isToolResult)
            {
                toolParts.Add(part);
            }
            else
            {
                assistantParts.Add(part);
            }
        }

        var messages = new List<Message>(2);
        if (assistantParts.Count > 0)
        {
            messages.Add(new Message
            {
                Role = MessageRole.Assistant,
                Parts = assistantParts,
            });
        }

        if (toolParts.Count > 0)
        {
            messages.Add(new Message
            {
                Role = MessageRole.Tool,
                Parts = toolParts,
            });
        }

        return messages;
    }

    private static (Part? part, bool isToolResult) MapBlock(JsonElement block)
    {
        var type = ReadString(block, "type");
        switch (type)
        {
            case "text":
                {
                    var text = ReadString(block, "text");
                    if (text.Length == 0)
                    {
                        return (null, false);
                    }

                    return (Part.TextPart(text), false);
                }
            case "thinking":
                {
                    var thinking = ReadString(block, "thinking");
                    if (thinking.Length == 0)
                    {
                        return (null, false);
                    }

                    var part = Part.ThinkingPart(thinking);
                    part.Metadata.ProviderType = type;
                    return (part, false);
                }
            case "redacted_thinking":
                {
                    var data = ReadString(block, "data");
                    if (data.Length == 0)
                    {
                        return (null, false);
                    }

                    var part = Part.ThinkingPart(data);
                    part.Metadata.ProviderType = type;
                    return (part, false);
                }
            case "tool_use":
            case "server_tool_use":
            case "mcp_tool_use":
                {
                    var inputJson = block.TryGetProperty("input", out var input)
                        ? Encoding.UTF8.GetBytes(input.GetRawText())
                        : [];

                    var part = Part.ToolCallPart(new ToolCall
                    {
                        Id = ReadString(block, "id"),
                        Name = FirstNonEmpty(ReadString(block, "name"), ReadString(block, "tool_name")),
                        InputJson = inputJson,
                    });
                    part.Metadata.ProviderType = type;
                    return (part, false);
                }
            case "tool_result":
            case "web_search_tool_result":
            case "web_fetch_tool_result":
            case "code_execution_tool_result":
            case "bash_code_execution_tool_result":
            case "text_editor_code_execution_tool_result":
            case "tool_search_tool_result":
            case "mcp_tool_result":
                {
                    var content = block.TryGetProperty("content", out var contentElement)
                        ? contentElement
                        : default;

                    var contentJson = content.ValueKind == JsonValueKind.Undefined
                        ? []
                        : Encoding.UTF8.GetBytes(content.GetRawText());

                    string contentText = content.ValueKind switch
                    {
                        JsonValueKind.String => content.GetString() ?? string.Empty,
                        JsonValueKind.Array => string.Join("\n", content.EnumerateArray().Select(item =>
                            item.ValueKind == JsonValueKind.Object && item.TryGetProperty("text", out var text)
                                ? text.GetString() ?? string.Empty
                                : item.ToString()
                        )),
                        JsonValueKind.Undefined => string.Empty,
                        _ => content.ToString(),
                    };

                    var part = Part.ToolResultPart(new ToolResult
                    {
                        ToolCallId = FirstNonEmpty(ReadString(block, "tool_use_id"), ReadString(block, "tool_call_id")),
                        Name = FirstNonEmpty(ReadString(block, "name"), ReadString(block, "tool_name")),
                        Content = contentText,
                        ContentJson = contentJson,
                        IsError = ReadBool(block, "is_error"),
                    });
                    part.Metadata.ProviderType = type;
                    return (part, true);
                }
            default:
                return (null, false);
        }
    }

    private static string ExtractSystemPrompt(JsonElement requestJson)
    {
        if (!requestJson.TryGetProperty("system", out var system))
        {
            return string.Empty;
        }

        if (system.ValueKind == JsonValueKind.Object && system.TryGetProperty("value", out var wrappedSystem))
        {
            system = wrappedSystem;
        }

        if (system.ValueKind == JsonValueKind.String)
        {
            return system.GetString() ?? string.Empty;
        }

        if (system.ValueKind != JsonValueKind.Array)
        {
            return string.Empty;
        }

        var chunks = new List<string>();
        foreach (var item in system.EnumerateArray())
        {
            if (item.ValueKind == JsonValueKind.String)
            {
                var value = item.GetString();
                if (!string.IsNullOrWhiteSpace(value))
                {
                    chunks.Add(value);
                }

                continue;
            }

            if (item.ValueKind == JsonValueKind.Object && item.TryGetProperty("text", out var text))
            {
                var value = text.GetString();
                if (!string.IsNullOrWhiteSpace(value))
                {
                    chunks.Add(value);
                }
            }
        }

        return string.Join("\n\n", chunks);
    }

    private static List<ToolDefinition> MapTools(JsonElement requestJson)
    {
        if (!requestJson.TryGetProperty("tools", out var tools) || tools.ValueKind != JsonValueKind.Array)
        {
            return [];
        }

        var mapped = new List<ToolDefinition>();
        foreach (var tool in tools.EnumerateArray())
        {
            var name = FirstNonEmpty(ReadString(tool, "name"), ReadString(tool, "tool_name"));
            if (name.Length == 0)
            {
                continue;
            }

            byte[] schema = [];
            if (tool.TryGetProperty("input_schema", out var inputSchema))
            {
                schema = Encoding.UTF8.GetBytes(inputSchema.GetRawText());
            }

            mapped.Add(new ToolDefinition
            {
                Name = name,
                Description = ReadString(tool, "description"),
                Type = FirstNonEmpty(ReadString(tool, "type"), "function"),
                InputSchemaJson = schema,
            });
        }

        return mapped;
    }

    private static TokenUsage MapUsage(JsonElement usage)
    {
        if (usage.ValueKind != JsonValueKind.Object)
        {
            return new TokenUsage();
        }

        var input = ReadLong(usage, "input_tokens");
        var output = ReadLong(usage, "output_tokens");
        var total = ReadLong(usage, "total_tokens");

        if (total == 0)
        {
            total = input + output;
        }

        return new TokenUsage
        {
            InputTokens = input,
            OutputTokens = output,
            TotalTokens = total,
            CacheReadInputTokens = ReadLong(usage, "cache_read_input_tokens"),
            CacheWriteInputTokens = ReadLong(usage, "cache_creation_input_tokens"),
        };
    }

    private static string ResolveModelName(JsonElement requestJson, AnthropicSigilOptions options)
    {
        if (!string.IsNullOrWhiteSpace(options.ModelName))
        {
            return options.ModelName;
        }

        var model = ReadString(requestJson, "model");
        if (!string.IsNullOrWhiteSpace(model))
        {
            return model;
        }

        return "unknown";
    }

    private static List<Artifact> BuildRequestResponseArtifacts(
        AnthropicSigilOptions options,
        JsonElement requestJson,
        JsonElement responseJson,
        List<ToolDefinition> tools
    )
    {
        var artifacts = new List<Artifact>(3);

        if (options.IncludeRequestArtifact)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.Request, "anthropic.request", requestJson));
        }

        if (options.IncludeResponseArtifact)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.Response, "anthropic.response", responseJson));
        }

        if (options.IncludeToolsArtifact && tools.Count > 0)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.Tools, "anthropic.tools", tools));
        }

        return artifacts;
    }

    private static List<Artifact> BuildStreamArtifacts(
        AnthropicSigilOptions options,
        JsonElement requestJson,
        List<ToolDefinition> tools,
        AnthropicStreamSummary summary
    )
    {
        var artifacts = new List<Artifact>(4);

        if (options.IncludeRequestArtifact)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.Request, "anthropic.request", requestJson));
        }

        if (options.IncludeToolsArtifact && tools.Count > 0)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.Tools, "anthropic.tools", tools));
        }

        if (options.IncludeEventsArtifact)
        {
            artifacts.Add(Artifact.JsonArtifact(
                ArtifactKind.ProviderEvent,
                "anthropic.stream_events",
                summary.Events.Select(SerializeEvent).ToList()
            ));
        }

        if (options.IncludeResponseArtifact && summary.FinalMessage != null)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.Response, "anthropic.response", summary.FinalMessage));
        }

        return artifacts;
    }

    private static Generation AppendEventsArtifact(Generation generation, AnthropicStreamSummary summary, AnthropicSigilOptions? options)
    {
        var effective = options ?? new AnthropicSigilOptions();
        if (!effective.IncludeEventsArtifact || summary.Events.Count == 0)
        {
            return generation;
        }

        generation.Artifacts.Add(Artifact.JsonArtifact(
            ArtifactKind.ProviderEvent,
            "anthropic.stream_events",
            summary.Events.Select(SerializeEvent).ToList()
        ));

        return generation;
    }

    private static JsonElement SerializeJson<T>(T value)
    {
        return JsonSerializer.SerializeToElement(value);
    }

    private static JsonElement NormalizeRequestJson(JsonElement requestJson)
    {
        if (requestJson.ValueKind != JsonValueKind.Object)
        {
            return requestJson;
        }

        if (requestJson.TryGetProperty("bodyProperties", out var bodyCamel) && bodyCamel.ValueKind == JsonValueKind.Object)
        {
            return bodyCamel;
        }

        if (requestJson.TryGetProperty("BodyProperties", out var bodyPascal) && bodyPascal.ValueKind == JsonValueKind.Object)
        {
            return bodyPascal;
        }

        if (requestJson.TryGetProperty("rawBodyData", out var rawBodyCamel) && rawBodyCamel.ValueKind == JsonValueKind.Object)
        {
            return rawBodyCamel;
        }

        if (requestJson.TryGetProperty("RawBodyData", out var rawBodyPascal) && rawBodyPascal.ValueKind == JsonValueKind.Object)
        {
            return rawBodyPascal;
        }

        return requestJson;
    }

    private static JsonElement SerializeEvent(RawMessageStreamEvent streamEvent)
    {
        if (streamEvent?.Value == null)
        {
            return new JsonElement();
        }

        return JsonSerializer.SerializeToElement(streamEvent.Value);
    }

    private static JsonElement ReadObject(JsonElement element, string name)
    {
        if (element.ValueKind == JsonValueKind.Object && element.TryGetProperty(name, out var value))
        {
            return value;
        }

        return new JsonElement();
    }

    private static string ReadString(JsonElement element, string name)
    {
        if (element.ValueKind == JsonValueKind.Object
            && element.TryGetProperty(name, out var value)
            && value.ValueKind == JsonValueKind.String)
        {
            return value.GetString() ?? string.Empty;
        }

        return string.Empty;
    }

    private static long ReadLong(JsonElement element, string name)
    {
        if (element.ValueKind == JsonValueKind.Object
            && element.TryGetProperty(name, out var value)
            && value.ValueKind == JsonValueKind.Number
            && value.TryGetInt64(out var parsed))
        {
            return parsed;
        }

        return 0;
    }

    private static long? ReadNullableLong(JsonElement element, string name)
    {
        if (element.ValueKind != JsonValueKind.Object || !element.TryGetProperty(name, out var value))
        {
            return null;
        }

        if (value.ValueKind == JsonValueKind.Null || value.ValueKind == JsonValueKind.Undefined)
        {
            return null;
        }

        if (value.ValueKind == JsonValueKind.Number && value.TryGetInt64(out var parsed))
        {
            return parsed;
        }

        if (value.ValueKind == JsonValueKind.String && long.TryParse(value.GetString(), out var parsedString))
        {
            return parsedString;
        }

        return null;
    }

    private static double? ReadNullableDouble(JsonElement element, string name)
    {
        if (element.ValueKind != JsonValueKind.Object || !element.TryGetProperty(name, out var value))
        {
            return null;
        }

        if (value.ValueKind == JsonValueKind.Null || value.ValueKind == JsonValueKind.Undefined)
        {
            return null;
        }

        if (value.ValueKind == JsonValueKind.Number && value.TryGetDouble(out var parsed))
        {
            return parsed;
        }

        if (value.ValueKind == JsonValueKind.String && double.TryParse(value.GetString(), out var parsedString))
        {
            return parsedString;
        }

        return null;
    }

    private static bool ReadBool(JsonElement element, string name)
    {
        if (element.ValueKind == JsonValueKind.Object
            && element.TryGetProperty(name, out var value)
            && (value.ValueKind == JsonValueKind.True || value.ValueKind == JsonValueKind.False))
        {
            return value.GetBoolean();
        }

        return false;
    }

    private static string? CanonicalToolChoice(JsonElement toolChoice)
    {
        if (toolChoice.ValueKind == JsonValueKind.Undefined || toolChoice.ValueKind == JsonValueKind.Null)
        {
            return null;
        }

        if (toolChoice.ValueKind == JsonValueKind.String)
        {
            var normalized = (toolChoice.GetString() ?? string.Empty).Trim().ToLowerInvariant();
            return normalized.Length == 0 ? null : normalized;
        }

        if (toolChoice.ValueKind == JsonValueKind.Object || toolChoice.ValueKind == JsonValueKind.Array)
        {
            return CanonicalJson(toolChoice);
        }

        var fallback = toolChoice.ToString().Trim().ToLowerInvariant();
        return fallback.Length == 0 ? null : fallback;
    }

    private static bool? ResolveThinkingEnabled(JsonElement thinking)
    {
        if (thinking.ValueKind == JsonValueKind.Undefined || thinking.ValueKind == JsonValueKind.Null)
        {
            return null;
        }

        if (thinking.ValueKind == JsonValueKind.True || thinking.ValueKind == JsonValueKind.False)
        {
            return thinking.GetBoolean();
        }

        if (thinking.ValueKind == JsonValueKind.String)
        {
            var normalized = (thinking.GetString() ?? string.Empty).Trim().ToLowerInvariant();
            return normalized switch
            {
                "enabled" or "adaptive" => true,
                "disabled" => false,
                _ => null,
            };
        }

        if (thinking.ValueKind == JsonValueKind.Object)
        {
            if (thinking.TryGetProperty("enabled", out var enabled)
                && (enabled.ValueKind == JsonValueKind.True || enabled.ValueKind == JsonValueKind.False))
            {
                return enabled.GetBoolean();
            }

            var mode = FirstNonEmpty(ReadString(thinking, "type"), ReadString(thinking, "mode"));
            return mode.Trim().ToLowerInvariant() switch
            {
                "enabled" or "adaptive" => true,
                "disabled" => false,
                _ => null,
            };
        }

        return null;
    }

    private static long? ResolveThinkingBudget(JsonElement thinking)
    {
        if (thinking.ValueKind != JsonValueKind.Object)
        {
            return null;
        }

        return ReadNullableLong(thinking, "budget_tokens");
    }

    private static Dictionary<string, object?> MetadataWithThinkingBudget(
        IReadOnlyDictionary<string, object?> metadata,
        long? thinkingBudget
    )
    {
        var outMetadata = new Dictionary<string, object?>(metadata, StringComparer.Ordinal);
        if (thinkingBudget.HasValue)
        {
            outMetadata[ThinkingBudgetMetadataKey] = thinkingBudget.Value;
        }
        return outMetadata;
    }

    private static Dictionary<string, object?> MergeServerToolUsageMetadata(
        IReadOnlyDictionary<string, object?> metadata,
        JsonElement usage
    )
    {
        var outMetadata = new Dictionary<string, object?>(metadata, StringComparer.Ordinal);
        if (usage.ValueKind != JsonValueKind.Object)
        {
            return outMetadata;
        }

        var serverToolUse = ReadObject(usage, "server_tool_use");
        if (serverToolUse.ValueKind != JsonValueKind.Object)
        {
            return outMetadata;
        }

        var webSearchRequests = ReadLong(serverToolUse, "web_search_requests");
        var webFetchRequests = ReadLong(serverToolUse, "web_fetch_requests");

        if (webSearchRequests > 0)
        {
            outMetadata[ServerToolUseWebSearchRequestsMetadataKey] = webSearchRequests;
        }

        if (webFetchRequests > 0)
        {
            outMetadata[ServerToolUseWebFetchRequestsMetadataKey] = webFetchRequests;
        }

        var totalRequests = webSearchRequests + webFetchRequests;
        if (totalRequests > 0)
        {
            outMetadata[ServerToolUseTotalRequestsMetadataKey] = totalRequests;
        }

        return outMetadata;
    }

    private static string CanonicalJson(JsonElement element)
    {
        using var stream = new MemoryStream();
        using var writer = new Utf8JsonWriter(stream);
        WriteCanonicalElement(writer, element);
        writer.Flush();
        return Encoding.UTF8.GetString(stream.ToArray());
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

    private static MessageRole ParseRole(string role)
    {
        return role.Trim().ToLowerInvariant() switch
        {
            "assistant" => MessageRole.Assistant,
            "tool" => MessageRole.Tool,
            _ => MessageRole.User,
        };
    }

    private static string FirstNonEmpty(params string[] values)
    {
        foreach (var value in values)
        {
            if (!string.IsNullOrWhiteSpace(value))
            {
                return value;
            }
        }

        return string.Empty;
    }

    private sealed class StreamBlock
    {
        private readonly string _type;
        private readonly string _id;
        private readonly string _name;
        private readonly string _toolUseId;
        private readonly StringBuilder _buffer;

        public long Index { get; }

        private StreamBlock(long index, string type, string id, string name, string toolUseId, string seed)
        {
            Index = index;
            _type = type;
            _id = id;
            _name = name;
            _toolUseId = toolUseId;
            _buffer = new StringBuilder(seed);
        }

        public static StreamBlock FromStart(long index, JsonElement block)
        {
            var type = ReadString(block, "type");
            var id = ReadString(block, "id");
            var name = FirstNonEmpty(ReadString(block, "name"), ReadString(block, "tool_name"));
            var toolUseId = FirstNonEmpty(ReadString(block, "tool_use_id"), ReadString(block, "tool_call_id"));

            string seed = type switch
            {
                "text" => ReadString(block, "text"),
                "thinking" => ReadString(block, "thinking"),
                "redacted_thinking" => ReadString(block, "data"),
                "tool_use" or "server_tool_use" or "mcp_tool_use" when block.TryGetProperty("input", out var input)
                    => input.GetRawText(),
                _ => string.Empty,
            };

            return new StreamBlock(index, type, id, name, toolUseId, seed);
        }

        public void ApplyDelta(JsonElement delta)
        {
            var deltaType = ReadString(delta, "type");
            switch (deltaType)
            {
                case "text_delta":
                    _buffer.Append(ReadString(delta, "text"));
                    break;
                case "thinking_delta":
                    _buffer.Append(ReadString(delta, "thinking"));
                    break;
                case "input_json_delta":
                    _buffer.Append(ReadString(delta, "partial_json"));
                    break;
            }
        }

        public (Part? part, bool isToolResult) ToPart()
        {
            switch (_type)
            {
                case "text":
                    {
                        var text = _buffer.ToString().Trim();
                        return text.Length == 0 ? (null, false) : (Part.TextPart(text), false);
                    }
                case "thinking":
                case "redacted_thinking":
                    {
                        var thinking = _buffer.ToString();
                        if (thinking.Length == 0)
                        {
                            return (null, false);
                        }

                        var part = Part.ThinkingPart(thinking);
                        part.Metadata.ProviderType = _type;
                        return (part, false);
                    }
                case "tool_use":
                case "server_tool_use":
                case "mcp_tool_use":
                    {
                        var part = Part.ToolCallPart(new ToolCall
                        {
                            Id = _id,
                            Name = _name,
                            InputJson = Encoding.UTF8.GetBytes(_buffer.ToString()),
                        });
                        part.Metadata.ProviderType = _type;
                        return (part, false);
                    }
                case "tool_result":
                case "web_search_tool_result":
                case "web_fetch_tool_result":
                case "code_execution_tool_result":
                case "bash_code_execution_tool_result":
                case "text_editor_code_execution_tool_result":
                case "tool_search_tool_result":
                case "mcp_tool_result":
                    {
                        var payload = _buffer.ToString();
                        var part = Part.ToolResultPart(new ToolResult
                        {
                            ToolCallId = _toolUseId,
                            Name = _name,
                            Content = payload,
                            ContentJson = Encoding.UTF8.GetBytes(payload),
                        });
                        part.Metadata.ProviderType = _type;
                        return (part, true);
                    }
                default:
                    return (null, false);
            }
        }
    }
}
