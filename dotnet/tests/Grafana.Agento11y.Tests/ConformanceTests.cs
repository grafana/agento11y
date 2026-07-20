using System.Collections.Concurrent;
using System.Diagnostics;
using System.Diagnostics.Metrics;
using System.Text;
using Xunit;
using Agento11yProto = Agento11y.V1;

namespace Grafana.Agento11y.Tests;

public sealed class ConformanceTests
{
    [Fact]
    public async Task SyncRoundtripSemantics()
    {
        await using var env = new ConformanceEnv();
        var requestArtifact = Artifact.JsonArtifact(ArtifactKind.Request, "request", new { ok = true });
        var responseArtifact = Artifact.JsonArtifact(ArtifactKind.Response, "response", new { status = "ok" });
        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Id = "gen-roundtrip",
            ConversationId = "conv-roundtrip",
            ConversationTitle = "Roundtrip conversation",
            UserId = "user-roundtrip",
            AgentName = "agent-roundtrip",
            AgentVersion = "v-roundtrip",
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            SystemPrompt = "be concise",
            MaxTokens = 256,
            Temperature = 0.2,
            TopP = 0.9,
            ToolChoice = "required",
            ThinkingEnabled = false,
            Tools =
            {
                new ToolDefinition
                {
                    Name = "weather",
                    Description = "Get weather",
                    Type = "function",
                    InputSchemaJson = Encoding.UTF8.GetBytes("{\"type\":\"object\"}"),
                },
            },
            Tags = new Dictionary<string, string>(StringComparer.Ordinal) { ["tenant"] = "dev" },
            Metadata = new Dictionary<string, object?>(StringComparer.Ordinal)
            {
                ["trace"] = "roundtrip",
                ["agento11y.gen_ai.request.thinking.budget_tokens"] = 2048L,
            },
        });
        recorder.SetResult(new Generation
        {
            ResponseId = "resp-roundtrip",
            ResponseModel = "gpt-5-2026",
            Input =
            {
                Message.UserTextMessage("hello"),
            },
            Output =
            {
                new Message
                {
                    Role = MessageRole.Assistant,
                    Parts =
                    {
                        Part.ThinkingPart("reasoning"),
                        Part.ToolCallPart(new ToolCall
                        {
                            Id = "call-1",
                            Name = "weather",
                            InputJson = Encoding.UTF8.GetBytes("{\"city\":\"Paris\"}"),
                        }),
                        Part.TextPart("Checking weather"),
                    },
                },
                new Message
                {
                    Role = MessageRole.Tool,
                    Parts =
                    {
                        Part.ToolResultPart(new ToolResult
                        {
                            ToolCallId = "call-1",
                            Name = "weather",
                            Content = "sunny",
                            ContentJson = Encoding.UTF8.GetBytes("{\"temp_c\":18}"),
                        }),
                    },
                },
            },
            Usage = new TokenUsage
            {
                InputTokens = 12,
                OutputTokens = 7,
                TotalTokens = 19,
                CacheReadInputTokens = 2,
                CacheWriteInputTokens = 1,
                ReasoningTokens = 4,
            },
            StopReason = "stop",
            Tags = new Dictionary<string, string>(StringComparer.Ordinal) { ["region"] = "eu" },
            Metadata = new Dictionary<string, object?>(StringComparer.Ordinal) { ["result"] = "ok" },
            Artifacts =
            {
                requestArtifact,
                responseArtifact,
            },
        });
        recorder.End();

        await env.ShutdownAsync();

        var generation = env.SingleGeneration();
        var span = env.GenerationSpan();

        Assert.Equal(Agento11yProto.GenerationMode.Sync, generation.Mode);
        Assert.Equal("generateText", generation.OperationName);
        Assert.Equal("conv-roundtrip", generation.ConversationId);
        Assert.Equal("agent-roundtrip", generation.AgentName);
        Assert.Equal("agent-roundtrip", recorder.LastGeneration!.AgentName);
        Assert.Equal("v-roundtrip", generation.AgentVersion);
        Assert.Equal(span.TraceId.ToHexString(), generation.TraceId);
        Assert.Equal(span.SpanId.ToHexString(), generation.SpanId);
        Assert.Equal("be concise", generation.SystemPrompt);
        Assert.Equal("Roundtrip conversation", generation.Metadata.Fields["agento11y.conversation.title"].StringValue);
        Assert.Equal("user-roundtrip", generation.Metadata.Fields["agento11y.user.id"].StringValue);
        Assert.Equal("sdk-dotnet", generation.Metadata.Fields["agento11y.sdk.name"].StringValue);
        Assert.Equal("hello", generation.Input[0].Parts[0].Text);
        Assert.Equal("reasoning", generation.Output[0].Parts[0].Thinking);
        Assert.Equal("weather", generation.Output[0].Parts[1].ToolCall.Name);
        Assert.Equal("Checking weather", generation.Output[0].Parts[2].Text);
        Assert.Equal("sunny", generation.Output[1].Parts[0].ToolResult.Content);
        Assert.Equal(256L, generation.MaxTokens);
        Assert.Equal(0.2, generation.Temperature, 10);
        Assert.Equal(0.9, generation.TopP, 10);
        Assert.Equal("required", generation.ToolChoice);
        Assert.False(generation.ThinkingEnabled);
        Assert.Equal(12L, generation.Usage.InputTokens);
        Assert.Equal(7L, generation.Usage.OutputTokens);
        Assert.Equal(19L, generation.Usage.TotalTokens);
        Assert.Equal(2L, generation.Usage.CacheReadInputTokens);
        Assert.Equal(1L, generation.Usage.CacheWriteInputTokens);
        Assert.Equal(4L, generation.Usage.ReasoningTokens);
        Assert.Equal("stop", generation.StopReason);
        Assert.Equal("dev", generation.Tags["tenant"]);
        Assert.Equal("eu", generation.Tags["region"]);
        Assert.Equal(2, generation.RawArtifacts.Count);
        Assert.Equal("generateText", span.GetTagItem("gen_ai.operation.name")?.ToString());
        Assert.Equal("Roundtrip conversation", span.GetTagItem("agento11y.conversation.title")?.ToString());
        Assert.Equal("user-roundtrip", span.GetTagItem("user.id")?.ToString());
        Assert.Contains("gen_ai.client.operation.duration", env.MetricNames);
        Assert.Contains("gen_ai.client.token.usage", env.MetricNames);
        Assert.DoesNotContain("gen_ai.client.time_to_first_token", env.MetricNames);
        Assert.True(
            env.HasMetricTag("gen_ai.client.token.usage", "gen_ai.agent.version", "v-roundtrip"),
            "token usage metrics should include the agent version tag"
        );
    }

    [Theory]
    [InlineData("Explicit", "Context", "Meta", "Explicit")]
    [InlineData("", "Context", "", "Context")]
    [InlineData("", "", "Meta", "Meta")]
    [InlineData("  Padded  ", "", "", "Padded")]
    [InlineData("   ", "", "", "")]
    public async Task ConversationTitleSemantics(string startTitle, string contextTitle, string metadataTitle, string expected)
    {
        await using var env = new ConformanceEnv();
        using var titleScope = contextTitle.Length > 0 ? Agento11yContext.WithConversationTitle(contextTitle) : NullScope.Instance;

        var start = new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            ConversationTitle = startTitle,
        };
        if (metadataTitle.Length > 0)
        {
            start.Metadata["agento11y.conversation.title"] = metadataTitle;
        }

        var recorder = env.Client.StartGeneration(start);
        recorder.SetResult(new Generation());
        recorder.End();

        await env.ShutdownAsync();

        var generation = env.SingleGeneration();
        var span = env.GenerationSpan();
        if (expected.Length == 0)
        {
            Assert.False(generation.Metadata.Fields.ContainsKey("agento11y.conversation.title"));
            Assert.Null(span.GetTagItem("agento11y.conversation.title"));
            return;
        }

        Assert.Equal(expected, generation.Metadata.Fields["agento11y.conversation.title"].StringValue);
        Assert.Equal(expected, span.GetTagItem("agento11y.conversation.title")?.ToString());
    }

    [Theory]
    [InlineData("explicit", "ctx", "canonical", "legacy", "explicit")]
    [InlineData("", "ctx", "", "", "ctx")]
    [InlineData("", "", "canonical", "", "canonical")]
    [InlineData("", "", "", "legacy", "legacy")]
    [InlineData("", "", "canonical", "legacy", "canonical")]
    [InlineData("  padded  ", "", "", "", "padded")]
    public async Task UserIdSemantics(string startUserId, string contextUserId, string canonicalUserId, string legacyUserId, string expected)
    {
        await using var env = new ConformanceEnv();
        using var userScope = contextUserId.Length > 0 ? Agento11yContext.WithUserId(contextUserId) : NullScope.Instance;

        var start = new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            UserId = startUserId,
        };
        if (canonicalUserId.Length > 0)
        {
            start.Metadata["agento11y.user.id"] = canonicalUserId;
        }
        if (legacyUserId.Length > 0)
        {
            start.Metadata["user.id"] = legacyUserId;
        }

        var recorder = env.Client.StartGeneration(start);
        recorder.SetResult(new Generation());
        recorder.End();

        await env.ShutdownAsync();

        var generation = env.SingleGeneration();
        var span = env.GenerationSpan();
        Assert.Equal(expected, generation.Metadata.Fields["agento11y.user.id"].StringValue);
        Assert.Equal(expected, span.GetTagItem("user.id")?.ToString());
    }

    [Theory]
    [InlineData("agent-explicit", "v1.2.3", "", "", "", "", "agent-explicit", "v1.2.3")]
    [InlineData("", "", "agent-context", "v-context", "", "", "agent-context", "v-context")]
    [InlineData("agent-seed", "v-seed", "", "", "agent-result", "v-result", "agent-result", "v-result")]
    [InlineData("", "", "", "", "", "", "", "")]
    public async Task AgentIdentitySemantics(
        string startName,
        string startVersion,
        string contextName,
        string contextVersion,
        string resultName,
        string resultVersion,
        string expectedName,
        string expectedVersion
    )
    {
        await using var env = new ConformanceEnv();
        using var agentNameScope = contextName.Length > 0 ? Agento11yContext.WithAgentName(contextName) : NullScope.Instance;
        using var agentVersionScope = contextVersion.Length > 0 ? Agento11yContext.WithAgentVersion(contextVersion) : NullScope.Instance;

        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            AgentName = startName,
            AgentVersion = startVersion,
        });
        recorder.SetResult(new Generation
        {
            AgentName = resultName,
            AgentVersion = resultVersion,
        });
        recorder.End();

        await env.ShutdownAsync();

        var generation = env.SingleGeneration();
        var span = env.GenerationSpan();
        Assert.Equal(expectedName, generation.AgentName);
        Assert.Equal(expectedVersion, generation.AgentVersion);
        Assert.Equal(expectedName.Length == 0 ? null : expectedName, span.GetTagItem("gen_ai.agent.name")?.ToString());
        Assert.Equal(expectedVersion.Length == 0 ? null : expectedVersion, span.GetTagItem("gen_ai.agent.version")?.ToString());
    }

    // echo -n "1.2.3" | shasum -a 256
    private const string EffectiveVersionDigest1_2_3 =
        "sha256:c47f5b18b8a430e698b9fe15e51f6119984e78334bcf3f45e210d30c37ef2f9e";

    [Fact]
    public async Task EffectiveVersionUnsetLeavesProtoFieldAbsent()
    {
        await using var env = new ConformanceEnv();
        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
        });
        recorder.SetResult(new Generation());
        recorder.End();
        Assert.Null(recorder.Error);
        await env.ShutdownAsync();

        var generation = env.SingleGeneration();
        Assert.False(generation.HasEffectiveVersion);
    }

    [Fact]
    public async Task EffectiveVersionRawHashesToPinnedDigest()
    {
        await using var env = new ConformanceEnv();
        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            EffectiveVersion = "1.2.3",
        });
        recorder.SetResult(new Generation());
        recorder.End();
        Assert.Null(recorder.Error);
        await env.ShutdownAsync();

        var generation = env.SingleGeneration();
        Assert.Equal(EffectiveVersionDigest1_2_3, generation.EffectiveVersion);
    }

    [Fact]
    public async Task EffectiveVersionWhitespaceOnlyLeavesProtoFieldAbsent()
    {
        await using var env = new ConformanceEnv();
        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            EffectiveVersion = "   ",
        });
        recorder.SetResult(new Generation());
        recorder.End();
        Assert.Null(recorder.Error);
        await env.ShutdownAsync();

        var generation = env.SingleGeneration();
        Assert.False(generation.HasEffectiveVersion);
    }

    [Fact]
    public async Task EffectiveVersionSurroundingWhitespaceIsTrimmedBeforeHashing()
    {
        await using var env = new ConformanceEnv();
        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            EffectiveVersion = "  1.2.3\t\n",
        });
        recorder.SetResult(new Generation());
        recorder.End();
        Assert.Null(recorder.Error);
        await env.ShutdownAsync();

        var generation = env.SingleGeneration();
        Assert.Equal(EffectiveVersionDigest1_2_3, generation.EffectiveVersion);
    }

    // echo -n "result-only" | shasum -a 256
    private const string EffectiveVersionDigestResultOnly =
        "sha256:f61f2b041f07a7e4a58a926df31279f4c11ebd1f716147d8ee8cbfad6a69f30e";

    [Fact]
    public async Task EffectiveVersionStartFallsThroughWhenResultIsEmpty()
    {
        await using var env = new ConformanceEnv();
        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            EffectiveVersion = "1.2.3",
        });
        recorder.SetResult(new Generation());
        recorder.End();
        Assert.Null(recorder.Error);
        await env.ShutdownAsync();

        var generation = env.SingleGeneration();
        Assert.Equal(EffectiveVersionDigest1_2_3, generation.EffectiveVersion);
    }

    [Fact]
    public async Task EffectiveVersionStartFallsThroughWhenResultIsWhitespaceOnly()
    {
        await using var env = new ConformanceEnv();
        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            EffectiveVersion = "1.2.3",
        });
        recorder.SetResult(new Generation { EffectiveVersion = "   " });
        recorder.End();
        Assert.Null(recorder.Error);
        await env.ShutdownAsync();

        var generation = env.SingleGeneration();
        Assert.Equal(EffectiveVersionDigest1_2_3, generation.EffectiveVersion);
    }

    [Fact]
    public async Task EffectiveVersionResultWinsOverStart()
    {
        await using var env = new ConformanceEnv();
        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            EffectiveVersion = "ignored",
        });
        recorder.SetResult(new Generation { EffectiveVersion = "result-only" });
        recorder.End();
        Assert.Null(recorder.Error);
        await env.ShutdownAsync();

        var generation = env.SingleGeneration();
        Assert.Equal(EffectiveVersionDigestResultOnly, generation.EffectiveVersion);
    }

    [Fact]
    public async Task StreamingTelemetrySemantics()
    {
        await using var env = new ConformanceEnv();
        var start = new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            StartedAt = new DateTimeOffset(2026, 3, 12, 9, 0, 0, TimeSpan.Zero),
        };
        var recorder = env.Client.StartStreamingGeneration(start);
        recorder.SetFirstTokenAt(start.StartedAt.Value.AddMilliseconds(250));
        recorder.SetResult(new Generation
        {
            Usage = new TokenUsage { InputTokens = 4, OutputTokens = 3, TotalTokens = 7 },
            StartedAt = start.StartedAt,
            CompletedAt = start.StartedAt.Value.AddSeconds(1),
        });
        recorder.End();

        await env.ShutdownAsync();

        var generation = env.SingleGeneration();
        var span = env.GenerationSpan();
        Assert.Equal(Agento11yProto.GenerationMode.Stream, generation.Mode);
        Assert.Equal("streamText", generation.OperationName);
        Assert.Equal("streamText gpt-5", span.DisplayName);
        Assert.Contains("gen_ai.client.operation.duration", env.MetricNames);
        Assert.Contains("gen_ai.client.time_to_first_token", env.MetricNames);
    }

    [Fact]
    public async Task ToolExecutionSemantics()
    {
        await using var env = new ConformanceEnv();
        using var titleScope = Agento11yContext.WithConversationTitle("Context title");
        using var agentNameScope = Agento11yContext.WithAgentName("agent-context");
        using var agentVersionScope = Agento11yContext.WithAgentVersion("v-context");

        var recorder = env.Client.StartToolExecution(new ToolExecutionStart
        {
            ToolName = "weather",
            ToolCallId = "call-weather-1",
            ToolType = "function",
            RequestProvider = "openai",
            RequestModel = "gpt-5",
            ContentCapture = ContentCaptureMode.Full,
        });
        recorder.SetResult(new ToolExecutionEnd
        {
            Arguments = new Dictionary<string, object?>(StringComparer.Ordinal)
            {
                ["city"] = "Paris",
            },
            Result = new Dictionary<string, object?>(StringComparer.Ordinal)
            {
                ["forecast"] = "sunny",
            },
        });
        recorder.End();

        await env.ShutdownAsync();

        var span = env.OperationSpan("execute_tool");
        Assert.Empty(env.Ingest.Requests);
        Assert.Equal("execute_tool weather", span.DisplayName);
        Assert.Equal("execute_tool", span.GetTagItem("gen_ai.operation.name"));
        Assert.Equal("weather", span.GetTagItem("gen_ai.tool.name"));
        Assert.Equal("call-weather-1", span.GetTagItem("gen_ai.tool.call.id"));
        Assert.Equal("function", span.GetTagItem("gen_ai.tool.type"));
        Assert.Contains("Paris", span.GetTagItem("gen_ai.tool.call.arguments")?.ToString());
        Assert.Contains("sunny", span.GetTagItem("gen_ai.tool.call.result")?.ToString());
        Assert.Equal("openai", span.GetTagItem("gen_ai.provider.name")?.ToString());
        Assert.Equal("gpt-5", span.GetTagItem("gen_ai.request.model")?.ToString());
        Assert.Equal("Context title", span.GetTagItem("agento11y.conversation.title")?.ToString());
        Assert.Equal("agent-context", span.GetTagItem("gen_ai.agent.name")?.ToString());
        Assert.Equal("v-context", span.GetTagItem("gen_ai.agent.version")?.ToString());
        Assert.Contains("gen_ai.client.operation.duration", env.MetricNames);
        Assert.DoesNotContain("gen_ai.client.time_to_first_token", env.MetricNames);
    }

    [Fact]
    public async Task EmbeddingSemantics()
    {
        await using var env = new ConformanceEnv();
        using var agentNameScope = Agento11yContext.WithAgentName("agent-context");
        using var agentVersionScope = Agento11yContext.WithAgentVersion("v-context");

        var recorder = env.Client.StartEmbedding(new EmbeddingStart
        {
            Model = new ModelRef { Provider = "openai", Name = "text-embedding-3-small" },
            Dimensions = 512,
        });
        recorder.SetResult(new EmbeddingResult
        {
            InputCount = 2,
            InputTokens = 8,
            InputTexts = { "hello", "world" },
            ResponseModel = "text-embedding-3-small",
            Dimensions = 512,
        });
        recorder.End();

        await env.ShutdownAsync();

        var span = env.OperationSpan("embeddings");
        Assert.Empty(env.Ingest.Requests);
        Assert.Equal("embeddings text-embedding-3-small", span.DisplayName);
        Assert.Equal("embeddings", span.GetTagItem("gen_ai.operation.name"));
        Assert.Equal("agent-context", span.GetTagItem("gen_ai.agent.name"));
        Assert.Equal("v-context", span.GetTagItem("gen_ai.agent.version"));
        Assert.Equal(2, span.GetTagItem("gen_ai.embeddings.input_count"));
        Assert.Equal(512L, span.GetTagItem("gen_ai.embeddings.dimension.count"));
        Assert.Equal("text-embedding-3-small", span.GetTagItem("gen_ai.response.model"));
        Assert.Contains("gen_ai.client.operation.duration", env.MetricNames);
        Assert.Contains("gen_ai.client.token.usage", env.MetricNames);
        Assert.DoesNotContain("gen_ai.client.time_to_first_token", env.MetricNames);
        Assert.DoesNotContain("gen_ai.client.tool_calls_per_operation", env.MetricNames);
    }

    [Fact]
    public async Task ValidationAndCallErrorSemantics()
    {
        await using var env = new ConformanceEnv();
        var invalid = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "anthropic", Name = "claude-sonnet-4-5" },
        });
        invalid.SetResult(new Generation
        {
            Input =
            {
                new Message
                {
                    Role = MessageRole.User,
                    Parts =
                    {
                        Part.ToolCallPart(new ToolCall { Name = "weather" }),
                    },
                },
            },
        });
        invalid.End();

        Assert.NotNull(invalid.Error);
        Assert.Empty(env.Ingest.Requests);
        Assert.Equal("validation_error", env.GenerationSpan().GetTagItem("error.type")?.ToString());

        var callError = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
        });
        callError.SetCallError(new InvalidOperationException("provider unavailable"));
        callError.SetResult(new Generation());
        callError.End();

        await env.ShutdownAsync();

        var generation = env.SingleGeneration();
        var spans = env.Spans.ToArray();
        Assert.Null(callError.Error);
        Assert.Equal("provider unavailable", generation.CallError);
        Assert.Equal("provider unavailable", generation.Metadata.Fields["call_error"].StringValue);
        Assert.Equal("provider_call_error", spans[^1].GetTagItem("error.type")?.ToString());
    }

    [Fact]
    public async Task RatingSubmissionSemantics()
    {
        await using var env = new ConformanceEnv();
        var response = await env.Client.SubmitConversationRatingAsync(
            "conv-rating",
            new SubmitConversationRatingRequest
            {
                RatingId = "rat-1",
                Rating = ConversationRatingValue.Bad,
                Comment = "wrong answer",
                Metadata = new Dictionary<string, object?>(StringComparer.Ordinal)
                {
                    ["channel"] = "assistant",
                },
            },
            TestContext.Current.CancellationToken
        );

        await env.ShutdownAsync();

        Assert.True(env.Rating.Requests.TryDequeue(out var captured));
        Assert.Equal("/api/v1/conversations/conv-rating/ratings", captured.Path);
        Assert.Equal("rat-1", response.Rating.RatingId);
        Assert.True(response.Summary.HasBadRating);

        using var body = System.Text.Json.JsonDocument.Parse(captured.Body);
        Assert.Equal("rat-1", body.RootElement.GetProperty("rating_id").GetString());
        Assert.Equal("CONVERSATION_RATING_VALUE_BAD", body.RootElement.GetProperty("rating").GetString());
        Assert.Equal("wrong answer", body.RootElement.GetProperty("comment").GetString());
    }

    [Fact]
    public async Task ClientTagAttributes()
    {
        await using var env = new ConformanceEnv(tags: new Dictionary<string, string>(StringComparer.Ordinal)
        {
            ["team"] = "payments",
        });
        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            Tags = new Dictionary<string, string>(StringComparer.Ordinal) { ["call_only"] = "yes" },
        });
        recorder.SetResult(new Generation
        {
            Usage = new TokenUsage { InputTokens = 4, OutputTokens = 2 },
        });
        recorder.End();
        await env.ShutdownAsync();

        var span = env.GenerationSpan();
        Assert.Equal("payments", span.GetTagItem("agento11y.tag.team")?.ToString());
        Assert.Null(span.GetTagItem("agento11y.tag.call_only"));

        Assert.True(
            env.HasMetricTag("gen_ai.client.operation.duration", "agento11y.tag.team", "payments"),
            "operation duration metrics should include the client tag"
        );
    }

    [Fact]
    public async Task ShutdownFlushSemantics()
    {
        await using var env = new ConformanceEnv(batchSize: 10);
        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            ConversationId = "conv-shutdown",
            AgentName = "agent-shutdown",
            AgentVersion = "v-shutdown",
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
        });
        recorder.SetResult(new Generation());
        recorder.End();

        Assert.Empty(env.Ingest.Requests);
        await env.ShutdownAsync();

        var generation = env.SingleGeneration();
        Assert.Equal("conv-shutdown", generation.ConversationId);
        Assert.Equal("agent-shutdown", generation.AgentName);
        Assert.Equal("v-shutdown", generation.AgentVersion);
    }

    // Guards the agento11y rename: the SDK emits only agento11y.* names for
    // SDK-owned span attributes, metadata keys, and tag projections, while
    // caller-supplied metadata and tags pass through untouched even when they
    // use the legacy sigil.* namespace.
    [Fact]
    public async Task NoSdkOwnedLegacyAgento11yNamespace()
    {
        await using var env = new ConformanceEnv();
        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Id = "gen-legacy-namespace",
            ConversationId = "conv-legacy-namespace",
            ConversationTitle = "Legacy namespace check",
            UserId = "user-legacy",
            AgentName = "agent-legacy",
            AgentVersion = "1.0.0",
            Model = new ModelRef { Provider = "anthropic", Name = "claude-sonnet-4-5" },
            ToolChoice = "auto",
            ThinkingEnabled = true,
            Tags = new Dictionary<string, string>(StringComparer.Ordinal) { ["sigil.caller_tag"] = "caller-tag" },
            Metadata = new Dictionary<string, object?>(StringComparer.Ordinal) { ["sigil.caller_key"] = "caller-value" },
        });
        recorder.SetResult(new Generation
        {
            Output = { Message.AssistantTextMessage("ok") },
            Usage = new TokenUsage { InputTokens = 1, OutputTokens = 1, TotalTokens = 2 },
        });
        recorder.End();

        await env.ShutdownAsync();

        var span = env.GenerationSpan();
        var legacySpanAttrs = span.TagObjects
            .Select(tag => tag.Key)
            .Where(key => key.StartsWith("sigil.", StringComparison.Ordinal))
            .ToList();
        Assert.Empty(legacySpanAttrs);
        Assert.Equal("gen-legacy-namespace", span.GetTagItem("agento11y.generation.id")?.ToString());
        Assert.Equal("sdk-dotnet", span.GetTagItem("agento11y.sdk.name")?.ToString());

        var generation = env.SingleGeneration();
        var legacyMetadataKeys = generation.Metadata.Fields.Keys
            .Where(key => key.StartsWith("sigil.", StringComparison.Ordinal) && key != "sigil.caller_key")
            .ToList();
        Assert.Empty(legacyMetadataKeys);
        Assert.Equal("caller-value", generation.Metadata.Fields["sigil.caller_key"].StringValue);
        Assert.Equal("sdk-dotnet", generation.Metadata.Fields["agento11y.sdk.name"].StringValue);
        Assert.Equal("user-legacy", generation.Metadata.Fields["agento11y.user.id"].StringValue);
        Assert.Equal("caller-tag", generation.Tags["sigil.caller_tag"]);
    }

    private sealed class ConformanceEnv : IAsyncDisposable
    {
        private bool _shutdown;
        private readonly MeterListener _meterListener;
        private readonly ActivityListener _activityListener;

        public GrpcIngestServer Ingest { get; }
        public RatingCaptureServer Rating { get; }
        public Agento11yClient Client { get; }
        public ConcurrentQueue<Activity> Spans { get; } = new();
        public ConcurrentDictionary<string, byte> MetricNames { get; } = new(StringComparer.Ordinal);
        public ConcurrentQueue<MetricMeasurement> MetricMeasurements { get; } = new();

        public ConformanceEnv(int batchSize = 1, Dictionary<string, string>? tags = null)
        {
            _activityListener = new ActivityListener
            {
                ShouldListenTo = source => source.Name == "github.com/grafana/sigil/sdks/dotnet",
                Sample = static (ref _) => ActivitySamplingResult.AllDataAndRecorded,
                ActivityStopped = activity =>
                {
                    Spans.Enqueue(activity);
                },
            };
            ActivitySource.AddActivityListener(_activityListener);

            _meterListener = new MeterListener();
            _meterListener.InstrumentPublished += (instrument, listener) =>
            {
                if (instrument.Name.StartsWith("gen_ai.client.", StringComparison.Ordinal))
                {
                    listener.EnableMeasurementEvents(instrument);
                }
            };
            _meterListener.SetMeasurementEventCallback<double>((instrument, _, tags, _) =>
            {
                MetricNames[instrument.Name] = 0;
                var tagSnapshot = new Dictionary<string, object?>(StringComparer.Ordinal);
                foreach (var tag in tags)
                {
                    tagSnapshot[tag.Key] = tag.Value;
                }
                MetricMeasurements.Enqueue(new MetricMeasurement(instrument.Name, tagSnapshot));
            });
            _meterListener.Start();

            Ingest = new GrpcIngestServer();
            Rating = new RatingCaptureServer((_, _, _) =>
                (
                    200,
                    "application/json",
                    Encoding.UTF8.GetBytes(
                        """
                        {
                          "rating":{
                            "rating_id":"rat-1",
                            "conversation_id":"conv-rating",
                            "rating":"CONVERSATION_RATING_VALUE_BAD",
                            "created_at":"2026-03-12T09:00:00Z"
                          },
                          "summary":{
                            "total_count":1,
                            "good_count":0,
                            "bad_count":1,
                            "latest_rating":"CONVERSATION_RATING_VALUE_BAD",
                            "latest_rated_at":"2026-03-12T09:00:00Z",
                            "has_bad_rating":true
                          }
                        }
                        """
                    )
                )
            );
            var config = new Agento11yClientConfig
            {
                Api = new ApiConfig
                {
                    Endpoint = $"http://127.0.0.1:{Rating.Port}",
                },
                GenerationExport = new GenerationExportConfig
                {
                    Protocol = GenerationExportProtocol.Grpc,
                    Endpoint = $"127.0.0.1:{Ingest.Port}",
                    Insecure = true,
                    BatchSize = batchSize,
                    QueueSize = 10,
                    FlushInterval = TimeSpan.FromHours(1),
                    MaxRetries = 1,
                    InitialBackoff = TimeSpan.FromMilliseconds(1),
                    MaxBackoff = TimeSpan.FromMilliseconds(2),
                },
            };
            if (tags != null)
            {
                config.Tags = tags;
            }
            Client = new Agento11yClient(config);
        }

        public async Task ShutdownAsync()
        {
            if (_shutdown)
            {
                return;
            }

            _shutdown = true;
            await Client.ShutdownAsync();
            _meterListener.Dispose();
            _activityListener.Dispose();
            Ingest.Dispose();
            Rating.Dispose();
        }

        public Agento11yProto.Generation SingleGeneration()
        {
            Assert.Single(Ingest.Requests);
            Assert.Single(Ingest.Requests[0].Request.Generations);
            return Ingest.Requests[0].Request.Generations[0];
        }

        public Activity GenerationSpan()
        {
            return OperationSpan(["generateText", "streamText"]);
        }

        public Activity OperationSpan(string operationName)
        {
            return OperationSpan([operationName]);
        }

        public bool HasMetricTag(string metricName, string key, string value)
        {
            foreach (var measurement in MetricMeasurements)
            {
                if (!string.Equals(measurement.Name, metricName, StringComparison.Ordinal))
                {
                    continue;
                }
                if (measurement.Tags.TryGetValue(key, out var got) && string.Equals(got?.ToString(), value, StringComparison.Ordinal))
                {
                    return true;
                }
            }

            return false;
        }

        private Activity OperationSpan(string[] operationNames)
        {
            var span = Spans
                .LastOrDefault(activity => operationNames.Contains(activity.GetTagItem("gen_ai.operation.name")?.ToString(), StringComparer.Ordinal));
            Assert.NotNull(span);
            return span!;
        }

        public async ValueTask DisposeAsync()
        {
            await ShutdownAsync();
        }
    }

    private sealed record MetricMeasurement(string Name, IReadOnlyDictionary<string, object?> Tags);

    private sealed class NullScope : IDisposable
    {
        public static NullScope Instance { get; } = new();

        public void Dispose()
        {
        }
    }
}
