using System.Diagnostics;
using System.Diagnostics.Metrics;
using Xunit;

namespace Grafana.Sigil.Tests;

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

    private sealed class ExemplarHarness : IDisposable
    {
        private readonly MeterListener _meterListener;
        private readonly ActivityListener _activityListener;

        public string? CapturedActivityId { get; private set; }

        public ExemplarHarness()
        {
            _activityListener = new ActivityListener
            {
                ShouldListenTo = source => source.Name == SigilClient.InstrumentationName,
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

        public SigilClient NewClient()
        {
            var exporter = new CapturingGenerationExporter();
            var config = TestHelpers.TestConfig(exporter);
            return new SigilClient(config);
        }

        public void Dispose()
        {
            _meterListener.Dispose();
            _activityListener.Dispose();
        }
    }
}
