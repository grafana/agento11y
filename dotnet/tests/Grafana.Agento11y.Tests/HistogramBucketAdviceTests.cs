using System.Collections.Concurrent;
using OpenTelemetry;
using OpenTelemetry.Metrics;
using Xunit;

namespace Grafana.Agento11y.Tests;

public sealed class HistogramBucketAdviceTests
{
    private static readonly double[] ExpectedDurationBuckets =
    {
        0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28,
        2.56, 5.12, 10.24, 20.48, 40.96, 81.92,
    };

    private static readonly double[] ExpectedTokenUsageBuckets =
    {
        1, 4, 16, 64, 256, 1024, 4096, 16384,
        65536, 262144, 1048576, 4194304, 16777216, 67108864,
    };

    [Fact]
    public async Task Histograms_UseSemconvBucketBoundaries()
    {
        var captured = new ConcurrentDictionary<string, double[]>(StringComparer.Ordinal);

        var exporter = new CapturingMetricExporter(captured);
        using var meterProvider = Sdk.CreateMeterProviderBuilder()
            .AddMeter(Agento11yClient.InstrumentationName)
            .AddReader(new BaseExportingMetricReader(exporter))
            .Build()!;

        var generationExporter = new CapturingGenerationExporter();
        var config = TestHelpers.TestConfig(generationExporter);

        await using (var client = new Agento11yClient(config))
        {
            var start = TestHelpers.CreateSeedStart("gen-buckets");
            start.Mode = null;
            start.OperationName = string.Empty;

            var recorder = client.StartStreamingGeneration(start);
            recorder.SetFirstTokenAt(start.StartedAt!.Value.AddMilliseconds(50));

            var result = TestHelpers.CreateSeedResult("gen-buckets");
            result.Mode = GenerationMode.Stream;
            result.OperationName = "streamText";
            recorder.SetResult(result);
            recorder.End();

            await client.ShutdownAsync(TestContext.Current.CancellationToken);
        }

        meterProvider.ForceFlush();

        Assert.True(
            captured.TryGetValue("gen_ai.client.operation.duration", out var durationBounds),
            "expected gen_ai.client.operation.duration data point");
        Assert.Equal(ExpectedDurationBuckets, durationBounds);

        Assert.True(
            captured.TryGetValue("gen_ai.client.time_to_first_token", out var ttftBounds),
            "expected gen_ai.client.time_to_first_token data point");
        Assert.Equal(ExpectedDurationBuckets, ttftBounds);

        Assert.True(
            captured.TryGetValue("gen_ai.client.token.usage", out var tokenUsageBounds),
            "expected gen_ai.client.token.usage data point");
        Assert.Equal(ExpectedTokenUsageBuckets, tokenUsageBounds);
    }

    [Fact]
    public async Task ToolSkills_SplitSeriesWithoutChangingAggregateDuration()
    {
        var captured = new ConcurrentDictionary<string, double[]>(StringComparer.Ordinal);
        var points = new ConcurrentDictionary<string, HistogramPoint>(StringComparer.Ordinal);
        using var meterProvider = Sdk.CreateMeterProviderBuilder()
            .AddMeter(Agento11yClient.InstrumentationName)
            .AddReader(new BaseExportingMetricReader(new CapturingMetricExporter(captured, points)))
            .Build()!;
        await using var client = new Agento11yClient(TestHelpers.TestConfig(new CapturingGenerationExporter()));
        var startedAt = new DateTimeOffset(2026, 10, 1, 0, 0, 0, TimeSpan.Zero);
        foreach (var (skill, duration) in new[] { ("review", 0.125), ("test", 0.25), ("review", 0.125), ("", 0.5) })
        {
            var recorder = client.StartToolExecution(new ToolExecutionStart
            {
                ToolName = "load_skill",
                ExecutionKind = skill.Length > 0 ? ToolExecutionKind.LoadSkill : ToolExecutionKind.Unset,
                SkillName = skill,
                StartedAt = startedAt,
            });
            recorder.SetResult(new ToolExecutionEnd { CompletedAt = startedAt.AddSeconds(duration) });
            recorder.End();
            recorder.End();
        }
        Assert.True(meterProvider.ForceFlush());

        Assert.Equal("gen_ai.client.operation.duration", Assert.Single(captured).Key);
        Assert.Equal(ExpectedDurationBuckets, captured["gen_ai.client.operation.duration"]);
        Assert.Equal(3, points.Count);
        Assert.Equal(2, points["review"].Count);
        Assert.Equal(0.25, points["review"].Sum);
        Assert.Equal(1, points["test"].Count);
        Assert.Equal(0.25, points["test"].Sum);
        Assert.Equal(1, points[""].Count);
        Assert.Equal(0.5, points[""].Sum);
        Assert.Equal(4, points.Values.Sum(point => point.Count));
        Assert.Equal(1.0, points.Values.Sum(point => point.Sum));
        var aggregateBuckets = new long[ExpectedDurationBuckets.Length + 1];
        foreach (var (skill, point) in points)
        {
            var expectedTags = new Dictionary<string, object?>
            {
                ["gen_ai.operation.name"] = "execute_tool",
                ["gen_ai.provider.name"] = "",
                ["gen_ai.request.model"] = "",
                ["gen_ai.tool.name"] = "load_skill",
                ["gen_ai.agent.name"] = "",
                ["error.type"] = "",
                ["error.category"] = "",
            };
            if (skill.Length > 0)
            {
                expectedTags["gen_ai.skill.name"] = skill;
                expectedTags["agento11y.tool.execution.kind"] = "load_skill";
            }
            Assert.Equal(expectedTags.OrderBy(pair => pair.Key, StringComparer.Ordinal),
                point.Tags.OrderBy(pair => pair.Key, StringComparer.Ordinal));
            for (var index = 0; index < aggregateBuckets.Length; index++)
            {
                aggregateBuckets[index] += point.Buckets[index];
            }
        }
        var expectedBuckets = new long[ExpectedDurationBuckets.Length + 1];
        expectedBuckets[4] = 2;
        expectedBuckets[5] = 1;
        expectedBuckets[6] = 1;
        Assert.Equal(expectedBuckets, aggregateBuckets);
    }

    private sealed record HistogramPoint(long Count, double Sum, long[] Buckets, IReadOnlyDictionary<string, object?> Tags);

    private sealed class CapturingMetricExporter : BaseExporter<Metric>
    {
        private readonly ConcurrentDictionary<string, double[]> _captured;
        private readonly ConcurrentDictionary<string, HistogramPoint>? _points;

        public CapturingMetricExporter(ConcurrentDictionary<string, double[]> captured,
            ConcurrentDictionary<string, HistogramPoint>? points = null)
        {
            _captured = captured;
            _points = points;
        }

        public override ExportResult Export(in Batch<Metric> batch)
        {
            foreach (var metric in batch)
            {
                if (metric.MetricType != MetricType.Histogram)
                {
                    continue;
                }

                foreach (ref readonly var point in metric.GetMetricPoints())
                {
                    var buckets = new List<double>();
                    var counts = new List<long>();
                    foreach (var bucket in point.GetHistogramBuckets())
                    {
                        if (!double.IsPositiveInfinity(bucket.ExplicitBound))
                        {
                            buckets.Add(bucket.ExplicitBound);
                        }
                        counts.Add(bucket.BucketCount);
                    }
                    _captured[metric.Name] = buckets.ToArray();
                    if (_points != null)
                    {
                        var tags = new Dictionary<string, object?>(StringComparer.Ordinal);
                        foreach (var tag in point.Tags)
                        {
                            tags.Add(tag.Key, tag.Value);
                        }
                        var skill = tags.TryGetValue("gen_ai.skill.name", out var value) ? (string)value! : "";
                        _points[skill] = new HistogramPoint(point.GetHistogramCount(), point.GetHistogramSum(), counts.ToArray(), tags);
                    }
                }
            }

            return ExportResult.Success;
        }
    }
}
