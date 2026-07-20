using System.Collections.Concurrent;
using System.Diagnostics;
using System.Diagnostics.Metrics;
using Xunit;

namespace Grafana.Agento11y.Tests;

public sealed class ClientTagsTests
{
    private const string ClientTagProjectKey = "agento11y.tag.project";

    private sealed class TagsHarness : IAsyncDisposable
    {
        private readonly ActivityListener _activityListener;
        private readonly MeterListener _meterListener;

        public CapturingGenerationExporter Exporter { get; } = new();
        public Agento11yClient Client { get; }
        public ConcurrentQueue<Activity> Spans { get; } = new();
        public ConcurrentQueue<(string Name, Dictionary<string, object?> Tags)> Measurements { get; } = new();

        public TagsHarness(Dictionary<string, string>? tags = null)
        {
            _activityListener = new ActivityListener
            {
                ShouldListenTo = source => source.Name == "github.com/grafana/sigil/sdks/dotnet",
                Sample = static (ref _) => ActivitySamplingResult.AllDataAndRecorded,
                ActivityStopped = activity => Spans.Enqueue(activity),
            };
            ActivitySource.AddActivityListener(_activityListener);

            _meterListener = new MeterListener();
            _meterListener.InstrumentPublished += (instrument, listener) =>
            {
                if (instrument.Name.StartsWith("gen_ai.client.", StringComparison.Ordinal))
                {
                    listener.EnableMeasurementEvents(instrument);
                }
            };
            _meterListener.SetMeasurementEventCallback<double>((instrument, _, tags, _) =>
            {
                var tagMap = new Dictionary<string, object?>(StringComparer.Ordinal);
                foreach (var tag in tags)
                {
                    tagMap[tag.Key] = tag.Value;
                }
                Measurements.Enqueue((instrument.Name, tagMap));
            });
            _meterListener.Start();

            var config = TestHelpers.TestConfig(Exporter);
            if (tags != null)
            {
                config.Tags = tags;
            }
            Client = new Agento11yClient(config);
        }

        public void AssertMeasurementsCarryTag(string metricName, string key, string value)
        {
            var measurements = Measurements.Where(m => string.Equals(m.Name, metricName, StringComparison.Ordinal)).ToList();
            Assert.NotEmpty(measurements);
            foreach (var measurement in measurements)
            {
                Assert.True(
                    measurement.Tags.TryGetValue(key, out var got) && string.Equals(got?.ToString(), value, StringComparison.Ordinal),
                    $"expected {key}={value} on every {metricName} measurement"
                );
            }
        }

        public async ValueTask DisposeAsync()
        {
            await Client.ShutdownAsync();
            _meterListener.Dispose();
            _activityListener.Dispose();
        }
    }

    private static Activity SingleSpan(TagsHarness harness, string? operationName = null)
    {
        var spans = harness.Spans.Where(span =>
        {
            var operation = span.GetTagItem("gen_ai.operation.name")?.ToString();
            return operationName == null
                ? operation != "execute_tool" && operation != "embeddings"
                : operation == operationName;
        }).ToList();
        Assert.Single(spans);
        return spans[0];
    }

    [Fact]
    public async Task ClientTags_OnGenerationSpanAndMetrics()
    {
        await using var harness = new TagsHarness(new Dictionary<string, string>(StringComparer.Ordinal)
        {
            ["project"] = "checkout-svc",
        });

        var start = TestHelpers.CreateSeedStart("gen-client-tags");
        start.Mode = null;
        start.OperationName = string.Empty;
        var recorder = harness.Client.StartStreamingGeneration(start);
        recorder.SetFirstTokenAt(start.StartedAt!.Value.AddMilliseconds(250));

        var result = TestHelpers.CreateSeedResult("gen-client-tags");
        result.Mode = GenerationMode.Stream;
        result.OperationName = "streamText";
        recorder.SetResult(result);
        recorder.End();

        var span = SingleSpan(harness);
        Assert.Equal("checkout-svc", span.GetTagItem(ClientTagProjectKey)?.ToString());

        harness.AssertMeasurementsCarryTag("gen_ai.client.operation.duration", ClientTagProjectKey, "checkout-svc");
        harness.AssertMeasurementsCarryTag("gen_ai.client.token.usage", ClientTagProjectKey, "checkout-svc");
        harness.AssertMeasurementsCarryTag("gen_ai.client.time_to_first_token", ClientTagProjectKey, "checkout-svc");
        harness.AssertMeasurementsCarryTag("gen_ai.client.tool_calls_per_operation", ClientTagProjectKey, "checkout-svc");
    }

    [Fact]
    public async Task ClientTags_OnEmbeddingAndToolSpansAndMetrics()
    {
        await using var harness = new TagsHarness(new Dictionary<string, string>(StringComparer.Ordinal)
        {
            ["project"] = "embed-tools",
        });

        var embedding = harness.Client.StartEmbedding(new EmbeddingStart
        {
            Model = new ModelRef { Provider = "openai", Name = "text-embedding-3-small" },
        });
        embedding.SetResult(new EmbeddingResult { InputTokens = 1 });
        embedding.End();

        var tool = harness.Client.StartToolExecution(new ToolExecutionStart
        {
            ToolName = "weather",
        });
        tool.SetResult(new ToolExecutionEnd { Result = "sunny" });
        tool.End();

        var embeddingSpan = SingleSpan(harness, "embeddings");
        Assert.Equal("embed-tools", embeddingSpan.GetTagItem(ClientTagProjectKey)?.ToString());

        var toolSpan = SingleSpan(harness, "execute_tool");
        Assert.Equal("embed-tools", toolSpan.GetTagItem(ClientTagProjectKey)?.ToString());

        // Embedding duration + tool duration share the operation.duration
        // instrument; embedding token usage is the token.usage instrument.
        harness.AssertMeasurementsCarryTag("gen_ai.client.operation.duration", ClientTagProjectKey, "embed-tools");
        harness.AssertMeasurementsCarryTag("gen_ai.client.token.usage", ClientTagProjectKey, "embed-tools");
    }

    [Fact]
    public async Task ClientTags_AreNormalizedAndSorted()
    {
        var attributes = Agento11yClient.TagAttributes(new Dictionary<string, string>(StringComparer.Ordinal)
        {
            [" z "] = " last ",
            ["   "] = "discard",
            [" a "] = "",
        });

        Assert.Equal(2, attributes.Length);
        Assert.Equal("agento11y.tag.a", attributes[0].Key);
        Assert.Equal("", attributes[0].Value);
        Assert.Equal("agento11y.tag.z", attributes[1].Key);
        Assert.Equal("last", attributes[1].Value);

        await using var harness = new TagsHarness(new Dictionary<string, string>(StringComparer.Ordinal)
        {
            [" z "] = " last ",
            ["   "] = "discard",
            [" a "] = "",
        });

        var recorder = harness.Client.StartGeneration(TestHelpers.CreateSeedStart("gen-normalized"));
        recorder.SetResult(TestHelpers.CreateSeedResult("gen-normalized"));
        recorder.End();

        var span = SingleSpan(harness);
        Assert.Equal("", span.GetTagItem("agento11y.tag.a")?.ToString());
        Assert.Equal("last", span.GetTagItem("agento11y.tag.z")?.ToString());
        foreach (var tag in span.TagObjects)
        {
            Assert.Equal(tag.Key, tag.Key.Trim());
        }
    }

    [Fact]
    public async Task EmptyClientTags_AreNoOp()
    {
        await using var harness = new TagsHarness();

        var recorder = harness.Client.StartGeneration(TestHelpers.CreateSeedStart("gen-no-tags"));
        recorder.SetResult(TestHelpers.CreateSeedResult("gen-no-tags"));
        recorder.End();

        var embedding = harness.Client.StartEmbedding(new EmbeddingStart
        {
            Model = new ModelRef { Provider = "openai", Name = "text-embedding-3-small" },
        });
        embedding.SetResult(new EmbeddingResult { InputTokens = 1 });
        embedding.End();

        var tool = harness.Client.StartToolExecution(new ToolExecutionStart
        {
            ToolName = "weather",
        });
        tool.SetResult(new ToolExecutionEnd { Result = "sunny" });
        tool.End();

        foreach (var span in harness.Spans)
        {
            foreach (var tag in span.TagObjects)
            {
                Assert.False(
                    tag.Key.StartsWith(Agento11yClient.SpanAttrTagPrefix, StringComparison.Ordinal),
                    $"unexpected {tag.Key} on span {span.DisplayName}"
                );
            }
        }

        foreach (var measurement in harness.Measurements)
        {
            foreach (var tag in measurement.Tags)
            {
                Assert.False(
                    tag.Key.StartsWith(Agento11yClient.SpanAttrTagPrefix, StringComparison.Ordinal),
                    $"unexpected {tag.Key} on metric {measurement.Name}"
                );
            }
        }
    }

    [Fact]
    public async Task PerCallGenerationTags_StayExportOnly()
    {
        await using var harness = new TagsHarness();

        var start = TestHelpers.CreateSeedStart("gen-per-call");
        start.Tags.Clear();
        start.Tags["call_only"] = "yes";
        var recorder = harness.Client.StartGeneration(start);
        var result = TestHelpers.CreateSeedResult("gen-per-call");
        result.Tags.Clear();
        recorder.SetResult(result);
        recorder.End();
        await harness.Client.FlushAsync(TestContext.Current.CancellationToken);

        var span = SingleSpan(harness);
        Assert.Null(span.GetTagItem("agento11y.tag.call_only"));

        foreach (var measurement in harness.Measurements)
        {
            Assert.False(
                measurement.Tags.ContainsKey("agento11y.tag.call_only"),
                $"per-call tag must not appear on metric {measurement.Name}"
            );
        }

        Assert.Single(harness.Exporter.Requests);
        var generation = harness.Exporter.Requests[0].Generations[0];
        Assert.Equal("yes", generation.Tags["call_only"]);
    }
}
