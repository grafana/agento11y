using System.Diagnostics;
using System.Diagnostics.Metrics;
using System.Text.Json;
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
        AssertNonToolTags(harness, "generateText");
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
        AssertNonToolTags(harness, "embeddings");
    }

    public static TheoryData<ContentCaptureMode, ActivitySamplingResult, ToolExecutionKind, string?, bool> ToolMetricCases()
    {
        var cases = new TheoryData<ContentCaptureMode, ActivitySamplingResult, ToolExecutionKind, string?, bool>();
        foreach (var capture in Enum.GetValues<ContentCaptureMode>())
        {
            foreach (var sampling in new[] { ActivitySamplingResult.AllDataAndRecorded,
                ActivitySamplingResult.PropagationData, ActivitySamplingResult.None })
            {
                foreach (var kind in new[] { ToolExecutionKind.LoadSkill, ToolExecutionKind.ReadSkillResource,
                    ToolExecutionKind.Command, ToolExecutionKind.Unset, (ToolExecutionKind)999 })
                {
                    foreach (var skill in new string?[] { " code-review ", " \t ", null })
                    {
                        cases.Add(capture, sampling, kind, skill, false);
                        cases.Add(capture, sampling, kind, skill, true);
                    }
                }
            }
        }
        return cases;
    }

    [Theory]
    [MemberData(nameof(ToolMetricCases))]
    public async Task ToolExecutionMetrics_PreserveStructuralIdentity(
        ContentCaptureMode capture, ActivitySamplingResult sampling, ToolExecutionKind kind, string? skill, bool failed)
    {
        using var harness = new ExemplarHarness(sampling);
        await using var client = harness.NewClient();
        var startedAt = new DateTimeOffset(2026, 10, 1, 0, 0, 0, TimeSpan.Zero);
        var seed = new ToolExecutionStart
        {
            ToolName = " weather ",
            AgentName = "test-agent",
            AgentVersion = " 1.2.3 ",
            RequestProvider = " openai ",
            RequestModel = " gpt-5 ",
            ExecutionKind = kind,
            SkillName = skill!,
            SkillDescription = "private description",
            SkillSourceURI = "file:///private/skill.md",
            SkillResourceName = "private/resource.md",
            ProcessExecutableName = "private-executable",
            ProcessExecutablePath = "/private/bin/executable",
            ToolDescription = "private tool description",
            ToolCallId = "private-call",
            ConversationId = "private-conversation",
            ContentCapture = capture,
            StartedAt = startedAt,
        };
        var recorder = client.StartToolExecution(seed);
        var activityId = Activity.Current?.Id;
        seed.SkillName = "mutated";
        seed.ExecutionKind = ToolExecutionKind.Unset;
        var result = new ToolExecutionEnd
        {
            Arguments = new { secret = "private arguments" },
            Result = "private result",
            ProcessExitCode = 7,
            CompletedAt = startedAt.AddMilliseconds(125),
        };
        recorder.SetResult(result);
        result.CompletedAt = startedAt.AddDays(1);
        if (failed)
        {
            recorder.SetExecutionError(new TimeoutException("private failure"));
        }
        recorder.End();
        recorder.End();

        var measurement = Assert.Single(harness.Measurements);
        Assert.Equal("gen_ai.client.operation.duration", measurement.Name);
        Assert.Equal(0.125, measurement.Value);
        var expected = ToolTags("test-agent", failed ? "tool_execution_error" : "", failed ? "timeout" : "");
        expected["gen_ai.provider.name"] = "openai";
        expected["gen_ai.request.model"] = "gpt-5";
        expected["gen_ai.agent.version"] = "1.2.3";
        expected["agento11y.tag.team"] = "sdk";
        var wireKind = kind switch
        {
            ToolExecutionKind.LoadSkill => "load_skill",
            ToolExecutionKind.ReadSkillResource => "read_skill_resource",
            ToolExecutionKind.Command => "command",
            _ => null,
        };
        if (wireKind != null)
        {
            expected["agento11y.tool.execution.kind"] = wireKind;
            if (!string.IsNullOrWhiteSpace(skill))
            {
                expected["gen_ai.skill.name"] = skill.Trim();
            }
        }
        AssertTags(expected, measurement.Tags);
        Assert.Equal(activityId, measurement.ActivityId);
        Assert.Equal(failed, recorder.Error != null);
        if (sampling == ActivitySamplingResult.None)
        {
            Assert.Null(measurement.ActivityId);
            Assert.Empty(harness.Spans);
        }
        else
        {
            Assert.NotNull(measurement.ActivityId);
            var span = Assert.Single(harness.Spans);
            Assert.Equal(failed ? ActivityStatusCode.Error : ActivityStatusCode.Ok, span.Status);
            Assert.Equal(failed ? "tool_execution_error" : null, span.GetTagItem("error.type"));
            Assert.Equal(failed ? "timeout" : null, span.GetTagItem("error.category"));
        }
        Assert.Equal(new[] { "gen_ai.client.operation.duration", "gen_ai.client.time_to_first_token",
            "gen_ai.client.token.usage", "gen_ai.client.tool_calls_per_operation" },
            harness.Instruments.Keys.OrderBy(name => name, StringComparer.Ordinal));
        Assert.Equal("s", harness.Instruments[measurement.Name].Unit);
    }

    [Theory]
    [InlineData(ContentCaptureMode.Full, ActivitySamplingResult.AllDataAndRecorded, false)]
    [InlineData(ContentCaptureMode.MetadataOnly, ActivitySamplingResult.AllDataAndRecorded, true)]
    [InlineData(ContentCaptureMode.Full, ActivitySamplingResult.None, false)]
    public async Task ToolExecutionMetrics_ResultCloneFailureKeepsExistingSemantics(
        ContentCaptureMode capture, ActivitySamplingResult sampling, bool failed)
    {
        using var harness = new ExemplarHarness(sampling);
        await using var client = harness.NewClient();
        var recorder = client.StartToolExecution(new ToolExecutionStart
        {
            ToolName = "weather",
            ExecutionKind = ToolExecutionKind.LoadSkill,
            SkillName = " code-review ",
            ContentCapture = capture,
        });
        var cycle = new Dictionary<string, object?>();
        cycle["self"] = cycle;
        Assert.Throws<JsonException>(() => recorder.SetResult(new ToolExecutionEnd { Result = cycle }));
        Assert.Throws<NotSupportedException>(() => recorder.SetResult(new ToolExecutionEnd { Arguments = typeof(string) }));
        Assert.Empty(harness.Measurements);
        Assert.Null(recorder.Error);
        if (failed)
        {
            recorder.SetExecutionError(new InvalidOperationException("failed execution"));
        }
        recorder.End();
        recorder.End();

        var measurement = Assert.Single(harness.Measurements);
        var expected = ToolTags("", failed ? "tool_execution_error" : "", failed ? "sdk_error" : "");
        expected["gen_ai.skill.name"] = "code-review";
        expected["agento11y.tool.execution.kind"] = "load_skill";
        expected["agento11y.tag.team"] = "sdk";
        AssertTags(expected, measurement.Tags);
        Assert.Equal(failed, recorder.Error != null);
        if (sampling != ActivitySamplingResult.None)
        {
            var span = Assert.Single(harness.Spans);
            Assert.Equal(failed ? ActivityStatusCode.Error : ActivityStatusCode.Ok, span.Status);
            Assert.Equal(failed ? "tool_execution_error" : null, span.GetTagItem("error.type"));
        }
    }

    private static Dictionary<string, object?> ToolTags(string agent, string errorType, string errorCategory) => new()
    {
        ["gen_ai.operation.name"] = "execute_tool",
        ["gen_ai.provider.name"] = "",
        ["gen_ai.request.model"] = "",
        ["gen_ai.tool.name"] = "weather",
        ["gen_ai.agent.name"] = agent,
        ["error.type"] = errorType,
        ["error.category"] = errorCategory,
    };

    private static void AssertNonToolTags(ExemplarHarness harness, string operation)
    {
        var duration = Assert.Single(harness.Measurements, item => item.Name == "gen_ai.client.operation.duration");
        var expected = new Dictionary<string, object?>
        {
            ["gen_ai.operation.name"] = operation,
            ["gen_ai.provider.name"] = "openai",
            ["gen_ai.request.model"] = operation == "embeddings" ? "text-embedding-3-small" : "gpt-5",
            ["gen_ai.agent.name"] = operation == "embeddings" ? "test-agent" : "agent-seed",
            ["error.type"] = "",
            ["error.category"] = "",
            ["agento11y.tag.team"] = "sdk",
        };
        if (operation != "embeddings")
        {
            expected["gen_ai.agent.version"] = "1.2.3";
        }
        AssertTags(expected, duration.Tags);
        Assert.All(harness.Measurements, measurement =>
        {
            Assert.DoesNotContain("gen_ai.skill.name", measurement.Tags.Keys);
            Assert.DoesNotContain("agento11y.tool.execution.kind", measurement.Tags.Keys);
        });
    }

    private static void AssertTags(IReadOnlyDictionary<string, object?> expected, IReadOnlyDictionary<string, object?> actual) =>
        Assert.Equal(expected.OrderBy(pair => pair.Key, StringComparer.Ordinal),
            actual.OrderBy(pair => pair.Key, StringComparer.Ordinal));

    private sealed record Measurement(string Name, double Value, IReadOnlyDictionary<string, object?> Tags, string? ActivityId);

    private sealed class ExemplarHarness : IDisposable
    {
        private readonly MeterListener _meterListener;
        private readonly ActivityListener _activityListener;

        public string? CapturedActivityId { get; private set; }
        public List<Measurement> Measurements { get; } = [];
        public List<Activity> Spans { get; } = [];
        public Dictionary<string, Instrument> Instruments { get; } = new(StringComparer.Ordinal);

        public ExemplarHarness(ActivitySamplingResult sampling = ActivitySamplingResult.AllDataAndRecorded)
        {
            _activityListener = new ActivityListener
            {
                ShouldListenTo = source => source.Name == Agento11yClient.InstrumentationName,
                Sample = (ref ActivityCreationOptions<ActivityContext> _) => sampling,
                ActivityStopped = activity => Spans.Add(activity),
            };
            ActivitySource.AddActivityListener(_activityListener);

            _meterListener = new MeterListener();
            _meterListener.InstrumentPublished += (instrument, meterListener) =>
            {
                if (instrument.Meter.Name == Agento11yClient.InstrumentationName)
                {
                    Instruments[instrument.Name] = instrument;
                    meterListener.EnableMeasurementEvents(instrument);
                }
            };
            _meterListener.SetMeasurementEventCallback<double>((instrument, value, tags, _) =>
            {
                CapturedActivityId = Activity.Current?.Id;
                var tagMap = new Dictionary<string, object?>(StringComparer.Ordinal);
                foreach (var tag in tags)
                {
                    tagMap.Add(tag.Key, tag.Value);
                }
                Measurements.Add(new Measurement(instrument.Name, value, tagMap, CapturedActivityId));
            });
            _meterListener.Start();
        }

        public Agento11yClient NewClient()
        {
            var config = TestHelpers.TestConfig(new CapturingGenerationExporter());
            config.Tags = new Dictionary<string, string> { ["team"] = "sdk" };
            return new Agento11yClient(config);
        }

        public void Dispose()
        {
            _meterListener.Dispose();
            _activityListener.Dispose();
        }
    }
}
