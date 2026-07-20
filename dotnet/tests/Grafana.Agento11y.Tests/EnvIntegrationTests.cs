using Xunit;

namespace Grafana.Agento11y.Tests;

public sealed class EnvIntegrationTests
{
    private static GenerationStart MinimalStart()
    {
        return new GenerationStart
        {
            Id = "gen-1",
            ConversationId = "conv-1",
            Mode = GenerationMode.Sync,
            OperationName = "chat",
            Model = new ModelRef { Provider = "openai", Name = "gpt-4o" },
        };
    }

    private static Generation BareResult()
    {
        return new Generation
        {
            Usage = new TokenUsage { InputTokens = 1, OutputTokens = 1 },
            StopReason = "stop",
        };
    }

    [Fact]
    public void ResolveFromEnvFillsConfigDefaults()
    {
        var caller = new Agento11yClientConfig();
        var env = new Dictionary<string, string?>
        {
            ["SIGIL_AGENT_NAME"] = "env-agent",
            ["SIGIL_AGENT_VERSION"] = "1.2.3",
            ["SIGIL_USER_ID"] = "user-1",
            ["SIGIL_TAGS"] = "service=demo,team=ai",
        };

        var (resolved, _) = EnvConfig.ResolveFromEnv(k => env.TryGetValue(k, out var v) ? v : null, caller);

        Assert.Equal("env-agent", resolved.AgentName);
        Assert.Equal("1.2.3", resolved.AgentVersion);
        Assert.Equal("user-1", resolved.UserId);
        Assert.Equal("demo", resolved.Tags["service"]);
        Assert.Equal("ai", resolved.Tags["team"]);
    }

    [Fact]
    public void ResolveFromEnvFillsConfigDefaultsFromPreferredNames()
    {
        var caller = new Agento11yClientConfig();
        var env = new Dictionary<string, string?>
        {
            ["AGENTO11Y_AGENT_NAME"] = "env-agent",
            ["AGENTO11Y_AGENT_VERSION"] = "1.2.3",
            ["AGENTO11Y_USER_ID"] = "user-1",
            ["AGENTO11Y_TAGS"] = "service=demo,team=ai",
        };

        var (resolved, _) = EnvConfig.ResolveFromEnv(k => env.TryGetValue(k, out var v) ? v : null, caller);

        Assert.Equal("env-agent", resolved.AgentName);
        Assert.Equal("1.2.3", resolved.AgentVersion);
        Assert.Equal("user-1", resolved.UserId);
        Assert.Equal("demo", resolved.Tags["service"]);
        Assert.Equal("ai", resolved.Tags["team"]);
    }

    [Fact]
    public void CallerConfigOverridesEnv()
    {
        var caller = new Agento11yClientConfig { AgentName = "caller-agent" };
        var env = new Dictionary<string, string?> { ["SIGIL_AGENT_NAME"] = "env-agent" };

        var (resolved, _) = EnvConfig.ResolveFromEnv(k => env.TryGetValue(k, out var v) ? v : null, caller);

        Assert.Equal("caller-agent", resolved.AgentName);
    }

    [Fact]
    public async Task PerCallSeedTagWinsOverConfigTag()
    {
        var exporter = new CapturingGenerationExporter();
        var config = TestHelpers.TestConfig(exporter);
        config.Tags["service"] = "demo";
        config.Tags["team"] = "ai";

        await using (var client = new Agento11yClient(config))
        {
            var start = MinimalStart();
            start.Tags["team"] = "obs";
            var rec = client.StartGeneration(start);
            rec.SetResult(BareResult());
            rec.End();
            Assert.Null(rec.Error);
            await client.FlushAsync(TestContext.Current.CancellationToken);
        }

        Assert.NotEmpty(exporter.Requests);
        var captured = exporter.Requests[0].Generations[0];
        Assert.Equal("demo", captured.Tags["service"]);
        Assert.Equal("obs", captured.Tags["team"]);
    }

    [Fact]
    public async Task ConfigIdentityFallsThroughToGenerationStart()
    {
        var exporter = new CapturingGenerationExporter();
        var config = TestHelpers.TestConfig(exporter);
        config.AgentName = "env-agent";
        config.AgentVersion = "1.2.3";
        config.UserId = "user-1";

        await using (var client = new Agento11yClient(config))
        {
            var rec = client.StartGeneration(MinimalStart());
            rec.SetResult(BareResult());
            rec.End();
            Assert.Null(rec.Error);
            await client.FlushAsync(TestContext.Current.CancellationToken);
        }

        var captured = exporter.Requests[0].Generations[0];
        Assert.Equal("env-agent", captured.AgentName);
        Assert.Equal("1.2.3", captured.AgentVersion);
        Assert.Equal("user-1", captured.UserId);
    }

    [Fact]
    public async Task PerCallSeedIdentityOverridesConfigDefault()
    {
        var exporter = new CapturingGenerationExporter();
        var config = TestHelpers.TestConfig(exporter);
        config.AgentName = "env-agent";
        config.UserId = "env-user";

        await using (var client = new Agento11yClient(config))
        {
            var start = MinimalStart();
            start.AgentName = "call-agent";
            start.UserId = "call-user";
            var rec = client.StartGeneration(start);
            rec.SetResult(BareResult());
            rec.End();
            Assert.Null(rec.Error);
            await client.FlushAsync(TestContext.Current.CancellationToken);
        }

        var captured = exporter.Requests[0].Generations[0];
        Assert.Equal("call-agent", captured.AgentName);
        Assert.Equal("call-user", captured.UserId);
    }

    [Fact]
    public void ExplicitInsecureFalseBeatsEnvTrue()
    {
        var caller = new Agento11yClientConfig();
        caller.GenerationExport.Insecure = false;
        var env = new Dictionary<string, string?> { ["SIGIL_INSECURE"] = "true" };

        var (resolved, _) = EnvConfig.ResolveFromEnv(k => env.TryGetValue(k, out var v) ? v : null, caller);

        Assert.False(resolved.GenerationExport.Insecure);
    }

    [Fact]
    public void NoEnvNoCallerInsecureResolvesToFalseAfterConfigResolver()
    {
        var caller = new Agento11yClientConfig();
        var resolved = ConfigResolverTestHook.Resolve(caller, _ => null);
        Assert.NotNull(resolved.GenerationExport.Insecure);
        Assert.False(resolved.GenerationExport.Insecure!.Value);
    }
}

internal static class ConfigResolverTestHook
{
    public static Agento11yClientConfig Resolve(Agento11yClientConfig? config, Func<string, string?> envLookup)
    {
        // Wrapper that goes through ConfigResolver.Resolve via internal access.
        return ConfigResolver.Resolve(config, envLookup);
    }
}
