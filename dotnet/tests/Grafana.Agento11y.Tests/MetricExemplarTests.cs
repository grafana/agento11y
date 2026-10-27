using System.Diagnostics;
using System.Diagnostics.Metrics;
using OpenTelemetry;
using OpenTelemetry.Metrics;
using Xunit;

namespace Grafana.Agento11y.Tests;

public sealed class MetricExemplarTests
{
    [Fact]
    public async Task GenerationMetrics_RecordedWhileActivityIsCurrent()
    {
        using var harness = new ExemplarHarness();
        await using var client = harness.NewClient();

        var recorder = client.StartGeneration(TestHelpers.CreateSeedStart());
        recorder.SetResult(TestHelpers.CreateSeedResult());
        recorder.End();

        Assert.NotNull(harness.CapturedActivityId);
    }

    [Fact]
    public async Task EmbeddingMetrics_RecordedWhileActivityIsCurrent()
    {
        using var harness = new ExemplarHarness();
        await using var client = harness.NewClient();

        var recorder = client.StartEmbedding(new EmbeddingStart
        {
            Model = new ModelRef { Provider = "openai", Name = "text-embedding-3-small" },
            AgentName = "test-agent",
        });
        recorder.SetResult(new EmbeddingResult { InputTokens = 42, InputCount = 1 });
        recorder.End();

        Assert.NotNull(harness.CapturedActivityId);
    }

    [Fact]
    public async Task ToolExecutionMetrics_RecordedWhileActivityIsCurrent()
    {
        using var harness = new ExemplarHarness();
        await using var client = harness.NewClient();

        var recorder = client.StartToolExecution(new ToolExecutionStart
        {
            ToolName = "weather",
            AgentName = "test-agent",
        });
        recorder.SetResult(new ToolExecutionEnd { Result = "sunny" });
        recorder.End();

        Assert.NotNull(harness.CapturedActivityId);
    }

    [Fact]
    public async Task ToolDuration_ExportsSameToolExemplarAsLegacyByDefault()
    {
        using var activities = new ActivityListener
        {
            ShouldListenTo = source => source.Name == Agento11yClient.InstrumentationName,
            Sample = static (ref ActivityCreationOptions<ActivityContext> _) => ActivitySamplingResult.AllDataAndRecorded,
        };
        ActivitySource.AddActivityListener(activities);
        using var exporter = new ExemplarExporter();
        using var provider = Sdk.CreateMeterProviderBuilder()
            .AddMeter(Agento11yClient.InstrumentationName)
            .SetExemplarFilter(ExemplarFilterType.TraceBased)
            .AddReader(new BaseExportingMetricReader(exporter))
            .Build();
        var config = TestHelpers.TestConfig(new CapturingGenerationExporter());
        await using var client = new Agento11yClient(config);
        var recorder = client.StartToolExecution(new ToolExecutionStart { ToolName = "read" });
        var context = Activity.Current!.Context;
        recorder.End();
        provider.ForceFlush();
        var draft = Assert.Single(exporter.Exemplars["gen_ai.execute_tool.duration"]);
        var legacy = Assert.Single(exporter.Exemplars["gen_ai.client.operation.duration"]);
        Assert.Equal(context.TraceId, draft.TraceId);
        Assert.Equal(context.SpanId, draft.SpanId);
        Assert.Equal(legacy, draft);
    }

    private sealed class ExemplarExporter : BaseExporter<Metric>
    {
        public Dictionary<string, List<(ActivityTraceId TraceId, ActivitySpanId SpanId, double Value)>> Exemplars { get; } = [];

        public override ExportResult Export(in Batch<Metric> batch)
        {
            foreach (var metric in batch)
            {
                var captured = new List<(ActivityTraceId, ActivitySpanId, double)>();
                foreach (ref readonly var point in metric.GetMetricPoints())
                {
                    if (point.TryGetExemplars(out var exemplars))
                    {
                        foreach (ref readonly var exemplar in exemplars)
                        {
                            captured.Add((exemplar.TraceId, exemplar.SpanId, exemplar.DoubleValue));
                        }
                    }
                }
                Exemplars[metric.Name] = captured;
            }
            return ExportResult.Success;
        }
    }

    private sealed class ExemplarHarness : IDisposable
    {
        private readonly MeterListener _meterListener;
        private readonly ActivityListener _activityListener;

        public string? CapturedActivityId { get; private set; }

        public ExemplarHarness()
        {
            _activityListener = new ActivityListener
            {
                ShouldListenTo = source => source.Name == Agento11yClient.InstrumentationName,
                Sample = static (ref ActivityCreationOptions<ActivityContext> _) => ActivitySamplingResult.AllDataAndRecorded,
            };
            ActivitySource.AddActivityListener(_activityListener);

            _meterListener = new MeterListener();
            _meterListener.InstrumentPublished += (instrument, meterListener) =>
            {
                if (instrument.Name == "gen_ai.client.operation.duration")
                {
                    meterListener.EnableMeasurementEvents(instrument);
                }
            };
            _meterListener.SetMeasurementEventCallback<double>((instrument, _, _, _) =>
            {
                CapturedActivityId = Activity.Current?.Id;
            });
            _meterListener.Start();
        }

        public Agento11yClient NewClient()
        {
            var exporter = new CapturingGenerationExporter();
            var config = TestHelpers.TestConfig(exporter);
            return new Agento11yClient(config);
        }

        public void Dispose()
        {
            _meterListener.Dispose();
            _activityListener.Dispose();
        }
    }
}
