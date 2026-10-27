using System.Diagnostics;
using System.Diagnostics.Metrics;
using System.Reflection;
using System.Text.Json;
using Xunit;

namespace Grafana.Agento11y.Tests;

public sealed class ToolMetricsTests
{
    private const string Draft = "gen_ai.execute_tool.duration";
    private const string Legacy = "gen_ai.client.operation.duration";
    private const string Kind = "agento11y.tool.execution.kind";
    private static readonly DateTimeOffset Started = new(2026, 2, 13, 10, 0, 0, TimeSpan.Zero);

    private static JsonDocument LoadCases()
    {
        var root = typeof(ToolMetricsTests).Assembly.GetCustomAttributes<AssemblyMetadataAttribute>()
            .First(attribute => attribute.Key == "SolutionRoot").Value!;
        return JsonDocument.Parse(File.ReadAllText(Path.Combine(root, "..", "conformance", "tool-metrics", "cases.json")));
    }

    public static TheoryData<string, string> SharedCases()
    {
        using var fixture = LoadCases();
        Assert.Equal(1, fixture.RootElement.GetProperty("version").GetInt32());
        Assert.Equal(Draft, fixture.RootElement.GetProperty("instrument").GetString());
        Assert.Equal("s", fixture.RootElement.GetProperty("unit").GetString());
        var cases = new TheoryData<string, string>();
        foreach (var item in fixture.RootElement.GetProperty("cases").EnumerateArray())
        {
            cases.Add(item.GetProperty("id").GetString()!, item.GetRawText());
        }
        return cases;
    }

    [Theory]
    [MemberData(nameof(SharedCases))]
    public async Task SharedMatrix(string id, string caseJson)
    {
        Assert.NotEmpty(id);
        using var document = JsonDocument.Parse(caseJson);
        var item = document.RootElement;
        var start = item.GetProperty("start");
        item.TryGetProperty("end", out var end);
        using var harness = new Harness();
        await using var client = new Agento11yClient(Config());
        var executions = Integer(item, "executions", 1);
        for (var i = 0; i < executions; i++)
        {
            var recorder = client.StartToolExecution(new ToolExecutionStart
            {
                ToolName = Text(start, "tool_name"),
                ExecutionKind = Text(start, "execution_kind") switch
                {
                    "" => ToolExecutionKind.Unset,
                    "load_skill" => ToolExecutionKind.LoadSkill,
                    "read_skill_resource" => ToolExecutionKind.ReadSkillResource,
                    "command" => ToolExecutionKind.Command,
                    _ => (ToolExecutionKind)999,
                },
                SkillName = Text(start, "skill_name"),
                SkillResourceName = Text(start, "skill_resource_name"),
                StartedAt = Started,
            });
            recorder.SetResult(new ToolExecutionEnd
            {
                VerifiedSkillName = Text(end, "verified_skill_name"),
                VerifiedSkillResourceName = Text(end, "verified_skill_resource_name"),
                ProcessExitCode = end.ValueKind == JsonValueKind.Object && end.TryGetProperty("process_exit_code", out var exit)
                    ? exit.GetInt64() : null,
                ProcessExitCodeIsError = end.ValueKind == JsonValueKind.Object && end.TryGetProperty("process_exit_code_is_error", out var isError)
                    ? isError.GetBoolean() : null,
                CompletedAt = Started.AddSeconds(0.32),
            });
            var error = Text(item, "error");
            if (error.Length > 0)
            {
                recorder.SetExecutionError(new InvalidOperationException(error));
            }
            for (var j = 0; j < Integer(item, "end_calls", 1); j++)
            {
                recorder.End();
            }
        }

        var draft = harness.Measurements.Where(m => m.Name == Draft).ToArray();
        Assert.Equal(Integer(item, "count", 0), draft.Length);
        var expected = item.GetProperty("attributes").EnumerateObject().ToDictionary(
            p => p.Name, p => p.Value.ValueKind == JsonValueKind.Number ? (object?)p.Value.GetInt64() : p.Value.GetString());
        // The shared fixture's tool_error denotes an explicit execution error.
        if (expected.GetValueOrDefault("error.type") as string == "tool_error")
        {
            expected["error.type"] = "tool_execution_error";
        }
        Assert.All(draft, measurement =>
        {
            Assert.Equal(0.32, measurement.Value);
            Assert.Equal("s", measurement.Unit);
            Assert.Equal(expected.OrderBy(p => p.Key), measurement.Tags.OrderBy(p => p.Key));
            if (measurement.Tags.TryGetValue("error.type", out var errorType))
            {
                Assert.IsType<string>(errorType);
            }
            if (measurement.Tags.TryGetValue("process.exit.code", out var code))
            {
                Assert.IsType<long>(code);
            }
        });
        var legacy = harness.Measurements.Where(m => m.Name == Legacy).ToArray();
        Assert.Equal(executions, legacy.Length);
        Assert.All(legacy, measurement =>
        {
            Assert.Equal(0.32, measurement.Value);
            Assert.Equal(Text(item, "error").Length > 0 ? "tool_execution_error" : "", measurement.Tags["error.type"]);
            Assert.False(measurement.Tags.ContainsKey(Kind));
            Assert.False(measurement.Tags.ContainsKey("gen_ai.skill.name"));
            Assert.False(measurement.Tags.ContainsKey("process.exit.code"));
        });
    }

    [Fact]
    public async Task ToolMetricsAreEmittedByDefault()
    {
        using var harness = new Harness();
        await using var client = new Agento11yClient(Config());
        var recorder = client.StartToolExecution(new ToolExecutionStart { ToolName = "read" });
        recorder.End();
        Assert.Single(harness.Measurements, m => m.Name == Draft);
        Assert.Single(harness.Measurements, m => m.Name == Legacy);
    }

    public static TheoryData<ContentCaptureMode, int, bool, bool> SerializationCases()
    {
        var cases = new TheoryData<ContentCaptureMode, int, bool, bool>();
        foreach (var mode in new[] { ContentCaptureMode.Full, ContentCaptureMode.NoToolContent,
                     ContentCaptureMode.MetadataOnly, ContentCaptureMode.FullWithMetadataSpans })
        {
            // -1: no listener, 0: unsampled, 1: sampled.
            foreach (var sampling in new[] { -1, 0, 1 })
            {
                foreach (var executionError in new[] { false, true })
                {
                    foreach (var badArguments in new[] { false, true })
                    {
                        cases.Add(mode, sampling, executionError, badArguments);
                    }
                }
            }
        }
        return cases;
    }

    [Theory]
    [MemberData(nameof(SerializationCases))]
    public async Task SerializationPreservesCallerBehaviorAndMetricIsolation(ContentCaptureMode mode, int sampling, bool executionError, bool badArguments)
    {
        using var harness = new Harness(sampling);
        await using var client = new Agento11yClient(Config());
        var recorder = client.StartToolExecution(new ToolExecutionStart
        {
            ToolName = "Skill",
            SkillName = "attempted",
            ExecutionKind = ToolExecutionKind.LoadSkill,
            ContentCapture = mode,
        });
        if (executionError)
        {
            recorder.SetExecutionError(new InvalidOperationException("execution failed"));
        }
        var callerError = Record.Exception(() =>
        {
            try
            {
                recorder.SetResult(new ToolExecutionEnd
                {
                    VerifiedSkillName = "verified",
                    Arguments = badArguments ? new Unserializable() : "safe",
                    Result = badArguments ? "safe" : new Unserializable(),
                });
            }
            catch (Exception ex)
            {
                recorder.SetExecutionError(ex);
                throw;
            }
            finally
            {
                recorder.End();
                recorder.End();
            }
        });
        Assert.IsType<InvalidOperationException>(callerError);
        Assert.Equal("cannot serialize user content", callerError.Message);
        Assert.Same(callerError, recorder.Error);
        var draft = Assert.Single(harness.Measurements, m => m.Name == Draft);
        Assert.Equal("verified", draft.Tags["gen_ai.skill.name"]);
        Assert.Equal(executionError ? "tool_execution_error" : null, draft.Tags.GetValueOrDefault("error.type"));
        var legacy = Assert.Single(harness.Measurements, m => m.Name == Legacy);
        Assert.Equal("tool_execution_error", legacy.Tags["error.type"]);
        if (sampling == 1)
        {
            var span = Assert.Single(harness.Spans);
            Assert.Equal(ActivityStatusCode.Error, span.Status);
            Assert.Equal("attempted", span.GetTagItem("gen_ai.skill.name"));
            Assert.Equal("execute_tool Skill attempted", span.DisplayName);
        }
        else
        {
            Assert.Empty(harness.Spans);
        }
    }

    [Theory]
    [InlineData(null)]
    [InlineData(false)]
    [InlineData(true)]
    public async Task CopiesAndCallerMutationsAreIsolated(bool? exitIsError)
    {
        using var harness = new Harness(1);
        await using var client = new Agento11yClient(Config());
        var start = new ToolExecutionStart
        {
            ToolName = "exec",
            ExecutionKind = ToolExecutionKind.Command,
            SkillName = "attempted",
            ContentCapture = ContentCaptureMode.Full,
            StartedAt = Started,
        };
        var recorder = client.StartToolExecution(start);
        start.ExecutionKind = ToolExecutionKind.Unset;
        start.ToolName = "changed";
        var original = new ToolExecutionEnd
        {
            VerifiedSkillName = "verified",
            VerifiedSkillResourceName = "script.py",
            ProcessExitCode = -2,
            ProcessExitCodeIsError = exitIsError,
            CompletedAt = Started.AddSeconds(2),
            Result = new Dictionary<string, string> { ["value"] = "original" },
        };
        var copy = InternalUtils.DeepClone(InternalUtils.DeepClone(original));
        Assert.Equal(exitIsError, copy.ProcessExitCodeIsError);
        Assert.Equal("verified", copy.VerifiedSkillName);
        Assert.Equal("script.py", copy.VerifiedSkillResourceName);
        recorder.SetResult(copy);
        copy.VerifiedSkillName = "mutated";
        copy.VerifiedSkillResourceName = "mutated";
        copy.ProcessExitCode = 0;
        copy.ProcessExitCodeIsError = true;
        copy.CompletedAt = Started.AddDays(1);
        copy.Result = "mutated";
        ((Dictionary<string, string>)original.Result)["value"] = "mutated";
        recorder.End();
        var draft = Assert.Single(harness.Measurements, m => m.Name == Draft);
        Assert.Equal(2, draft.Value);
        Assert.Equal("exec", draft.Tags["gen_ai.tool.name"]);
        Assert.Equal("command", draft.Tags[Kind]);
        Assert.Equal("verified", draft.Tags["gen_ai.skill.name"]);
        Assert.Equal("script.py", draft.Tags["gen_ai.skill.resource.name"]);
        Assert.Equal(-2L, draft.Tags["process.exit.code"]);
        Assert.Equal(exitIsError == false ? null : "-2", draft.Tags.GetValueOrDefault("error.type"));
        var span = Assert.Single(harness.Spans);
        Assert.Contains("original", span.GetTagItem("gen_ai.tool.call.result")?.ToString());
        Assert.Equal(ActivityStatusCode.Ok, span.Status);
    }

    [Theory]
    [InlineData(false, null)]
    [InlineData(true, "7")]
    public async Task FailedContentSnapshotStillIsolatesCommandCompletion(bool exitIsError, string? errorType)
    {
        using var harness = new Harness(1);
        await using var client = new Agento11yClient(Config());
        var recorder = client.StartToolExecution(new ToolExecutionStart
        {
            ToolName = "exec",
            ExecutionKind = ToolExecutionKind.Command,
            ContentCapture = ContentCaptureMode.Full,
            StartedAt = Started,
        });
        var result = new ToolExecutionEnd
        {
            VerifiedSkillName = "review",
            VerifiedSkillResourceName = "check.py",
            ProcessExitCode = 7,
            ProcessExitCodeIsError = exitIsError,
            CompletedAt = Started.AddSeconds(1),
            Result = new Unserializable(),
        };
        Assert.Throws<InvalidOperationException>(() => recorder.SetResult(result));
        result.VerifiedSkillName = "mutated";
        result.VerifiedSkillResourceName = "mutated";
        result.ProcessExitCode = 0;
        result.ProcessExitCodeIsError = !exitIsError;
        result.CompletedAt = Started.AddDays(1);
        result.Result = "mutated";
        recorder.End();
        var draft = Assert.Single(harness.Measurements, m => m.Name == Draft);
        Assert.Equal(1, draft.Value);
        Assert.Equal("review", draft.Tags["gen_ai.skill.name"]);
        Assert.Equal("check.py", draft.Tags["gen_ai.skill.resource.name"]);
        Assert.Equal(7L, draft.Tags["process.exit.code"]);
        Assert.Equal(errorType, draft.Tags.GetValueOrDefault("error.type"));
        Assert.NotNull(recorder.Error);
        Assert.Equal("tool_execution_error", Assert.Single(harness.Measurements, m => m.Name == Legacy).Tags["error.type"]);
        Assert.Equal(ActivityStatusCode.Error, Assert.Single(harness.Spans).Status);
    }

    [Fact]
    public async Task SetResultSnapshotsMutableContentDirectly()
    {
        using var harness = new Harness(1);
        await using var client = new Agento11yClient(Config());
        var recorder = client.StartToolExecution(new ToolExecutionStart { ToolName = "read", ContentCapture = ContentCaptureMode.Full });
        var content = new Dictionary<string, string> { ["value"] = "before" };
        recorder.SetResult(new ToolExecutionEnd { Arguments = content, Result = content });
        content["value"] = "after";
        recorder.End();
        var span = Assert.Single(harness.Spans);
        Assert.Contains("before", span.GetTagItem("gen_ai.tool.call.arguments")?.ToString());
        Assert.Contains("before", span.GetTagItem("gen_ai.tool.call.result")?.ToString());
    }

    [Theory]
    [InlineData(false, false)]
    [InlineData(true, false)]
    [InlineData(true, true)]
    public async Task ReplacementClearsIdentityOverrideAndSerializationFailure(bool reportSnapshotError, bool replaceFailedSnapshot)
    {
        using var harness = new Harness(1);
        await using var client = new Agento11yClient(Config());
        var recorder = client.StartToolExecution(new ToolExecutionStart
        {
            ToolName = "exec",
            ExecutionKind = ToolExecutionKind.Command,
            ContentCapture = ContentCaptureMode.Full,
        });
        var snapshotError = Assert.Throws<InvalidOperationException>(() => recorder.SetResult(new ToolExecutionEnd
        {
            VerifiedSkillName = "old",
            VerifiedSkillResourceName = "old.py",
            ProcessExitCode = 1,
            ProcessExitCodeIsError = false,
            Result = new Unserializable(),
        }));
        if (replaceFailedSnapshot)
        {
            Assert.Throws<InvalidOperationException>(() => recorder.SetResult(new ToolExecutionEnd { Result = new Unserializable() }));
        }
        recorder.SetResult(new ToolExecutionEnd { ProcessExitCode = 2, Result = "success" });
        if (reportSnapshotError)
        {
            recorder.SetExecutionError(snapshotError);
        }
        recorder.End();
        var draft = Assert.Single(harness.Measurements, m => m.Name == Draft);
        Assert.False(draft.Tags.ContainsKey("gen_ai.skill.name"));
        Assert.False(draft.Tags.ContainsKey("gen_ai.skill.resource.name"));
        Assert.Equal("2", draft.Tags["error.type"]);
        Assert.Same(reportSnapshotError ? snapshotError : null, recorder.Error);
        Assert.Equal(reportSnapshotError ? ActivityStatusCode.Error : ActivityStatusCode.Ok, Assert.Single(harness.Spans).Status);
        recorder.SetResult(new ToolExecutionEnd { VerifiedSkillName = "too late" });
        recorder.End();
        Assert.Single(harness.Measurements, m => m.Name == Draft);
    }

    [Theory]
    [InlineData(-1)]
    [InlineData(0)]
    [InlineData(1)]
    public async Task AttributesAreAllowlistedAndSamplingUsesAttemptedIdentity(int sampling)
    {
        using var fixture = LoadCases();
        using var harness = new Harness(sampling);
        var config = Config();
        var reserved = fixture.RootElement.GetProperty("reserved_attributes").EnumerateArray().Select(v => v.GetString()!).ToArray();
        foreach (var key in reserved)
        {
            config.Tags[key] = "spoof";
        }
        config.Tags["team"] = "sdk";
        await using var client = new Agento11yClient(config);
        var recorder = client.StartToolExecution(new ToolExecutionStart
        {
            ToolName = "read",
            ToolType = "function",
            ToolCallId = "private-call",
            ToolDescription = "private-description",
            ExecutionKind = ToolExecutionKind.ReadSkillResource,
            SkillName = "attempted",
            SkillResourceName = "attempted.md",
            SkillDescription = "private-description",
            SkillSourceURI = "file:///private",
            ProcessExecutableName = "private",
            ProcessExecutablePath = "/private",
            ConversationId = "private-conversation",
            ConversationTitle = "private-title",
            AgentName = "agent",
            AgentVersion = "1",
            RequestProvider = "provider",
            RequestModel = "model",
        });
        recorder.SetResult(new ToolExecutionEnd { VerifiedSkillResourceName = "known.md", ProcessExitCode = 9 });
        recorder.End();
        var tags = Assert.Single(harness.Measurements, m => m.Name == Draft).Tags;
        Assert.Equal("function", tags["gen_ai.tool.type"]);
        Assert.Equal("agent", tags["gen_ai.agent.name"]);
        Assert.Equal("1", tags["gen_ai.agent.version"]);
        Assert.Equal("provider", tags["gen_ai.provider.name"]);
        Assert.Equal("model", tags["gen_ai.request.model"]);
        Assert.Equal("sdk", tags["agento11y.tag.team"]);
        foreach (var key in reserved)
        {
            Assert.Equal("spoof", tags["agento11y.tag." + key]);
            if (key != Kind && key != "gen_ai.skill.resource.name")
            {
                Assert.False(tags.ContainsKey(key), key);
            }
        }
        Assert.Equal("read_skill_resource", tags[Kind]);
        Assert.Equal("known.md", tags["gen_ai.skill.resource.name"]);
        Assert.DoesNotContain(tags.Keys, k => k is "gen_ai.tool.call.id" or "gen_ai.conversation.id" or "agento11y.conversation.title"
            or "gen_ai.tool.description" or "gen_ai.tool.call.arguments" or "gen_ai.tool.call.result");
        if (sampling >= 0)
        {
            var samplingTags = Assert.Single(harness.SamplingTags);
            Assert.Equal("attempted", samplingTags["gen_ai.skill.name"]);
            Assert.Equal("attempted.md", samplingTags["gen_ai.skill.resource.name"]);
            Assert.False(samplingTags.ContainsKey(Kind));
        }
    }

    [Theory]
    [InlineData(-2, 0)]
    [InlineData(0, 0)]
    [InlineData(0.64, 0.64)]
    public async Task DurationAndExemplarContextArePreserved(double elapsed, double expected)
    {
        using var harness = new Harness(1);
        await using var client = new Agento11yClient(Config());
        var recorder = client.StartToolExecution(new ToolExecutionStart { ToolName = "read", StartedAt = Started });
        recorder.SetResult(new ToolExecutionEnd { CompletedAt = Started.AddSeconds(elapsed) });
        recorder.End();
        recorder.End();
        var draft = Assert.Single(harness.Measurements, m => m.Name == Draft);
        var legacy = Assert.Single(harness.Measurements, m => m.Name == Legacy);
        Assert.Equal(expected, draft.Value);
        Assert.Equal(expected, legacy.Value);
        Assert.NotNull(draft.ActivityId);
        Assert.Equal(legacy.ActivityId, draft.ActivityId);
        Assert.Equal(Assert.Single(harness.Spans).Id, draft.ActivityId);
    }

    [Theory]
    [InlineData(true)]
    [InlineData(false)]
    public async Task ToolInstrumentFailuresDoNotDisableLegacyOrCompletion(bool creationFailure)
    {
        using var harness = new Harness(1);
        using var faultyListener = new MeterListener();
        faultyListener.InstrumentPublished = (instrument, listener) =>
        {
            if (instrument.Meter.Name != Agento11yClient.InstrumentationName || instrument.Name != Draft)
            {
                return;
            }
            if (creationFailure)
            {
                throw new InvalidOperationException("tool instrument creation failed");
            }
            listener.EnableMeasurementEvents(instrument);
        };
        faultyListener.SetMeasurementEventCallback<double>((_, _, _, _) => throw new InvalidOperationException("tool record failed"));
        faultyListener.Start();
        await using var client = new Agento11yClient(Config());
        var recorder = client.StartToolExecution(new ToolExecutionStart { ToolName = "read" });
        recorder.End();
        recorder.End();
        Assert.Null(recorder.Error);
        Assert.Single(harness.Measurements, m => m.Name == Legacy);
        Assert.Single(harness.Spans);
    }

    private static string Text(JsonElement value, string key) => value.ValueKind == JsonValueKind.Object && value.TryGetProperty(key, out var text)
        ? text.GetString()! : string.Empty;

    private static int Integer(JsonElement value, string key, int fallback) => value.TryGetProperty(key, out var number) ? number.GetInt32() : fallback;

    private static Agento11yClientConfig Config() => new()
    {
        GenerationExport = new GenerationExportConfig { Protocol = GenerationExportProtocol.None, FlushInterval = TimeSpan.FromHours(1) },
    };

    private sealed class Unserializable
    {
        public string Value => throw new InvalidOperationException("cannot serialize user content");
    }

    private sealed record Measurement(string Name, string? Unit, double Value, Dictionary<string, object?> Tags, string? ActivityId);

    private sealed class Harness : IDisposable
    {
        private readonly MeterListener _meterListener;
        private readonly ActivityListener? _activityListener;
        public List<Measurement> Measurements { get; } = [];
        public List<Activity> Spans { get; } = [];
        public List<Dictionary<string, object?>> SamplingTags { get; } = [];

        public Harness(int sampling = -1)
        {
            if (sampling >= 0)
            {
                _activityListener = new ActivityListener
                {
                    ShouldListenTo = source => source.Name == Agento11yClient.InstrumentationName,
                    Sample = (ref ActivityCreationOptions<ActivityContext> options) =>
                    {
                        SamplingTags.Add(options.Tags!.ToDictionary(t => t.Key, t => t.Value));
                        return sampling == 0 ? ActivitySamplingResult.None : ActivitySamplingResult.AllDataAndRecorded;
                    },
                    ActivityStopped = activity => Spans.Add(activity),
                };
                ActivitySource.AddActivityListener(_activityListener);
            }
            _meterListener = new MeterListener
            {
                InstrumentPublished = (instrument, listener) =>
                {
                    if (instrument.Meter.Name == Agento11yClient.InstrumentationName && instrument.Name is Draft or Legacy)
                    {
                        Assert.IsType<Histogram<double>>(instrument);
                        listener.EnableMeasurementEvents(instrument);
                    }
                },
            };
            _meterListener.SetMeasurementEventCallback<double>((instrument, value, tags, _) =>
            {
                var attributes = new Dictionary<string, object?>();
                foreach (var tag in tags)
                {
                    Assert.True(attributes.TryAdd(tag.Key, tag.Value), $"duplicate metric key {tag.Key}");
                }
                Measurements.Add(new Measurement(instrument.Name, instrument.Unit, value, attributes, Activity.Current?.Id));
            });
            _meterListener.Start();
        }

        public void Dispose()
        {
            _meterListener.Dispose();
            _activityListener?.Dispose();
        }
    }
}
