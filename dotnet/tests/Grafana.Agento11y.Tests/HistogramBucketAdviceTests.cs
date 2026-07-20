using System.Collections.Concurrent;
using OpenTelemetry;
using OpenTelemetry.Metrics;
using Xunit;

namespace Grafana.Sigil.Tests;

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
            .AddMeter(SigilClient.InstrumentationName)
            .AddReader(new BaseExportingMetricReader(exporter))
            .Build()!;

        var generationExporter = new CapturingGenerationExporter();
        var config = TestHelpers.TestConfig(generationExporter);

        await using (var client = new SigilClient(config))
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

    private sealed class CapturingMetricExporter : BaseExporter<Metric>
    {
        private readonly ConcurrentDictionary<string, double[]> _captured;

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
                    var buckets = new List<double>();
                    foreach (var bucket in point.GetHistogramBuckets())
                    {
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
