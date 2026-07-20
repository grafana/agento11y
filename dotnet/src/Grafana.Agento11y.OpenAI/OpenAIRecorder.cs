using global::OpenAI.Chat;
using global::OpenAI.Embeddings;
using global::OpenAI.Responses;

namespace Grafana.Sigil.OpenAI;

public static class OpenAIRecorder
{
    public static async Task<ChatCompletion> CompleteChatAsync(
        SigilClient client,
        ChatClient provider,
        IEnumerable<ChatMessage> messages,
        ChatCompletionOptions? requestOptions = null,
        OpenAISigilOptions? options = null,
        CancellationToken cancellationToken = default
    )
    {
        ArgumentNullException.ThrowIfNull(provider);

        var effective = options ?? new OpenAISigilOptions();
        var modelName = ResolveInitialModelName(effective, provider.Model);

        return await CompleteChatAsync(
            client,
            messages,
            async (requestMessages, opts, ct) =>
            {
                var result = await provider.CompleteChatAsync(requestMessages, opts, ct).ConfigureAwait(false);
                return result.Value;
            },
            requestOptions,
            effective with { ModelName = modelName },
            cancellationToken
        ).ConfigureAwait(false);
    }

    public static async Task<ChatCompletion> CompleteChatAsync(
        SigilClient client,
        IEnumerable<ChatMessage> messages,
        Func<IEnumerable<ChatMessage>, ChatCompletionOptions?, CancellationToken, Task<ChatCompletion>> invoke,
        ChatCompletionOptions? requestOptions = null,
        OpenAISigilOptions? options = null,
        CancellationToken cancellationToken = default
    )
    {
        ArgumentNullException.ThrowIfNull(client);
        ArgumentNullException.ThrowIfNull(invoke);

        var effective = options ?? new OpenAISigilOptions();
        var messageList = messages?.ToList() ?? throw new ArgumentNullException(nameof(messages));
        var modelName = ResolveInitialModelName(effective, fallback: null);

        var recorder = client.StartGeneration(new GenerationStart
        {
            ConversationId = effective.ConversationId,
            AgentName = effective.AgentName,
            AgentVersion = effective.AgentVersion,
            Model = new ModelRef
            {
                Provider = effective.ProviderName,
                Name = modelName,
            },
            Mode = GenerationMode.Sync,
        });

        try
        {
            var response = await invoke(messageList, requestOptions, cancellationToken).ConfigureAwait(false);
            Exception? mappingError = null;
            Generation generation;

            try
            {
                var responseModel = string.IsNullOrWhiteSpace(response?.Model) ? modelName : response.Model;
                generation = OpenAIGenerationMapper.ChatCompletionsFromRequestResponse(
                    responseModel,
                    messageList,
                    requestOptions,
                    response!,
                    effective with { ModelName = responseModel }
                );
            }
            catch (Exception ex)
            {
                mappingError = ex;
                generation = new Generation();
            }

            recorder.SetResult(generation, mappingError);
            return response!;
        }
        catch (Exception ex)
        {
            recorder.SetCallError(ex);
            throw;
        }
        finally
        {
            recorder.End();
        }
    }

    public static async Task<OpenAIChatCompletionsStreamSummary> CompleteChatStreamingAsync(
        SigilClient client,
        ChatClient provider,
        IEnumerable<ChatMessage> messages,
        ChatCompletionOptions? requestOptions = null,
        OpenAISigilOptions? options = null,
        CancellationToken cancellationToken = default
    )
    {
        ArgumentNullException.ThrowIfNull(provider);

        var effective = options ?? new OpenAISigilOptions();
        var modelName = ResolveInitialModelName(effective, provider.Model);

        return await CompleteChatStreamingAsync(
            client,
            messages,
            (requestMessages, opts, ct) => provider.CompleteChatStreamingAsync(requestMessages, opts, ct),
            requestOptions,
            effective with { ModelName = modelName },
            cancellationToken
        ).ConfigureAwait(false);
    }

    public static async Task<OpenAIChatCompletionsStreamSummary> CompleteChatStreamingAsync(
        SigilClient client,
        IEnumerable<ChatMessage> messages,
        Func<IEnumerable<ChatMessage>, ChatCompletionOptions?, CancellationToken, IAsyncEnumerable<StreamingChatCompletionUpdate>> invoke,
        ChatCompletionOptions? requestOptions = null,
        OpenAISigilOptions? options = null,
        CancellationToken cancellationToken = default
    )
    {
        ArgumentNullException.ThrowIfNull(client);
        ArgumentNullException.ThrowIfNull(invoke);

        var effective = options ?? new OpenAISigilOptions();
        var messageList = messages?.ToList() ?? throw new ArgumentNullException(nameof(messages));
        var modelName = ResolveInitialModelName(effective, fallback: null);

        var recorder = client.StartStreamingGeneration(new GenerationStart
        {
            ConversationId = effective.ConversationId,
            AgentName = effective.AgentName,
            AgentVersion = effective.AgentVersion,
            Model = new ModelRef
            {
                Provider = effective.ProviderName,
                Name = modelName,
            },
            Mode = GenerationMode.Stream,
        });

        try
        {
            var summary = new OpenAIChatCompletionsStreamSummary();
            await foreach (var update in invoke(messageList, requestOptions, cancellationToken).WithCancellation(cancellationToken))
            {
                if (!summary.FirstChunkAt.HasValue)
                {
                    var firstChunkAt = DateTimeOffset.UtcNow;
                    summary.FirstChunkAt = firstChunkAt;
                    recorder.SetFirstTokenAt(firstChunkAt);
                }
                summary.Updates.Add(update);
            }

            Exception? mappingError = null;
            Generation generation;
            try
            {
                generation = OpenAIGenerationMapper.ChatCompletionsFromStream(modelName, messageList, requestOptions, summary, effective);
            }
            catch (Exception ex)
            {
                mappingError = ex;
                generation = new Generation();
            }

            recorder.SetResult(generation, mappingError);
            return summary;
        }
        catch (Exception ex)
        {
            recorder.SetCallError(ex);
            throw;
        }
        finally
        {
            recorder.End();
        }
    }

    public static async Task<ResponseResult> CreateResponseAsync(
        SigilClient client,
        ResponsesClient provider,
        IEnumerable<ResponseItem> inputItems,
        CreateResponseOptions? requestOptions = null,
        OpenAISigilOptions? options = null,
        CancellationToken cancellationToken = default
    )
    {
        ArgumentNullException.ThrowIfNull(provider);

        var effective = options ?? new OpenAISigilOptions();
        var modelName = ResolveInitialModelName(effective, provider.GetType().GetProperty("Model")?.GetValue(provider) as string);

        return await CreateResponseAsync(
            client,
            inputItems,
            async (items, opts, ct) =>
            {
                var callOptions = BuildResponseCreateOptions(items, opts);
                var result = await provider.CreateResponseAsync(callOptions, ct).ConfigureAwait(false);
                return result.Value;
            },
            requestOptions,
            effective with { ModelName = modelName },
            cancellationToken
        ).ConfigureAwait(false);
    }

    public static async Task<ResponseResult> CreateResponseAsync(
        SigilClient client,
        IEnumerable<ResponseItem> inputItems,
        Func<IEnumerable<ResponseItem>, CreateResponseOptions?, CancellationToken, Task<ResponseResult>> invoke,
        CreateResponseOptions? requestOptions = null,
        OpenAISigilOptions? options = null,
        CancellationToken cancellationToken = default
    )
    {
        ArgumentNullException.ThrowIfNull(client);
        ArgumentNullException.ThrowIfNull(invoke);

        var effective = options ?? new OpenAISigilOptions();
        var itemList = inputItems?.ToList() ?? throw new ArgumentNullException(nameof(inputItems));
        var modelName = ResolveInitialModelName(effective, fallback: null);

        var recorder = client.StartGeneration(new GenerationStart
        {
            ConversationId = effective.ConversationId,
            AgentName = effective.AgentName,
            AgentVersion = effective.AgentVersion,
            Model = new ModelRef
            {
                Provider = effective.ProviderName,
                Name = modelName,
            },
            Mode = GenerationMode.Sync,
        });

        try
        {
            var response = await invoke(itemList, requestOptions, cancellationToken).ConfigureAwait(false);
            Exception? mappingError = null;
            Generation generation;

            try
            {
                var responseModel = string.IsNullOrWhiteSpace(response?.Model) ? modelName : response.Model;
                generation = OpenAIGenerationMapper.ResponsesFromRequestResponse(
                    responseModel,
                    itemList,
                    requestOptions,
                    response!,
                    effective with { ModelName = responseModel }
                );
            }
            catch (Exception ex)
            {
                mappingError = ex;
                generation = new Generation();
            }

            recorder.SetResult(generation, mappingError);
            return response!;
        }
        catch (Exception ex)
        {
            recorder.SetCallError(ex);
            throw;
        }
        finally
        {
            recorder.End();
        }
    }

    public static async Task<OpenAIResponsesStreamSummary> CreateResponseStreamingAsync(
        SigilClient client,
        ResponsesClient provider,
        IEnumerable<ResponseItem> inputItems,
        CreateResponseOptions? requestOptions = null,
        OpenAISigilOptions? options = null,
        CancellationToken cancellationToken = default
    )
    {
        ArgumentNullException.ThrowIfNull(provider);

        var effective = options ?? new OpenAISigilOptions();
        var modelName = ResolveInitialModelName(effective, provider.GetType().GetProperty("Model")?.GetValue(provider) as string);

        return await CreateResponseStreamingAsync(
            client,
            inputItems,
            (items, opts, ct) =>
            {
                var callOptions = BuildResponseCreateOptions(items, opts);
                return provider.CreateResponseStreamingAsync(callOptions, ct);
            },
            requestOptions,
            effective with { ModelName = modelName },
            cancellationToken
        ).ConfigureAwait(false);
    }

    public static async Task<OpenAIResponsesStreamSummary> CreateResponseStreamingAsync(
        SigilClient client,
        IEnumerable<ResponseItem> inputItems,
        Func<IEnumerable<ResponseItem>, CreateResponseOptions?, CancellationToken, IAsyncEnumerable<StreamingResponseUpdate>> invoke,
        CreateResponseOptions? requestOptions = null,
        OpenAISigilOptions? options = null,
        CancellationToken cancellationToken = default
    )
    {
        ArgumentNullException.ThrowIfNull(client);
        ArgumentNullException.ThrowIfNull(invoke);

        var effective = options ?? new OpenAISigilOptions();
        var itemList = inputItems?.ToList() ?? throw new ArgumentNullException(nameof(inputItems));
        var modelName = ResolveInitialModelName(effective, fallback: null);

        var recorder = client.StartStreamingGeneration(new GenerationStart
        {
            ConversationId = effective.ConversationId,
            AgentName = effective.AgentName,
            AgentVersion = effective.AgentVersion,
            Model = new ModelRef
            {
                Provider = effective.ProviderName,
                Name = modelName,
            },
            Mode = GenerationMode.Stream,
        });

        try
        {
            var summary = new OpenAIResponsesStreamSummary();
            await foreach (var streamEvent in invoke(itemList, requestOptions, cancellationToken).WithCancellation(cancellationToken))
            {
                if (!summary.FirstChunkAt.HasValue)
                {
                    var firstChunkAt = DateTimeOffset.UtcNow;
                    summary.FirstChunkAt = firstChunkAt;
                    recorder.SetFirstTokenAt(firstChunkAt);
                }
                summary.Events.Add(streamEvent);
                if (streamEvent is StreamingResponseCompletedUpdate completed && completed.Response != null)
                {
                    summary.FinalResponse = completed.Response;
                }
            }

            Exception? mappingError = null;
            Generation generation;
            try
            {
                generation = OpenAIGenerationMapper.ResponsesFromStream(modelName, itemList, requestOptions, summary, effective);
            }
            catch (Exception ex)
            {
                mappingError = ex;
                generation = new Generation();
            }

            recorder.SetResult(generation, mappingError);
            return summary;
        }
        catch (Exception ex)
        {
            recorder.SetCallError(ex);
            throw;
        }
        finally
        {
            recorder.End();
        }
    }

    public static async Task<OpenAIEmbeddingCollection> GenerateEmbeddingsAsync(
        SigilClient client,
        EmbeddingClient provider,
        IEnumerable<string> inputs,
        EmbeddingGenerationOptions? requestOptions = null,
        OpenAISigilOptions? options = null,
        CancellationToken cancellationToken = default
    )
    {
        ArgumentNullException.ThrowIfNull(provider);

        var effective = options ?? new OpenAISigilOptions();
        var modelName = ResolveInitialModelName(effective, provider.Model);

        return await GenerateEmbeddingsAsync(
            client,
            inputs,
            async (requestInputs, opts, ct) =>
            {
                var result = await provider.GenerateEmbeddingsAsync(requestInputs, opts, ct).ConfigureAwait(false);
                return result.Value;
            },
            requestOptions,
            effective with { ModelName = modelName },
            cancellationToken
        ).ConfigureAwait(false);
    }

    public static async Task<OpenAIEmbeddingCollection> GenerateEmbeddingsAsync(
        SigilClient client,
        IEnumerable<string> inputs,
        Func<IEnumerable<string>, EmbeddingGenerationOptions?, CancellationToken, Task<OpenAIEmbeddingCollection>> invoke,
        EmbeddingGenerationOptions? requestOptions = null,
        OpenAISigilOptions? options = null,
        CancellationToken cancellationToken = default
    )
    {
        ArgumentNullException.ThrowIfNull(client);
        ArgumentNullException.ThrowIfNull(invoke);

        var effective = options ?? new OpenAISigilOptions();
        var inputList = inputs?.ToList() ?? throw new ArgumentNullException(nameof(inputs));
        var modelName = ResolveInitialModelName(effective, fallback: null);
        var recorder = client.StartEmbedding(OpenAIGenerationMapper.EmbeddingsStart(modelName, requestOptions, effective));

        try
        {
            var response = await invoke(inputList, requestOptions, cancellationToken).ConfigureAwait(false);
            recorder.SetResult(OpenAIGenerationMapper.EmbeddingsFromRequestResponse(modelName, inputList, requestOptions, response));
            return response;
        }
        catch (Exception ex)
        {
            recorder.SetCallError(ex);
            throw;
        }
        finally
        {
            recorder.End();
        }
    }

    private static string ResolveInitialModelName(OpenAISigilOptions options, string? fallback)
    {
        if (!string.IsNullOrWhiteSpace(options.ModelName))
        {
            return options.ModelName;
        }

        if (!string.IsNullOrWhiteSpace(fallback))
        {
            return fallback;
        }

        return "unknown";
    }

    private static CreateResponseOptions BuildResponseCreateOptions(
        IEnumerable<ResponseItem> inputItems,
        CreateResponseOptions? requestOptions
    )
    {
        var options = CloneResponseCreateOptions(requestOptions);
        options.InputItems.Clear();
        foreach (var item in inputItems)
        {
            if (item != null)
            {
                options.InputItems.Add(item);
            }
        }
        return options;
    }

    private static CreateResponseOptions CloneResponseCreateOptions(CreateResponseOptions? source)
    {
        if (source == null)
        {
            return new CreateResponseOptions();
        }

        var clone = new CreateResponseOptions
        {
            BackgroundModeEnabled = source.BackgroundModeEnabled,
            ConversationOptions = source.ConversationOptions,
            EndUserId = source.EndUserId,
            Instructions = source.Instructions,
            MaxOutputTokenCount = source.MaxOutputTokenCount,
            MaxToolCallCount = source.MaxToolCallCount,
            Model = source.Model,
            ParallelToolCallsEnabled = source.ParallelToolCallsEnabled,
            PreviousResponseId = source.PreviousResponseId,
            ReasoningOptions = source.ReasoningOptions,
            SafetyIdentifier = source.SafetyIdentifier,
            ServiceTier = source.ServiceTier,
            StoredOutputEnabled = source.StoredOutputEnabled,
            StreamingEnabled = source.StreamingEnabled,
            Temperature = source.Temperature,
            TextOptions = source.TextOptions,
            ToolChoice = source.ToolChoice,
            TopLogProbabilityCount = source.TopLogProbabilityCount,
            TopP = source.TopP,
            TruncationMode = source.TruncationMode,
        };

        foreach (var included in source.IncludedProperties)
        {
            clone.IncludedProperties.Add(included);
        }

        foreach (var tool in source.Tools)
        {
            clone.Tools.Add(tool);
        }

        foreach (var metadata in source.Metadata)
        {
            clone.Metadata[metadata.Key] = metadata.Value;
        }

        return clone;
    }
}
