using Google.GenAI.Types;
using System.Diagnostics;
using System.Reflection;
using Xunit;
using GPart = Google.GenAI.Types.Part;

namespace Grafana.Sigil.Gemini.Tests;

public sealed class GeminiConformanceTests
{
    private const string DefaultModel = "gemini-2.5-pro";

    [Fact]
    public void FromRequestResponse_MapsSyncModeAndDefaultsRawArtifactsOff()
    {
        var contents = CreateContents();
        var config = CreateConfig();
        var response = CreateResponse();

        var generation = GeminiGenerationMapper.FromRequestResponse(
            DefaultModel,
            contents,
            config,
            response,
            new GeminiSigilOptions
            {
                ConversationId = "conv-1",
                AgentName = "agent-gemini",
                AgentVersion = "v-gemini",
            }
        );

        Assert.Equal(GenerationMode.Sync, generation.Mode);
        Assert.Equal("conv-1", generation.ConversationId);
        Assert.Equal("resp_1", generation.ResponseId);
        Assert.Equal("gemini-2.5-pro-001", generation.ResponseModel);
        Assert.Equal("STOP", generation.StopReason);
        Assert.Equal(444, generation.MaxTokens);
        Assert.InRange(generation.Temperature ?? 0, 0.149, 0.151);
        Assert.InRange(generation.TopP ?? 0, 0.799, 0.801);
        Assert.Contains("any", generation.ToolChoice ?? string.Empty, StringComparison.OrdinalIgnoreCase);
        Assert.True(generation.ThinkingEnabled);
        Assert.Equal(1536L, ReadThinkingBudget(generation));
        Assert.Equal("medium", ReadThinkingLevel(generation));
        Assert.Equal(7L, ReadMetadataLong(generation, "sigil.gen_ai.usage.tool_use_prompt_tokens"));
        Assert.Equal(170, generation.Usage.TotalTokens);
        Assert.Equal(12, generation.Usage.CacheReadInputTokens);
        Assert.Equal(10, generation.Usage.ReasoningTokens);
        Assert.Empty(generation.Artifacts);
        Assert.Contains(generation.Input, message => message.Role == MessageRole.Tool);
    }

    [Fact]
    public void FromStream_MapsStreamMode_AndRawArtifactsOptIn()
    {
        var contents = CreateContents();
        var config = CreateConfig();
        var summary = new GeminiStreamSummary();
        summary.Responses.Add(new GenerateContentResponse
        {
            ResponseId = "resp_stream_1",
            ModelVersion = "gemini-2.5-pro-001",
            Candidates =
            [
                new Candidate
                {
                    Content = new Content
                    {
                        Role = "model",
                        Parts =
                        [
                            new GPart
                            {
                                FunctionCall = new FunctionCall
                                {
                                    Id = "call_weather",
                                    Name = "weather",
                                    Args = new Dictionary<string, object>
                                    {
                                        ["city"] = "Paris",
                                    },
                                },
                            },
                        ],
                    },
                },
            ],
        });
        summary.Responses.Add(new GenerateContentResponse
        {
            ResponseId = "resp_stream_2",
            ModelVersion = "gemini-2.5-pro-001",
            Candidates =
            [
                new Candidate
                {
                    FinishReason = FinishReason.Stop,
                    Content = new Content
                    {
                        Role = "model",
                        Parts =
                        [
                            new GPart
                            {
                                Text = "It is sunny.",
                            },
                        ],
                    },
                },
            ],
            UsageMetadata = new GenerateContentResponseUsageMetadata
            {
                PromptTokenCount = 20,
                CandidatesTokenCount = 6,
                TotalTokenCount = 26,
                ToolUsePromptTokenCount = 4,
            },
        });

        var generation = GeminiGenerationMapper.FromStream(DefaultModel, contents, config, summary, new GeminiSigilOptions().WithRawArtifacts());

        Assert.Equal(GenerationMode.Stream, generation.Mode);
        Assert.Equal("resp_stream_2", generation.ResponseId);
        Assert.Equal("STOP", generation.StopReason);
        Assert.Equal(444, generation.MaxTokens);
        Assert.InRange(generation.Temperature ?? 0, 0.149, 0.151);
        Assert.InRange(generation.TopP ?? 0, 0.799, 0.801);
        Assert.Contains("any", generation.ToolChoice ?? string.Empty, StringComparison.OrdinalIgnoreCase);
        Assert.True(generation.ThinkingEnabled);
        Assert.Equal(1536L, ReadThinkingBudget(generation));
        Assert.Equal("medium", ReadThinkingLevel(generation));
        Assert.Equal(4L, ReadMetadataLong(generation, "sigil.gen_ai.usage.tool_use_prompt_tokens"));
        Assert.Equal(26, generation.Usage.TotalTokens);
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

        var contents = CreateContents();
        var config = CreateConfig();

        await Assert.ThrowsAsync<InvalidOperationException>(() => GeminiRecorder.GenerateContentAsync(
            client,
            DefaultModel,
            contents,
            (_, _, _, _) => throw new InvalidOperationException("provider failed"),
            config,
            new GeminiSigilOptions
            {
                ModelName = "gemini-2.5-pro",
            },
            TestContext.Current.CancellationToken
        ));

        var streamSummary = await GeminiRecorder.GenerateContentStreamAsync(
            client,
            DefaultModel,
            contents,
            (_, _, _, _) => StreamResponses(),
            config,
            new GeminiSigilOptions
            {
                ModelName = "gemini-2.5-pro",
            },
            TestContext.Current.CancellationToken
        );

        Assert.NotEmpty(streamSummary.Responses);

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

        var summary = await GeminiRecorder.GenerateContentStreamAsync(
            client,
            DefaultModel,
            CreateContents(),
            (_, _, _, _) => EmptyStreamResponses(),
            CreateConfig(),
            new GeminiSigilOptions
            {
                ModelName = DefaultModel,
            },
            TestContext.Current.CancellationToken
        );

        Assert.Empty(summary.Responses);

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
        Assert.Throws<ArgumentNullException>(() => GeminiGenerationMapper.FromRequestResponse(
            DefaultModel,
            CreateContents(),
            CreateConfig(),
            response: null!,
            new GeminiSigilOptions()
        ));
        Assert.Throws<ArgumentException>(() => GeminiGenerationMapper.FromStream(
            DefaultModel,
            CreateContents(),
            CreateConfig(),
            new GeminiStreamSummary(),
            new GeminiSigilOptions()
        ));
    }

    [Fact]
    public void EmbeddingFromResponse_MapsInputCountUsageAndDimensions()
    {
        var embedContents = CreateEmbeddingContents();
        var config = new EmbedContentConfig
        {
            OutputDimensionality = 64,
        };
        var response = CreateEmbeddingResponse();

        var result = GeminiGenerationMapper.EmbeddingFromResponse(
            "gemini-embedding-001",
            embedContents,
            config,
            response
        );

        Assert.Equal(2, result.InputCount);
        Assert.Equal(18, result.InputTokens);
        Assert.Equal(3, result.Dimensions);
        Assert.Equal<string>(["alpha", "beta"], result.InputTexts);
    }

    [Fact]
    public async Task Recorder_EmbedContent_DoesNotEnqueueAndPropagatesProviderErrors()
    {
        var exporter = new CapturingExporter();
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

        var model = "gemini-embedding-001";
        var contents = CreateEmbeddingContents();
        var config = new EmbedContentConfig
        {
            OutputDimensionality = 64,
        };

        await Assert.ThrowsAsync<InvalidOperationException>(() => GeminiRecorder.EmbedContentAsync(
            client,
            model,
            contents,
            (_, _, _, _) => Task.FromException<EmbedContentResponse>(new InvalidOperationException("embedding provider failed")),
            config,
            new GeminiSigilOptions
            {
                ModelName = model,
            },
            TestContext.Current.CancellationToken
        ));

        var wrapped = await GeminiRecorder.EmbedContentAsync(
            client,
            model,
            contents,
            (_, _, _, _) => Task.FromResult(CreateEmbeddingResponse()),
            config,
            new GeminiSigilOptions
            {
                ModelName = model,
            },
            TestContext.Current.CancellationToken
        );

        Assert.NotEmpty(wrapped.Embeddings ?? []);

        await client.FlushAsync(TestContext.Current.CancellationToken);
        await client.ShutdownAsync(TestContext.Current.CancellationToken);

        Assert.Empty(exporter.Requests);
    }

    [Fact]
    public void FromRequestResponse_MapsThinkingDisabled()
    {
        var config = CreateConfig();
        config.ThinkingConfig = new ThinkingConfig
        {
            IncludeThoughts = false,
        };

        var generation = GeminiGenerationMapper.FromRequestResponse(DefaultModel, CreateContents(), config, CreateResponse());
        Assert.False(generation.ThinkingEnabled);
    }

    private static List<Content> CreateContents()
    {
        return
        [
            new Content
            {
                Role = "user",
                Parts =
                [
                    new GPart
                    {
                        Text = "What is the weather in Paris?",
                    },
                ],
            },
            new Content
            {
                Role = "user",
                Parts =
                [
                    new GPart
                    {
                        FunctionResponse = new FunctionResponse
                        {
                            Id = "call_weather",
                            Name = "weather",
                            Response = new Dictionary<string, object>
                            {
                                ["output"] = "18C and sunny",
                            },
                        },
                    },
                ],
            },
        ];
    }

    private static GenerateContentConfig CreateConfig()
    {
        var thinkingConfig = new ThinkingConfig
        {
            IncludeThoughts = true,
            ThinkingBudget = 1536,
        };
        SetIfPresent(thinkingConfig, "ThinkingLevel", "medium");

        return new GenerateContentConfig
        {
            MaxOutputTokens = 444,
            Temperature = 0.15f,
            TopP = 0.8f,
            ToolConfig = new ToolConfig
            {
                FunctionCallingConfig = new FunctionCallingConfig
                {
                    Mode = FunctionCallingConfigMode.Any,
                },
            },
            ThinkingConfig = thinkingConfig,
            SystemInstruction = new Content
            {
                Role = "user",
                Parts =
                [
                    new GPart
                    {
                        Text = "Be concise.",
                    },
                ],
            },
            Tools =
            [
                new Tool
                {
                    FunctionDeclarations =
                    [
                        new FunctionDeclaration
                        {
                            Name = "weather",
                            Description = "Get weather",
                            ParametersJsonSchema = new Dictionary<string, object>
                            {
                                ["type"] = "object",
                            },
                        },
                    ],
                },
            ],
        };
    }

    private static GenerateContentResponse CreateResponse()
    {
        return new GenerateContentResponse
        {
            ResponseId = "resp_1",
            ModelVersion = "gemini-2.5-pro-001",
            Candidates =
            [
                new Candidate
                {
                    FinishReason = FinishReason.Stop,
                    Content = new Content
                    {
                        Role = "model",
                        Parts =
                        [
                            new GPart
                            {
                                FunctionCall = new FunctionCall
                                {
                                    Id = "call_weather",
                                    Name = "weather",
                                    Args = new Dictionary<string, object>
                                    {
                                        ["city"] = "Paris",
                                    },
                                },
                            },
                            new GPart
                            {
                                Text = "It is 18C and sunny.",
                            },
                        ],
                    },
                },
            ],
            UsageMetadata = new GenerateContentResponseUsageMetadata
            {
                PromptTokenCount = 120,
                CandidatesTokenCount = 40,
                TotalTokenCount = 170,
                CachedContentTokenCount = 12,
                ThoughtsTokenCount = 10,
                ToolUsePromptTokenCount = 7,
            },
        };
    }

    private static List<Content> CreateEmbeddingContents()
    {
        return
        [
            new Content
            {
                Role = "user",
                Parts =
                [
                    new GPart
                    {
                        Text = "alpha",
                    },
                ],
            },
            new Content
            {
                Role = "user",
                Parts =
                [
                    new GPart
                    {
                        Text = "beta",
                    },
                ],
            },
        ];
    }

    private static EmbedContentResponse CreateEmbeddingResponse()
    {
        return new EmbedContentResponse
        {
            Embeddings =
            [
                new ContentEmbedding
                {
                    Values = [0.1, 0.2, 0.3],
                    Statistics = new ContentEmbeddingStatistics
                    {
                        TokenCount = 11,
                    },
                },
                new ContentEmbedding
                {
                    Statistics = new ContentEmbeddingStatistics
                    {
                        TokenCount = 7,
                    },
                },
            ],
        };
    }

    private static long ReadThinkingBudget(Generation generation)
    {
        var raw = generation.Metadata["sigil.gen_ai.request.thinking.budget_tokens"];
        return raw switch
        {
            System.Text.Json.JsonElement json
                when json.ValueKind == System.Text.Json.JsonValueKind.Number && json.TryGetInt64(out var parsed) => parsed,
            IConvertible convertible => Convert.ToInt64(convertible),
            _ => throw new InvalidOperationException("unexpected thinking budget metadata type"),
        };
    }

    private static string ReadThinkingLevel(Generation generation)
    {
        var raw = generation.Metadata["sigil.gen_ai.request.thinking.level"];
        return raw switch
        {
            System.Text.Json.JsonElement json when json.ValueKind == System.Text.Json.JsonValueKind.String => json.GetString() ?? string.Empty,
            string text => text,
            _ => throw new InvalidOperationException("unexpected thinking level metadata type"),
        };
    }

    private static long ReadMetadataLong(Generation generation, string key)
    {
        var raw = generation.Metadata[key];
        return raw switch
        {
            System.Text.Json.JsonElement json
                when json.ValueKind == System.Text.Json.JsonValueKind.Number && json.TryGetInt64(out var parsed) => parsed,
            IConvertible convertible => Convert.ToInt64(convertible),
            _ => throw new InvalidOperationException($"unexpected metadata type for {key}"),
        };
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
                var normalized = text.Trim();
                if (normalized.Length == 0)
                {
                    return null;
                }

                var direct = Enum.GetNames(targetType)
                    .FirstOrDefault(name => string.Equals(name, normalized, StringComparison.OrdinalIgnoreCase));
                if (direct != null)
                {
                    return Enum.Parse(targetType, direct, ignoreCase: true);
                }

                var suffixMatch = Enum.GetNames(targetType)
                    .FirstOrDefault(name => name.EndsWith(normalized, StringComparison.OrdinalIgnoreCase));
                if (suffixMatch != null)
                {
                    return Enum.Parse(targetType, suffixMatch, ignoreCase: true);
                }

                return null;
            }

            try
            {
                return Enum.ToObject(targetType, value);
            }
            catch
            {
                return null;
            }
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

        try
        {
            return Convert.ChangeType(value, targetType);
        }
        catch
        {
            return value;
        }
    }

    private static async IAsyncEnumerable<GenerateContentResponse> StreamResponses()
    {
        yield return new GenerateContentResponse
        {
            ResponseId = "resp_stream_recorder",
            ModelVersion = "gemini-2.5-pro-001",
            Candidates =
            [
                new Candidate
                {
                    FinishReason = FinishReason.Stop,
                    Content = new Content
                    {
                        Role = "model",
                        Parts =
                        [
                            new GPart
                            {
                                Text = "hello",
                            },
                        ],
                    },
                },
            ],
            UsageMetadata = new GenerateContentResponseUsageMetadata
            {
                PromptTokenCount = 1,
                CandidatesTokenCount = 1,
                TotalTokenCount = 2,
                ToolUsePromptTokenCount = 1,
            },
        };

        await Task.CompletedTask;
    }

    private static async IAsyncEnumerable<GenerateContentResponse> EmptyStreamResponses()
    {
        await Task.CompletedTask;
        yield break;
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
