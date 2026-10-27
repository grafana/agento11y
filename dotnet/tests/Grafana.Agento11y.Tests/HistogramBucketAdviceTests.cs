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

            for (var i = 0; i < 3; i++)
            {
                var tool = client.StartToolExecution(new ToolExecutionStart
                {
                    ToolName = "Skill",
                    ExecutionKind = ToolExecutionKind.LoadSkill,
                    StartedAt = start.StartedAt,
                });
                tool.SetResult(new ToolExecutionEnd
                {
                    VerifiedSkillName = "review",
                    CompletedAt = start.StartedAt!.Value.AddSeconds(0.32),
                });
                tool.End();
                tool.End();
            }

            await client.ShutdownAsync(TestContext.Current.CancellationToken);
        }

        meterProvider.ForceFlush();

        Assert.True(
            captured.TryGetValue("gen_ai.client.operation.duration", out var durationBounds),
            "expected gen_ai.client.operation.duration data point");
        Assert.Equal(ExpectedDurationBuckets, durationBounds);

        Assert.True(captured.TryGetValue("gen_ai.execute_tool.duration", out var toolBounds));
        Assert.Equal(ExpectedDurationBuckets, toolBounds);
        Assert.Equal(3, exporter.ToolCount);
        Assert.Equal(0.96, exporter.ToolSum, precision: 8);
        Assert.Equal(ExpectedDurationBuckets.Length + 1, exporter.ToolBucketCounts.Count);
        Assert.All(exporter.ToolBucketCounts, bucket => Assert.Equal(bucket.Key == 0.32 ? 3L : 0L, bucket.Value));

        Assert.True(
            captured.TryGetValue("gen_ai.client.time_to_first_token", out var ttftBounds),
            "expected gen_ai.client.time_to_first_token data point");
        Assert.Equal(ExpectedDurationBuckets, ttftBounds);

        Assert.True(
            captured.TryGetValue("gen_ai.client.token.usage", out var tokenUsageBounds),
            "expected gen_ai.client.token.usage data point");
        Assert.Equal(ExpectedTokenUsageBuckets, tokenUsageBounds);
    }

    private sealed class CapturingMetricExporter : BaseExporter<Metric>
    {
        private readonly ConcurrentDictionary<string, double[]> _captured;
        public long ToolCount { get; private set; }
        public double ToolSum { get; private set; }
        public Dictionary<double, long> ToolBucketCounts { get; } = [];

        public CapturingMetricExporter(ConcurrentDictionary<string, double[]> captured)
        {
            _captured = captured;
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
                    if (metric.Name == "gen_ai.execute_tool.duration")
                    {
                        Assert.Equal("s", metric.Unit);
                        ToolCount = point.GetHistogramCount();
                        ToolSum = point.GetHistogramSum();
                    }
                    var buckets = new List<double>();
                    foreach (var bucket in point.GetHistogramBuckets())
                    {
                        if (metric.Name == "gen_ai.execute_tool.duration")
                        {
                            ToolBucketCounts[bucket.ExplicitBound] = bucket.BucketCount;
                        }
                        if (!double.IsPositiveInfinity(bucket.ExplicitBound))
                        {
                            buckets.Add(bucket.ExplicitBound);
                        }
                    }
                    _captured[metric.Name] = buckets.ToArray();
                    break;
                }
            }

            return ExportResult.Success;
        }
    }
}
