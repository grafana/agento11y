using System.Diagnostics;
using Xunit;

namespace Grafana.Sigil.Tests;

public sealed class RuntimeLifecycleTests
{
    [Fact]
    public async Task Constructor_HandlesNullEmbeddingCaptureConfig()
    {
        var exporter = new CapturingGenerationExporter();
        var config = TestHelpers.TestConfig(exporter);
        config.EmbeddingCapture = null!;

        await using var _ = new SigilClient(config);

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

        await using var client = new SigilClient(config);

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

        await using var client = new SigilClient(config);

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

        await using var client = new SigilClient(config);

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

        await using var client = new SigilClient(config);

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

        var client = new SigilClient(config);
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

        await using var client = new SigilClient(config);

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
        await using var client = new SigilClient(TestHelpers.TestConfig(exporter));

        using var conversationScope = SigilContext.WithConversationId("conv-context");
        using var agentNameScope = SigilContext.WithAgentName("agent-context");
        using var agentVersionScope = SigilContext.WithAgentVersion("v-context");

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
        await using var client = new SigilClient(TestHelpers.TestConfig(exporter));

        using var conversationScope = SigilContext.WithConversationId("conv-context");
        using var agentNameScope = SigilContext.WithAgentName("agent-context");
        using var agentVersionScope = SigilContext.WithAgentVersion("v-context");

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
        await using var client = new SigilClient(TestHelpers.TestConfig(exporter));

        var start = TestHelpers.CreateSeedStart("gen-sdk-metadata");
        start.Metadata["sigil.sdk.name"] = "seed-value";

        var result = TestHelpers.CreateSeedResult("gen-sdk-metadata");
        result.Metadata["sigil.sdk.name"] = "result-value";

        var recorder = client.StartGeneration(start);
        recorder.SetResult(result);
        recorder.End();

        Assert.NotNull(recorder.LastGeneration);
        Assert.Equal("sdk-dotnet", recorder.LastGeneration!.Metadata["sigil.sdk.name"]?.ToString());
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

        await using var client = new SigilClient(config);
        var recorder = client.StartGeneration(TestHelpers.CreateSeedStart("gen-span-sdk"));
        recorder.SetResult(TestHelpers.CreateSeedResult("gen-span-sdk"));
        recorder.End();
        await client.ShutdownAsync(TestContext.Current.CancellationToken);

        Assert.Single(spans);
        var span = spans[0];
        Assert.Equal("sdk-dotnet", span.GetTagItem("sigil.sdk.name"));
    }

    [Fact]
    public async Task ToolSpan_ContainsSdkIdentityTags()
    {
        var exporter = new CapturingGenerationExporter();
        await using var client = new SigilClient(TestHelpers.TestConfig(exporter));

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
        Assert.Equal("sdk-dotnet", span.GetTagItem("sigil.sdk.name"));
    }

    private static void EndSuccessfulGeneration(SigilClient client, string id)
    {
        StartAndEnd(client, id);
    }

    private static GenerationRecorder StartAndEnd(SigilClient client, string id)
    {
        var recorder = client.StartGeneration(TestHelpers.CreateSeedStart(id));
        recorder.SetResult(TestHelpers.CreateSeedResult(id));
        recorder.End();
        return recorder;
    }
}
