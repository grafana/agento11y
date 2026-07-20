using System.Collections.Concurrent;
using System.Diagnostics.Metrics;
using Xunit;

namespace Grafana.Sigil.Tests;

public sealed class MetricsTelemetryTests
{
    [Fact]
    public async Task StreamingGeneration_RecordsAllSdkMetricInstruments()
    {
        var observations = new ConcurrentBag<(string InstrumentName, double Value, Dictionary<string, object?> Tags)>();

        using var listener = new MeterListener();
        listener.InstrumentPublished += (instrument, meterListener) =>
        {
            if (instrument.Name.StartsWith("gen_ai.client.", StringComparison.Ordinal))
            {
                meterListener.EnableMeasurementEvents(instrument);
            }
        };
        listener.SetMeasurementEventCallback<double>((instrument, measurement, tags, _) =>
        {
            var tagMap = new Dictionary<string, object?>(StringComparer.Ordinal);
            foreach (var tag in tags)
            {
                tagMap[tag.Key] = tag.Value;
            }
            observations.Add((instrument.Name, measurement, tagMap));
        });
        listener.Start();

        var exporter = new CapturingGenerationExporter();
        var config = TestHelpers.TestConfig(exporter);

        await using var client = new SigilClient(config);
        var start = TestHelpers.CreateSeedStart("gen-metrics");
        start.Mode = null;
        start.OperationName = string.Empty;

        var recorder = client.StartStreamingGeneration(start);
        recorder.SetFirstTokenAt(start.StartedAt!.Value.AddMilliseconds(250));

        var result = TestHelpers.CreateSeedResult("gen-metrics");
        result.Mode = GenerationMode.Stream;
        result.OperationName = "streamText";
        result.Usage.CacheWriteInputTokens = 3;
        result.Usage.ReasoningTokens = 7;
        recorder.SetResult(result);
        recorder.End();

        await client.ShutdownAsync(TestContext.Current.CancellationToken);

        var instrumentNames = observations.Select(item => item.InstrumentName).ToHashSet(StringComparer.Ordinal);
        Assert.Contains("gen_ai.client.operation.duration", instrumentNames);
        Assert.Contains("gen_ai.client.token.usage", instrumentNames);
        Assert.Contains("gen_ai.client.time_to_first_token", instrumentNames);
        Assert.Contains("gen_ai.client.tool_calls_per_operation", instrumentNames);

        var tokenTypes = observations
            .Where(item => string.Equals(item.InstrumentName, "gen_ai.client.token.usage", StringComparison.Ordinal))
            .Select(item => item.Tags.TryGetValue("gen_ai.token.type", out var tokenType) ? tokenType as string : null)
            .Where(value => !string.IsNullOrWhiteSpace(value))
            .ToHashSet(StringComparer.Ordinal);

        Assert.Contains("cache_write", tokenTypes);
        Assert.Contains("reasoning", tokenTypes);
        Assert.DoesNotContain("cache_creation", tokenTypes);
    }
}
