using System.Collections.Concurrent;
using System.Diagnostics;
using System.Diagnostics.Metrics;
using Xunit;

namespace Grafana.Sigil.Tests;

public sealed class EmbeddingTelemetryTests
{
    [Fact]
    public async Task EmbeddingSpan_SetsAttributes_AndDoesNotEnqueueGeneration()
    {
        var exporter = new CapturingGenerationExporter();
        var config = TestHelpers.TestConfig(exporter);
        var spans = new List<Activity>();

        using var listener = NewEmbeddingListener(spans);
        await using var client = new SigilClient(config);

        var recorder = client.StartEmbedding(new EmbeddingStart
        {
            AgentName = "agent-embed",
            AgentVersion = "v-embed",
            Model = new ModelRef
            {
                Provider = "openai",
                Name = "text-embedding-3-small",
            },
            Dimensions = 256,
            EncodingFormat = "float",
        });
        recorder.SetResult(new EmbeddingResult
        {
            InputCount = 2,
            InputTokens = 45,
            InputTexts = ["first", "second"],
            ResponseModel = "text-embedding-3-small",
        });
        recorder.End();

        await client.ShutdownAsync(TestContext.Current.CancellationToken);

        Assert.Null(recorder.Error);
        Assert.Empty(exporter.Requests);
        Assert.Single(spans);

        var span = spans[0];
        Assert.Equal("embeddings text-embedding-3-small", span.DisplayName);
        Assert.Equal("embeddings", span.GetTagItem("gen_ai.operation.name"));
        Assert.Equal("openai", span.GetTagItem("gen_ai.provider.name"));
        Assert.Equal("text-embedding-3-small", span.GetTagItem("gen_ai.request.model"));
        Assert.Equal("agent-embed", span.GetTagItem("gen_ai.agent.name"));
        Assert.Equal("v-embed", span.GetTagItem("gen_ai.agent.version"));
        Assert.Equal(256L, span.GetTagItem("gen_ai.embeddings.dimension.count"));
        Assert.Equal(2, span.GetTagItem("gen_ai.embeddings.input_count"));
        Assert.Equal(45L, span.GetTagItem("gen_ai.usage.input_tokens"));
        Assert.Equal("text-embedding-3-small", span.GetTagItem("gen_ai.response.model"));
        Assert.Equal(ActivityStatusCode.Ok, span.Status);
        Assert.Contains("float", ReadTagStringValues(span.GetTagItem("gen_ai.request.encoding_formats")));
        Assert.Null(span.GetTagItem("gen_ai.embeddings.input_texts"));
    }

    [Fact]
    public async Task EmbeddingSpan_CapturesInputTexts_WhenEnabledAndTruncates()
    {
        var config = TestHelpers.TestConfig(new CapturingGenerationExporter());
        config.EmbeddingCapture = new EmbeddingCaptureConfig
        {
            CaptureInput = true,
            MaxInputItems = 2,
            MaxTextLength = 8,
        };

        var spans = new List<Activity>();
        using var listener = NewEmbeddingListener(spans);
        await using var client = new SigilClient(config);

        var recorder = client.StartEmbedding(new EmbeddingStart
        {
            Model = new ModelRef
            {
                Provider = "openai",
                Name = "text-embedding-3-small",
            },
        });
        recorder.SetResult(new EmbeddingResult
        {
            InputCount = 3,
            InputTokens = 12,
            InputTexts =
            [
                "12345678",
                "123456789",
                "ignored",
            ],
        });
        recorder.End();

        await client.ShutdownAsync(TestContext.Current.CancellationToken);

        Assert.Single(spans);
        var captured = ReadTagStringValues(spans[0].GetTagItem("gen_ai.embeddings.input_texts"));
        Assert.Equal(["12345678", "12345..."], captured);
    }

    private static readonly string[] expected = ["😀😀..."];

    [Fact]
    public async Task EmbeddingSpan_TruncationPreservesSurrogatePairs()
    {
        var config = TestHelpers.TestConfig(new CapturingGenerationExporter());
        config.EmbeddingCapture = new EmbeddingCaptureConfig
        {
            CaptureInput = true,
            MaxInputItems = 1,
            MaxTextLength = 5, // 6 code points → truncate to 2 code points + "..." = 5 code points
        };

        var spans = new List<Activity>();
        using var listener = NewEmbeddingListener(spans);
        await using var client = new SigilClient(config);

        var recorder = client.StartEmbedding(new EmbeddingStart
        {
            Model = new ModelRef
            {
                Provider = "openai",
                Name = "text-embedding-3-small",
            },
        });
        recorder.SetResult(new EmbeddingResult
        {
            InputCount = 1,
            InputTexts = ["😀😀😀😀😀😀"], // 6 emoji = 6 code points
        });
        recorder.End();

        await client.ShutdownAsync(TestContext.Current.CancellationToken);

        Assert.Single(spans);
        var captured = ReadTagStringValues(spans[0].GetTagItem("gen_ai.embeddings.input_texts"));
        Assert.Equal(expected, captured); // First 2 code points + "..."
    }

    [Fact]
    public async Task EmbeddingRecorder_SetCallError_MarksProviderErrorWithoutLocalError()
    {
        var config = TestHelpers.TestConfig(new CapturingGenerationExporter());
        var spans = new List<Activity>();
        using var listener = NewEmbeddingListener(spans);
        await using var client = new SigilClient(config);

        var recorder = client.StartEmbedding(new EmbeddingStart
        {
            Model = new ModelRef
            {
                Provider = "openai",
                Name = "text-embedding-3-small",
            },
        });
        recorder.SetCallError(new InvalidOperationException("provider failed with status 429"));
        recorder.End();

        await client.ShutdownAsync(TestContext.Current.CancellationToken);

        Assert.Null(recorder.Error);
        Assert.Single(spans);
        var span = spans[0];
        Assert.Equal(ActivityStatusCode.Error, span.Status);
        Assert.Equal("provider_call_error", span.GetTagItem("error.type"));
        Assert.Equal("rate_limit", span.GetTagItem("error.category"));
    }

    [Fact]
    public async Task EmbeddingRecorder_InvalidResultSetsLocalValidationError()
    {
        var config = TestHelpers.TestConfig(new CapturingGenerationExporter());
        var spans = new List<Activity>();
        using var listener = NewEmbeddingListener(spans);
        await using var client = new SigilClient(config);

        var recorder = client.StartEmbedding(new EmbeddingStart
        {
            Model = new ModelRef
            {
                Provider = "openai",
                Name = "text-embedding-3-small",
            },
        });
        recorder.SetResult(new EmbeddingResult
        {
            InputTokens = -1,
        });
        recorder.End();

        await client.ShutdownAsync(TestContext.Current.CancellationToken);

        Assert.NotNull(recorder.Error);
        Assert.IsType<ValidationException>(recorder.Error);
        Assert.Contains("embedding validation failed", recorder.Error!.Message, StringComparison.Ordinal);
        Assert.Single(spans);
        var span = spans[0];
        Assert.Equal("validation_error", span.GetTagItem("error.type"));
        Assert.Equal("sdk_error", span.GetTagItem("error.category"));
        Assert.Equal(ActivityStatusCode.Error, span.Status);
    }

    [Fact]
    public async Task EmbeddingRecorder_UsesContextAgentDefaults()
    {
        var config = TestHelpers.TestConfig(new CapturingGenerationExporter());
        var spans = new List<Activity>();
        using var listener = NewEmbeddingListener(spans);
        await using var client = new SigilClient(config);

        using var agentScope = SigilContext.WithAgentName("agent-context");
        using var versionScope = SigilContext.WithAgentVersion("v-context");

        var recorder = client.StartEmbedding(new EmbeddingStart
        {
            Model = new ModelRef
            {
                Provider = "openai",
                Name = "text-embedding-3-small",
            },
        });
        recorder.End();

        await client.ShutdownAsync(TestContext.Current.CancellationToken);

        Assert.Single(spans);
        var span = spans[0];
        Assert.Equal("agent-context", span.GetTagItem("gen_ai.agent.name"));
        Assert.Equal("v-context", span.GetTagItem("gen_ai.agent.version"));
    }

    [Fact]
    public async Task EmbeddingRecorder_RecordsDurationAndInputTokenMetrics_Once()
    {
        var observations = new ConcurrentBag<(string InstrumentName, double Value, Dictionary<string, object?> Tags)>();

        using var meterListener = new MeterListener();
        meterListener.InstrumentPublished += (instrument, listener) =>
        {
            if (instrument.Name.StartsWith("gen_ai.client.", StringComparison.Ordinal))
            {
                listener.EnableMeasurementEvents(instrument);
            }
        };
        meterListener.SetMeasurementEventCallback<double>((instrument, measurement, tags, _) =>
        {
            var mapped = new Dictionary<string, object?>(StringComparer.Ordinal);
            foreach (var tag in tags)
            {
                mapped[tag.Key] = tag.Value;
            }

            observations.Add((instrument.Name, measurement, mapped));
        });
        meterListener.Start();

        var config = TestHelpers.TestConfig(new CapturingGenerationExporter());
        await using var client = new SigilClient(config);

        var recorder = client.StartEmbedding(new EmbeddingStart
        {
            StartedAt = DateTimeOffset.UtcNow.AddMilliseconds(-25),
            AgentName = "agent-metrics",
            Model = new ModelRef
            {
                Provider = "openai",
                Name = "text-embedding-3-small",
            },
        });
        recorder.SetResult(new EmbeddingResult
        {
            InputCount = 1,
            InputTokens = 21,
        });
        recorder.End();
        recorder.End();

        await client.ShutdownAsync(TestContext.Current.CancellationToken);

        var durationRecords = observations
            .Where(item => string.Equals(item.InstrumentName, "gen_ai.client.operation.duration", StringComparison.Ordinal))
            .ToList();
        Assert.Single(durationRecords);
        Assert.Equal("embeddings", durationRecords[0].Tags["gen_ai.operation.name"]);

        var tokenRecords = observations
            .Where(item => string.Equals(item.InstrumentName, "gen_ai.client.token.usage", StringComparison.Ordinal))
            .ToList();
        Assert.Single(tokenRecords);
        Assert.Equal("input", tokenRecords[0].Tags["gen_ai.token.type"]);
        Assert.Equal("embeddings", tokenRecords[0].Tags["gen_ai.operation.name"]);

        var instrumentNames = observations.Select(item => item.InstrumentName).ToHashSet(StringComparer.Ordinal);
        Assert.DoesNotContain("gen_ai.client.time_to_first_token", instrumentNames);
        Assert.DoesNotContain("gen_ai.client.tool_calls_per_operation", instrumentNames);
    }

    private static ActivityListener NewEmbeddingListener(List<Activity> spans)
    {
        var listener = new ActivityListener
        {
            ShouldListenTo = source => source.Name == "github.com/grafana/sigil/sdks/dotnet",
            Sample = static (ref _) => ActivitySamplingResult.AllDataAndRecorded,
            ActivityStopped = activity =>
            {
                if (activity.GetTagItem("gen_ai.operation.name")?.ToString() == "embeddings")
                {
                    spans.Add(activity);
                }
            },
        };
        ActivitySource.AddActivityListener(listener);
        return listener;
    }

    private static string[] ReadTagStringValues(object? value)
    {
        return value switch
        {
            string text => [text],
            string[] values => values,
            IEnumerable<string> values => [.. values],
            _ => [],
        };
    }
}
