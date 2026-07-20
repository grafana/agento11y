using global::OpenAI.Chat;
using global::OpenAI.Embeddings;
using System.Text;
using System.Text.Json;
using OpenAIResponses = global::OpenAI.Responses;

namespace Grafana.Sigil.OpenAI;

public static class OpenAIGenerationMapper
{
    private const string ThinkingBudgetMetadataKey = "sigil.gen_ai.request.thinking.budget_tokens";

    public static Generation ChatCompletionsFromRequestResponse(
        string modelName,
        IReadOnlyList<ChatMessage> messages,
        ChatCompletionOptions? requestOptions,
        ChatCompletion response,
        OpenAISigilOptions? options = null
    )
    {
        ArgumentNullException.ThrowIfNull(response);

        var effective = options ?? new OpenAISigilOptions();
        var requestMessages = messages ?? [];

        var (input, systemPrompt) = MapRequestMessages(requestMessages);
        var output = MapResponseMessages(response);
        var tools = MapChatCompletionsTools(requestOptions);
        var thinkingBudget = ResolveChatCompletionsThinkingBudget(requestOptions);

        var responseModel = string.IsNullOrWhiteSpace(response.Model) ? modelName : response.Model;

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
            ResponseId = response.Id,
            ResponseModel = responseModel,
            SystemPrompt = systemPrompt,
            MaxTokens = ResolveChatCompletionsRequestMaxTokens(requestOptions),
            Temperature = ReadNullableDoubleProperty(requestOptions, "Temperature"),
            TopP = ReadNullableDoubleProperty(requestOptions, "TopP"),
            ToolChoice = CanonicalToolChoice(ReadProperty(requestOptions, "ToolChoice")),
            ThinkingEnabled = ResolveChatCompletionsThinkingEnabled(requestOptions),
            Input = input,
            Output = output,
            Tools = tools,
            Usage = MapChatCompletionsUsage(response.Usage),
            StopReason = OpenAIJsonHelpers.NormalizeStopReason(response.FinishReason.ToString()),
            Tags = new Dictionary<string, string>(effective.Tags, StringComparer.Ordinal),
            Metadata = MetadataWithThinkingBudget(effective.Metadata, thinkingBudget),
            Artifacts = BuildChatCompletionsArtifactsForRequestResponse(
                effective,
                modelName,
                systemPrompt,
                input,
                output,
                tools,
                response
            ),
            Mode = GenerationMode.Sync,
        };

        GenerationValidator.Validate(generation);
        return generation;
    }

    public static Generation ResponsesFromRequestResponse(
        string modelName,
        IReadOnlyList<OpenAIResponses.ResponseItem> inputItems,
        OpenAIResponses.CreateResponseOptions? requestOptions,
        OpenAIResponses.ResponseResult response,
        OpenAISigilOptions? options = null
    )
    {
        ArgumentNullException.ThrowIfNull(response);

        var effective = options ?? new OpenAISigilOptions();
        var requestItems = inputItems ?? [];
        var (input, mappedSystemPrompt) = MapResponsesInputItems(requestItems);
        var systemPrompt = OpenAIJsonHelpers.MergeSystemPrompt(
        [
            requestOptions?.Instructions ?? string.Empty,
            mappedSystemPrompt,
        ]);
        var outputItems = response.OutputItems?.ToList() ?? [];
        var output = MapResponsesOutputItems(outputItems);
        var tools = MapResponsesTools(requestOptions);
        var thinkingBudget = ResolveResponsesThinkingBudget(requestOptions);

        var responseModel = string.IsNullOrWhiteSpace(response.Model) ? modelName : response.Model;

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
            ResponseId = response.Id ?? string.Empty,
            ResponseModel = responseModel,
            SystemPrompt = systemPrompt,
            MaxTokens = requestOptions?.MaxOutputTokenCount,
            Temperature = ReadNullableDoubleProperty(requestOptions, "Temperature"),
            TopP = ReadNullableDoubleProperty(requestOptions, "TopP"),
            ToolChoice = CanonicalToolChoice(requestOptions?.ToolChoice),
            ThinkingEnabled = ResolveResponsesThinkingEnabled(requestOptions),
            Input = input,
            Output = output,
            Tools = tools,
            Usage = MapResponsesUsage(response.Usage),
            StopReason = NormalizeResponsesStopReason(response),
            Tags = new Dictionary<string, string>(effective.Tags, StringComparer.Ordinal),
            Metadata = MetadataWithThinkingBudget(effective.Metadata, thinkingBudget),
            Artifacts = BuildResponsesArtifactsForRequestResponse(
                effective,
                modelName,
                systemPrompt,
                input,
                output,
                tools,
                response
            ),
            Mode = GenerationMode.Sync,
        };

        GenerationValidator.Validate(generation);
        return generation;
    }

    public static EmbeddingStart EmbeddingsStart(
        string modelName,
        EmbeddingGenerationOptions? requestOptions,
        OpenAISigilOptions? options = null
    )
    {
        var effective = options ?? new OpenAISigilOptions();

        var start = new EmbeddingStart
        {
            AgentName = effective.AgentName,
            AgentVersion = effective.AgentVersion,
            Model = new ModelRef
            {
                Provider = effective.ProviderName,
                Name = modelName ?? string.Empty,
            },
            Tags = new Dictionary<string, string>(effective.Tags, StringComparer.Ordinal),
            Metadata = new Dictionary<string, object?>(effective.Metadata, StringComparer.Ordinal),
        };

        if (requestOptions?.Dimensions is int dimensions && dimensions > 0)
        {
            start.Dimensions = dimensions;
        }

        return start;
    }

    public static EmbeddingResult EmbeddingsFromRequestResponse(
        string modelName,
        IReadOnlyList<string>? inputs,
        EmbeddingGenerationOptions? requestOptions,
        OpenAIEmbeddingCollection? response
    )
    {
        var result = new EmbeddingResult
        {
            InputCount = inputs?.Count ?? 0,
            InputTexts = EmbeddingInputTexts(inputs),
        };

        if (response == null)
        {
            result.ResponseModel = modelName ?? string.Empty;
            if (requestOptions?.Dimensions is int requestedDimensions && requestedDimensions > 0)
            {
                result.Dimensions = requestedDimensions;
            }

            return result;
        }

        if (response.Usage != null)
        {
            result.InputTokens = response.Usage.InputTokenCount;
        }

        result.ResponseModel = string.IsNullOrWhiteSpace(response.Model) ? modelName : response.Model;

        if (response.Count > 0)
        {
            var first = response[0];
            if (first != null)
            {
                var vector = first.ToFloats();
                if (vector.Length > 0)
                {
                    result.Dimensions = vector.Length;
                }
            }
        }

        if (!result.Dimensions.HasValue && requestOptions?.Dimensions is int fallbackDimensions && fallbackDimensions > 0)
        {
            result.Dimensions = fallbackDimensions;
        }

        return result;
    }

    public static Generation ResponsesFromStream(
        string modelName,
        IReadOnlyList<OpenAIResponses.ResponseItem> inputItems,
        OpenAIResponses.CreateResponseOptions? requestOptions,
        OpenAIResponsesStreamSummary summary,
        OpenAISigilOptions? options = null
    )
    {
        ArgumentNullException.ThrowIfNull(summary);

        if (summary.FinalResponse != null)
        {
            var finalGeneration = ResponsesFromRequestResponse(modelName, inputItems, requestOptions, summary.FinalResponse, options);
            finalGeneration.Mode = GenerationMode.Stream;
            return AppendResponsesStreamEventsArtifact(finalGeneration, summary, options);
        }

        if (summary.Events.Count == 0)
        {
            throw new ArgumentException("stream summary must contain events or a final response", nameof(summary));
        }

        var effective = options ?? new OpenAISigilOptions();
        var requestItems = inputItems ?? [];
        var (input, mappedSystemPrompt) = MapResponsesInputItems(requestItems);
        var systemPrompt = OpenAIJsonHelpers.MergeSystemPrompt(
        [
            requestOptions?.Instructions ?? string.Empty,
            mappedSystemPrompt,
        ]);

        var outputByIndex = new SortedDictionary<int, OpenAIResponses.ResponseItem>();
        var streamToolCalls = new Dictionary<string, ResponsesStreamToolCall>(StringComparer.Ordinal);
        var assistantText = new StringBuilder();
        var assistantRefusal = new StringBuilder();
        var eventStopReason = string.Empty;
        OpenAIResponses.ResponseResult? finalResponse = null;

        foreach (var streamEvent in summary.Events)
        {
            switch (streamEvent)
            {
                case OpenAIResponses.StreamingResponseOutputTextDeltaUpdate deltaUpdate:
                    if (!string.IsNullOrWhiteSpace(deltaUpdate.Delta))
                    {
                        assistantText.Append(deltaUpdate.Delta);
                    }
                    break;
                case OpenAIResponses.StreamingResponseOutputTextDoneUpdate doneUpdate:
                    if (!string.IsNullOrWhiteSpace(doneUpdate.Text))
                    {
                        assistantText.Append(doneUpdate.Text);
                    }
                    break;
                case OpenAIResponses.StreamingResponseRefusalDeltaUpdate refusalDelta:
                    if (!string.IsNullOrWhiteSpace(refusalDelta.Delta))
                    {
                        assistantRefusal.Append(refusalDelta.Delta);
                    }
                    break;
                case OpenAIResponses.StreamingResponseRefusalDoneUpdate refusalDone:
                    if (!string.IsNullOrWhiteSpace(refusalDone.Refusal))
                    {
                        assistantRefusal.Append(refusalDone.Refusal);
                    }
                    break;
                case OpenAIResponses.StreamingResponseFunctionCallArgumentsDeltaUpdate callDelta:
                    CaptureResponsesStreamToolCall(streamToolCalls, callDelta.ItemId, callDelta.Delta?.ToString(), null, null);
                    break;
                case OpenAIResponses.StreamingResponseFunctionCallArgumentsDoneUpdate callDone:
                    CaptureResponsesStreamToolCall(streamToolCalls, callDone.ItemId, null, null, callDone.FunctionArguments?.ToString());
                    break;
                case OpenAIResponses.StreamingResponseMcpCallArgumentsDeltaUpdate mcpDelta:
                    CaptureResponsesStreamToolCall(streamToolCalls, mcpDelta.ItemId, mcpDelta.Delta?.ToString(), null, null);
                    break;
                case OpenAIResponses.StreamingResponseMcpCallArgumentsDoneUpdate mcpDone:
                    CaptureResponsesStreamToolCall(streamToolCalls, mcpDone.ItemId, null, null, mcpDone.ToolArguments?.ToString());
                    break;
                case OpenAIResponses.StreamingResponseOutputItemAddedUpdate itemAdded when itemAdded.Item != null:
                    outputByIndex[itemAdded.OutputIndex] = itemAdded.Item;
                    CaptureResponsesStreamToolCallFromItem(streamToolCalls, itemAdded.Item);
                    break;
                case OpenAIResponses.StreamingResponseOutputItemDoneUpdate itemDone when itemDone.Item != null:
                    outputByIndex[itemDone.OutputIndex] = itemDone.Item;
                    CaptureResponsesStreamToolCallFromItem(streamToolCalls, itemDone.Item);
                    break;
                case OpenAIResponses.StreamingResponseCompletedUpdate completed when completed.Response != null:
                    finalResponse = completed.Response;
                    break;
                case OpenAIResponses.StreamingResponseIncompleteUpdate incomplete when incomplete.Response != null:
                    finalResponse = incomplete.Response;
                    break;
                case OpenAIResponses.StreamingResponseFailedUpdate failed when failed.Response != null:
                    finalResponse = failed.Response;
                    break;
                case OpenAIResponses.StreamingResponseErrorUpdate errorUpdate:
                    eventStopReason = string.IsNullOrWhiteSpace(errorUpdate.Code) ? "error" : errorUpdate.Code;
                    break;
            }
        }

        if (finalResponse != null)
        {
            summary.FinalResponse = finalResponse;
            if (outputByIndex.Count == 0 && finalResponse.OutputItems != null)
            {
                for (var index = 0; index < finalResponse.OutputItems.Count; index++)
                {
                    var item = finalResponse.OutputItems[index];
                    if (item != null)
                    {
                        outputByIndex[index] = item;
                    }
                }
            }
        }

        var output = outputByIndex.Count > 0
            ? MapResponsesOutputItems([.. outputByIndex.Values])
            : BuildResponsesFallbackOutput(assistantText, assistantRefusal, streamToolCalls);

        var tools = MapResponsesTools(requestOptions);
        var thinkingBudget = ResolveResponsesThinkingBudget(requestOptions);

        var responseId = finalResponse?.Id ?? string.Empty;
        var responseModel = string.IsNullOrWhiteSpace(finalResponse?.Model) ? modelName : finalResponse.Model;
        var usage = MapResponsesUsage(finalResponse?.Usage);
        var stopReason = string.IsNullOrWhiteSpace(eventStopReason)
            ? NormalizeResponsesStopReason(finalResponse)
            : OpenAIJsonHelpers.NormalizeStopReason(eventStopReason);

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
            SystemPrompt = systemPrompt,
            MaxTokens = requestOptions?.MaxOutputTokenCount,
            Temperature = ReadNullableDoubleProperty(requestOptions, "Temperature"),
            TopP = ReadNullableDoubleProperty(requestOptions, "TopP"),
            ToolChoice = CanonicalToolChoice(requestOptions?.ToolChoice),
            ThinkingEnabled = ResolveResponsesThinkingEnabled(requestOptions),
            Input = input,
            Output = output,
            Tools = tools,
            Usage = usage,
            StopReason = stopReason,
            Tags = new Dictionary<string, string>(effective.Tags, StringComparer.Ordinal),
            Metadata = MetadataWithThinkingBudget(effective.Metadata, thinkingBudget),
            Artifacts = BuildResponsesArtifactsForStream(
                effective,
                modelName,
                systemPrompt,
                input,
                tools,
                summary
            ),
            Mode = GenerationMode.Stream,
        };

        GenerationValidator.Validate(generation);
        return generation;
    }

    public static Generation ChatCompletionsFromStream(
        string modelName,
        IReadOnlyList<ChatMessage> messages,
        ChatCompletionOptions? requestOptions,
        OpenAIChatCompletionsStreamSummary summary,
        OpenAISigilOptions? options = null
    )
    {
        ArgumentNullException.ThrowIfNull(summary);

        if (summary.FinalResponse != null)
        {
            var finalGeneration = ChatCompletionsFromRequestResponse(modelName, messages, requestOptions, summary.FinalResponse, options);
            finalGeneration.Mode = GenerationMode.Stream;
            return AppendChatCompletionsStreamEventsArtifact(finalGeneration, summary, options);
        }

        if (summary.Updates.Count == 0)
        {
            throw new ArgumentException("stream summary must contain updates or a final response", nameof(summary));
        }

        var effective = options ?? new OpenAISigilOptions();
        var requestMessages = messages ?? [];
        var (input, systemPrompt) = MapRequestMessages(requestMessages);

        var responseId = string.Empty;
        var responseModel = modelName;
        var stopReason = string.Empty;
        var usage = new TokenUsage();
        var textBuilder = new StringBuilder();

        var toolCalls = new Dictionary<int, StreamToolCall>();
        var toolOrder = new List<int>();

        foreach (var update in summary.Updates)
        {
            if (!string.IsNullOrWhiteSpace(update.CompletionId))
            {
                responseId = update.CompletionId;
            }

            if (!string.IsNullOrWhiteSpace(update.Model))
            {
                responseModel = update.Model;
            }

            if (update.Usage != null)
            {
                usage = MapChatCompletionsUsage(update.Usage);
            }

            if (update.FinishReason.HasValue)
            {
                stopReason = OpenAIJsonHelpers.NormalizeStopReason(update.FinishReason.Value.ToString());
            }

            foreach (var part in update.ContentUpdate)
            {
                if (part.Kind == ChatMessageContentPartKind.Text && !string.IsNullOrWhiteSpace(part.Text))
                {
                    textBuilder.Append(part.Text);
                }

                if (part.Kind == ChatMessageContentPartKind.Refusal && !string.IsNullOrWhiteSpace(part.Refusal))
                {
                    textBuilder.Append(part.Refusal);
                }
            }

            foreach (var toolCallUpdate in update.ToolCallUpdates)
            {
                if (!toolCalls.TryGetValue(toolCallUpdate.Index, out var call))
                {
                    call = new StreamToolCall();
                    toolCalls[toolCallUpdate.Index] = call;
                    toolOrder.Add(toolCallUpdate.Index);
                }

                if (!string.IsNullOrWhiteSpace(toolCallUpdate.ToolCallId))
                {
                    call.Id = toolCallUpdate.ToolCallId;
                }

                if (!string.IsNullOrWhiteSpace(toolCallUpdate.FunctionName))
                {
                    call.Name = toolCallUpdate.FunctionName;
                }

                var chunk = toolCallUpdate.FunctionArgumentsUpdate?.ToString() ?? string.Empty;
                if (!string.IsNullOrWhiteSpace(chunk))
                {
                    call.Arguments.Append(chunk);
                }
            }
        }

        var assistantParts = new List<Part>(Math.Max(1, toolOrder.Count + 1));
        var generated = textBuilder.ToString().Trim();
        if (generated.Length > 0)
        {
            assistantParts.Add(Part.TextPart(generated));
        }

        foreach (var index in toolOrder)
        {
            if (!toolCalls.TryGetValue(index, out var call) || string.IsNullOrWhiteSpace(call.Name))
            {
                continue;
            }

            var part = Part.ToolCallPart(new ToolCall
            {
                Id = call.Id,
                Name = call.Name,
                InputJson = OpenAIJsonHelpers.ParseJsonOrString(call.Arguments.ToString()),
            });
            part.Metadata.ProviderType = "tool_call";
            assistantParts.Add(part);
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

        var tools = MapChatCompletionsTools(requestOptions);
        var thinkingBudget = ResolveChatCompletionsThinkingBudget(requestOptions);

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
            SystemPrompt = systemPrompt,
            MaxTokens = ResolveChatCompletionsRequestMaxTokens(requestOptions),
            Temperature = ReadNullableDoubleProperty(requestOptions, "Temperature"),
            TopP = ReadNullableDoubleProperty(requestOptions, "TopP"),
            ToolChoice = CanonicalToolChoice(ReadProperty(requestOptions, "ToolChoice")),
            ThinkingEnabled = ResolveChatCompletionsThinkingEnabled(requestOptions),
            Input = input,
            Output = output,
            Tools = tools,
            Usage = usage,
            StopReason = stopReason,
            Tags = new Dictionary<string, string>(effective.Tags, StringComparer.Ordinal),
            Metadata = MetadataWithThinkingBudget(effective.Metadata, thinkingBudget),
            Artifacts = BuildChatCompletionsArtifactsForStream(
                effective,
                modelName,
                systemPrompt,
                input,
                tools,
                summary
            ),
            Mode = GenerationMode.Stream,
        };

        GenerationValidator.Validate(generation);
        return generation;
    }

    private static List<string> EmbeddingInputTexts(IReadOnlyList<string>? inputs)
    {
        if (inputs == null || inputs.Count == 0)
        {
            return [];
        }

        var mapped = new List<string>(inputs.Count);
        foreach (var input in inputs)
        {
            mapped.Add(input ?? string.Empty);
        }

        return mapped;
    }

    private static (List<Message> input, string systemPrompt) MapResponsesInputItems(
        IReadOnlyList<OpenAIResponses.ResponseItem> inputItems
    )
    {
        var input = new List<Message>(inputItems.Count);
        var systemChunks = new List<string>();

        foreach (var item in inputItems)
        {
            switch (item)
            {
                case OpenAIResponses.MessageResponseItem messageItem:
                    {
                        var role = messageItem.Role;
                        if (role == OpenAIResponses.MessageRole.System || role == OpenAIResponses.MessageRole.Developer)
                        {
                            systemChunks.Add(ExtractResponsesContentText(messageItem.Content));
                            continue;
                        }

                        var parts = MapResponsesContentParts(messageItem.Content);
                        if (parts.Count == 0)
                        {
                            continue;
                        }

                        input.Add(new Message
                        {
                            Role = role == OpenAIResponses.MessageRole.Assistant
                                ? MessageRole.Assistant
                                : MessageRole.User,
                            Parts = parts,
                        });
                        continue;
                    }
                case OpenAIResponses.FunctionCallResponseItem functionCall:
                    {
                        var part = Part.ToolCallPart(new ToolCall
                        {
                            Id = functionCall.CallId,
                            Name = functionCall.FunctionName,
                            InputJson = OpenAIJsonHelpers.ToBytes(functionCall.FunctionArguments),
                        });
                        part.Metadata.ProviderType = "tool_call";
                        input.Add(new Message
                        {
                            Role = MessageRole.Assistant,
                            Parts = [part],
                        });
                        continue;
                    }
                case OpenAIResponses.FunctionCallOutputResponseItem functionOutput:
                    {
                        var outputText = functionOutput.FunctionOutput ?? string.Empty;
                        if (string.IsNullOrWhiteSpace(outputText))
                        {
                            continue;
                        }

                        var part = Part.ToolResultPart(new ToolResult
                        {
                            ToolCallId = functionOutput.CallId,
                            Content = outputText,
                            ContentJson = OpenAIJsonHelpers.ParseJsonOrString(outputText),
                        });
                        part.Metadata.ProviderType = "tool_result";
                        input.Add(new Message
                        {
                            Role = MessageRole.Tool,
                            Parts = [part],
                        });
                        continue;
                    }
                case OpenAIResponses.ReasoningResponseItem reasoningItem:
                    {
                        var summary = reasoningItem.GetSummaryText();
                        if (string.IsNullOrWhiteSpace(summary))
                        {
                            continue;
                        }

                        input.Add(new Message
                        {
                            Role = MessageRole.Assistant,
                            Parts = [Part.ThinkingPart(summary)],
                        });
                        continue;
                    }
                default:
                    {
                        var fallback = JsonSerializer.Serialize(item);
                        if (string.IsNullOrWhiteSpace(fallback))
                        {
                            continue;
                        }

                        input.Add(Message.UserTextMessage(fallback));
                        continue;
                    }
            }
        }

        return (input, OpenAIJsonHelpers.MergeSystemPrompt(systemChunks));
    }

    private static List<Message> MapResponsesOutputItems(
        List<OpenAIResponses.ResponseItem> outputItems
    )
    {
        var output = new List<Message>(outputItems.Count);

        foreach (var item in outputItems)
        {
            switch (item)
            {
                case OpenAIResponses.MessageResponseItem messageItem:
                    {
                        var parts = MapResponsesContentParts(messageItem.Content);
                        if (parts.Count == 0)
                        {
                            continue;
                        }

                        output.Add(new Message
                        {
                            Role = messageItem.Role == OpenAIResponses.MessageRole.Assistant
                                ? MessageRole.Assistant
                                : MessageRole.User,
                            Parts = parts,
                        });
                        continue;
                    }
                case OpenAIResponses.FunctionCallResponseItem functionCall:
                    {
                        var part = Part.ToolCallPart(new ToolCall
                        {
                            Id = functionCall.CallId,
                            Name = functionCall.FunctionName,
                            InputJson = OpenAIJsonHelpers.ToBytes(functionCall.FunctionArguments),
                        });
                        part.Metadata.ProviderType = "tool_call";
                        output.Add(new Message
                        {
                            Role = MessageRole.Assistant,
                            Parts = [part],
                        });
                        continue;
                    }
                case OpenAIResponses.FunctionCallOutputResponseItem functionOutput:
                    {
                        var outputText = functionOutput.FunctionOutput ?? string.Empty;
                        if (string.IsNullOrWhiteSpace(outputText))
                        {
                            continue;
                        }

                        var part = Part.ToolResultPart(new ToolResult
                        {
                            ToolCallId = functionOutput.CallId,
                            Content = outputText,
                            ContentJson = OpenAIJsonHelpers.ParseJsonOrString(outputText),
                        });
                        part.Metadata.ProviderType = "tool_result";
                        output.Add(new Message
                        {
                            Role = MessageRole.Tool,
                            Parts = [part],
                        });
                        continue;
                    }
                case OpenAIResponses.ReasoningResponseItem reasoningItem:
                    {
                        var summary = reasoningItem.GetSummaryText();
                        if (string.IsNullOrWhiteSpace(summary))
                        {
                            continue;
                        }

                        output.Add(new Message
                        {
                            Role = MessageRole.Assistant,
                            Parts = [Part.ThinkingPart(summary)],
                        });
                        continue;
                    }
                default:
                    {
                        output.Add(Message.AssistantTextMessage(JsonSerializer.Serialize(item)));
                        continue;
                    }
            }
        }

        return output;
    }

    private static List<Part> MapResponsesContentParts(
        IEnumerable<OpenAIResponses.ResponseContentPart> contentParts
    )
    {
        var parts = contentParts is ICollection<OpenAIResponses.ResponseContentPart> collection
            ? new List<Part>(collection.Count)
            : [];
        foreach (var contentPart in contentParts)
        {
            switch (contentPart.Kind)
            {
                case OpenAIResponses.ResponseContentPartKind.InputText:
                case OpenAIResponses.ResponseContentPartKind.OutputText:
                    if (!string.IsNullOrWhiteSpace(contentPart.Text))
                    {
                        parts.Add(Part.TextPart(contentPart.Text));
                    }
                    break;
                case OpenAIResponses.ResponseContentPartKind.Refusal:
                    if (!string.IsNullOrWhiteSpace(contentPart.Refusal))
                    {
                        parts.Add(Part.TextPart(contentPart.Refusal));
                    }
                    break;
                case OpenAIResponses.ResponseContentPartKind.InputFile:
                    {
                        var fileRef = contentPart.InputFileId ?? contentPart.InputFilename ?? "inline_file";
                        parts.Add(Part.TextPart("[input_file:" + fileRef + "]"));
                        break;
                    }
                case OpenAIResponses.ResponseContentPartKind.InputImage:
                    {
                        var imageRef = contentPart.InputImageFileId ?? "inline_image";
                        parts.Add(Part.TextPart("[input_image:" + imageRef + "]"));
                        break;
                    }
            }
        }

        return parts;
    }

    private static string ExtractResponsesContentText(
        IEnumerable<OpenAIResponses.ResponseContentPart> contentParts
    )
    {
        var chunks = contentParts is ICollection<OpenAIResponses.ResponseContentPart> collection
            ? new List<string>(collection.Count)
            : [];
        foreach (var contentPart in contentParts)
        {
            if ((contentPart.Kind == OpenAIResponses.ResponseContentPartKind.InputText
                    || contentPart.Kind == OpenAIResponses.ResponseContentPartKind.OutputText)
                && !string.IsNullOrWhiteSpace(contentPart.Text))
            {
                chunks.Add(contentPart.Text);
                continue;
            }

            if (contentPart.Kind == OpenAIResponses.ResponseContentPartKind.Refusal
                && !string.IsNullOrWhiteSpace(contentPart.Refusal))
            {
                chunks.Add(contentPart.Refusal);
            }
        }

        return string.Join("\n", chunks);
    }

    private static List<Message> BuildResponsesFallbackOutput(
        StringBuilder assistantText,
        StringBuilder assistantRefusal,
        Dictionary<string, ResponsesStreamToolCall> streamToolCalls
    )
    {
        var output = new List<Message>();
        var assistantParts = new List<Part>();

        var text = assistantText.ToString().Trim();
        if (text.Length > 0)
        {
            assistantParts.Add(Part.TextPart(text));
        }

        var refusal = assistantRefusal.ToString().Trim();
        if (refusal.Length > 0)
        {
            assistantParts.Add(Part.TextPart(refusal));
        }

        foreach (var streamToolCall in streamToolCalls.Values)
        {
            if (string.IsNullOrWhiteSpace(streamToolCall.Name))
            {
                continue;
            }

            var part = Part.ToolCallPart(new ToolCall
            {
                Id = streamToolCall.Id,
                Name = streamToolCall.Name,
                InputJson = OpenAIJsonHelpers.ParseJsonOrString(streamToolCall.Arguments.ToString()),
            });
            part.Metadata.ProviderType = "tool_call";
            assistantParts.Add(part);
        }

        if (assistantParts.Count > 0)
        {
            output.Add(new Message
            {
                Role = MessageRole.Assistant,
                Parts = assistantParts,
            });
        }

        return output;
    }

    private static void CaptureResponsesStreamToolCall(
        Dictionary<string, ResponsesStreamToolCall> streamToolCalls,
        string? itemId,
        string? chunk,
        string? functionName,
        string? finalArguments
    )
    {
        if (string.IsNullOrWhiteSpace(itemId))
        {
            return;
        }

        if (!streamToolCalls.TryGetValue(itemId, out var toolCall))
        {
            toolCall = new ResponsesStreamToolCall
            {
                Id = itemId,
            };
            streamToolCalls[itemId] = toolCall;
        }

        if (!string.IsNullOrWhiteSpace(functionName))
        {
            toolCall.Name = functionName;
        }

        if (!string.IsNullOrWhiteSpace(finalArguments))
        {
            toolCall.Arguments.Clear();
            toolCall.Arguments.Append(finalArguments);
            return;
        }

        if (!string.IsNullOrWhiteSpace(chunk))
        {
            toolCall.Arguments.Append(chunk);
        }
    }

    private static void CaptureResponsesStreamToolCallFromItem(
        Dictionary<string, ResponsesStreamToolCall> streamToolCalls,
        OpenAIResponses.ResponseItem responseItem
    )
    {
        if (responseItem is OpenAIResponses.FunctionCallResponseItem functionCall)
        {
            CaptureResponsesStreamToolCall(
                streamToolCalls,
                responseItem.Id,
                null,
                functionCall.FunctionName,
                functionCall.FunctionArguments?.ToString()
            );
        }
    }

    private static List<ToolDefinition> MapResponsesTools(
        OpenAIResponses.CreateResponseOptions? requestOptions
    )
    {
        var mapped = new List<ToolDefinition>();
        if (requestOptions == null)
        {
            return mapped;
        }

        foreach (var tool in requestOptions.Tools)
        {
            if (tool is OpenAIResponses.FunctionTool functionTool
                && !string.IsNullOrWhiteSpace(functionTool.FunctionName))
            {
                mapped.Add(new ToolDefinition
                {
                    Name = functionTool.FunctionName,
                    Description = functionTool.FunctionDescription ?? string.Empty,
                    Type = "function",
                    InputSchemaJson = OpenAIJsonHelpers.ToBytes(functionTool.FunctionParameters),
                });
                continue;
            }

            var typeName = tool.GetType().Name;
            var normalizedType = typeName.EndsWith("Tool", StringComparison.Ordinal)
                ? typeName[..^4]
                : typeName;
            mapped.Add(new ToolDefinition
            {
                Name = normalizedType.ToLowerInvariant(),
                Description = string.Empty,
                Type = normalizedType.ToLowerInvariant(),
                InputSchemaJson = [],
            });
        }

        return mapped;
    }

    private static TokenUsage MapResponsesUsage(
        OpenAIResponses.ResponseTokenUsage? usage
    )
    {
        if (usage == null)
        {
            return new TokenUsage();
        }

        var mapped = new TokenUsage
        {
            InputTokens = usage.InputTokenCount,
            OutputTokens = usage.OutputTokenCount,
            TotalTokens = usage.TotalTokenCount,
            CacheReadInputTokens = usage.InputTokenDetails?.CachedTokenCount ?? 0,
            ReasoningTokens = usage.OutputTokenDetails?.ReasoningTokenCount ?? 0,
        };
        if (mapped.TotalTokens == 0)
        {
            mapped.TotalTokens = mapped.InputTokens + mapped.OutputTokens;
        }
        return mapped;
    }

    private static string NormalizeResponsesStopReason(
        OpenAIResponses.ResponseResult? response
    )
    {
        if (response == null || response.Status == null)
        {
            return string.Empty;
        }

        return response.Status.Value switch
        {
            OpenAIResponses.ResponseStatus.Completed => "stop",
            OpenAIResponses.ResponseStatus.Cancelled => "cancelled",
            OpenAIResponses.ResponseStatus.InProgress => "in_progress",
            OpenAIResponses.ResponseStatus.Queued => "queued",
            OpenAIResponses.ResponseStatus.Failed => "error",
            OpenAIResponses.ResponseStatus.Incomplete => NormalizeIncompleteStopReason(response.IncompleteStatusDetails),
            _ => OpenAIJsonHelpers.NormalizeStopReason(response.Status.Value.ToString()),
        };
    }

    private static string NormalizeIncompleteStopReason(
        OpenAIResponses.ResponseIncompleteStatusDetails? incompleteStatusDetails
    )
    {
        var reason = incompleteStatusDetails?.Reason?.ToString() ?? string.Empty;
        var normalized = reason.Trim().ToLowerInvariant();
        return normalized switch
        {
            "max_output_tokens" => "length",
            "content_filter" => "content_filter",
            _ => normalized.Length == 0 ? "incomplete" : normalized,
        };
    }

    private static List<Artifact> BuildResponsesArtifactsForRequestResponse(
        OpenAISigilOptions options,
        string modelName,
        string systemPrompt,
        IReadOnlyList<Message> input,
        IReadOnlyList<Message> output,
        List<ToolDefinition> tools,
        OpenAIResponses.ResponseResult response
    )
    {
        var artifacts = new List<Artifact>(3);

        if (options.IncludeRequestArtifact)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.Request, "openai.responses.request", new
            {
                model = modelName,
                system_prompt = systemPrompt,
                input,
            }));
        }

        if (options.IncludeResponseArtifact)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.Response, "openai.responses.response", new
            {
                id = response.Id,
                model = response.Model,
                status = response.Status?.ToString(),
                stop_reason = NormalizeResponsesStopReason(response),
                output,
                usage = MapResponsesUsage(response.Usage),
            }));
        }

        if (options.IncludeToolsArtifact && tools.Count > 0)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.Tools, "openai.responses.tools", tools));
        }

        return artifacts;
    }

    private static List<Artifact> BuildResponsesArtifactsForStream(
        OpenAISigilOptions options,
        string modelName,
        string systemPrompt,
        IReadOnlyList<Message> input,
        List<ToolDefinition> tools,
        OpenAIResponsesStreamSummary summary
    )
    {
        var artifacts = new List<Artifact>(4);

        if (options.IncludeRequestArtifact)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.Request, "openai.responses.request", new
            {
                model = modelName,
                system_prompt = systemPrompt,
                input,
            }));
        }

        if (options.IncludeToolsArtifact && tools.Count > 0)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.Tools, "openai.responses.tools", tools));
        }

        if (options.IncludeEventsArtifact)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.ProviderEvent, "openai.responses.stream_events", summary.Events));
        }

        if (options.IncludeResponseArtifact && summary.FinalResponse != null)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.Response, "openai.responses.response", summary.FinalResponse));
        }

        return artifacts;
    }

    private static Generation AppendResponsesStreamEventsArtifact(
        Generation generation,
        OpenAIResponsesStreamSummary summary,
        OpenAISigilOptions? options
    )
    {
        var effective = options ?? new OpenAISigilOptions();
        if (!effective.IncludeEventsArtifact || summary.Events.Count == 0)
        {
            return generation;
        }

        generation.Artifacts.Add(Artifact.JsonArtifact(
            ArtifactKind.ProviderEvent,
            "openai.responses.stream_events",
            summary.Events
        ));
        return generation;
    }

    private static bool? ResolveResponsesThinkingEnabled(
        OpenAIResponses.CreateResponseOptions? requestOptions
    )
    {
        return requestOptions?.ReasoningOptions == null ? null : true;
    }

    private static long? ResolveResponsesThinkingBudget(
        OpenAIResponses.CreateResponseOptions? requestOptions
    )
    {
        if (requestOptions?.ReasoningOptions == null)
        {
            return null;
        }

        return ReadNullableLongProperty(
            requestOptions.ReasoningOptions,
            "BudgetTokens",
            "ThinkingBudget",
            "MaxOutputTokens",
            "MaxCompletionTokens",
            "budget_tokens",
            "thinking_budget",
            "max_output_tokens"
        );
    }

    private static (List<Message> input, string systemPrompt) MapRequestMessages(IReadOnlyList<ChatMessage> messages)
    {
        var input = new List<Message>(messages.Count);
        var systemChunks = new List<string>();

        foreach (var message in messages)
        {
            switch (message)
            {
                case SystemChatMessage:
                    systemChunks.Add(ExtractMessageText(message.Content));
                    continue;
                case ToolChatMessage toolMessage:
                    {
                        var content = ExtractMessageText(toolMessage.Content);
                        if (content.Length == 0)
                        {
                            continue;
                        }

                        var part = Part.ToolResultPart(new ToolResult
                        {
                            ToolCallId = toolMessage.ToolCallId,
                            Content = content,
                            ContentJson = OpenAIJsonHelpers.ParseJsonOrString(content),
                        });
                        part.Metadata.ProviderType = "tool_result";

                        input.Add(new Message
                        {
                            Role = MessageRole.Tool,
                            Parts = [part],
                        });
                        continue;
                    }
                case AssistantChatMessage assistantMessage:
                    {
                        var parts = new List<Part>();
                        parts.AddRange(MapContentParts(assistantMessage.Content));

                        foreach (var call in assistantMessage.ToolCalls)
                        {
                            var part = Part.ToolCallPart(new ToolCall
                            {
                                Id = call.Id,
                                Name = call.FunctionName,
                                InputJson = OpenAIJsonHelpers.ToBytes(call.FunctionArguments),
                            });
                            part.Metadata.ProviderType = "tool_call";
                            parts.Add(part);
                        }

                        if (parts.Count > 0)
                        {
                            input.Add(new Message
                            {
                                Role = MessageRole.Assistant,
                                Parts = parts,
                            });
                        }

                        continue;
                    }
                case UserChatMessage:
                    {
                        var parts = MapContentParts(message.Content);
                        if (parts.Count > 0)
                        {
                            input.Add(new Message
                            {
                                Role = MessageRole.User,
                                Parts = parts,
                            });
                        }

                        continue;
                    }
                default:
                    {
                        if (message.GetType().Name == "DeveloperChatMessage")
                        {
                            systemChunks.Add(ExtractMessageText(message.Content));
                            continue;
                        }

                        var parts = MapContentParts(message.Content);
                        if (parts.Count > 0)
                        {
                            input.Add(new Message
                            {
                                Role = MessageRole.User,
                                Parts = parts,
                            });
                        }

                        continue;
                    }
            }
        }

        return (input, OpenAIJsonHelpers.MergeSystemPrompt(systemChunks));
    }

    private static List<Message> MapResponseMessages(ChatCompletion response)
    {
        var parts = new List<Part>();

        parts.AddRange(MapContentParts(response.Content));

        if (!string.IsNullOrWhiteSpace(response.Refusal))
        {
            parts.Add(Part.TextPart(response.Refusal));
        }

        foreach (var toolCall in response.ToolCalls)
        {
            var part = Part.ToolCallPart(new ToolCall
            {
                Id = toolCall.Id,
                Name = toolCall.FunctionName,
                InputJson = OpenAIJsonHelpers.ToBytes(toolCall.FunctionArguments),
            });
            part.Metadata.ProviderType = "tool_call";
            parts.Add(part);
        }

        if (parts.Count == 0)
        {
            return [];
        }

        return
        [
            new()
            {
                Role = MessageRole.Assistant,
                Parts = parts,
            },
        ];
    }

    private static List<Part> MapContentParts(ChatMessageContent? content)
    {
        var parts = new List<Part>();
        if (content == null)
        {
            return parts;
        }

        foreach (var item in content)
        {
            if (item.Kind == ChatMessageContentPartKind.Text && !string.IsNullOrWhiteSpace(item.Text))
            {
                parts.Add(Part.TextPart(item.Text));
                continue;
            }

            if (item.Kind == ChatMessageContentPartKind.Refusal && !string.IsNullOrWhiteSpace(item.Refusal))
            {
                parts.Add(Part.TextPart(item.Refusal));
            }
        }

        return parts;
    }

    private static string ExtractMessageText(ChatMessageContent? content)
    {
        if (content == null)
        {
            return string.Empty;
        }

        var chunks = new List<string>(content.Count);
        foreach (var item in content)
        {
            if (item.Kind == ChatMessageContentPartKind.Text && !string.IsNullOrWhiteSpace(item.Text))
            {
                chunks.Add(item.Text);
                continue;
            }

            if (item.Kind == ChatMessageContentPartKind.Refusal && !string.IsNullOrWhiteSpace(item.Refusal))
            {
                chunks.Add(item.Refusal);
            }
        }

        return string.Join("\n", chunks);
    }

    private static List<ToolDefinition> MapChatCompletionsTools(ChatCompletionOptions? requestOptions)
    {
        var mapped = new List<ToolDefinition>();
        if (requestOptions == null)
        {
            return mapped;
        }

        foreach (var tool in requestOptions.Tools)
        {
            if (string.IsNullOrWhiteSpace(tool.FunctionName))
            {
                continue;
            }

            mapped.Add(new ToolDefinition
            {
                Name = tool.FunctionName,
                Description = tool.FunctionDescription ?? string.Empty,
                Type = tool.Kind.ToString().ToLowerInvariant(),
                InputSchemaJson = OpenAIJsonHelpers.ToBytes(tool.FunctionParameters),
            });
        }

        return mapped;
    }

    private static TokenUsage MapChatCompletionsUsage(ChatTokenUsage? usage)
    {
        if (usage == null)
        {
            return new TokenUsage();
        }

        var mapped = new TokenUsage
        {
            InputTokens = usage.InputTokenCount,
            OutputTokens = usage.OutputTokenCount,
            TotalTokens = usage.TotalTokenCount,
            CacheReadInputTokens = usage.InputTokenDetails?.CachedTokenCount ?? 0,
            ReasoningTokens = usage.OutputTokenDetails?.ReasoningTokenCount ?? 0,
        };

        if (mapped.TotalTokens == 0)
        {
            mapped.TotalTokens = mapped.InputTokens + mapped.OutputTokens;
        }

        return mapped;
    }

    private static List<Artifact> BuildChatCompletionsArtifactsForRequestResponse(
        OpenAISigilOptions options,
        string modelName,
        string systemPrompt,
        IReadOnlyList<Message> input,
        IReadOnlyList<Message> output,
        List<ToolDefinition> tools,
        ChatCompletion response
    )
    {
        var artifacts = new List<Artifact>(3);

        if (options.IncludeRequestArtifact)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.Request, "openai.chat.request", new
            {
                model = modelName,
                system_prompt = systemPrompt,
                input,
            }));
        }

        if (options.IncludeResponseArtifact)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.Response, "openai.chat.response", new
            {
                id = response.Id,
                model = response.Model,
                finish_reason = OpenAIJsonHelpers.NormalizeStopReason(response.FinishReason.ToString()),
                output,
                usage = MapChatCompletionsUsage(response.Usage),
            }));
        }

        if (options.IncludeToolsArtifact && tools.Count > 0)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.Tools, "openai.chat.tools", tools));
        }

        return artifacts;
    }

    private static List<Artifact> BuildChatCompletionsArtifactsForStream(
        OpenAISigilOptions options,
        string modelName,
        string systemPrompt,
        IReadOnlyList<Message> input,
        List<ToolDefinition> tools,
        OpenAIChatCompletionsStreamSummary summary
    )
    {
        var artifacts = new List<Artifact>(4);

        if (options.IncludeRequestArtifact)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.Request, "openai.chat.request", new
            {
                model = modelName,
                system_prompt = systemPrompt,
                input,
            }));
        }

        if (options.IncludeToolsArtifact && tools.Count > 0)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.Tools, "openai.chat.tools", tools));
        }

        if (options.IncludeEventsArtifact)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.ProviderEvent, "openai.chat.stream_events", summary.Updates));
        }

        if (options.IncludeResponseArtifact && summary.FinalResponse != null)
        {
            artifacts.Add(Artifact.JsonArtifact(ArtifactKind.Response, "openai.chat.response", summary.FinalResponse));
        }

        return artifacts;
    }

    private static Generation AppendChatCompletionsStreamEventsArtifact(
        Generation generation,
        OpenAIChatCompletionsStreamSummary summary,
        OpenAISigilOptions? options
    )
    {
        var effective = options ?? new OpenAISigilOptions();
        if (!effective.IncludeEventsArtifact || summary.Updates.Count == 0)
        {
            return generation;
        }

        generation.Artifacts.Add(Artifact.JsonArtifact(
            ArtifactKind.ProviderEvent,
            "openai.chat.stream_events",
            summary.Updates
        ));
        return generation;
    }

    private sealed class StreamToolCall
    {
        public string Id { get; set; } = string.Empty;

        public string Name { get; set; } = string.Empty;

        public StringBuilder Arguments { get; } = new();
    }

    private sealed class ResponsesStreamToolCall
    {
        public string Id { get; set; } = string.Empty;

        public string Name { get; set; } = string.Empty;

        public StringBuilder Arguments { get; } = new();
    }

    private static long? ResolveChatCompletionsRequestMaxTokens(ChatCompletionOptions? requestOptions)
    {
        return ReadNullableLongProperty(requestOptions, "MaxCompletionTokens", "MaxTokens", "MaxOutputTokenCount");
    }

    private static bool? ResolveChatCompletionsThinkingEnabled(ChatCompletionOptions? requestOptions)
    {
        if (requestOptions == null)
        {
            return null;
        }

        var reasoning = ReadProperty(requestOptions, "Reasoning")
            ?? ReadProperty(requestOptions, "ReasoningEffortLevel")
            ?? ReadProperty(requestOptions, "ReasoningOptions");
        return reasoning == null ? null : true;
    }

    private static long? ResolveChatCompletionsThinkingBudget(ChatCompletionOptions? requestOptions)
    {
        if (requestOptions == null)
        {
            return null;
        }

        var reasoning = ReadProperty(requestOptions, "Reasoning")
            ?? ReadProperty(requestOptions, "ReasoningOptions");
        if (reasoning == null)
        {
            return null;
        }

        return ReadNullableLongProperty(
            reasoning,
            "BudgetTokens",
            "ThinkingBudget",
            "MaxOutputTokens",
            "MaxCompletionTokens",
            "budget_tokens",
            "thinking_budget",
            "max_output_tokens"
        );
    }

    private static object? ReadProperty(object? instance, params string[] names)
    {
        if (instance == null)
        {
            return null;
        }

        if (instance is IReadOnlyDictionary<string, object?> readOnlyMap)
        {
            foreach (var name in names)
            {
                if (readOnlyMap.TryGetValue(name, out var mappedValue) && mappedValue != null)
                {
                    return mappedValue;
                }
            }
        }

        if (instance is IDictionary<string, object?> map)
        {
            foreach (var name in names)
            {
                if (map.TryGetValue(name, out var mappedValue) && mappedValue != null)
                {
                    return mappedValue;
                }
            }
        }

        if (instance is JsonElement json && json.ValueKind == JsonValueKind.Object)
        {
            foreach (var name in names)
            {
                if (json.TryGetProperty(name, out var value))
                {
                    return value;
                }
            }
        }

        var flags = System.Reflection.BindingFlags.Instance
            | System.Reflection.BindingFlags.Public
            | System.Reflection.BindingFlags.NonPublic
            | System.Reflection.BindingFlags.IgnoreCase;

        foreach (var name in names)
        {
            var property = instance.GetType().GetProperty(name, flags);
            if (property == null)
            {
                var field = instance.GetType().GetField(name, flags);
                if (field == null)
                {
                    continue;
                }

                var fieldValue = field.GetValue(instance);
                if (fieldValue != null)
                {
                    return fieldValue;
                }

                continue;
            }

            var value = property.GetValue(instance);
            if (value != null)
            {
                return value;
            }
        }

        return null;
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

    private static long? ReadNullableLongProperty(object? instance, params string[] names)
    {
        var value = ReadProperty(instance, names);
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
            float v => (long)v,
            double v => (long)v,
            decimal v => (long)v,
            _ when long.TryParse(value.ToString(), out var parsed) => parsed,
            _ => null,
        };
    }

    private static double? ReadNullableDoubleProperty(object? instance, params string[] names)
    {
        var value = ReadProperty(instance, names);
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

        var structured = TryMapStructuredToolChoice(value);
        if (!string.IsNullOrWhiteSpace(structured))
        {
            return structured;
        }

        try
        {
            var element = JsonSerializer.SerializeToElement(value);
            var canonical = CanonicalJson(element);
            if (canonical == "{}")
            {
                var normalized = value.ToString()?.Trim().ToLowerInvariant();
                return string.IsNullOrWhiteSpace(normalized) ? null : normalized;
            }

            return canonical;
        }
        catch
        {
            var normalized = value.ToString()?.Trim().ToLowerInvariant();
            return string.IsNullOrWhiteSpace(normalized) ? null : normalized;
        }
    }

    private static string? TryMapStructuredToolChoice(object value)
    {
        var kind = ReadProperty(value, "Kind", "Type", "Mode", "_type", "_predefinedValue")?.ToString()?.Trim();
        var functionName = ReadProperty(value, "FunctionName", "Name")?.ToString()?.Trim();

        if (string.IsNullOrWhiteSpace(functionName))
        {
            var function = ReadProperty(value, "Function", "_function");
            functionName = ReadProperty(function, "Name", "<Name>k__BackingField")?.ToString()?.Trim();
        }

        var normalizedKind = kind?.ToLowerInvariant();
        if (string.IsNullOrWhiteSpace(functionName)
            && (normalizedKind == "none" || normalizedKind == "auto" || normalizedKind == "required"))
        {
            return normalizedKind;
        }

        if (string.IsNullOrWhiteSpace(kind) && string.IsNullOrWhiteSpace(functionName))
        {
            return null;
        }

        var toolChoice = new SortedDictionary<string, object?>(StringComparer.Ordinal);
        if (!string.IsNullOrWhiteSpace(kind))
        {
            toolChoice["type"] = normalizedKind;
        }

        if (!string.IsNullOrWhiteSpace(functionName))
        {
            toolChoice["function"] = new SortedDictionary<string, object?>(StringComparer.Ordinal)
            {
                ["name"] = functionName,
            };
        }

        return JsonSerializer.Serialize(toolChoice);
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
}
