using Anthropic.Models.Messages;
using System.Diagnostics;
using System.Reflection;
using System.Text.Json;
using Xunit;
using AnthropicMessage = Anthropic.Models.Messages.Message;

namespace Grafana.Sigil.Anthropic.Tests;

public sealed class AnthropicConformanceTests
{
    [Fact]
    public void FromRequestResponse_MapsSyncModeAndDefaultsRawArtifactsOff()
    {
        var request = CreateRequest();
        var response = CreateResponse();

        var generation = AnthropicGenerationMapper.FromRequestResponse(
            request,
            response,
            new AnthropicSigilOptions
            {
                ConversationId = "conv-1",
                AgentName = "agent-anthropic",
                AgentVersion = "v-anthropic",
            }
        );

        Assert.Equal(GenerationMode.Sync, generation.Mode);
        Assert.Equal("conv-1", generation.ConversationId);
        Assert.Equal("Be precise.", generation.SystemPrompt);
        Assert.Equal("msg_1", generation.ResponseId);
        Assert.Equal("end_turn", generation.StopReason);
        Assert.Equal(512, generation.MaxTokens);
        Assert.Equal(0.3, generation.Temperature);
        Assert.Equal(0.8, generation.TopP);
        Assert.Contains("weather", generation.ToolChoice ?? string.Empty, StringComparison.OrdinalIgnoreCase);
        Assert.True(generation.ThinkingEnabled);
        Assert.Equal(2048L, ReadThinkingBudget(generation));
        Assert.Equal(2L, ReadMetadataLong(generation, "sigil.gen_ai.usage.server_tool_use.web_search_requests"));
        var webFetchRequests = TryReadMetadataLong(generation, "sigil.gen_ai.usage.server_tool_use.web_fetch_requests") ?? 0L;
        Assert.Equal(2L + webFetchRequests, ReadMetadataLong(generation, "sigil.gen_ai.usage.server_tool_use.total_requests"));
        Assert.Equal(162, generation.Usage.TotalTokens);
        Assert.Equal(30, generation.Usage.CacheReadInputTokens);
        Assert.Equal(10, generation.Usage.CacheWriteInputTokens);
        Assert.Empty(generation.Artifacts);
    }

    [Fact]
    public void FromStream_MapsStreamMode_AndRawArtifactsOptIn()
    {
        var request = CreateRequest();
        var summary = new AnthropicStreamSummary();
        summary.Events.Add(CreateMessageStartEvent("msg_stream_1", "stream output"));
        summary.Events.Add(CreateMessageDeltaEvent(80, 25, 8, 4, 3, 2));

        var generation = AnthropicGenerationMapper.FromStream(request, summary, new AnthropicSigilOptions().WithRawArtifacts());

        Assert.Equal(GenerationMode.Stream, generation.Mode);
        Assert.Equal("msg_stream_1", generation.ResponseId);
        Assert.Equal("end_turn", generation.StopReason);
        Assert.Equal(512, generation.MaxTokens);
        Assert.Equal(0.3, generation.Temperature);
        Assert.Equal(0.8, generation.TopP);
        Assert.Contains("weather", generation.ToolChoice ?? string.Empty, StringComparison.OrdinalIgnoreCase);
        Assert.True(generation.ThinkingEnabled);
        Assert.Equal(2048L, ReadThinkingBudget(generation));
        Assert.Equal(3L, ReadMetadataLong(generation, "sigil.gen_ai.usage.server_tool_use.web_search_requests"));
        var streamWebFetchRequests = TryReadMetadataLong(generation, "sigil.gen_ai.usage.server_tool_use.web_fetch_requests") ?? 0L;
        Assert.Equal(3L + streamWebFetchRequests, ReadMetadataLong(generation, "sigil.gen_ai.usage.server_tool_use.total_requests"));
        Assert.Equal(105, generation.Usage.TotalTokens);
        Assert.Contains(generation.Artifacts, artifact => artifact.Kind == ArtifactKind.ProviderEvent);
    }

    [Fact]
    public async Task Recorder_SyncAndStreamModes_AreRecordedWithProviderErrorPropagation()
    {
        var exporter = new CapturingExporter();
        var client = new SigilClient(new SigilClientConfig
        {
            GenerationExporter = exporter,
            GenerationExport = new GenerationExportConfig
            {
                BatchSize = 1,
                QueueSize = 10,
                FlushInterval = TimeSpan.FromHours(1),
            },
        });

        var request = CreateRequest();

        await Assert.ThrowsAsync<InvalidOperationException>(() => AnthropicRecorder.MessageAsync(
            client,
            request,
            (_, _) => throw new InvalidOperationException("provider failed"),
            new AnthropicSigilOptions
            {
                ModelName = "claude-sonnet-4-5",
            },
            TestContext.Current.CancellationToken
        ));

        var streamSummary = await AnthropicRecorder.MessageStreamAsync(
            client,
            request,
            (_, _) => StreamEvents(),
            new AnthropicSigilOptions
            {
                ModelName = "claude-sonnet-4-5",
            },
            TestContext.Current.CancellationToken
        );

        Assert.NotEmpty(streamSummary.Events);

        await client.FlushAsync(TestContext.Current.CancellationToken);
        await client.ShutdownAsync(TestContext.Current.CancellationToken);

        var generations = exporter.Requests.SelectMany(request => request.Generations).ToList();
        Assert.True(generations.Count >= 2);
        Assert.Contains(generations, generation => generation.Mode == GenerationMode.Sync && generation.CallError.Contains("provider failed", StringComparison.Ordinal));
        Assert.Contains(generations, generation => generation.Mode == GenerationMode.Stream);
    }

    [Fact]
    public async Task Recorder_StreamMappingErrors_PreserveReturnedSummaries_AndMarkSpans()
    {
        var exporter = new CapturingExporter();
        var spans = new List<Activity>();
        using var listener = NewGenerationListener(spans);
        ActivitySource.AddActivityListener(listener);

        await using var client = new SigilClient(new SigilClientConfig
        {
            GenerationExporter = exporter,
            GenerationExport = new GenerationExportConfig
            {
                BatchSize = 1,
                QueueSize = 10,
                FlushInterval = TimeSpan.FromHours(1),
            },
        });

        var summary = await AnthropicRecorder.MessageStreamAsync(
            client,
            CreateRequest(),
            (_, _) => EmptyStreamEvents(),
            new AnthropicSigilOptions
            {
                ModelName = "claude-sonnet-4-5",
            },
            TestContext.Current.CancellationToken
        );

        Assert.Empty(summary.Events);

        await client.FlushAsync(TestContext.Current.CancellationToken);
        await client.ShutdownAsync(TestContext.Current.CancellationToken);

        var generations = exporter.Requests.SelectMany(request => request.Generations).ToList();
        Assert.Single(generations);
        Assert.Equal(GenerationMode.Stream, generations[0].Mode);
        Assert.Equal(string.Empty, generations[0].CallError);
        Assert.Single(spans, span => span.GetTagItem("error.type")?.ToString() == "mapping_error");
    }

    [Fact]
    public void MapperRejectsMissingOrMalformedResponses()
    {
        Assert.Throws<ArgumentNullException>(() => AnthropicGenerationMapper.FromRequestResponse(
            CreateRequest(),
            response: null!,
            new AnthropicSigilOptions()
        ));
        Assert.Throws<ArgumentException>(() => AnthropicGenerationMapper.FromStream(
            CreateRequest(),
            new AnthropicStreamSummary(),
            new AnthropicSigilOptions()
        ));
    }

    [Fact]
    public void EmbeddingConformance_IsExplicitlyUnsupportedWithoutPublicSurface()
    {
        Assert.NotNull(typeof(AnthropicRecorder));
        Assert.Null(typeof(AnthropicRecorder).Assembly.GetType("Grafana.Sigil.Anthropic.AnthropicEmbeddings"));
    }

    private static MessageCreateParams CreateRequest()
    {
        var request = new MessageCreateParams
        {
            MaxTokens = 512,
            Model = Model.ClaudeSonnet4_5,
            System = "Be precise.",
            Messages =
            [
                new MessageParam
                {
                    Role = Role.User,
                    Content = "What's the weather in Paris?",
                },
            ],
        };

        SetIfPresent(request, "Temperature", 0.3);
        SetIfPresent(request, "TopP", 0.8);

        var toolChoice = CreateType(
            request.GetType().Assembly,
            "Anthropic.Models.Messages.ToolChoiceTool",
            instance => SetIfPresent(instance, "Name", "weather")
        );
        if (toolChoice != null)
        {
            SetIfPresent(request, "ToolChoice", toolChoice);
        }

        var thinking = CreateType(
            request.GetType().Assembly,
            "Anthropic.Models.Messages.ThinkingConfigEnabled",
            instance => SetIfPresent(instance, "BudgetTokens", 2048L)
        );
        if (thinking != null)
        {
            SetIfPresent(request, "Thinking", thinking);
        }

        return request;
    }

    private static long ReadThinkingBudget(Generation generation)
    {
        var raw = generation.Metadata["sigil.gen_ai.request.thinking.budget_tokens"];
        return raw switch
        {
            JsonElement json when json.ValueKind == JsonValueKind.Number && json.TryGetInt64(out var parsed) => parsed,
            IConvertible convertible => Convert.ToInt64(convertible),
            _ => throw new InvalidOperationException("unexpected thinking budget metadata type"),
        };
    }

    private static long ReadMetadataLong(Generation generation, string key)
    {
        var raw = generation.Metadata[key];
        return raw switch
        {
            JsonElement json when json.ValueKind == JsonValueKind.Number && json.TryGetInt64(out var parsed) => parsed,
            IConvertible convertible => Convert.ToInt64(convertible),
            _ => throw new InvalidOperationException($"unexpected metadata type for {key}"),
        };
    }

    private static long? TryReadMetadataLong(Generation generation, string key)
    {
        if (!generation.Metadata.TryGetValue(key, out var raw))
        {
            return null;
        }

        return raw switch
        {
            JsonElement json when json.ValueKind == JsonValueKind.Number && json.TryGetInt64(out var parsed) => parsed,
            IConvertible convertible => Convert.ToInt64(convertible),
            _ => null,
        };
    }

    private static async IAsyncEnumerable<RawMessageStreamEvent> EmptyStreamEvents()
    {
        await Task.CompletedTask;
        yield break;
    }

    private static AnthropicMessage CreateResponse()
    {
        var usage = new Usage
        {
            InputTokens = 120,
            OutputTokens = 42,
            CacheReadInputTokens = 30,
            CacheCreationInputTokens = 10,
            InferenceGeo = "us",
            CacheCreation = null,
            ServerToolUse = null,
            ServiceTier = null,
        };
        var serverToolUse = CreateType(
            typeof(Usage).Assembly,
            "Anthropic.Models.Messages.ServerToolUsage",
            instance =>
            {
                SetIfPresent(instance, "WebSearchRequests", 2L);
                SetIfPresent(instance, "WebFetchRequests", 1L);
            }
        );
        if (serverToolUse != null)
        {
            SetIfPresent(usage, "ServerToolUse", serverToolUse);
        }

        return new AnthropicMessage
        {
            Container = default!,
            ID = "msg_1",
            Content =
            [
                new TextBlock
                {
                    Text = "It's 18C and sunny.",
                    Citations = null,
                    Type = JsonSerializer.SerializeToElement("text"),
                },
                new ThinkingBlock
                {
                    Signature = "sig_1",
                    Thinking = "done",
                    Type = JsonSerializer.SerializeToElement("thinking"),
                },
            ],
            Model = Model.ClaudeSonnet4_5,
            StopReason = StopReason.EndTurn,
            StopSequence = null,
            Usage = usage,
        };
    }

    private static async IAsyncEnumerable<RawMessageStreamEvent> StreamEvents()
    {
        yield return CreateMessageStartEvent("msg_stream_recorder", "hello");
        yield return CreateMessageDeltaEvent(2, 1, null, null, null, null);
        await Task.CompletedTask;
    }

    private static RawMessageStreamEvent CreateMessageStartEvent(string id, string text)
    {
        return new RawMessageStreamEvent(new RawMessageStartEvent
        {
            Type = JsonSerializer.SerializeToElement("message_start"),
            Message = new AnthropicMessage
            {
                Container = default!,
                ID = id,
                Model = Model.ClaudeSonnet4_5,
                Content =
                [
                    new TextBlock
                    {
                        Type = JsonSerializer.SerializeToElement("text"),
                        Text = text,
                        Citations = null,
                    },
                ],
                StopReason = null,
                StopSequence = null,
                Usage = new Usage
                {
                    InputTokens = 0,
                    OutputTokens = 0,
                    CacheCreation = null,
                    CacheCreationInputTokens = null,
                    CacheReadInputTokens = null,
                    InferenceGeo = "us",
                    ServerToolUse = null,
                    ServiceTier = null,
                },
            },
        });
    }

    private static RawMessageStreamEvent CreateMessageDeltaEvent(
        long inputTokens,
        long outputTokens,
        long? cacheReadTokens,
        long? cacheCreationTokens,
        long? webSearchRequests,
        long? webFetchRequests
    )
    {
        var usage = new MessageDeltaUsage
        {
            InputTokens = inputTokens,
            OutputTokens = outputTokens,
            CacheReadInputTokens = cacheReadTokens,
            CacheCreationInputTokens = cacheCreationTokens,
            ServerToolUse = null,
        };
        var serverToolUse = CreateType(
            typeof(MessageDeltaUsage).Assembly,
            "Anthropic.Models.Messages.ServerToolUsage",
            instance =>
            {
                if (webSearchRequests.HasValue)
                {
                    SetIfPresent(instance, "WebSearchRequests", webSearchRequests.Value);
                }
                if (webFetchRequests.HasValue)
                {
                    SetIfPresent(instance, "WebFetchRequests", webFetchRequests.Value);
                }
            }
        );
        if (serverToolUse != null)
        {
            SetIfPresent(usage, "ServerToolUse", serverToolUse);
        }

        return new RawMessageStreamEvent(new RawMessageDeltaEvent
        {
            Type = JsonSerializer.SerializeToElement("message_delta"),
            Delta = new Delta
            {
                Container = default!,
                StopReason = StopReason.EndTurn,
                StopSequence = null,
            },
            Usage = usage,
        });
    }

    private static object? CreateType(Assembly assembly, string typeName, Action<object>? configure = null)
    {
        var type = assembly.GetType(typeName);
        if (type == null)
        {
            return null;
        }

        var instance = Activator.CreateInstance(type);
        if (instance == null)
        {
            return null;
        }

        configure?.Invoke(instance);
        return instance;
    }

    private static void SetIfPresent(object target, string propertyName, object? value)
    {
        var property = target.GetType().GetProperty(propertyName);
        if (property == null || !property.CanWrite)
        {
            return;
        }

        var converted = ConvertIfNeeded(value, property.PropertyType);
        if (converted != null || !property.PropertyType.IsValueType || Nullable.GetUnderlyingType(property.PropertyType) != null)
        {
            property.SetValue(target, converted);
        }
    }

    private static object? ConvertIfNeeded(object? value, System.Type destinationType)
    {
        if (value == null)
        {
            return null;
        }

        var targetType = Nullable.GetUnderlyingType(destinationType) ?? destinationType;
        if (targetType.IsInstanceOfType(value))
        {
            return value;
        }

        if (targetType.IsEnum)
        {
            if (value is string text)
            {
                return Enum.Parse(targetType, text, ignoreCase: true);
            }

            return Enum.ToObject(targetType, value);
        }

        var implicitOperator = targetType
            .GetMethods(BindingFlags.Public | BindingFlags.Static)
            .FirstOrDefault(method =>
                method.Name == "op_Implicit"
                && method.ReturnType == targetType
                && method.GetParameters().Length == 1
                && method.GetParameters()[0].ParameterType.IsInstanceOfType(value));
        if (implicitOperator != null)
        {
            return implicitOperator.Invoke(null, [value]);
        }

        var convertingCtor = targetType
            .GetConstructors(BindingFlags.Public | BindingFlags.NonPublic | BindingFlags.Instance)
            .FirstOrDefault(ctor =>
                ctor.GetParameters().Length == 1
                && ctor.GetParameters()[0].ParameterType.IsInstanceOfType(value));
        if (convertingCtor != null)
        {
            return convertingCtor.Invoke([value]);
        }

        var wrapperCtor = targetType
            .GetConstructors(BindingFlags.Public | BindingFlags.NonPublic | BindingFlags.Instance)
            .FirstOrDefault(ctor =>
            {
                var parameters = ctor.GetParameters();
                if (parameters.Length <= 1 || !parameters[0].ParameterType.IsInstanceOfType(value))
                {
                    return false;
                }

                for (var index = 1; index < parameters.Length; index++)
                {
                    var parameterType = parameters[index].ParameterType;
                    if (parameterType.IsValueType && Nullable.GetUnderlyingType(parameterType) == null)
                    {
                        return false;
                    }
                }

                return true;
            });
        if (wrapperCtor != null)
        {
            var parameters = wrapperCtor.GetParameters();
            var args = new object?[parameters.Length];
            args[0] = value;
            for (var index = 1; index < parameters.Length; index++)
            {
                args[index] = null;
            }

            return wrapperCtor.Invoke(args);
        }

        try
        {
            return Convert.ChangeType(value, targetType);
        }
        catch
        {
            return value;
        }
    }

    private sealed class CapturingExporter : IGenerationExporter
    {
        public List<ExportGenerationsRequest> Requests { get; } = [];

        public Task<ExportGenerationsResponse> ExportGenerationsAsync(ExportGenerationsRequest request, CancellationToken cancellationToken)
        {
            Requests.Add(request);
            return Task.FromResult(new ExportGenerationsResponse
            {
                Results = [.. request.Generations.Select(generation => new ExportGenerationResult
                {
                    GenerationId = generation.Id,
                    Accepted = true,
                })],
            });
        }

        public Task ShutdownAsync(CancellationToken cancellationToken)
        {
            return Task.CompletedTask;
        }
    }

    private static ActivityListener NewGenerationListener(List<Activity> spans)
    {
        return new ActivityListener
        {
            ShouldListenTo = source => source.Name == "github.com/grafana/sigil/sdks/dotnet",
            Sample = static (ref _) => ActivitySamplingResult.AllDataAndRecorded,
            ActivityStopped = activity =>
            {
                if (activity.GetTagItem("gen_ai.operation.name")?.ToString() != "execute_tool")
                {
                    spans.Add(activity);
                }
            },
        };
    }
}
