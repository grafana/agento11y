using System.Diagnostics;
using System.Diagnostics.Metrics;
using System.Globalization;
using System.Net;
using System.Reflection;
using System.Text;
using System.Text.Json;
using System.Text.RegularExpressions;

namespace Grafana.Sigil;

public sealed partial class SigilClient : IAsyncDisposable
{
    internal const string InstrumentationName = "github.com/grafana/sigil/sdks/dotnet";
    internal const string DefaultOperationNameSync = "generateText";
    internal const string DefaultOperationNameStream = "streamText";
    internal const string DefaultOperationNameEmbedding = "embeddings";

    internal const string SpanAttrGenerationId = "sigil.generation.id";
    internal const string SpanAttrSdkName = "sigil.sdk.name";
    internal const string SpanAttrConversationId = "gen_ai.conversation.id";
    internal const string SpanAttrConversationTitle = "sigil.conversation.title";
    internal const string SpanAttrUserId = "user.id";
    internal const string SpanAttrAgentName = "gen_ai.agent.name";
    internal const string SpanAttrAgentVersion = "gen_ai.agent.version";
    internal const string SpanAttrErrorType = "error.type";
    internal const string SpanAttrErrorCategory = "error.category";
    internal const string SpanAttrOperationName = "gen_ai.operation.name";
    internal const string SpanAttrProviderName = "gen_ai.provider.name";
    internal const string SpanAttrRequestModel = "gen_ai.request.model";
    internal const string SpanAttrRequestMaxTokens = "gen_ai.request.max_tokens";
    internal const string SpanAttrRequestTemperature = "gen_ai.request.temperature";
    internal const string SpanAttrRequestTopP = "gen_ai.request.top_p";
    internal const string SpanAttrRequestToolChoice = "sigil.gen_ai.request.tool_choice";
    internal const string SpanAttrRequestThinkingEnabled = "sigil.gen_ai.request.thinking.enabled";
    internal const string SpanAttrRequestThinkingBudget = "sigil.gen_ai.request.thinking.budget_tokens";
    internal const string SpanAttrResponseId = "gen_ai.response.id";
    internal const string SpanAttrResponseModel = "gen_ai.response.model";
    internal const string SpanAttrFinishReasons = "gen_ai.response.finish_reasons";
    internal const string SpanAttrInputTokens = "gen_ai.usage.input_tokens";
    internal const string SpanAttrOutputTokens = "gen_ai.usage.output_tokens";
    internal const string SpanAttrEmbeddingInputCount = "gen_ai.embeddings.input_count";
    internal const string SpanAttrEmbeddingInputTexts = "gen_ai.embeddings.input_texts";
    internal const string SpanAttrEmbeddingDimCount = "gen_ai.embeddings.dimension.count";
    internal const string SpanAttrRequestEncodingFormats = "gen_ai.request.encoding_formats";
    internal const string SpanAttrCacheReadTokens = "gen_ai.usage.cache_read_input_tokens";
    internal const string SpanAttrCacheWriteTokens = "gen_ai.usage.cache_write_input_tokens";
    internal const string SpanAttrCacheCreationTokens = "gen_ai.usage.cache_creation_input_tokens";
    internal const string SpanAttrReasoningTokens = "gen_ai.usage.reasoning_tokens";
    internal const string SpanAttrToolName = "gen_ai.tool.name";
    internal const string SpanAttrToolCallId = "gen_ai.tool.call.id";
    internal const string SpanAttrToolType = "gen_ai.tool.type";
    internal const string SpanAttrToolDescription = "gen_ai.tool.description";
    internal const string SpanAttrToolCallArguments = "gen_ai.tool.call.arguments";
    internal const string SpanAttrToolCallResult = "gen_ai.tool.call.result";
    private const int MaxRatingConversationIdLen = 255;
    private const int MaxRatingIdLen = 128;
    private const int MaxRatingGenerationIdLen = 255;
    private const int MaxRatingActorIdLen = 255;
    private const int MaxRatingSourceLen = 64;
    private const int MaxRatingCommentBytes = 4096;
    private const int MaxRatingMetadataBytes = 16 * 1024;

    internal const string MetricOperationDuration = "gen_ai.client.operation.duration";
    internal const string MetricTokenUsage = "gen_ai.client.token.usage";
    internal const string MetricTimeToFirstToken = "gen_ai.client.time_to_first_token";
    internal const string MetricToolCallsPerOperation = "gen_ai.client.tool_calls_per_operation";
    internal const string MetricAttrTokenType = "gen_ai.token.type";
    internal const string MetricTokenTypeInput = "input";
    internal const string MetricTokenTypeOutput = "output";
    internal const string MetricTokenTypeCacheRead = "cache_read";
    internal const string MetricTokenTypeCacheWrite = "cache_write";
    internal const string MetricTokenTypeCacheCreation = "cache_creation";
    internal const string MetricTokenTypeReasoning = "reasoning";

    internal static readonly double[] DurationBucketsSeconds =
    {
        0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28,
        2.56, 5.12, 10.24, 20.48, 40.96, 81.92,
    };

    internal static readonly double[] TokenUsageBuckets =
    {
        1, 4, 16, 64, 256, 1024, 4096, 16384,
        65536, 262144, 1048576, 4194304, 16777216, 67108864,
    };

#if NET
    [GeneratedRegex(@"\b([1-5][0-9][0-9])\b", RegexOptions.Compiled)]
    private static partial Regex StatusCodeRegex();
#else
    private static readonly Regex StatusCodeRegex = new(@"\b([1-5][0-9][0-9])\b", RegexOptions.Compiled);
#endif

    internal const string SdkName = "sdk-dotnet";
    internal const string MetadataUserIdKey = "sigil.user.id";
    internal const string MetadataLegacyUserIdKey = "user.id";
    internal const string MetadataKeyContentCaptureMode = "sigil.sdk.content_capture_mode";

    internal readonly SigilClientConfig _config;
    private readonly IGenerationExporter _generationExporter;
    private readonly ActivitySource _activitySource;
    private readonly Meter _meter;
    private readonly Histogram<double> _operationDurationHistogram;
    private readonly Histogram<double> _tokenUsageHistogram;
    private readonly Histogram<double> _ttftHistogram;
    private readonly Histogram<double> _toolCallsHistogram;
    private readonly EmbeddingCaptureConfig _embeddingCapture;
    private readonly Action<string> _log;
    private readonly HttpClient _ratingHttpClient = new(new HttpClientHandler
    {
        UseCookies = false,
    })
    {
        Timeout = TimeSpan.FromSeconds(10),
    };

#if NET10_0_OR_GREATER
    private readonly Lock _pendingLock = new();
    private readonly Lock _flushBackgroundLock = new();
    private readonly Lock _stateLock = new();
#else
    private readonly object _pendingLock = new();
    private readonly object _flushBackgroundLock = new();
    private readonly object _stateLock = new();
#endif

    private readonly List<Generation> _pending = [];
    private readonly SemaphoreSlim _flushSemaphore = new(1, 1);
    private Task? _backgroundFlushTask;

    private readonly CancellationTokenSource _timerCts = new();
    private readonly Task _timerTask;

    private bool _shutdown;

    internal EmbeddingCaptureConfig EmbeddingCapture => _embeddingCapture;

    public SigilClient(SigilClientConfig? config = null)
    {
        _config = ConfigResolver.Resolve(config);
        _log = _config.Logger!;
        _embeddingCapture = InternalUtils.DeepClone(_config.EmbeddingCapture);

        _generationExporter = _config.GenerationExporter
            ?? _config.GenerationExport.Protocol!.Value switch
            {
                GenerationExportProtocol.Http => new HttpGenerationExporter(
                    _config.GenerationExport.Endpoint,
                    _config.GenerationExport.Headers
                ),
                GenerationExportProtocol.Grpc => new GrpcGenerationExporter(
                    _config.GenerationExport.Endpoint,
                    _config.GenerationExport.Insecure!.Value,
                    _config.GenerationExport.Headers
                ),
                GenerationExportProtocol.None => new NoopGenerationExporter(),
                _ => throw new InvalidOperationException(
                    $"unsupported generation export protocol {_config.GenerationExport.Protocol}"
                ),
            };

        _activitySource = new ActivitySource(InstrumentationName);
        _meter = new Meter(InstrumentationName);
        _operationDurationHistogram = _meter.CreateHistogram<double>(
            MetricOperationDuration,
            unit: "s",
            advice: new InstrumentAdvice<double> { HistogramBucketBoundaries = DurationBucketsSeconds });
        _tokenUsageHistogram = _meter.CreateHistogram<double>(
            MetricTokenUsage,
            unit: "token",
            advice: new InstrumentAdvice<double> { HistogramBucketBoundaries = TokenUsageBuckets });
        _ttftHistogram = _meter.CreateHistogram<double>(
            MetricTimeToFirstToken,
            unit: "s",
            advice: new InstrumentAdvice<double> { HistogramBucketBoundaries = DurationBucketsSeconds });
        _toolCallsHistogram = _meter.CreateHistogram<double>(MetricToolCallsPerOperation, "count");

        _timerTask = Task.Run(RunFlushTimerAsync);
    }

    public GenerationRecorder StartGeneration(GenerationStart start)
    {
        return StartGenerationInternal(start, GenerationMode.Sync);
    }

    public GenerationRecorder StartStreamingGeneration(GenerationStart start)
    {
        return StartGenerationInternal(start, GenerationMode.Stream);
    }

    public EmbeddingRecorder StartEmbedding(EmbeddingStart start)
    {
        EnsureNotShutdown();

        var seed = InternalUtils.DeepClone(start ?? new EmbeddingStart());

        if (string.IsNullOrWhiteSpace(seed.AgentName))
        {
            seed.AgentName = SigilContext.AgentNameFromContext() ?? string.Empty;
        }
        if (string.IsNullOrWhiteSpace(seed.AgentName))
        {
            seed.AgentName = _config.AgentName ?? string.Empty;
        }

        if (string.IsNullOrWhiteSpace(seed.AgentVersion))
        {
            seed.AgentVersion = SigilContext.AgentVersionFromContext() ?? string.Empty;
        }
        if (string.IsNullOrWhiteSpace(seed.AgentVersion))
        {
            seed.AgentVersion = _config.AgentVersion ?? string.Empty;
        }

        seed.StartedAt = seed.StartedAt.HasValue
            ? InternalUtils.Utc(seed.StartedAt.Value)
            : _config.UtcNow!();

        var activity = _activitySource.StartActivity(
            EmbeddingSpanName(seed.Model.Name),
            ActivityKind.Client,
            default(ActivityContext),
            tags: null,
            links: null,
            seed.StartedAt.Value
        );

        if (activity != null)
        {
            ApplyEmbeddingStartSpanAttributes(activity, seed);
        }

        return new EmbeddingRecorder(this, seed, seed.StartedAt.Value, activity);
    }

    public ToolExecutionRecorder StartToolExecution(ToolExecutionStart start)
    {
        EnsureNotShutdown();

        var seed = InternalUtils.DeepClone(start);
        seed.ToolName = (seed.ToolName ?? string.Empty).Trim();
        if (seed.ToolName.Length == 0)
        {
            return ToolExecutionRecorder.Noop;
        }

        if (string.IsNullOrWhiteSpace(seed.ConversationId))
        {
            seed.ConversationId = SigilContext.ConversationIdFromContext() ?? string.Empty;
        }

        if (string.IsNullOrWhiteSpace(seed.ConversationTitle))
        {
            seed.ConversationTitle = SigilContext.ConversationTitleFromContext() ?? string.Empty;
        }

        if (string.IsNullOrWhiteSpace(seed.AgentName))
        {
            seed.AgentName = SigilContext.AgentNameFromContext() ?? string.Empty;
        }
        if (string.IsNullOrWhiteSpace(seed.AgentName))
        {
            seed.AgentName = _config.AgentName ?? string.Empty;
        }

        if (string.IsNullOrWhiteSpace(seed.AgentVersion))
        {
            seed.AgentVersion = SigilContext.AgentVersionFromContext() ?? string.Empty;
        }
        if (string.IsNullOrWhiteSpace(seed.AgentVersion))
        {
            seed.AgentVersion = _config.AgentVersion ?? string.Empty;
        }

        seed.StartedAt = seed.StartedAt.HasValue
            ? InternalUtils.Utc(seed.StartedAt.Value)
            : _config.UtcNow!();

        // Resolve content capture before the span starts so MetadataOnly never
        // attaches sensitive content to live span attributes.
        var ctxMode = SigilContext.ContentCaptureModeFromContext();
        var ctxSet = SigilContext.HasContentCaptureModeInContext();
        var effectiveClientDefault = _config.ContentCapture;
        if (seed.ContentCapture == ContentCaptureMode.Default && !ctxSet)
        {
            var resolverMode = CallContentCaptureResolver(_config.ContentCaptureResolver, null, _log);
            effectiveClientDefault = ResolveContentCaptureMode(resolverMode, _config.ContentCapture);
        }
        var toolMode = ResolveToolContentCaptureMode(seed.ContentCapture, ctxMode, ctxSet, effectiveClientDefault);
#pragma warning disable CS0618 // IncludeContent is obsolete
        var includeContent = toolMode switch
        {
            ContentCaptureMode.MetadataOnly => false,
            ContentCaptureMode.Full => true,
            _ => seed.IncludeContent,
        };
#pragma warning restore CS0618

        if (toolMode == ContentCaptureMode.MetadataOnly)
        {
            seed.ToolDescription = string.Empty;
            seed.ConversationTitle = string.Empty;
        }

        var activity = _activitySource.StartActivity(
            ToolSpanName(seed.ToolName),
            ActivityKind.Internal,
            default(ActivityContext),
            tags: null,
            links: null,
            seed.StartedAt!.Value
        );

        if (activity != null)
        {
            ApplyToolSpanAttributes(activity, seed);
        }

        return new ToolExecutionRecorder(this, seed, seed.StartedAt!.Value, includeContent, toolMode == ContentCaptureMode.MetadataOnly, activity);
    }

    public async Task<SubmitConversationRatingResponse> SubmitConversationRatingAsync(
        string conversationId,
        SubmitConversationRatingRequest request,
        CancellationToken cancellationToken = default
    )
    {
        EnsureNotShutdown();

        var normalizedConversationId = (conversationId ?? string.Empty).Trim();
        if (normalizedConversationId.Length == 0)
        {
            throw new ValidationException("sigil conversation rating validation failed: conversationId is required");
        }

        if (normalizedConversationId.Length > MaxRatingConversationIdLen)
        {
            throw new ValidationException("sigil conversation rating validation failed: conversationId is too long");
        }

        var resolverMode = CallContentCaptureResolver(_config.ContentCaptureResolver, request?.Metadata, _log);
        var effectiveMode = ResolveContentCaptureMode(resolverMode, ResolveClientContentCaptureMode(_config.ContentCapture));

        var normalizedRequest = NormalizeConversationRatingRequest(request);

        // Strip comment when MetadataOnly. Done after clone to avoid mutating
        // the caller's request object (reference type, unlike Go's value type).
        if (effectiveMode == ContentCaptureMode.MetadataOnly)
        {
            normalizedRequest.Comment = string.Empty;
        }

        var endpoint = BuildConversationRatingEndpoint(
            _config.Api.Endpoint,
            _config.GenerationExport.Insecure!.Value,
            normalizedConversationId
        );

        var payload = new Dictionary<string, object?>(StringComparer.Ordinal)
        {
            ["rating_id"] = normalizedRequest.RatingId,
            ["rating"] = ToWireConversationRatingValue(normalizedRequest.Rating),
        };
        if (!string.IsNullOrWhiteSpace(normalizedRequest.Comment))
        {
            payload["comment"] = normalizedRequest.Comment;
        }
        if (normalizedRequest.Metadata.Count > 0)
        {
            payload["metadata"] = normalizedRequest.Metadata;
        }
        if (!string.IsNullOrWhiteSpace(normalizedRequest.GenerationId))
        {
            payload["generation_id"] = normalizedRequest.GenerationId;
        }
        if (!string.IsNullOrWhiteSpace(normalizedRequest.RaterId))
        {
            payload["rater_id"] = normalizedRequest.RaterId;
        }
        if (!string.IsNullOrWhiteSpace(normalizedRequest.Source))
        {
            payload["source"] = normalizedRequest.Source;
        }

        var body = JsonSerializer.Serialize(payload);
        using var httpRequest = new HttpRequestMessage(HttpMethod.Post, endpoint)
        {
            Content = new StringContent(body, Encoding.UTF8, "application/json"),
        };
        foreach (var header in _config.GenerationExport.Headers)
        {
            httpRequest.Headers.TryAddWithoutValidation(header.Key, header.Value);
        }

        HttpResponseMessage response;
        try
        {
            response = await _ratingHttpClient.SendAsync(httpRequest, cancellationToken).ConfigureAwait(false);
        }
        catch (Exception ex)
        {
            throw new RatingTransportException("sigil conversation rating transport failed", ex);
        }
        using (response)
        {
            var responseBody = (await ReadResponseBodyAsync(response.Content, cancellationToken).ConfigureAwait(false)).Trim();
            if (response.StatusCode == HttpStatusCode.BadRequest)
            {
                throw new ValidationException(
                    $"sigil conversation rating validation failed: {RatingErrorText(responseBody, (int)response.StatusCode)}"
                );
            }
            if (response.StatusCode == HttpStatusCode.Conflict)
            {
                throw new RatingConflictException(
                    $"sigil conversation rating conflict: {RatingErrorText(responseBody, (int)response.StatusCode)}"
                );
            }
            if (!response.IsSuccessStatusCode)
            {
                throw new RatingTransportException(
                    $"sigil conversation rating transport failed: status {(int)response.StatusCode}: {RatingErrorText(responseBody, (int)response.StatusCode)}"
                );
            }
            if (string.IsNullOrWhiteSpace(responseBody))
            {
                throw new RatingTransportException("sigil conversation rating transport failed: empty response payload");
            }

            return ParseSubmitConversationRatingResponse(responseBody);
        }
    }

    public async Task FlushAsync(CancellationToken cancellationToken = default)
    {
        EnsureNotShutdown();
        await FlushInternalAsync(cancellationToken).ConfigureAwait(false);
    }

    public async Task ShutdownAsync(CancellationToken cancellationToken = default)
    {
        lock (_stateLock)
        {
            if (_shutdown)
            {
                return;
            }

            _shutdown = true;
        }

        _timerCts.Cancel();

        try
        {
            await _timerTask.ConfigureAwait(false);
        }
        catch (OperationCanceledException)
        {
            // Ignore.
        }

        try
        {
            await FlushInternalAsync(cancellationToken).ConfigureAwait(false);
        }
        catch (Exception ex)
        {
            _log($"sigil generation export flush on shutdown failed: {ex}");
        }

        try
        {
            await _generationExporter.ShutdownAsync(cancellationToken).ConfigureAwait(false);
        }
        catch (Exception ex)
        {
            _log($"sigil generation exporter shutdown failed: {ex}");
        }

        _activitySource.Dispose();
        _meter.Dispose();
        _ratingHttpClient.Dispose();
    }

    public async ValueTask DisposeAsync()
    {
        await ShutdownAsync().ConfigureAwait(false);
    }

    private GenerationRecorder StartGenerationInternal(GenerationStart start, GenerationMode defaultMode)
    {
        EnsureNotShutdown();

        // Capture original metadata before DeepClone, which converts values to
        // JsonElement via JSON round-trip. The resolver should see the caller's
        // original types (string, bool, long, etc.).
        var originalMetadata = start.Metadata;
        var seed = InternalUtils.DeepClone(start);

        seed.Mode ??= defaultMode;

        if (string.IsNullOrWhiteSpace(seed.OperationName))
        {
            seed.OperationName = DefaultOperationNameForMode(seed.Mode!.Value);
        }

        if (string.IsNullOrWhiteSpace(seed.ConversationId))
        {
            seed.ConversationId = SigilContext.ConversationIdFromContext() ?? string.Empty;
        }

        if (string.IsNullOrWhiteSpace(seed.ConversationTitle))
        {
            seed.ConversationTitle = SigilContext.ConversationTitleFromContext() ?? string.Empty;
        }

        if (string.IsNullOrWhiteSpace(seed.UserId))
        {
            seed.UserId = SigilContext.UserIdFromContext() ?? string.Empty;
        }
        if (string.IsNullOrWhiteSpace(seed.UserId))
        {
            seed.UserId = _config.UserId ?? string.Empty;
        }

        if (string.IsNullOrWhiteSpace(seed.AgentName))
        {
            seed.AgentName = SigilContext.AgentNameFromContext() ?? string.Empty;
        }
        if (string.IsNullOrWhiteSpace(seed.AgentName))
        {
            seed.AgentName = _config.AgentName ?? string.Empty;
        }

        if (string.IsNullOrWhiteSpace(seed.AgentVersion))
        {
            seed.AgentVersion = SigilContext.AgentVersionFromContext() ?? string.Empty;
        }
        if (string.IsNullOrWhiteSpace(seed.AgentVersion))
        {
            seed.AgentVersion = _config.AgentVersion ?? string.Empty;
        }

        // Merge config-default tags as a base layer; per-call seed tags win.
        if (_config.Tags != null && _config.Tags.Count > 0)
        {
            seed.Tags = MergeTagsConfigUnderSeed(_config.Tags, seed.Tags);
        }

        seed.StartedAt = seed.StartedAt.HasValue
            ? InternalUtils.Utc(seed.StartedAt.Value)
            : _config.UtcNow!();

        // Resolve content capture mode before the span starts so MetadataOnly never
        // attaches sensitive content to live span attributes.
        var ccMode = seed.ContentCapture;
        if (ccMode == ContentCaptureMode.Default)
        {
            var resolverMode = CallContentCaptureResolver(_config.ContentCaptureResolver, originalMetadata, _log);
            ccMode = ResolveClientContentCaptureMode(ResolveContentCaptureMode(resolverMode, _config.ContentCapture));
        }
        else
        {
            ccMode = NormalizeContentCaptureMode(ccMode);
        }

        var spanGeneration = new Generation
        {
            Id = seed.Id,
            ConversationId = seed.ConversationId,
            ConversationTitle = ccMode == ContentCaptureMode.MetadataOnly ? string.Empty : seed.ConversationTitle,
            UserId = seed.UserId,
            AgentName = seed.AgentName,
            AgentVersion = seed.AgentVersion,
            Mode = seed.Mode,
            OperationName = seed.OperationName,
            Model = InternalUtils.DeepClone(seed.Model),
            MaxTokens = seed.MaxTokens,
            Temperature = seed.Temperature,
            TopP = seed.TopP,
            ToolChoice = seed.ToolChoice,
            ThinkingEnabled = seed.ThinkingEnabled,
            ParentGenerationIds = [.. seed.ParentGenerationIds],
            EffectiveVersion = seed.EffectiveVersion,
        };

        var activity = _activitySource.StartActivity(
            GenerationSpanName(seed.OperationName, seed.Model.Name),
            ActivityKind.Client,
            default(ActivityContext),
            tags: null,
            links: null,
            seed.StartedAt!.Value
        );

        if (activity != null)
        {
            ApplyGenerationSpanAttributes(activity, spanGeneration);
        }

        var recorder = new GenerationRecorder(this, seed, seed.StartedAt!.Value, ccMode, activity);
        recorder.SetContextScope(SigilContext.PushContentCaptureMode(ccMode));
        return recorder;
    }

    internal void PersistGeneration(Generation generation)
    {
        try
        {
            GenerationValidator.Validate(generation);
        }
        catch (Exception ex)
        {
            throw new ValidationException($"sigil: generation validation failed: {ex.Message}");
        }

        var proto = ProtoMapping.ToProto(generation);
        if (_config.GenerationExport.PayloadMaxBytes > 0)
        {
            var payloadSize = proto.CalculateSize();
            if (payloadSize > _config.GenerationExport.PayloadMaxBytes)
            {
                throw new EnqueueException(
                    $"generation payload exceeds max bytes ({payloadSize} > {_config.GenerationExport.PayloadMaxBytes})"
                );
            }
        }

        lock (_stateLock)
        {
            if (_shutdown)
            {
                throw new ClientShutdownException("sigil: client is shutting down");
            }
        }

        bool shouldTriggerFlush;
        lock (_pendingLock)
        {
            if (_pending.Count >= _config.GenerationExport.QueueSize)
            {
                throw new QueueFullException("sigil: generation queue is full");
            }

            _pending.Add(InternalUtils.DeepClone(generation));
            shouldTriggerFlush = _pending.Count >= _config.GenerationExport.BatchSize;
        }

        if (shouldTriggerFlush)
        {
            TriggerBackgroundFlush();
        }
    }

    private void TriggerBackgroundFlush()
    {
        lock (_flushBackgroundLock)
        {
            if (_backgroundFlushTask is { IsCompleted: false })
            {
                return;
            }

            _backgroundFlushTask = Task.Run(async () =>
            {
                try
                {
                    await FlushInternalAsync(CancellationToken.None).ConfigureAwait(false);
                }
                catch (Exception ex)
                {
                    _log($"sigil generation export failed: {ex}");
                }
            });
        }
    }

    private async Task RunFlushTimerAsync()
    {
        while (!_timerCts.IsCancellationRequested)
        {
            try
            {
                await Task.Delay(_config.GenerationExport.FlushInterval, _timerCts.Token).ConfigureAwait(false);
            }
            catch (OperationCanceledException)
            {
                break;
            }

            TriggerBackgroundFlush();
        }
    }

    private async Task FlushInternalAsync(CancellationToken cancellationToken)
    {
        await _flushSemaphore.WaitAsync(cancellationToken).ConfigureAwait(false);
        try
        {
            while (true)
            {
                List<Generation> batch;
                lock (_pendingLock)
                {
                    if (_pending.Count == 0)
                    {
                        return;
                    }

                    var count = Math.Min(_pending.Count, _config.GenerationExport.BatchSize);
                    batch = [.. _pending.Take(count).Select(InternalUtils.DeepClone)];
                    _pending.RemoveRange(0, count);
                }

                var response = await ExportWithRetryAsync(new ExportGenerationsRequest { Generations = batch }, cancellationToken)
                    .ConfigureAwait(false);

                foreach (var result in response.Results)
                {
                    if (!result.Accepted)
                    {
                        _log($"sigil generation rejected id={result.GenerationId} error={result.Error}");
                    }
                }
            }
        }
        finally
        {
            _flushSemaphore.Release();
        }
    }

    private async Task<ExportGenerationsResponse> ExportWithRetryAsync(
        ExportGenerationsRequest request,
        CancellationToken cancellationToken
    )
    {
        var attempts = _config.GenerationExport.MaxRetries + 1;
        var backoff = _config.GenerationExport.InitialBackoff;
        var maxBackoff = _config.GenerationExport.MaxBackoff;
        if (backoff <= TimeSpan.Zero)
        {
            backoff = TimeSpan.FromMilliseconds(100);
        }

        Exception? lastError = null;
        for (var attempt = 0; attempt < attempts; attempt++)
        {
            try
            {
                return await _generationExporter.ExportGenerationsAsync(request, cancellationToken).ConfigureAwait(false);
            }
            catch (Exception ex)
            {
                lastError = ex;
                if (attempt == attempts - 1)
                {
                    break;
                }

                await _config.SleepAsync!(backoff, cancellationToken).ConfigureAwait(false);
                var next = backoff + backoff;
                backoff = next > maxBackoff ? maxBackoff : next;
            }
        }

        throw lastError ?? new InvalidOperationException("generation export failed");
    }

    private void EnsureNotShutdown()
    {
        lock (_stateLock)
        {
            if (_shutdown)
            {
                throw new ClientShutdownException("sigil: client is shutting down");
            }
        }
    }

    private static SubmitConversationRatingRequest NormalizeConversationRatingRequest(SubmitConversationRatingRequest? request)
    {
        var input = request ?? new SubmitConversationRatingRequest();
        var normalized = new SubmitConversationRatingRequest
        {
            RatingId = (input.RatingId ?? string.Empty).Trim(),
            Rating = input.Rating,
            Comment = (input.Comment ?? string.Empty).Trim(),
            Metadata = input.Metadata != null
                ? new Dictionary<string, object?>(input.Metadata, StringComparer.Ordinal)
                : new Dictionary<string, object?>(StringComparer.Ordinal),
            GenerationId = (input.GenerationId ?? string.Empty).Trim(),
            RaterId = (input.RaterId ?? string.Empty).Trim(),
            Source = (input.Source ?? string.Empty).Trim(),
        };

        if (normalized.RatingId.Length == 0)
        {
            throw new ValidationException("sigil conversation rating validation failed: ratingId is required");
        }
        if (normalized.RatingId.Length > MaxRatingIdLen)
        {
            throw new ValidationException("sigil conversation rating validation failed: ratingId is too long");
        }
        if (normalized.Rating != ConversationRatingValue.Good && normalized.Rating != ConversationRatingValue.Bad)
        {
            throw new ValidationException(
                "sigil conversation rating validation failed: rating must be CONVERSATION_RATING_VALUE_GOOD or CONVERSATION_RATING_VALUE_BAD"
            );
        }
        if (Encoding.UTF8.GetByteCount(normalized.Comment) > MaxRatingCommentBytes)
        {
            throw new ValidationException("sigil conversation rating validation failed: comment is too long");
        }
        if (normalized.GenerationId.Length > MaxRatingGenerationIdLen)
        {
            throw new ValidationException("sigil conversation rating validation failed: generationId is too long");
        }
        if (normalized.RaterId.Length > MaxRatingActorIdLen)
        {
            throw new ValidationException("sigil conversation rating validation failed: raterId is too long");
        }
        if (normalized.Source.Length > MaxRatingSourceLen)
        {
            throw new ValidationException("sigil conversation rating validation failed: source is too long");
        }

        if (normalized.Metadata.Count > 0)
        {
            byte[] metadataBytes;
            try
            {
                metadataBytes = JsonSerializer.SerializeToUtf8Bytes(normalized.Metadata);
            }
            catch (Exception ex)
            {
                throw new ValidationException($"sigil conversation rating validation failed: metadata must be valid JSON ({ex.Message})");
            }

            if (metadataBytes.Length > MaxRatingMetadataBytes)
            {
                throw new ValidationException("sigil conversation rating validation failed: metadata is too large");
            }
        }

        return normalized;
    }

    private static string BuildConversationRatingEndpoint(string apiEndpoint, bool insecure, string conversationId)
    {
        var baseUrl = BuildRatingBaseUrl(apiEndpoint, insecure);
        return $"{baseUrl}/api/v1/conversations/{Uri.EscapeDataString(conversationId)}/ratings";
    }

    private static string BuildRatingBaseUrl(string apiEndpoint, bool insecure)
    {
        var trimmedEndpoint = (apiEndpoint ?? string.Empty).Trim();
        if (trimmedEndpoint.Length == 0)
        {
            throw new RatingTransportException("sigil conversation rating transport failed: api endpoint is required");
        }

        if (trimmedEndpoint.StartsWith("http://", StringComparison.OrdinalIgnoreCase)
            || trimmedEndpoint.StartsWith("https://", StringComparison.OrdinalIgnoreCase))
        {
            if (!Uri.TryCreate(trimmedEndpoint, UriKind.Absolute, out var parsed) || string.IsNullOrWhiteSpace(parsed.Host))
            {
                throw new RatingTransportException(
                    "sigil conversation rating transport failed: api endpoint host is required"
                );
            }

            return $"{parsed.Scheme}://{parsed.Authority}";
        }

        var host = trimmedEndpoint;
        if (host.StartsWith("grpc://", StringComparison.OrdinalIgnoreCase))
        {
#if NET
            host = host["grpc://".Length..];
#else
            host = host.Substring("grpc://".Length);
#endif
        }
        var slashIndex = host.IndexOf('/');
        if (slashIndex >= 0)
        {
#if NET
            host = host[..slashIndex];
#else
            host = host.Substring(0, slashIndex);
#endif
        }
        host = host.Trim();
        if (host.Length == 0)
        {
            throw new RatingTransportException(
                "sigil conversation rating transport failed: api endpoint host is required"
            );
        }

        var scheme = insecure ? "http" : "https";
        return $"{scheme}://{host}";
    }

    private static SubmitConversationRatingResponse ParseSubmitConversationRatingResponse(string payload)
    {
        try
        {
            using var document = JsonDocument.Parse(payload);
            if (document.RootElement.ValueKind != JsonValueKind.Object)
            {
                throw new RatingTransportException("sigil conversation rating transport failed: invalid response payload");
            }

            var ratingElement = GetRequiredProperty(document.RootElement, "rating");
            var summaryElement = GetRequiredProperty(document.RootElement, "summary");

            return new SubmitConversationRatingResponse
            {
                Rating = ParseConversationRating(ratingElement),
                Summary = ParseConversationRatingSummary(summaryElement),
            };
        }
        catch (RatingTransportException)
        {
            throw;
        }
        catch (JsonException ex)
        {
            throw new RatingTransportException("sigil conversation rating transport failed: invalid JSON response", ex);
        }
    }

    private static ConversationRating ParseConversationRating(JsonElement element)
    {
        if (element.ValueKind != JsonValueKind.Object)
        {
            throw new RatingTransportException("sigil conversation rating transport failed: invalid rating payload");
        }

        var rating = new ConversationRating
        {
            RatingId = GetRequiredString(element, "rating_id"),
            ConversationId = GetRequiredString(element, "conversation_id"),
            Rating = ParseWireConversationRatingValue(GetRequiredString(element, "rating")),
            CreatedAt = ParseRequiredTimestamp(element, "created_at"),
        };

        if (TryGetOptionalString(element, "comment", out var comment))
        {
            rating.Comment = comment;
        }
        if (TryGetProperty(element, "metadata", out var metadataElement))
        {
            rating.Metadata = metadataElement.ValueKind switch
            {
                JsonValueKind.Object => ParseMetadataObject(metadataElement),
                JsonValueKind.Null => new Dictionary<string, object?>(StringComparer.Ordinal),
                _ => throw new RatingTransportException("sigil conversation rating transport failed: invalid rating payload"),
            };
        }
        if (TryGetOptionalString(element, "generation_id", out var generationId))
        {
            rating.GenerationId = generationId;
        }
        if (TryGetOptionalString(element, "rater_id", out var raterId))
        {
            rating.RaterId = raterId;
        }
        if (TryGetOptionalString(element, "source", out var source))
        {
            rating.Source = source;
        }

        return rating;
    }

    private static ConversationRatingSummary ParseConversationRatingSummary(JsonElement element)
    {
        if (element.ValueKind != JsonValueKind.Object)
        {
            throw new RatingTransportException("sigil conversation rating transport failed: invalid rating summary payload");
        }

        ConversationRatingValue? latestRating = null;
        if (TryGetProperty(element, "latest_rating", out var latestRatingElement))
        {
            latestRating = latestRatingElement.ValueKind switch
            {
                JsonValueKind.String => ParseWireConversationRatingValue(latestRatingElement.GetString() ?? string.Empty),
                JsonValueKind.Null => null,
                _ => throw new RatingTransportException(
                    "sigil conversation rating transport failed: invalid rating summary payload"
                ),
            };
        }

        DateTimeOffset? latestBadAt = null;
        if (TryGetProperty(element, "latest_bad_at", out var latestBadAtElement))
        {
            latestBadAt = latestBadAtElement.ValueKind switch
            {
                JsonValueKind.String => ParseTimestamp(latestBadAtElement.GetString() ?? string.Empty),
                JsonValueKind.Null => null,
                _ => throw new RatingTransportException(
                    "sigil conversation rating transport failed: invalid rating summary payload"
                ),
            };
        }

        return new ConversationRatingSummary
        {
            TotalCount = GetRequiredInt(element, "total_count"),
            GoodCount = GetRequiredInt(element, "good_count"),
            BadCount = GetRequiredInt(element, "bad_count"),
            LatestRating = latestRating,
            LatestRatedAt = ParseRequiredTimestamp(element, "latest_rated_at"),
            LatestBadAt = latestBadAt,
            HasBadRating = GetRequiredBool(element, "has_bad_rating"),
        };
    }

    private static ConversationRatingValue ParseWireConversationRatingValue(string value)
    {
        return value switch
        {
            "CONVERSATION_RATING_VALUE_GOOD" => ConversationRatingValue.Good,
            "CONVERSATION_RATING_VALUE_BAD" => ConversationRatingValue.Bad,
            _ => throw new RatingTransportException("sigil conversation rating transport failed: invalid rating payload"),
        };
    }

    private static string ToWireConversationRatingValue(ConversationRatingValue value)
    {
        return value switch
        {
            ConversationRatingValue.Good => "CONVERSATION_RATING_VALUE_GOOD",
            ConversationRatingValue.Bad => "CONVERSATION_RATING_VALUE_BAD",
            _ => throw new ValidationException(
                "sigil conversation rating validation failed: rating must be CONVERSATION_RATING_VALUE_GOOD or CONVERSATION_RATING_VALUE_BAD"
            ),
        };
    }

    private static string RatingErrorText(string body, int statusCode)
    {
        var trimmed = (body ?? string.Empty).Trim();
        if (trimmed.Length > 0)
        {
            return trimmed;
        }

        if (Enum.IsDefined(typeof(HttpStatusCode), statusCode))
        {
            return ((HttpStatusCode)statusCode).ToString();
        }

        return "status " + statusCode.ToString(CultureInfo.InvariantCulture);
    }

    private static JsonElement GetRequiredProperty(JsonElement element, string name)
    {
        if (!TryGetProperty(element, name, out var value))
        {
            throw new RatingTransportException("sigil conversation rating transport failed: invalid response payload");
        }

        return value;
    }

    private static bool TryGetProperty(JsonElement element, string name, out JsonElement value)
    {
        foreach (var property in element.EnumerateObject())
        {
            if (string.Equals(property.Name, name, StringComparison.Ordinal))
            {
                value = property.Value;
                return true;
            }
        }

        value = default;
        return false;
    }

    private static string GetRequiredString(JsonElement element, string name)
    {
        if (!TryGetProperty(element, name, out var value) || value.ValueKind != JsonValueKind.String)
        {
            throw new RatingTransportException("sigil conversation rating transport failed: invalid response payload");
        }

        var text = (value.GetString() ?? string.Empty).Trim();
        if (text.Length == 0)
        {
            throw new RatingTransportException("sigil conversation rating transport failed: invalid response payload");
        }

        return text;
    }

    private static bool TryGetOptionalString(JsonElement element, string name, out string value)
    {
        value = string.Empty;
        if (!TryGetProperty(element, name, out var raw))
        {
            return false;
        }

        if (raw.ValueKind == JsonValueKind.Null)
        {
            return false;
        }
        if (raw.ValueKind != JsonValueKind.String)
        {
            throw new RatingTransportException("sigil conversation rating transport failed: invalid response payload");
        }

        value = (raw.GetString() ?? string.Empty).Trim();
        return true;
    }

    private static int GetRequiredInt(JsonElement element, string name)
    {
        if (!TryGetProperty(element, name, out var value) || value.ValueKind != JsonValueKind.Number || !value.TryGetInt32(out var parsed))
        {
            throw new RatingTransportException("sigil conversation rating transport failed: invalid response payload");
        }

        return parsed;
    }

    private static bool GetRequiredBool(JsonElement element, string name)
    {
        if (!TryGetProperty(element, name, out var value)
            || (value.ValueKind != JsonValueKind.True && value.ValueKind != JsonValueKind.False))
        {
            throw new RatingTransportException("sigil conversation rating transport failed: invalid response payload");
        }

        return value.GetBoolean();
    }

    private static DateTimeOffset ParseRequiredTimestamp(JsonElement element, string name)
    {
        if (!TryGetProperty(element, name, out var value) || value.ValueKind != JsonValueKind.String)
        {
            throw new RatingTransportException("sigil conversation rating transport failed: invalid response payload");
        }

        return ParseTimestamp(value.GetString() ?? string.Empty);
    }

    private static DateTimeOffset ParseTimestamp(string raw)
    {
        if (!DateTimeOffset.TryParse(raw, CultureInfo.InvariantCulture, DateTimeStyles.RoundtripKind, out var timestamp))
        {
            throw new RatingTransportException("sigil conversation rating transport failed: invalid timestamp in response payload");
        }

        return timestamp;
    }

    private static Dictionary<string, object?> ParseMetadataObject(JsonElement value)
    {
        try
        {
            var metadata = JsonSerializer.Deserialize<Dictionary<string, object?>>(value.GetRawText());
            return metadata != null
                ? new Dictionary<string, object?>(metadata, StringComparer.Ordinal)
                : new Dictionary<string, object?>(StringComparer.Ordinal);
        }
        catch (Exception ex)
        {
            throw new RatingTransportException("sigil conversation rating transport failed: invalid rating payload", ex);
        }
    }

    internal static string DefaultOperationNameForMode(GenerationMode mode)
    {
        return mode == GenerationMode.Stream ? DefaultOperationNameStream : DefaultOperationNameSync;
    }

    internal static string GenerationSpanName(string operationName, string modelName)
    {
        var operation = string.IsNullOrWhiteSpace(operationName) ? DefaultOperationNameSync : operationName.Trim();
        var model = modelName?.Trim() ?? string.Empty;
        return model.Length == 0 ? operation : operation + " " + model;
    }

    internal static string EmbeddingSpanName(string modelName)
    {
        var model = modelName?.Trim() ?? string.Empty;
        return model.Length == 0 ? DefaultOperationNameEmbedding : DefaultOperationNameEmbedding + " " + model;
    }

    internal static string ToolSpanName(string toolName)
    {
        return "execute_tool " + toolName;
    }

    internal static void ApplyGenerationSpanAttributes(Activity activity, Generation generation)
    {
        activity.SetTag(SpanAttrOperationName, OperationName(generation));
        activity.SetTag(SpanAttrSdkName, SdkName);

        if (!string.IsNullOrWhiteSpace(generation.Id))
        {
            activity.SetTag(SpanAttrGenerationId, generation.Id);
        }

        if (!string.IsNullOrWhiteSpace(generation.ConversationId))
        {
            activity.SetTag(SpanAttrConversationId, generation.ConversationId);
        }

        if (!string.IsNullOrWhiteSpace(generation.ConversationTitle))
        {
            activity.SetTag(SpanAttrConversationTitle, generation.ConversationTitle);
        }

        if (!string.IsNullOrWhiteSpace(generation.UserId))
        {
            activity.SetTag(SpanAttrUserId, generation.UserId);
        }

        if (!string.IsNullOrWhiteSpace(generation.AgentName))
        {
            activity.SetTag(SpanAttrAgentName, generation.AgentName);
        }

        if (!string.IsNullOrWhiteSpace(generation.AgentVersion))
        {
            activity.SetTag(SpanAttrAgentVersion, generation.AgentVersion);
        }

        if (!string.IsNullOrWhiteSpace(generation.Model.Provider))
        {
            activity.SetTag(SpanAttrProviderName, generation.Model.Provider);
        }

        if (!string.IsNullOrWhiteSpace(generation.Model.Name))
        {
            activity.SetTag(SpanAttrRequestModel, generation.Model.Name);
        }

        if (generation.MaxTokens.HasValue)
        {
            activity.SetTag(SpanAttrRequestMaxTokens, generation.MaxTokens.Value);
        }

        if (generation.Temperature.HasValue)
        {
            activity.SetTag(SpanAttrRequestTemperature, generation.Temperature.Value);
        }

        if (generation.TopP.HasValue)
        {
            activity.SetTag(SpanAttrRequestTopP, generation.TopP.Value);
        }

        if (!string.IsNullOrWhiteSpace(generation.ToolChoice))
        {
            activity.SetTag(SpanAttrRequestToolChoice, generation.ToolChoice);
        }

        if (generation.ThinkingEnabled.HasValue)
        {
            activity.SetTag(SpanAttrRequestThinkingEnabled, generation.ThinkingEnabled.Value);
        }
        if (TryGetThinkingBudgetFromMetadata(generation.Metadata, out var thinkingBudget))
        {
            activity.SetTag(SpanAttrRequestThinkingBudget, thinkingBudget);
        }

        if (!string.IsNullOrWhiteSpace(generation.ResponseId))
        {
            activity.SetTag(SpanAttrResponseId, generation.ResponseId);
        }

        if (!string.IsNullOrWhiteSpace(generation.ResponseModel))
        {
            activity.SetTag(SpanAttrResponseModel, generation.ResponseModel);
        }

        if (!string.IsNullOrWhiteSpace(generation.StopReason))
        {
            activity.SetTag(SpanAttrFinishReasons, new[] { generation.StopReason });
        }

        if (generation.Usage.InputTokens != 0)
        {
            activity.SetTag(SpanAttrInputTokens, generation.Usage.InputTokens);
        }

        if (generation.Usage.OutputTokens != 0)
        {
            activity.SetTag(SpanAttrOutputTokens, generation.Usage.OutputTokens);
        }

        if (generation.Usage.CacheReadInputTokens != 0)
        {
            activity.SetTag(SpanAttrCacheReadTokens, generation.Usage.CacheReadInputTokens);
        }

        if (generation.Usage.CacheWriteInputTokens != 0)
        {
            activity.SetTag(SpanAttrCacheWriteTokens, generation.Usage.CacheWriteInputTokens);
        }

        if (generation.Usage.CacheCreationInputTokens != 0)
        {
            activity.SetTag(SpanAttrCacheCreationTokens, generation.Usage.CacheCreationInputTokens);
        }

        if (generation.Usage.ReasoningTokens != 0)
        {
            activity.SetTag(SpanAttrReasoningTokens, generation.Usage.ReasoningTokens);
        }
    }

    internal static void ApplyEmbeddingStartSpanAttributes(Activity activity, EmbeddingStart start)
    {
        activity.SetTag(SpanAttrOperationName, DefaultOperationNameEmbedding);
        activity.SetTag(SpanAttrSdkName, SdkName);

        if (!string.IsNullOrWhiteSpace(start.Model.Provider))
        {
            activity.SetTag(SpanAttrProviderName, start.Model.Provider);
        }
        if (!string.IsNullOrWhiteSpace(start.Model.Name))
        {
            activity.SetTag(SpanAttrRequestModel, start.Model.Name);
        }
        if (!string.IsNullOrWhiteSpace(start.AgentName))
        {
            activity.SetTag(SpanAttrAgentName, start.AgentName);
        }
        if (!string.IsNullOrWhiteSpace(start.AgentVersion))
        {
            activity.SetTag(SpanAttrAgentVersion, start.AgentVersion);
        }
        if (start.Dimensions.HasValue)
        {
            activity.SetTag(SpanAttrEmbeddingDimCount, start.Dimensions.Value);
        }
        if (!string.IsNullOrWhiteSpace(start.EncodingFormat))
        {
            activity.SetTag(SpanAttrRequestEncodingFormats, new[] { start.EncodingFormat });
        }
    }

    internal static void ApplyEmbeddingEndSpanAttributes(
        Activity activity,
        EmbeddingResult result,
        EmbeddingCaptureConfig captureConfig
    )
    {
        activity.SetTag(SpanAttrEmbeddingInputCount, result.InputCount);

        if (result.InputTokens != 0)
        {
            activity.SetTag(SpanAttrInputTokens, result.InputTokens);
        }
        if (!string.IsNullOrWhiteSpace(result.ResponseModel))
        {
            activity.SetTag(SpanAttrResponseModel, result.ResponseModel);
        }
        if (result.Dimensions.HasValue)
        {
            activity.SetTag(SpanAttrEmbeddingDimCount, result.Dimensions.Value);
        }
        if (captureConfig.CaptureInput)
        {
            var inputTexts = CaptureEmbeddingInputTexts(result.InputTexts, captureConfig);
            if (inputTexts.Count > 0)
            {
                activity.SetTag(SpanAttrEmbeddingInputTexts, inputTexts.ToArray());
            }
        }
    }

    internal static void ApplyToolSpanAttributes(Activity activity, ToolExecutionStart tool)
    {
        activity.SetTag(SpanAttrOperationName, "execute_tool");
        activity.SetTag(SpanAttrToolName, tool.ToolName);
        activity.SetTag(SpanAttrSdkName, SdkName);

        if (!string.IsNullOrWhiteSpace(tool.ToolCallId))
        {
            activity.SetTag(SpanAttrToolCallId, tool.ToolCallId);
        }

        if (!string.IsNullOrWhiteSpace(tool.ToolType))
        {
            activity.SetTag(SpanAttrToolType, tool.ToolType);
        }

        if (!string.IsNullOrWhiteSpace(tool.ToolDescription))
        {
            activity.SetTag(SpanAttrToolDescription, tool.ToolDescription);
        }

        if (!string.IsNullOrWhiteSpace(tool.ConversationId))
        {
            activity.SetTag(SpanAttrConversationId, tool.ConversationId);
        }

        if (!string.IsNullOrWhiteSpace(tool.ConversationTitle))
        {
            activity.SetTag(SpanAttrConversationTitle, tool.ConversationTitle);
        }

        if (!string.IsNullOrWhiteSpace(tool.AgentName))
        {
            activity.SetTag(SpanAttrAgentName, tool.AgentName);
        }

        if (!string.IsNullOrWhiteSpace(tool.AgentVersion))
        {
            activity.SetTag(SpanAttrAgentVersion, tool.AgentVersion);
        }
        if (!string.IsNullOrWhiteSpace(tool.RequestProvider))
        {
            activity.SetTag(SpanAttrProviderName, tool.RequestProvider);
        }
        if (!string.IsNullOrWhiteSpace(tool.RequestModel))
        {
            activity.SetTag(SpanAttrRequestModel, tool.RequestModel);
        }
    }

    internal static string OperationName(Generation generation)
    {
        if (!string.IsNullOrWhiteSpace(generation.OperationName))
        {
            return generation.OperationName;
        }

        return DefaultOperationNameForMode(generation.Mode ?? GenerationMode.Sync);
    }

    internal void RecordGenerationMetrics(
        Generation generation,
        string errorType,
        string errorCategory,
        DateTimeOffset? firstTokenAt
    )
    {
        if (!generation.StartedAt.HasValue || !generation.CompletedAt.HasValue)
        {
            return;
        }

        var startedAt = generation.StartedAt.Value;
        var completedAt = generation.CompletedAt.Value;
        var durationSeconds = Math.Max(0d, (completedAt - startedAt).TotalSeconds);

        _operationDurationHistogram.Record(
            durationSeconds,
            [
                new(SpanAttrOperationName, OperationName(generation)),
                new(SpanAttrProviderName, generation.Model.Provider ?? string.Empty),
                new(SpanAttrRequestModel, generation.Model.Name ?? string.Empty),
                new(SpanAttrAgentName, generation.AgentName ?? string.Empty),
                new(SpanAttrErrorType, errorType ?? string.Empty),
                new(SpanAttrErrorCategory, errorCategory ?? string.Empty),
            ]);

        RecordTokenUsage(generation, MetricTokenTypeInput, generation.Usage.InputTokens);
        RecordTokenUsage(generation, MetricTokenTypeOutput, generation.Usage.OutputTokens);
        RecordTokenUsage(generation, MetricTokenTypeCacheRead, generation.Usage.CacheReadInputTokens);
        RecordTokenUsage(generation, MetricTokenTypeCacheWrite, generation.Usage.CacheWriteInputTokens);
        RecordTokenUsage(generation, MetricTokenTypeCacheCreation, generation.Usage.CacheCreationInputTokens);
        RecordTokenUsage(generation, MetricTokenTypeReasoning, generation.Usage.ReasoningTokens);

        _toolCallsHistogram.Record(
            CountToolCallParts(generation.Output),
            [
                new(SpanAttrProviderName, generation.Model.Provider ?? string.Empty),
                new(SpanAttrRequestModel, generation.Model.Name ?? string.Empty),
                new(SpanAttrAgentName, generation.AgentName ?? string.Empty),
            ]);

        if (string.Equals(OperationName(generation), DefaultOperationNameStream, StringComparison.Ordinal)
            && firstTokenAt.HasValue)
        {
            var ttftSeconds = (firstTokenAt.Value - startedAt).TotalSeconds;
            if (ttftSeconds >= 0d)
            {
                _ttftHistogram.Record(
                    ttftSeconds,
                    [
                        new(SpanAttrProviderName, generation.Model.Provider ?? string.Empty),
                        new(SpanAttrRequestModel, generation.Model.Name ?? string.Empty),
                        new(SpanAttrAgentName, generation.AgentName ?? string.Empty),
                    ]);
            }
        }
    }

    internal void RecordEmbeddingMetrics(
        EmbeddingStart seed,
        EmbeddingResult result,
        DateTimeOffset startedAt,
        DateTimeOffset completedAt,
        string errorType,
        string errorCategory
    )
    {
        var durationSeconds = Math.Max(0d, (completedAt - startedAt).TotalSeconds);
        _operationDurationHistogram.Record(
            durationSeconds,
            [
                new(SpanAttrOperationName, DefaultOperationNameEmbedding),
                new(SpanAttrProviderName, seed.Model.Provider ?? string.Empty),
                new(SpanAttrRequestModel, seed.Model.Name ?? string.Empty),
                new(SpanAttrAgentName, seed.AgentName ?? string.Empty),
                new(SpanAttrErrorType, errorType ?? string.Empty),
                new(SpanAttrErrorCategory, errorCategory ?? string.Empty),
            ]);

        if (result.InputTokens != 0L)
        {
            _tokenUsageHistogram.Record(
                result.InputTokens,
                [
                    new(SpanAttrOperationName, DefaultOperationNameEmbedding),
                    new(SpanAttrProviderName, seed.Model.Provider ?? string.Empty),
                    new(SpanAttrRequestModel, seed.Model.Name ?? string.Empty),
                    new(SpanAttrAgentName, seed.AgentName ?? string.Empty),
                    new(MetricAttrTokenType, MetricTokenTypeInput),
                ]);
        }
    }

    internal void RecordToolExecutionMetrics(
        ToolExecutionStart seed,
        DateTimeOffset startedAt,
        DateTimeOffset completedAt,
        Exception? finalError
    )
    {
        var durationSeconds = Math.Max(0d, (completedAt - startedAt).TotalSeconds);
        var errorType = finalError == null ? string.Empty : "tool_execution_error";
        var errorCategory = finalError == null ? string.Empty : ErrorCategoryFromException(finalError, true);

        _operationDurationHistogram.Record(
            durationSeconds,
            [
                new(SpanAttrOperationName, "execute_tool"),
                new(SpanAttrProviderName, (seed.RequestProvider ?? string.Empty).Trim()),
                new(SpanAttrRequestModel, (seed.RequestModel ?? string.Empty).Trim()),
                new(SpanAttrToolName, (seed.ToolName ?? string.Empty).Trim()),
                new(SpanAttrAgentName, seed.AgentName ?? string.Empty),
                new(SpanAttrErrorType, errorType),
                new(SpanAttrErrorCategory, errorCategory),
            ]);
    }

    internal static string ErrorCategoryFromException(Exception? error, bool fallbackSdk)
    {
        if (error == null)
        {
            return fallbackSdk ? "sdk_error" : string.Empty;
        }

        if (error is TimeoutException or OperationCanceledException)
        {
            return "timeout";
        }

        var message = error.Message ?? string.Empty;
        if (message.Contains("timeout", StringComparison.OrdinalIgnoreCase)
            || message.Contains("deadline exceeded", StringComparison.OrdinalIgnoreCase))
        {
            return "timeout";
        }

        var statusCode = ExtractStatusCode(error);
        if (statusCode == 429)
        {
            return "rate_limit";
        }

        if (statusCode is 401 or 403)
        {
            return "auth_error";
        }

        if (statusCode == 408)
        {
            return "timeout";
        }

        if (statusCode.HasValue && statusCode.Value >= 500 && statusCode.Value <= 599)
        {
            return "server_error";
        }

        if (statusCode.HasValue && statusCode.Value >= 400 && statusCode.Value <= 499)
        {
            return "client_error";
        }

        return fallbackSdk ? "sdk_error" : string.Empty;
    }

    private void RecordTokenUsage(Generation generation, string tokenType, long value)
    {
        if (value == 0L)
        {
            return;
        }

        _tokenUsageHistogram.Record(
            value,
            [
                new(SpanAttrOperationName, OperationName(generation)),
                new(SpanAttrProviderName, generation.Model.Provider ?? string.Empty),
                new(SpanAttrRequestModel, generation.Model.Name ?? string.Empty),
                new(SpanAttrAgentName, generation.AgentName ?? string.Empty),
                new(MetricAttrTokenType, tokenType),
            ]);
    }

    private static long CountToolCallParts(IReadOnlyList<Message> messages)
    {
        long total = 0;
        foreach (var message in messages)
        {
            foreach (var part in message.Parts)
            {
                if (part.Kind == PartKind.ToolCall)
                {
                    total++;
                }
            }
        }

        return total;
    }

    private static int? ExtractStatusCode(Exception error)
    {
        var direct = ReadStatusCodeValue(error);
        if (direct.HasValue)
        {
            return direct;
        }

        foreach (var propertyName in new[] { "Response", "Error" })
        {
            var property = error.GetType().GetProperty(propertyName, BindingFlags.Public | BindingFlags.NonPublic | BindingFlags.Instance);
            var nested = property?.GetValue(error);
            if (nested != null)
            {
                var nestedValue = ReadStatusCodeValue(nested);
                if (nestedValue.HasValue)
                {
                    return nestedValue;
                }
            }
        }

#if NET
        var matches = StatusCodeRegex().Matches(error.Message ?? string.Empty);
#else
        var matches = StatusCodeRegex.Matches(error.Message ?? string.Empty);
#endif

        foreach (Match match in matches)
        {
            if (int.TryParse(match.Value, NumberStyles.Integer, CultureInfo.InvariantCulture, out var parsed)
                && parsed is >= 100 and <= 599)
            {
                return parsed;
            }
        }

        return null;
    }

    private static int? ReadStatusCodeValue(object value)
    {
        foreach (var memberName in new[] { "StatusCode", "Status", "statusCode", "status" })
        {
            var property = value.GetType().GetProperty(memberName, BindingFlags.Public | BindingFlags.NonPublic | BindingFlags.Instance);
            if (property != null)
            {
                var parsed = ConvertToStatusCode(property.GetValue(value));
                if (parsed.HasValue)
                {
                    return parsed;
                }
            }

            var field = value.GetType().GetField(memberName, BindingFlags.Public | BindingFlags.NonPublic | BindingFlags.Instance);
            if (field != null)
            {
                var parsed = ConvertToStatusCode(field.GetValue(value));
                if (parsed.HasValue)
                {
                    return parsed;
                }
            }
        }

        return null;
    }

    private static int? ConvertToStatusCode(object? value)
    {
        if (value == null)
        {
            return null;
        }

        if (value is int statusCode)
        {
            return statusCode is >= 100 and <= 599 ? statusCode : null;
        }

        if (value is long longStatus && longStatus is >= 100 and <= 599)
        {
            return (int)longStatus;
        }

        if (value is string text
            && int.TryParse(text, NumberStyles.Integer, CultureInfo.InvariantCulture, out var parsed)
            && parsed is >= 100 and <= 599)
        {
            return parsed;
        }

        return null;
    }

    private static bool TryGetThinkingBudgetFromMetadata(
        Dictionary<string, object?> metadata,
        out long thinkingBudget
    )
    {
        thinkingBudget = 0;
        if (!metadata.TryGetValue(SpanAttrRequestThinkingBudget, out var raw) || raw == null)
        {
            return false;
        }

        switch (raw)
        {
            case long value:
                thinkingBudget = value;
                return true;
            case int value:
                thinkingBudget = value;
                return true;
            case short value:
                thinkingBudget = value;
                return true;
            case byte value:
                thinkingBudget = value;
                return true;
            case ulong value when value <= long.MaxValue:
                thinkingBudget = (long)value;
                return true;
            case uint value:
                thinkingBudget = value;
                return true;
            case ushort value:
                thinkingBudget = value;
                return true;
            case sbyte value:
                thinkingBudget = value;
                return true;
            case double value when value % 1 == 0 && value >= long.MinValue && value <= long.MaxValue:
                thinkingBudget = (long)value;
                return true;
            case float value when value % 1 == 0 && value >= long.MinValue && value <= long.MaxValue:
                thinkingBudget = (long)value;
                return true;
            case decimal value when decimal.Truncate(value) == value && value >= long.MinValue && value <= long.MaxValue:
                thinkingBudget = (long)value;
                return true;
            case JsonElement json:
                if (json.ValueKind == JsonValueKind.Number && json.TryGetInt64(out var jsonInt))
                {
                    thinkingBudget = jsonInt;
                    return true;
                }
                if (json.ValueKind == JsonValueKind.String
                    && long.TryParse(json.GetString(), NumberStyles.Integer, CultureInfo.InvariantCulture, out var jsonParsed))
                {
                    thinkingBudget = jsonParsed;
                    return true;
                }
                return false;
            case string text:
                return long.TryParse(text, NumberStyles.Integer, CultureInfo.InvariantCulture, out thinkingBudget);
            default:
                return false;
        }
    }

    private static List<string> CaptureEmbeddingInputTexts(
        List<string> inputTexts,
        EmbeddingCaptureConfig captureConfig
    )
    {
        if (inputTexts == null || inputTexts.Count == 0)
        {
            return [];
        }

        var maxItems = Math.Max(1, captureConfig.MaxInputItems);
        var maxTextLength = Math.Max(1, captureConfig.MaxTextLength);
        var count = Math.Min(maxItems, inputTexts.Count);
        var captured = new List<string>(count);

        for (var index = 0; index < count; index++)
        {
            var text = inputTexts[index] ?? string.Empty;
            captured.Add(TruncateEmbeddingText(text, maxTextLength));
        }

        return captured;
    }

    private static string TruncateEmbeddingText(string text, int maxLength)
    {
        if (GetScalarCount(text) <= maxLength)
        {
            return text;
        }

        if (maxLength <= 0)
        {
            return string.Empty;
        }

        if (maxLength <= 3)
        {
            return TruncateAtScalarBoundary(text, maxLength);
        }

        return TruncateAtScalarBoundary(text, maxLength - 3) + "...";
    }

    private static int GetScalarCount(string text)
    {
        if (string.IsNullOrEmpty(text))
        {
            return 0;
        }

        var count = 0;
        for (var i = 0; i < text.Length; i++)
        {
            if (char.IsHighSurrogate(text[i]) && i + 1 < text.Length && char.IsLowSurrogate(text[i + 1]))
            {
                i++; // Skip the low surrogate, count surrogate pair as one
            }

            count++;
        }

        return count;
    }

    private static string TruncateAtScalarBoundary(string text, int maxScalars)
    {
        if (maxScalars <= 0)
        {
            return string.Empty;
        }

        if (GetScalarCount(text) <= maxScalars)
        {
            return text;
        }

        // Find the char index after maxScalars Unicode scalars
        var charIndex = 0;
        var scalarCount = 0;
        while (charIndex < text.Length && scalarCount < maxScalars)
        {
            if (char.IsHighSurrogate(text[charIndex]) && charIndex + 1 < text.Length && char.IsLowSurrogate(text[charIndex + 1]))
            {
                charIndex += 2; // Surrogate pair is one scalar
            }
            else
            {
                charIndex++;
            }

            scalarCount++;
        }

#if NET
        return text[..charIndex];
#else
        return text.Substring(0, charIndex);
#endif
    }

    internal static void RecordException(Activity activity, Exception error, bool redact = false)
    {
        if (activity == null || error == null)
        {
            return;
        }

        activity.SetTag("exception.type", error.GetType().FullName);
        if (redact)
        {
            return;
        }
        activity.SetTag("exception.message", error.Message);
        activity.SetTag("exception.stacktrace", error.ToString());
    }

    private static Task<string> ReadResponseBodyAsync(HttpContent content, CancellationToken cancellationToken)
    {
#if NETSTANDARD2_0
        return content.ReadAsStringAsync();
#else
        return content.ReadAsStringAsync(cancellationToken);
#endif
    }

    internal static ContentCaptureMode ResolveClientContentCaptureMode(ContentCaptureMode mode)
    {
        var normalized = NormalizeContentCaptureMode(mode);
        return normalized == ContentCaptureMode.Default ? ContentCaptureMode.NoToolContent : normalized;
    }

    internal static ContentCaptureMode ResolveContentCaptureMode(ContentCaptureMode @override, ContentCaptureMode fallback)
    {
        var normalizedOverride = NormalizeContentCaptureMode(@override);
        return normalizedOverride != ContentCaptureMode.Default ? normalizedOverride : NormalizeContentCaptureMode(fallback);
    }

    internal static ContentCaptureMode CallContentCaptureResolver(
        Func<IReadOnlyDictionary<string, object?>?, ContentCaptureMode>? resolver,
        IDictionary<string, object?>? metadata,
        Action<string>? logger = null)
    {
        if (resolver == null)
        {
            return ContentCaptureMode.Default;
        }

        ContentCaptureMode mode;
        try
        {
            mode = resolver(ToResolverMetadata(metadata));
        }
        catch (Exception ex)
        {
            logger?.Invoke($"sigil: content capture resolver threw, falling back to MetadataOnly: {ex.Message}");
            return ContentCaptureMode.MetadataOnly;
        }

        if (!Enum.IsDefined(typeof(ContentCaptureMode), mode))
        {
            logger?.Invoke($"sigil: content capture resolver returned undefined mode {(int)mode}, falling back to MetadataOnly");
            return ContentCaptureMode.MetadataOnly;
        }

        return mode;
    }

    private static ContentCaptureMode NormalizeContentCaptureMode(ContentCaptureMode mode)
    {
        return Enum.IsDefined(typeof(ContentCaptureMode), mode) ? mode : ContentCaptureMode.MetadataOnly;
    }

    private static IReadOnlyDictionary<string, object?>? ToResolverMetadata(IDictionary<string, object?>? metadata)
    {
        return metadata == null
            ? null
            : new System.Collections.ObjectModel.ReadOnlyDictionary<string, object?>(
                new Dictionary<string, object?>(metadata, StringComparer.Ordinal));
    }

    internal static ContentCaptureMode ResolveToolContentCaptureMode(
        ContentCaptureMode toolMode,
        ContentCaptureMode ctxMode,
        bool ctxSet,
        ContentCaptureMode clientDefault)
    {
        var resolved = ResolveClientContentCaptureMode(clientDefault);
        if (ctxSet)
        {
            resolved = NormalizeContentCaptureMode(ctxMode);
        }
        if (toolMode != ContentCaptureMode.Default)
        {
            resolved = NormalizeContentCaptureMode(toolMode);
        }

        return resolved;
    }

    internal static bool ShouldIncludeToolContent(
        ContentCaptureMode toolMode,
        ContentCaptureMode ctxMode,
        bool ctxSet,
        ContentCaptureMode clientDefault,
        bool legacyInclude)
    {
        return ResolveToolContentCaptureMode(toolMode, ctxMode, ctxSet, clientDefault) switch
        {
            ContentCaptureMode.MetadataOnly => false,
            ContentCaptureMode.Full => true,
            _ => legacyInclude,
        };
    }

    internal static bool IsContentStripped(Generation generation)
    {
        if (generation.Metadata == null || !generation.Metadata.TryGetValue(MetadataKeyContentCaptureMode, out var value))
        {
            return false;
        }

        // After DeepClone (JSON round-trip), string values become JsonElement.
        if (value is string s)
        {
            return s == "metadata_only";
        }

        if (value is JsonElement je && je.ValueKind == JsonValueKind.String)
        {
            return je.GetString() == "metadata_only";
        }

        return false;
    }

    internal static void StripContent(Generation generation, string errorCategory)
    {
        generation.SystemPrompt = string.Empty;
        generation.Artifacts = [];

        if (!string.IsNullOrEmpty(generation.CallError))
        {
            generation.CallError = !string.IsNullOrEmpty(errorCategory) ? errorCategory : "sdk_error";
        }
        generation.Metadata.Remove("call_error");

        generation.ConversationTitle = string.Empty;
        generation.Metadata.Remove(SpanAttrConversationTitle);

        foreach (var message in generation.Input)
        {
            StripMessageContent(message);
        }
        foreach (var message in generation.Output)
        {
            StripMessageContent(message);
        }
        foreach (var tool in generation.Tools)
        {
            tool.Description = string.Empty;
            tool.InputSchemaJson = [];
        }
    }

    private static void StripMessageContent(Message message)
    {
        foreach (var part in message.Parts)
        {
            part.Text = string.Empty;
            part.Thinking = string.Empty;
            if (part.ToolCall != null)
            {
                part.ToolCall.InputJson = [];
            }
            if (part.ToolResult != null)
            {
                part.ToolResult.Content = string.Empty;
                part.ToolResult.ContentJson = [];
            }
        }
    }

    /// <summary>
    /// Merges config-default tags under per-call seed tags. Seed entries win
    /// on key collision so per-call generation tags always take precedence
    /// over env-derived defaults (matches Go's <c>client.go:71-73</c> contract).
    /// Returns a fresh dictionary so later mutations do not reach the client
    /// config.
    /// </summary>
    internal static Dictionary<string, string> MergeTagsConfigUnderSeed(
        IReadOnlyDictionary<string, string> configTags,
        IReadOnlyDictionary<string, string>? seedTags
    )
    {
        var merged = new Dictionary<string, string>(StringComparer.Ordinal);
        foreach (var pair in configTags)
        {
            merged[pair.Key] = pair.Value;
        }
        if (seedTags != null)
        {
            foreach (var pair in seedTags)
            {
                merged[pair.Key] = pair.Value;
            }
        }
        return merged;
    }
}

public sealed class GenerationRecorder
{
    internal static readonly GenerationRecorder Noop = new(null, new GenerationStart(), DateTimeOffset.UtcNow, ContentCaptureMode.Default, null, true);

    private readonly SigilClient? _client;
    private readonly GenerationStart _seed;
    private readonly DateTimeOffset _startedAt;
    private readonly ContentCaptureMode _contentCaptureMode;
    private readonly Activity? _activity;
    private readonly bool _noop;
    private IDisposable? _contextScope;

#if NET10_0_OR_GREATER
    private readonly Lock _gate = new();
#else
    private readonly object _gate = new();
#endif

    private bool _ended;
    private Exception? _callError;
    private Exception? _mappingError;
    private Generation? _result;
    private DateTimeOffset? _firstTokenAt;
    private Dictionary<string, object?>? _extraMetadata;

    public Generation? LastGeneration { get; private set; }

    public Exception? Error { get; private set; }

    internal GenerationRecorder(
        SigilClient? client,
        GenerationStart seed,
        DateTimeOffset startedAt,
        ContentCaptureMode contentCaptureMode,
        Activity? activity,
        bool noop = false
    )
    {
        _client = client;
        _seed = seed;
        _startedAt = startedAt;
        _contentCaptureMode = contentCaptureMode;
        _activity = activity;
        _noop = noop;
    }

    internal void SetContextScope(IDisposable scope)
    {
        _contextScope = scope;
    }

    public void SetCallError(Exception error)
    {
        if (_noop || error == null)
        {
            return;
        }

        lock (_gate)
        {
            _callError = error;
        }
    }

    public void SetResult(Generation generation, Exception? mappingError = null)
    {
        if (_noop)
        {
            return;
        }

        lock (_gate)
        {
            _result = InternalUtils.DeepClone(generation);
            _mappingError = mappingError;
        }
    }

    public void SetFirstTokenAt(DateTimeOffset firstTokenAt)
    {
        if (_noop)
        {
            return;
        }

        lock (_gate)
        {
            _firstTokenAt = InternalUtils.Utc(firstTokenAt);
        }
    }

    /// <summary>Attaches cache diagnostic metadata. Call before <see cref="End"/>.</summary>
    public void SetCacheDiagnostics(string missReason, long? missedInputTokens = null, string? previousMessageId = null)
    {
        if (_noop)
        {
            return;
        }

        if (string.IsNullOrWhiteSpace(missReason))
        {
            return;
        }

        lock (_gate)
        {
            if (_ended)
            {
                return;
            }

            var meta = _extraMetadata ??= new Dictionary<string, object?>(StringComparer.Ordinal);
            meta.Remove(CacheDiagnostics.MissedInputTokensKey);
            meta.Remove(CacheDiagnostics.PreviousMessageIdKey);
            meta[CacheDiagnostics.MissReasonKey] = missReason.Trim();
            if (missedInputTokens.HasValue)
            {
                meta[CacheDiagnostics.MissedInputTokensKey] = missedInputTokens.Value.ToString(
                    System.Globalization.CultureInfo.InvariantCulture
                );
            }

            if (!string.IsNullOrWhiteSpace(previousMessageId))
            {
                meta[CacheDiagnostics.PreviousMessageIdKey] = previousMessageId.Trim();
            }
        }
    }

    public void End()
    {
        if (_noop)
        {
            return;
        }

        Exception? callError;
        Exception? mappingError;
        Generation result;
        DateTimeOffset? firstTokenAt;
        Dictionary<string, object?>? snapshotExtra;

        lock (_gate)
        {
            if (_ended)
            {
                return;
            }

            _ended = true;
            callError = _callError;
            mappingError = _mappingError;
            result = _result != null ? InternalUtils.DeepClone(_result) : new Generation();
            firstTokenAt = _firstTokenAt;
            snapshotExtra = _extraMetadata == null
                ? null
                : new Dictionary<string, object?>(_extraMetadata, StringComparer.Ordinal);
        }

        Generation generation;
        try
        {
            var completedAt = _client!._config.UtcNow!();
            generation = NormalizeGeneration(result, completedAt, callError, snapshotExtra);

            var modeValue = _contentCaptureMode.ToMetadataValue();
            if (modeValue.Length > 0)
            {
                generation.Metadata[SigilClient.MetadataKeyContentCaptureMode] = modeValue;
            }

            if (_contentCaptureMode == ContentCaptureMode.MetadataOnly)
            {
                var stripErrorCategory = SigilClient.ErrorCategoryFromException(callError, false);
                SigilClient.StripContent(generation, stripErrorCategory);
            }
        }
        finally
        {
            _contextScope?.Dispose();
            _contextScope = null;
        }

        var redactErrors = _contentCaptureMode == ContentCaptureMode.MetadataOnly;

        if (_activity != null)
        {
            generation.TraceId = _activity.TraceId.ToHexString();
            generation.SpanId = _activity.SpanId.ToHexString();

            _activity.DisplayName = SigilClient.GenerationSpanName(generation.OperationName, generation.Model.Name);
            SigilClient.ApplyGenerationSpanAttributes(_activity, generation);

            if (callError != null)
            {
                SigilClient.RecordException(_activity, callError, redactErrors);
            }

            if (mappingError != null)
            {
                SigilClient.RecordException(_activity, mappingError, redactErrors);
            }
        }

        Exception? localError = null;
        try
        {
            _client.PersistGeneration(generation);
        }
        catch (ValidationException ex)
        {
            localError = ex;
        }
        catch (EnqueueException ex)
        {
            localError = ex;
        }
        catch (Exception ex)
        {
            localError = new EnqueueException($"sigil: generation enqueue failed: {ex.Message}", ex);
        }

        var errorType = string.Empty;
        var errorCategory = string.Empty;
        if (callError != null)
        {
            errorType = "provider_call_error";
            errorCategory = SigilClient.ErrorCategoryFromException(callError, true);
        }
        else if (mappingError != null)
        {
            errorType = "mapping_error";
            errorCategory = "sdk_error";
        }
        else if (localError != null)
        {
            errorType = localError is ValidationException ? "validation_error" : "enqueue_error";
            errorCategory = "sdk_error";
        }

        if (_activity != null)
        {
            if (localError != null)
            {
                SigilClient.RecordException(_activity, localError, redactErrors);
            }

            if (errorType.Length > 0)
            {
                _activity.SetTag(SigilClient.SpanAttrErrorType, errorType);
                _activity.SetTag(SigilClient.SpanAttrErrorCategory, errorCategory);
                var statusDescription = redactErrors
                    ? errorCategory
                    : (callError ?? mappingError ?? localError)?.Message;
                _activity.SetStatus(ActivityStatusCode.Error, statusDescription);
            }
            else
            {
                _activity.SetStatus(ActivityStatusCode.Ok);
            }

            _activity.Stop();
        }

        _client.RecordGenerationMetrics(generation, errorType, errorCategory, firstTokenAt);

        LastGeneration = InternalUtils.DeepClone(generation);
        Error = localError;
    }

    private Generation NormalizeGeneration(
        Generation raw,
        DateTimeOffset completedAt,
        Exception? callError,
        IReadOnlyDictionary<string, object?>? extraMetadata
    )
    {
        var generation = InternalUtils.DeepClone(raw);

        generation.Id = FirstNonEmpty(generation.Id, _seed.Id, InternalUtils.NewRandomId("gen"));
        generation.ConversationId = FirstNonEmpty(generation.ConversationId, _seed.ConversationId);
        generation.ConversationTitle = FirstNonEmpty(generation.ConversationTitle, _seed.ConversationTitle);
        generation.UserId = FirstNonEmpty(generation.UserId, _seed.UserId);
        generation.AgentName = FirstNonEmpty(generation.AgentName, _seed.AgentName);
        generation.AgentVersion = FirstNonEmpty(generation.AgentVersion, _seed.AgentVersion);
        generation.Mode ??= _seed.Mode ?? GenerationMode.Sync;
        generation.OperationName = FirstNonEmpty(
            generation.OperationName,
            _seed.OperationName,
            SigilClient.DefaultOperationNameForMode(generation.Mode.Value)
        );

        generation.Model.Provider = FirstNonEmpty(generation.Model.Provider, _seed.Model.Provider);
        generation.Model.Name = FirstNonEmpty(generation.Model.Name, _seed.Model.Name);
        generation.SystemPrompt = FirstNonEmpty(generation.SystemPrompt, _seed.SystemPrompt);
        generation.MaxTokens ??= _seed.MaxTokens;
        generation.Temperature ??= _seed.Temperature;
        generation.TopP ??= _seed.TopP;
        generation.ToolChoice = FirstNonEmpty(generation.ToolChoice ?? string.Empty, _seed.ToolChoice ?? string.Empty);
        generation.ThinkingEnabled ??= _seed.ThinkingEnabled;
        if (generation.ParentGenerationIds.Count == 0)
        {
            generation.ParentGenerationIds.AddRange(_seed.ParentGenerationIds);
        }

        if (string.IsNullOrWhiteSpace(generation.EffectiveVersion))
        {
            generation.EffectiveVersion = _seed.EffectiveVersion;
        }

        if (generation.Tools.Count == 0)
        {
            generation.Tools = InternalUtils.DeepClone(_seed.Tools);
        }

        generation.Tags = Merge(_seed.Tags, generation.Tags);
        generation.Metadata = Merge(_seed.Metadata, generation.Metadata);
        if (extraMetadata != null)
        {
            foreach (var pair in extraMetadata)
            {
                generation.Metadata[pair.Key] = pair.Value;
            }
        }

        generation.ConversationTitle = FirstNonEmpty(
            generation.ConversationTitle,
            MetadataString(generation.Metadata, SigilClient.SpanAttrConversationTitle)
        );
        generation.ConversationTitle = NormalizeResolvedString(generation.ConversationTitle);
        if (!string.IsNullOrWhiteSpace(generation.ConversationTitle))
        {
            generation.Metadata[SigilClient.SpanAttrConversationTitle] = generation.ConversationTitle;
        }

        generation.UserId = FirstNonEmpty(
            generation.UserId,
            MetadataString(generation.Metadata, SigilClient.MetadataUserIdKey),
            MetadataString(generation.Metadata, SigilClient.MetadataLegacyUserIdKey)
        );
        generation.UserId = NormalizeResolvedString(generation.UserId);
        if (!string.IsNullOrWhiteSpace(generation.UserId))
        {
            generation.Metadata[SigilClient.MetadataUserIdKey] = generation.UserId;
        }

        generation.StartedAt = generation.StartedAt.HasValue
            ? InternalUtils.Utc(generation.StartedAt.Value)
            : _startedAt;
        generation.CompletedAt = generation.CompletedAt.HasValue
            ? InternalUtils.Utc(generation.CompletedAt.Value)
            : completedAt;

        if (callError != null)
        {
            if (string.IsNullOrWhiteSpace(generation.CallError))
            {
                generation.CallError = callError.Message;
            }

            generation.Metadata["call_error"] = callError.Message;
        }

        generation.Metadata[SigilClient.SpanAttrSdkName] = SigilClient.SdkName;
        generation.Usage = generation.Usage.Normalize();
        return generation;
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

    private static string MetadataString(Dictionary<string, object?> metadata, string key)
    {
        if (!metadata.TryGetValue(key, out var value) || value == null)
        {
            return string.Empty;
        }

        var text = value.ToString()?.Trim() ?? string.Empty;
        return text;
    }

    private static string NormalizeResolvedString(string value)
    {
        return value?.Trim() ?? string.Empty;
    }

    private static Dictionary<TKey, TValue> Merge<TKey, TValue>(
        IReadOnlyDictionary<TKey, TValue> left,
        IReadOnlyDictionary<TKey, TValue> right
    )
        where TKey : notnull
    {
        var merged = new Dictionary<TKey, TValue>();
        foreach (var pair in left)
        {
            merged[pair.Key] = pair.Value;
        }

        foreach (var pair in right)
        {
            merged[pair.Key] = pair.Value;
        }

        return merged;
    }
}

public sealed class EmbeddingRecorder
{
    private readonly SigilClient? _client;
    private readonly EmbeddingStart _seed;
    private readonly DateTimeOffset _startedAt;
    private readonly Activity? _activity;
    private readonly bool _noop;

#if NET10_0_OR_GREATER
    private readonly Lock _gate = new();
#else
    private readonly object _gate = new();
#endif

    private bool _ended;
    private Exception? _callError;
    private EmbeddingResult _result = new();

    public Exception? Error { get; private set; }

    internal EmbeddingRecorder(
        SigilClient? client,
        EmbeddingStart seed,
        DateTimeOffset startedAt,
        Activity? activity,
        bool noop = false
    )
    {
        _client = client;
        _seed = seed;
        _startedAt = startedAt;
        _activity = activity;
        _noop = noop;
    }

    public void SetCallError(Exception error)
    {
        if (_noop || error == null)
        {
            return;
        }

        lock (_gate)
        {
            _callError = error;
        }
    }

    public void SetResult(EmbeddingResult result)
    {
        if (_noop)
        {
            return;
        }

        lock (_gate)
        {
            _result = InternalUtils.DeepClone(result ?? new EmbeddingResult());
        }
    }

    public void End()
    {
        if (_noop)
        {
            return;
        }

        Exception? callError;
        EmbeddingResult result;

        lock (_gate)
        {
            if (_ended)
            {
                return;
            }

            _ended = true;
            callError = _callError;
            result = InternalUtils.DeepClone(_result);
        }

        Exception? localError = null;
        try
        {
            GenerationValidator.ValidateEmbeddingStart(_seed);
            GenerationValidator.ValidateEmbeddingResult(result);
        }
        catch (Exception ex)
        {
            localError = new ValidationException($"sigil: embedding validation failed: {ex.Message}");
        }

        var errorType = string.Empty;
        var errorCategory = string.Empty;
        if (callError != null)
        {
            errorType = "provider_call_error";
            errorCategory = SigilClient.ErrorCategoryFromException(callError, true);
        }
        else if (localError != null)
        {
            errorType = "validation_error";
            errorCategory = "sdk_error";
        }

        var completedAt = _client!._config.UtcNow!();
        if (_activity != null)
        {
            _activity.DisplayName = SigilClient.EmbeddingSpanName(_seed.Model.Name);
            SigilClient.ApplyEmbeddingStartSpanAttributes(_activity, _seed);
            SigilClient.ApplyEmbeddingEndSpanAttributes(_activity, result, _client.EmbeddingCapture);

            if (callError != null)
            {
                SigilClient.RecordException(_activity, callError);
            }
            if (localError != null)
            {
                SigilClient.RecordException(_activity, localError);
            }

            if (errorType.Length > 0)
            {
                _activity.SetTag(SigilClient.SpanAttrErrorType, errorType);
                _activity.SetTag(SigilClient.SpanAttrErrorCategory, errorCategory);
                _activity.SetStatus(ActivityStatusCode.Error, (callError ?? localError)?.Message);
            }
            else
            {
                _activity.SetStatus(ActivityStatusCode.Ok);
            }

            _activity.SetEndTime(completedAt.UtcDateTime);
            _activity.Stop();
        }

        _client.RecordEmbeddingMetrics(_seed, result, _startedAt, completedAt, errorType, errorCategory);
        Error = localError;
    }
}

public sealed class ToolExecutionRecorder
{
    internal static readonly ToolExecutionRecorder Noop = new(null, new ToolExecutionStart(), DateTimeOffset.UtcNow, false, false, null, true);

    private readonly SigilClient? _client;
    private readonly ToolExecutionStart _seed;
    private readonly DateTimeOffset _startedAt;
    private readonly bool _includeContent;
    private readonly bool _redactErrors;
    private readonly Activity? _activity;
    private readonly bool _noop;

#if NET10_0_OR_GREATER
    private readonly Lock _gate = new();
#else
    private readonly object _gate = new();
#endif

    private bool _ended;
    private Exception? _executionError;
    private ToolExecutionEnd _result = new();

    public Exception? Error { get; private set; }

    internal ToolExecutionRecorder(
        SigilClient? client,
        ToolExecutionStart seed,
        DateTimeOffset startedAt,
        bool includeContent,
        bool redactErrors,
        Activity? activity,
        bool noop = false
    )
    {
        _client = client;
        _seed = seed;
        _startedAt = startedAt;
        _includeContent = includeContent;
        _redactErrors = redactErrors;
        _activity = activity;
        _noop = noop;
    }

    public void SetExecutionError(Exception error)
    {
        if (_noop || error == null)
        {
            return;
        }

        lock (_gate)
        {
            _executionError = error;
        }
    }

    public void SetResult(ToolExecutionEnd result)
    {
        if (_noop)
        {
            return;
        }

        lock (_gate)
        {
            _result = InternalUtils.DeepClone(result);
        }
    }

    public void End()
    {
        if (_noop)
        {
            return;
        }

        Exception? executionError;
        ToolExecutionEnd result;

        lock (_gate)
        {
            if (_ended)
            {
                return;
            }

            _ended = true;
            executionError = _executionError;
            result = InternalUtils.DeepClone(_result);
        }

        var finalError = executionError;
        var completedAt = result.CompletedAt.HasValue
            ? InternalUtils.Utc(result.CompletedAt.Value)
            : _client!._config.UtcNow!();

        if (_activity != null)
        {
            _activity.DisplayName = SigilClient.ToolSpanName(_seed.ToolName);
            SigilClient.ApplyToolSpanAttributes(_activity, _seed);

            if (_includeContent)
            {
                try
                {
                    var arguments = InternalUtils.SerializeJson(result.Arguments);
                    if (!string.IsNullOrWhiteSpace(arguments))
                    {
                        _activity.SetTag(SigilClient.SpanAttrToolCallArguments, arguments);
                    }

                    var resultJson = InternalUtils.SerializeJson(result.Result);
                    if (!string.IsNullOrWhiteSpace(resultJson))
                    {
                        _activity.SetTag(SigilClient.SpanAttrToolCallResult, resultJson);
                    }
                }
                catch (Exception ex)
                {
                    finalError = finalError != null ? new AggregateException(finalError, ex) : ex;
                }
            }

            if (finalError != null)
            {
                SigilClient.RecordException(_activity, finalError, _redactErrors);
                var errorCategory = SigilClient.ErrorCategoryFromException(finalError, true);
                _activity.SetTag(SigilClient.SpanAttrErrorType, "tool_execution_error");
                _activity.SetTag(SigilClient.SpanAttrErrorCategory, errorCategory);
                _activity.SetStatus(ActivityStatusCode.Error, _redactErrors ? errorCategory : finalError.Message);
            }
            else
            {
                _activity.SetStatus(ActivityStatusCode.Ok);
            }

            _activity.SetEndTime(completedAt.UtcDateTime);
            _activity.Stop();
        }

        _client!.RecordToolExecutionMetrics(_seed, _startedAt, completedAt, finalError);
        Error = finalError;
    }
}
