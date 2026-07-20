using Google.GenAI;
using Google.GenAI.Types;

namespace Grafana.Sigil.Gemini;

public static class GeminiRecorder
{
    public static async Task<GenerateContentResponse> GenerateContentAsync(
        SigilClient client,
        Client provider,
        string model,
        IReadOnlyList<Content>? contents,
        GenerateContentConfig? config = null,
        GeminiSigilOptions? options = null,
        CancellationToken cancellationToken = default
    )
    {
        return provider == null
            ? throw new ArgumentNullException(nameof(provider))
            : await GenerateContentAsync(
            client,
            model,
            contents,
            provider.Models.GenerateContentAsync,
            config,
            options,
            cancellationToken
        ).ConfigureAwait(false);
    }

    public static async Task<GenerateContentResponse> GenerateContentAsync(
        SigilClient client,
        string model,
        IReadOnlyList<Content>? contents,
        Func<string, List<Content>, GenerateContentConfig?, CancellationToken, Task<GenerateContentResponse>> invoke,
        GenerateContentConfig? config = null,
        GeminiSigilOptions? options = null,
        CancellationToken cancellationToken = default
    )
    {
        ArgumentNullException.ThrowIfNull(client);

        if (string.IsNullOrWhiteSpace(model))
        {
            throw new ArgumentException("model is required", nameof(model));
        }

        ArgumentNullException.ThrowIfNull(invoke);

        var effective = options ?? new GeminiSigilOptions();
        var mappedContents = MapContents(contents);
        var modelName = ResolveModelName(model, effective);

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
            var response = await invoke(model, mappedContents, config, cancellationToken).ConfigureAwait(false);
            Exception? mappingError = null;
            Generation generation;

            try
            {
                generation = GeminiGenerationMapper.FromRequestResponse(model, mappedContents, config, response, effective with { ModelName = modelName });
            }
            catch (Exception ex)
            {
                mappingError = ex;
                generation = new Generation();
            }

            recorder.SetResult(generation, mappingError);
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

    public static async Task<GeminiStreamSummary> GenerateContentStreamAsync(
        SigilClient client,
        Client provider,
        string model,
        IReadOnlyList<Content>? contents,
        GenerateContentConfig? config = null,
        GeminiSigilOptions? options = null,
        CancellationToken cancellationToken = default
    )
    {
        return provider == null
            ? throw new ArgumentNullException(nameof(provider))
            : await GenerateContentStreamAsync(
            client,
            model,
            contents,
            provider.Models.GenerateContentStreamAsync,
            config,
            options,
            cancellationToken
        ).ConfigureAwait(false);
    }

    public static async Task<GeminiStreamSummary> GenerateContentStreamAsync(
        SigilClient client,
        string model,
        IReadOnlyList<Content>? contents,
        Func<string, List<Content>, GenerateContentConfig?, CancellationToken, IAsyncEnumerable<GenerateContentResponse>> invoke,
        GenerateContentConfig? config = null,
        GeminiSigilOptions? options = null,
        CancellationToken cancellationToken = default
    )
    {
        ArgumentNullException.ThrowIfNull(client);

        if (string.IsNullOrWhiteSpace(model))
        {
            throw new ArgumentException("model is required", nameof(model));
        }

        ArgumentNullException.ThrowIfNull(invoke);

        var effective = options ?? new GeminiSigilOptions();
        var mappedContents = MapContents(contents);
        var modelName = ResolveModelName(model, effective);

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
            var summary = new GeminiStreamSummary();
            await foreach (var response in invoke(model, mappedContents, config, cancellationToken).WithCancellation(cancellationToken))
            {
                if (response != null)
                {
                    if (!summary.FirstChunkAt.HasValue)
                    {
                        var firstChunkAt = DateTimeOffset.UtcNow;
                        summary.FirstChunkAt = firstChunkAt;
                        recorder.SetFirstTokenAt(firstChunkAt);
                    }
                    summary.Responses.Add(response);
                }
            }

            Exception? mappingError = null;
            Generation generation;
            try
            {
                generation = GeminiGenerationMapper.FromStream(model, mappedContents, config, summary, effective with { ModelName = modelName });
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

    public static async Task<EmbedContentResponse> EmbedContentAsync(
        SigilClient client,
        Client provider,
        string model,
        IReadOnlyList<Content>? contents,
        EmbedContentConfig? config = null,
        GeminiSigilOptions? options = null,
        CancellationToken cancellationToken = default
    )
    {
        return provider == null
            ? throw new ArgumentNullException(nameof(provider))
            : await EmbedContentAsync(
            client,
            model,
            contents,
            provider.Models.EmbedContentAsync,
            config,
            options,
            cancellationToken
        ).ConfigureAwait(false);
    }

    public static async Task<EmbedContentResponse> EmbedContentAsync(
        SigilClient client,
        string model,
        IReadOnlyList<Content>? contents,
        Func<string, List<Content>, EmbedContentConfig?, CancellationToken, Task<EmbedContentResponse>> invoke,
        EmbedContentConfig? config = null,
        GeminiSigilOptions? options = null,
        CancellationToken cancellationToken = default
    )
    {
        ArgumentNullException.ThrowIfNull(client);

        if (string.IsNullOrWhiteSpace(model))
        {
            throw new ArgumentException("model is required", nameof(model));
        }

        ArgumentNullException.ThrowIfNull(invoke);

        var effective = options ?? new GeminiSigilOptions();
        var mappedContents = MapContents(contents);
        var modelName = ResolveModelName(model, effective);
        var recorder = client.StartEmbedding(new EmbeddingStart
        {
            AgentName = effective.AgentName,
            AgentVersion = effective.AgentVersion,
            Model = new ModelRef
            {
                Provider = effective.ProviderName,
                Name = modelName,
            },
            Dimensions = config?.OutputDimensionality is int dimensions && dimensions > 0 ? dimensions : null,
        });

        try
        {
            var response = await invoke(model, mappedContents, config, cancellationToken).ConfigureAwait(false);
            recorder.SetResult(GeminiGenerationMapper.EmbeddingFromResponse(modelName, mappedContents, config, response));
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

    private static List<Content> MapContents(IReadOnlyList<Content>? contents)
    {
        var mapped = new List<Content>();
        if (contents == null)
        {
            return mapped;
        }

        foreach (var content in contents)
        {
            if (content != null)
            {
                mapped.Add(content);
            }
        }

        return mapped;
    }

    private static string ResolveModelName(string model, GeminiSigilOptions options)
    {
        if (!string.IsNullOrWhiteSpace(options.ModelName))
        {
            return options.ModelName;
        }

        if (!string.IsNullOrWhiteSpace(model))
        {
            return model;
        }

        return "unknown";
    }
}
