using System.Diagnostics;
using System.Text.Json;
using Xunit;

namespace Grafana.Agento11y.Tests;

public sealed class RuntimeLifecycleTests
{
    [Fact]
    public async Task Constructor_HandlesNullEmbeddingCaptureConfig()
    {
        var exporter = new CapturingGenerationExporter();
        var config = TestHelpers.TestConfig(exporter);
        config.EmbeddingCapture = null!;

        await using var _ = new Agento11yClient(config);

        Assert.NotNull(config.EmbeddingCapture);
        Assert.Equal(20, config.EmbeddingCapture.MaxInputItems);
        Assert.Equal(1024, config.EmbeddingCapture.MaxTextLength);
    }

    [Fact]
    public async Task GenerationExporter_FlushesByBatchSize()
    {
        var exporter = new CapturingGenerationExporter();
        var config = TestHelpers.TestConfig(exporter);
        config.GenerationExport.BatchSize = 2;

        await using var client = new Agento11yClient(config);

        EndSuccessfulGeneration(client, "gen-1");
        EndSuccessfulGeneration(client, "gen-2");

        await TestHelpers.WaitForAsync(
            () => exporter.Requests.Count == 1 && exporter.Requests[0].Generations.Count == 2,
            TimeSpan.FromSeconds(2),
            "batch-size flush was not observed"
        );
    }

    [Fact]
    public async Task GenerationExporter_FlushesByInterval()
    {
        var exporter = new CapturingGenerationExporter();
        var config = TestHelpers.TestConfig(exporter);
        config.GenerationExport.BatchSize = 10;
        config.GenerationExport.FlushInterval = TimeSpan.FromMilliseconds(30);

        await using var client = new Agento11yClient(config);

        EndSuccessfulGeneration(client, "gen-1");

        await TestHelpers.WaitForAsync(
            () => exporter.Requests.Count >= 1 && exporter.Requests[0].Generations.Count == 1,
            TimeSpan.FromSeconds(2),
            "interval flush was not observed"
        );
    }

    [Fact]
    public async Task GenerationExporter_RetriesWithExponentialBackoff()
    {
        var exporter = new CapturingGenerationExporter
        {
            FailuresBeforeSuccess = 3,
        };

        var delays = new List<TimeSpan>();
        var config = TestHelpers.TestConfig(exporter);
        config.GenerationExport.BatchSize = 10;
        config.GenerationExport.MaxRetries = 3;
        config.GenerationExport.InitialBackoff = TimeSpan.FromMilliseconds(10);
        config.GenerationExport.MaxBackoff = TimeSpan.FromMilliseconds(25);
        config.SleepAsync = (delay, _) =>
        {
            delays.Add(delay);
            return Task.CompletedTask;
        };

        await using var client = new Agento11yClient(config);

        EndSuccessfulGeneration(client, "gen-1");
        await client.FlushAsync(TestContext.Current.CancellationToken);

        Assert.Equal(4, exporter.Calls);
        Assert.Equal(
            new[]
            {
                TimeSpan.FromMilliseconds(10),
                TimeSpan.FromMilliseconds(20),
                TimeSpan.FromMilliseconds(25),
            },
            delays
        );
    }

    [Fact]
    public async Task GenerationRecorder_QueueFullSetsLocalError()
    {
        var exporter = new CapturingGenerationExporter();
        var config = TestHelpers.TestConfig(exporter);
        config.GenerationExport.QueueSize = 1;
        config.GenerationExport.BatchSize = 100;
        config.GenerationExport.FlushInterval = TimeSpan.FromHours(1);

        await using var client = new Agento11yClient(config);

        var first = StartAndEnd(client, "gen-1");
        Assert.Null(first.Error);

        var second = StartAndEnd(client, "gen-2");
        Assert.NotNull(second.Error);
        Assert.IsType<QueueFullException>(second.Error);
    }

    [Fact]
    public async Task Shutdown_FlushesPendingGenerations()
    {
        var exporter = new CapturingGenerationExporter();
        var config = TestHelpers.TestConfig(exporter);
        config.GenerationExport.BatchSize = 10;
        config.GenerationExport.FlushInterval = TimeSpan.FromHours(1);

        var client = new Agento11yClient(config);
        StartAndEnd(client, "gen-1");

        await client.ShutdownAsync(TestContext.Current.CancellationToken);

        Assert.Single(exporter.Requests);
        Assert.Single(exporter.Requests[0].Generations);
    }

    [Fact]
    public async Task Recorder_EndIsIdempotent()
    {
        var exporter = new CapturingGenerationExporter();
        var config = TestHelpers.TestConfig(exporter);
        config.GenerationExport.BatchSize = 1;

        await using var client = new Agento11yClient(config);

        var recorder = client.StartGeneration(TestHelpers.CreateSeedStart("gen-idempotent"));
        recorder.SetResult(TestHelpers.CreateSeedResult("gen-idempotent"));
        recorder.End();
        recorder.End();

        await client.FlushAsync(TestContext.Current.CancellationToken);
        Assert.Single(exporter.Requests);
        Assert.Single(exporter.Requests[0].Generations);
    }

    [Fact]
    public async Task ContextDefaults_AreAppliedWhenStartFieldsAreEmpty()
    {
        var exporter = new CapturingGenerationExporter();
        await using var client = new Agento11yClient(TestHelpers.TestConfig(exporter));

        using var conversationScope = Agento11yContext.WithConversationId("conv-context");
        using var agentNameScope = Agento11yContext.WithAgentName("agent-context");
        using var agentVersionScope = Agento11yContext.WithAgentVersion("v-context");

        var recorder = client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef
            {
                Provider = "openai",
                Name = "gpt-5",
            },
        });

        recorder.SetResult(new Generation
        {
            Input = { Message.UserTextMessage("hello") },
            Output = { Message.AssistantTextMessage("hi") },
        });
        recorder.End();

        Assert.NotNull(recorder.LastGeneration);
        Assert.Equal("conv-context", recorder.LastGeneration!.ConversationId);
        Assert.Equal("agent-context", recorder.LastGeneration.AgentName);
        Assert.Equal("v-context", recorder.LastGeneration.AgentVersion);
    }

    [Fact]
    public async Task ExplicitStartValues_OverrideContextDefaults()
    {
        var exporter = new CapturingGenerationExporter();
        await using var client = new Agento11yClient(TestHelpers.TestConfig(exporter));

        using var conversationScope = Agento11yContext.WithConversationId("conv-context");
        using var agentNameScope = Agento11yContext.WithAgentName("agent-context");
        using var agentVersionScope = Agento11yContext.WithAgentVersion("v-context");

        var recorder = client.StartGeneration(new GenerationStart
        {
            ConversationId = "conv-explicit",
            AgentName = "agent-explicit",
            AgentVersion = "v-explicit",
            Model = new ModelRef
            {
                Provider = "openai",
                Name = "gpt-5",
            },
        });

        recorder.SetResult(new Generation
        {
            Input = { Message.UserTextMessage("hello") },
            Output = { Message.AssistantTextMessage("hi") },
        });
        recorder.End();

        Assert.NotNull(recorder.LastGeneration);
        Assert.Equal("conv-explicit", recorder.LastGeneration!.ConversationId);
        Assert.Equal("agent-explicit", recorder.LastGeneration.AgentName);
        Assert.Equal("v-explicit", recorder.LastGeneration.AgentVersion);
    }

    [Fact]
    public async Task GenerationRecorder_OverridesConflictingSdkMetadataValues()
    {
        var exporter = new CapturingGenerationExporter();
        await using var client = new Agento11yClient(TestHelpers.TestConfig(exporter));

        var start = TestHelpers.CreateSeedStart("gen-sdk-metadata");
        start.Metadata["agento11y.sdk.name"] = "seed-value";

        var result = TestHelpers.CreateSeedResult("gen-sdk-metadata");
        result.Metadata["agento11y.sdk.name"] = "result-value";

        var recorder = client.StartGeneration(start);
        recorder.SetResult(result);
        recorder.End();

        Assert.NotNull(recorder.LastGeneration);
        Assert.Equal("sdk-dotnet", recorder.LastGeneration!.Metadata["agento11y.sdk.name"]?.ToString());
    }

    [Fact]
    public async Task GenerationSpan_ContainsSdkIdentityTags()
    {
        var exporter = new CapturingGenerationExporter();
        var config = TestHelpers.TestConfig(exporter);
        config.GenerationExport.BatchSize = 1;

        var spans = new List<Activity>();
        using var listener = new ActivityListener
        {
            ShouldListenTo = source => source.Name == "github.com/grafana/sigil/sdks/dotnet",
            Sample = static (ref _) => ActivitySamplingResult.AllDataAndRecorded,
            ActivityStopped = activity =>
            {
                if (activity.GetTagItem("gen_ai.operation.name")?.ToString() != "execute_tool")
                {
                    spans.Add(activity);
                }
            },
        };
        ActivitySource.AddActivityListener(listener);

        await using var client = new Agento11yClient(config);
        var recorder = client.StartGeneration(TestHelpers.CreateSeedStart("gen-span-sdk"));
        recorder.SetResult(TestHelpers.CreateSeedResult("gen-span-sdk"));
        recorder.End();
        await client.ShutdownAsync(TestContext.Current.CancellationToken);

        Assert.Single(spans);
        var span = spans[0];
        Assert.Equal("sdk-dotnet", span.GetTagItem("agento11y.sdk.name"));
    }

    [Fact]
    public async Task ToolSpan_ContainsSdkIdentityTags()
    {
        var exporter = new CapturingGenerationExporter();
        await using var client = new Agento11yClient(TestHelpers.TestConfig(exporter));

        var spans = new List<Activity>();
        using var listener = new ActivityListener
        {
            ShouldListenTo = source => source.Name == "github.com/grafana/sigil/sdks/dotnet",
            Sample = static (ref _) => ActivitySamplingResult.AllDataAndRecorded,
            ActivityStopped = activity =>
            {
                if (activity.GetTagItem("gen_ai.operation.name")?.ToString() == "execute_tool")
                {
                    spans.Add(activity);
                }
            },
        };
        ActivitySource.AddActivityListener(listener);

        var recorder = client.StartToolExecution(new ToolExecutionStart
        {
            ToolName = "weather",
        });
        recorder.SetResult(new ToolExecutionEnd
        {
            Arguments = new Dictionary<string, object?>
            {
                ["city"] = "Paris",
            },
            Result = new Dictionary<string, object?>
            {
                ["temp_c"] = 18,
            },
        });
        recorder.End();

        Assert.Single(spans);
        var span = spans[0];
        Assert.Equal("sdk-dotnet", span.GetTagItem("agento11y.sdk.name"));
    }

    [Fact]
    public void ToolExecutionModels_DefaultExecutionKindAndProcessExitCodeAreUnset()
    {
        var start = InternalUtils.DeepClone(new ToolExecutionStart());
        var end = InternalUtils.DeepClone(new ToolExecutionEnd());

        Assert.Equal(default, start.ExecutionKind);
        Assert.Equal(ToolExecutionKind.Unset, start.ExecutionKind);
        Assert.Empty(start.SkillName);
        Assert.Empty(start.SkillDescription);
        Assert.Empty(start.SkillSourceURI);
        Assert.Empty(start.SkillResourceName);
        Assert.Empty(start.ProcessExecutableName);
        Assert.Empty(start.ProcessExecutablePath);
        Assert.Null(end.ProcessExitCode);

        var absent = JsonSerializer.Deserialize<ToolExecutionEnd>("{}")!;
        Assert.Null(InternalUtils.DeepClone(absent).ProcessExitCode);
    }

    [Theory]
    [InlineData(ToolExecutionKind.Unset)]
    [InlineData(ToolExecutionKind.LoadSkill)]
    [InlineData(ToolExecutionKind.ReadSkillResource)]
    [InlineData(ToolExecutionKind.Command)]
    public void ToolExecutionStart_DeepClonePreservesSkillProcessFieldsAndIsolatesCopies(ToolExecutionKind executionKind)
    {
        var start = new ToolExecutionStart
        {
            ExecutionKind = executionKind,
            SkillName = "nono-sandbox",
            SkillDescription = "Inspect sandbox permissions",
            SkillSourceURI = "file:///skills/nono-sandbox/SKILL.md",
            SkillResourceName = "references/permissions.md",
            ProcessExecutableName = "nono",
            ProcessExecutablePath = "/usr/local/bin/nono",
        };

        var clone = InternalUtils.DeepClone(start);
        Assert.NotSame(start, clone);
        using var json = JsonDocument.Parse(InternalUtils.SerializeJson(start));
        Assert.Equal((int)executionKind, json.RootElement.GetProperty("executionKind").GetInt32());

        start.ExecutionKind = ToolExecutionKind.Command;
        start.SkillName = "changed skill";
        start.SkillDescription = "changed description";
        start.SkillSourceURI = "file:///changed/SKILL.md";
        start.SkillResourceName = "changed resource";
        start.ProcessExecutableName = "changed executable";
        start.ProcessExecutablePath = "/changed/executable";

        Assert.Equal(executionKind, clone.ExecutionKind);
        Assert.Equal("nono-sandbox", clone.SkillName);
        Assert.Equal("Inspect sandbox permissions", clone.SkillDescription);
        Assert.Equal("file:///skills/nono-sandbox/SKILL.md", clone.SkillSourceURI);
        Assert.Equal("references/permissions.md", clone.SkillResourceName);
        Assert.Equal("nono", clone.ProcessExecutableName);
        Assert.Equal("/usr/local/bin/nono", clone.ProcessExecutablePath);

        clone.ExecutionKind = ToolExecutionKind.Unset;
        clone.SkillName = string.Empty;
        clone.SkillDescription = string.Empty;
        clone.SkillSourceURI = string.Empty;
        clone.SkillResourceName = string.Empty;
        clone.ProcessExecutableName = string.Empty;
        clone.ProcessExecutablePath = string.Empty;

        Assert.Equal(ToolExecutionKind.Command, start.ExecutionKind);
        Assert.Equal("changed skill", start.SkillName);
        Assert.Equal("changed description", start.SkillDescription);
        Assert.Equal("file:///changed/SKILL.md", start.SkillSourceURI);
        Assert.Equal("changed resource", start.SkillResourceName);
        Assert.Equal("changed executable", start.ProcessExecutableName);
        Assert.Equal("/changed/executable", start.ProcessExecutablePath);
    }

    [Theory]
    [InlineData(null)]
    [InlineData(0L)]
    [InlineData(17L)]
    [InlineData(-1L)]
    [InlineData(long.MaxValue)]
    public void ToolExecutionEnd_DeepClonePreservesProcessExitCodeAndIsolatesCopies(long? exitCode)
    {
        var end = new ToolExecutionEnd { ProcessExitCode = exitCode };
        var clone = InternalUtils.DeepClone(end);
        var secondClone = InternalUtils.DeepClone(clone);

        Assert.NotSame(end, clone);
        Assert.NotSame(clone, secondClone);
        end.ProcessExitCode = 42;
        Assert.Equal(exitCode, clone.ProcessExitCode);
        Assert.Equal(exitCode, secondClone.ProcessExitCode);

        clone.ProcessExitCode = 99;
        Assert.Equal(42L, end.ProcessExitCode);
        Assert.Equal(exitCode, secondClone.ProcessExitCode);
    }

    private static void EndSuccessfulGeneration(Agento11yClient client, string id)
    {
        StartAndEnd(client, id);
    }

    private static GenerationRecorder StartAndEnd(Agento11yClient client, string id)
    {
        var recorder = client.StartGeneration(TestHelpers.CreateSeedStart(id));
        recorder.SetResult(TestHelpers.CreateSeedResult(id));
        recorder.End();
        return recorder;
    }
}
