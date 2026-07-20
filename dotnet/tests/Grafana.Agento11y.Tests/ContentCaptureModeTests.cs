using System.Collections.Concurrent;
using System.Diagnostics;
using System.Text;
using System.Text.Json;
using Xunit;
using Agento11yProto = Agento11y.V1;

namespace Grafana.Agento11y.Tests;

public sealed class ContentCaptureModeTests
{
    [Fact]
    public void ToMetadataValue_ReturnsCorrectStrings()
    {
        Assert.Equal("full", ContentCaptureMode.Full.ToMetadataValue());
        Assert.Equal("no_tool_content", ContentCaptureMode.NoToolContent.ToMetadataValue());
        Assert.Equal("metadata_only", ContentCaptureMode.MetadataOnly.ToMetadataValue());
        Assert.Equal("full_with_metadata_spans", ContentCaptureMode.FullWithMetadataSpans.ToMetadataValue());
        Assert.Equal(string.Empty, ContentCaptureMode.Default.ToMetadataValue());
    }

    [Theory]
    [InlineData(ContentCaptureMode.Full, ContentCaptureMode.Default, ContentCaptureMode.Full)]
    [InlineData(ContentCaptureMode.Full, ContentCaptureMode.MetadataOnly, ContentCaptureMode.MetadataOnly)]
    [InlineData(ContentCaptureMode.MetadataOnly, ContentCaptureMode.Default, ContentCaptureMode.MetadataOnly)]
    [InlineData(ContentCaptureMode.MetadataOnly, ContentCaptureMode.Full, ContentCaptureMode.Full)]
    public void ResolveContentCaptureMode_Semantics(
        ContentCaptureMode fallback,
        ContentCaptureMode @override,
        ContentCaptureMode expected)
    {
        var got = Agento11yClient.ResolveContentCaptureMode(@override, fallback);
        Assert.Equal(expected, got);
    }

    [Fact]
    public void ResolveContentCaptureMode_InvalidEnumFailsClosed()
    {
        Assert.Equal(
            ContentCaptureMode.MetadataOnly,
            Agento11yClient.ResolveContentCaptureMode((ContentCaptureMode)99, ContentCaptureMode.Full));
        Assert.Equal(
            ContentCaptureMode.MetadataOnly,
            Agento11yClient.ResolveContentCaptureMode(ContentCaptureMode.Default, (ContentCaptureMode)99));
    }

    [Fact]
    public void ResolveClientContentCaptureMode_DefaultBecomesNoToolContent()
    {
        Assert.Equal(ContentCaptureMode.NoToolContent, Agento11yClient.ResolveClientContentCaptureMode(ContentCaptureMode.Default));
        Assert.Equal(ContentCaptureMode.Full, Agento11yClient.ResolveClientContentCaptureMode(ContentCaptureMode.Full));
        Assert.Equal(ContentCaptureMode.MetadataOnly, Agento11yClient.ResolveClientContentCaptureMode(ContentCaptureMode.MetadataOnly));
    }

    [Fact]
    public void CallContentCaptureResolver_NilReturnsDefault()
    {
        var got = Agento11yClient.CallContentCaptureResolver(null, null);
        Assert.Equal(ContentCaptureMode.Default, got);
    }

    [Fact]
    public void CallContentCaptureResolver_ReturnsResolverResult()
    {
        var got = Agento11yClient.CallContentCaptureResolver(
            _ => ContentCaptureMode.MetadataOnly,
            new Dictionary<string, object?>());
        Assert.Equal(ContentCaptureMode.MetadataOnly, got);
    }

    [Fact]
    public void CallContentCaptureResolver_ReadsMetadata()
    {
        var got = Agento11yClient.CallContentCaptureResolver(
            metadata =>
            {
                if (metadata != null && metadata.TryGetValue("tenant_id", out var val) && val is string s && s == "opted-out")
                    return ContentCaptureMode.MetadataOnly;
                return ContentCaptureMode.Full;
            },
            new Dictionary<string, object?> { ["tenant_id"] = "opted-out" });
        Assert.Equal(ContentCaptureMode.MetadataOnly, got);
    }

    [Fact]
    public void CallContentCaptureResolver_PassesReadOnlyMetadata()
    {
        var metadata = new Dictionary<string, object?> { ["tenant_id"] = "original" };
        var got = Agento11yClient.CallContentCaptureResolver(
            received =>
            {
                Assert.NotNull(received);
                Assert.Throws<NotSupportedException>(() =>
                    ((IDictionary<string, object?>)received!)["tenant_id"] = "changed");
                return ContentCaptureMode.Full;
            },
            metadata);

        Assert.Equal(ContentCaptureMode.Full, got);
        Assert.Equal("original", metadata["tenant_id"]);
    }

    [Fact]
    public void CallContentCaptureResolver_ExceptionFailsClosed()
    {
        var got = Agento11yClient.CallContentCaptureResolver(
            _ => throw new InvalidOperationException("resolver bug"),
            null);
        Assert.Equal(ContentCaptureMode.MetadataOnly, got);
    }

    [Fact]
    public void StripContent_StripsAllSensitiveContent()
    {
        var gen = MakeTestGeneration();
        Agento11yClient.StripContent(gen, "rate_limit");

        Assert.Equal(string.Empty, gen.SystemPrompt);
        Assert.Equal(string.Empty, gen.Input[0].Parts[0].Text);
        Assert.Equal(string.Empty, gen.Output[0].Parts[0].Thinking);
        Assert.Empty(gen.Output[0].Parts[1].ToolCall!.InputJson);
        Assert.Equal(string.Empty, gen.Output[0].Parts[2].Text);
        Assert.Equal(string.Empty, gen.Input[1].Parts[0].ToolResult!.Content);
        Assert.Empty(gen.Input[1].Parts[0].ToolResult!.ContentJson);
        Assert.Empty(gen.Tools[0].Description);
        Assert.Empty(gen.Tools[0].InputSchemaJson);
        Assert.Empty(gen.Artifacts);
    }

    [Fact]
    public void StripContent_StripsConversationTitle()
    {
        var gen = MakeTestGeneration();
        gen.ConversationTitle = "Secret project discussion";
        gen.Metadata[Agento11yClient.SpanAttrConversationTitle] = "Secret project discussion";
        Agento11yClient.StripContent(gen, "rate_limit");

        Assert.Equal(string.Empty, gen.ConversationTitle);
        Assert.False(gen.Metadata.ContainsKey(Agento11yClient.SpanAttrConversationTitle));
    }

    [Fact]
    public void StripContent_PreservesMessageStructure()
    {
        var gen = MakeTestGeneration();
        Agento11yClient.StripContent(gen, "rate_limit");

        Assert.Equal(2, gen.Input.Count);
        Assert.Single(gen.Output);
        Assert.Equal(3, gen.Output[0].Parts.Count);
        Assert.Equal(MessageRole.User, gen.Input[0].Role);
        Assert.Equal(PartKind.Thinking, gen.Output[0].Parts[0].Kind);
        Assert.Equal("weather", gen.Output[0].Parts[1].ToolCall!.Name);
        Assert.Equal("call_1", gen.Output[0].Parts[1].ToolCall!.Id);
        Assert.Equal("call_1", gen.Input[1].Parts[0].ToolResult!.ToolCallId);
        Assert.Equal("weather", gen.Input[1].Parts[0].ToolResult!.Name);
    }

    [Fact]
    public void StripContent_PreservesOperationalMetadata()
    {
        var gen = MakeTestGeneration();
        Agento11yClient.StripContent(gen, "rate_limit");

        Assert.Equal("weather", gen.Tools[0].Name);
        Assert.Equal(120L, gen.Usage.InputTokens);
        Assert.Equal(42L, gen.Usage.OutputTokens);
        Assert.Equal("end_turn", gen.StopReason);
        Assert.Equal("claude-sonnet-4-5", gen.Model.Name);
        Assert.Equal("sdk-dotnet", gen.Metadata["agento11y.sdk.name"]);
    }

    [Fact]
    public void StripContent_ReplacesCallErrorWithCategory()
    {
        var gen = MakeTestGeneration();
        Agento11yClient.StripContent(gen, "rate_limit");

        Assert.Equal("rate_limit", gen.CallError);
        Assert.False(gen.Metadata.ContainsKey("call_error"));
    }

    [Fact]
    public void StripContent_FallsBackToSdkError()
    {
        var gen = MakeTestGeneration();
        Agento11yClient.StripContent(gen, "");

        Assert.Equal("sdk_error", gen.CallError);
    }

    [Fact]
    public async Task DefaultResolution_NoToolContent()
    {
        await using var env = new ContentCaptureEnv();
        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
        });
        recorder.SetResult(new Generation
        {
            Input = { Message.UserTextMessage("hello") },
            Output = { Message.AssistantTextMessage("world") },
            Usage = new TokenUsage { InputTokens = 10, OutputTokens = 5 },
        });
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var gen = env.SingleGeneration();
        Assert.Equal("no_tool_content", gen.Metadata.Fields[Agento11yClient.MetadataKeyContentCaptureMode].StringValue);
        Assert.Equal("hello", gen.Input[0].Parts[0].Text);
    }

    [Fact]
    public async Task ClientMetadataOnly_StripsContent()
    {
        await using var env = new ContentCaptureEnv(ContentCaptureMode.MetadataOnly);
        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
        });
        recorder.SetResult(new Generation
        {
            Input = { Message.UserTextMessage("hello") },
            Output = { Message.AssistantTextMessage("world") },
            Usage = new TokenUsage { InputTokens = 10, OutputTokens = 5 },
        });
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var gen = env.SingleGeneration();
        Assert.Equal("metadata_only", gen.Metadata.Fields[Agento11yClient.MetadataKeyContentCaptureMode].StringValue);
        Assert.Equal(string.Empty, gen.Input[0].Parts[0].Text);
        Assert.Equal(10L, gen.Usage.InputTokens);
    }

    [Fact]
    public async Task ClientMetadataOnly_DoesNotAttachConversationTitleToGenerationSpan()
    {
        await using var env = new ContentCaptureEnv(ContentCaptureMode.MetadataOnly);
        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            ConversationTitle = "Secret project discussion",
        });
        recorder.SetResult(new Generation
        {
            Input = { Message.UserTextMessage("hello") },
            Output = { Message.AssistantTextMessage("world") },
            Usage = new TokenUsage { InputTokens = 10, OutputTokens = 5 },
        });
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var span = env.Spans.Single(a => a.GetTagItem("gen_ai.operation.name")?.ToString() == "generateText");
        Assert.Null(span.GetTagItem(Agento11yClient.SpanAttrConversationTitle));
    }

    [Fact]
    public async Task ClientMetadataOnly_RedactsExceptionMessageAndStacktraceOnGenerationSpan()
    {
        await using var env = new ContentCaptureEnv(ContentCaptureMode.MetadataOnly);
        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
        });
        recorder.SetCallError(new InvalidOperationException("provider rejected: <secret prompt>"));
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var span = env.Spans.Single(a => a.GetTagItem("gen_ai.operation.name")?.ToString() == "generateText");
        Assert.NotNull(span.GetTagItem("exception.type"));
        Assert.Null(span.GetTagItem("exception.message"));
        Assert.Null(span.GetTagItem("exception.stacktrace"));
        Assert.Equal(ActivityStatusCode.Error, span.Status);
        Assert.DoesNotContain("secret prompt", span.StatusDescription ?? string.Empty);
    }

    [Fact]
    public async Task ClientFull_KeepsExceptionMessageOnGenerationSpan()
    {
        await using var env = new ContentCaptureEnv(ContentCaptureMode.Full);
        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
        });
        recorder.SetCallError(new InvalidOperationException("provider rejected: <secret prompt>"));
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var span = env.Spans.Single(a => a.GetTagItem("gen_ai.operation.name")?.ToString() == "generateText");
        Assert.Equal("provider rejected: <secret prompt>", span.GetTagItem("exception.message"));
        Assert.NotNull(span.GetTagItem("exception.stacktrace"));
    }

    [Fact]
    public async Task ToolMetadataOnly_RedactsExceptionMessageOnToolSpan()
    {
        await using var env = new ContentCaptureEnv(ContentCaptureMode.MetadataOnly);
        var recorder = env.Client.StartToolExecution(new ToolExecutionStart
        {
            ToolName = "test_tool",
        });
        recorder.SetExecutionError(new InvalidOperationException("tool failed: <secret arg>"));
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var span = env.ToolSpan();
        Assert.NotNull(span.GetTagItem("exception.type"));
        Assert.Null(span.GetTagItem("exception.message"));
        Assert.Null(span.GetTagItem("exception.stacktrace"));
        Assert.DoesNotContain("secret arg", span.StatusDescription ?? string.Empty);
    }

    [Fact]
    public async Task ClientFull_PreservesContent()
    {
        await using var env = new ContentCaptureEnv(ContentCaptureMode.Full);
        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
        });
        recorder.SetResult(new Generation
        {
            Input = { Message.UserTextMessage("hello") },
            Output = { Message.AssistantTextMessage("world") },
            Usage = new TokenUsage { InputTokens = 10, OutputTokens = 5 },
        });
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var gen = env.SingleGeneration();
        Assert.Equal("full", gen.Metadata.Fields[Agento11yClient.MetadataKeyContentCaptureMode].StringValue);
        Assert.Equal("hello", gen.Input[0].Parts[0].Text);
    }

    [Fact]
    public async Task PerGenerationOverride_MetadataOnlyOverridesClientFull()
    {
        await using var env = new ContentCaptureEnv(ContentCaptureMode.Full);
        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            ContentCapture = ContentCaptureMode.MetadataOnly,
        });
        recorder.SetResult(new Generation
        {
            Input = { Message.UserTextMessage("hello") },
            Output = { Message.AssistantTextMessage("world") },
            Usage = new TokenUsage { InputTokens = 10, OutputTokens = 5 },
        });
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var gen = env.SingleGeneration();
        Assert.Equal("metadata_only", gen.Metadata.Fields[Agento11yClient.MetadataKeyContentCaptureMode].StringValue);
        Assert.Equal(string.Empty, gen.Input[0].Parts[0].Text);
    }

    [Fact]
    public async Task PerGenerationOverride_FullOverridesClientMetadataOnly()
    {
        await using var env = new ContentCaptureEnv(ContentCaptureMode.MetadataOnly);
        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            ContentCapture = ContentCaptureMode.Full,
        });
        recorder.SetResult(new Generation
        {
            Input = { Message.UserTextMessage("hello") },
            Output = { Message.AssistantTextMessage("world") },
            Usage = new TokenUsage { InputTokens = 10, OutputTokens = 5 },
        });
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var gen = env.SingleGeneration();
        Assert.Equal("full", gen.Metadata.Fields[Agento11yClient.MetadataKeyContentCaptureMode].StringValue);
        Assert.Equal("hello", gen.Input[0].Parts[0].Text);
    }

    [Fact]
    public async Task Resolver_OverridesClientDefault()
    {
        await using var env = new ContentCaptureEnv(
            ContentCaptureMode.Full,
            _ => ContentCaptureMode.MetadataOnly);
        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
        });
        recorder.SetResult(new Generation
        {
            Input = { Message.UserTextMessage("hello") },
            Output = { Message.AssistantTextMessage("world") },
            Usage = new TokenUsage { InputTokens = 10, OutputTokens = 5 },
        });
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var gen = env.SingleGeneration();
        Assert.Equal("metadata_only", gen.Metadata.Fields[Agento11yClient.MetadataKeyContentCaptureMode].StringValue);
        Assert.Equal(string.Empty, gen.Input[0].Parts[0].Text);
    }

    [Fact]
    public async Task Resolver_PerGenOverrideTakesPrecedence()
    {
        await using var env = new ContentCaptureEnv(
            ContentCaptureMode.Default,
            _ => ContentCaptureMode.MetadataOnly);
        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            ContentCapture = ContentCaptureMode.Full,
        });
        recorder.SetResult(new Generation
        {
            Input = { Message.UserTextMessage("hello") },
            Output = { Message.AssistantTextMessage("world") },
            Usage = new TokenUsage { InputTokens = 10, OutputTokens = 5 },
        });
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var gen = env.SingleGeneration();
        Assert.Equal("full", gen.Metadata.Fields[Agento11yClient.MetadataKeyContentCaptureMode].StringValue);
        Assert.Equal("hello", gen.Input[0].Parts[0].Text);
    }

    [Fact]
    public async Task Resolver_PerGenOverrideDoesNotInvokeResolver()
    {
        var calls = 0;
        await using var env = new ContentCaptureEnv(
            ContentCaptureMode.Default,
            _ =>
            {
                calls++;
                return ContentCaptureMode.MetadataOnly;
            });
        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            ContentCapture = ContentCaptureMode.Full,
        });
        recorder.SetResult(new Generation
        {
            Input = { Message.UserTextMessage("hello") },
            Output = { Message.AssistantTextMessage("world") },
            Usage = new TokenUsage { InputTokens = 10, OutputTokens = 5 },
        });
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var gen = env.SingleGeneration();
        Assert.Equal(0, calls);
        Assert.Equal("full", gen.Metadata.Fields[Agento11yClient.MetadataKeyContentCaptureMode].StringValue);
    }

    [Fact]
    public async Task Resolver_ExceptionFailsClosed()
    {
        await using var env = new ContentCaptureEnv(
            ContentCaptureMode.Full,
            _ => throw new InvalidOperationException("oops"));
        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
        });
        recorder.SetResult(new Generation
        {
            Input = { Message.UserTextMessage("hello") },
            Output = { Message.AssistantTextMessage("world") },
            Usage = new TokenUsage { InputTokens = 10, OutputTokens = 5 },
        });
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var gen = env.SingleGeneration();
        Assert.Equal("metadata_only", gen.Metadata.Fields[Agento11yClient.MetadataKeyContentCaptureMode].StringValue);
        Assert.Equal(string.Empty, gen.Input[0].Parts[0].Text);
    }

    [Fact]
    public async Task ValidationAcceptsStrippedGeneration()
    {
        await using var env = new ContentCaptureEnv(ContentCaptureMode.MetadataOnly);
        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
        });
        recorder.SetResult(new Generation
        {
            Input = { Message.UserTextMessage("hello") },
            Output =
            {
                new Message
                {
                    Role = MessageRole.Assistant,
                    Parts =
                    {
                        Part.ThinkingPart("deep thoughts"),
                        Part.TextPart("answer"),
                    },
                },
            },
            Usage = new TokenUsage { InputTokens = 10, OutputTokens = 5 },
        });
        recorder.End();

        Assert.Null(recorder.Error);

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var gen = env.SingleGeneration();
        Assert.Equal(string.Empty, gen.Input[0].Parts[0].Text);
        Assert.Equal(string.Empty, gen.Output[0].Parts[0].Thinking);
        Assert.Equal(string.Empty, gen.Output[0].Parts[1].Text);
    }

    [Fact]
    public void ShouldIncludeToolContent_InvalidEnumFailsClosed()
    {
        Assert.False(Agento11yClient.ShouldIncludeToolContent(
            (ContentCaptureMode)99,
            ContentCaptureMode.Full,
            true,
            ContentCaptureMode.Full,
            true));
        Assert.False(Agento11yClient.ShouldIncludeToolContent(
            ContentCaptureMode.Default,
            (ContentCaptureMode)99,
            true,
            ContentCaptureMode.Full,
            true));
    }

    [Theory]
    [InlineData(ContentCaptureMode.Default, false, false, false)]
    [InlineData(ContentCaptureMode.Default, false, true, true)]
    [InlineData(ContentCaptureMode.Full, false, false, true)]
    [InlineData(ContentCaptureMode.Full, false, true, true)]
    [InlineData(ContentCaptureMode.MetadataOnly, false, true, false)]
    [InlineData(ContentCaptureMode.FullWithMetadataSpans, false, true, false)]
    public void ShouldIncludeToolContent_NoContext(
        ContentCaptureMode clientDefault,
        bool ctxSet,
        bool legacyInclude,
        bool wantContent)
    {
        var got = Agento11yClient.ShouldIncludeToolContent(
            ContentCaptureMode.Default,
            ContentCaptureMode.Default,
            ctxSet,
            clientDefault,
            legacyInclude);
        Assert.Equal(wantContent, got);
    }

    [Theory]
    [InlineData(ContentCaptureMode.MetadataOnly, true, true, false)]
    [InlineData(ContentCaptureMode.FullWithMetadataSpans, true, true, false)]
    [InlineData(ContentCaptureMode.Full, true, true, true)]
    [InlineData(ContentCaptureMode.NoToolContent, true, false, false)]
    [InlineData(ContentCaptureMode.NoToolContent, true, true, true)]
    public void ShouldIncludeToolContent_WithContext(
        ContentCaptureMode ctxMode,
        bool ctxSet,
        bool legacyInclude,
        bool wantContent)
    {
        var got = Agento11yClient.ShouldIncludeToolContent(
            ContentCaptureMode.Default,
            ctxMode,
            ctxSet,
            ContentCaptureMode.Full,
            legacyInclude);
        Assert.Equal(wantContent, got);
    }

    [Theory]
    [InlineData(ContentCaptureMode.Full, ContentCaptureMode.MetadataOnly, true, true)]
    [InlineData(ContentCaptureMode.MetadataOnly, ContentCaptureMode.Full, true, false)]
    [InlineData(ContentCaptureMode.FullWithMetadataSpans, ContentCaptureMode.Full, true, false)]
    public void ShouldIncludeToolContent_PerToolOverride(
        ContentCaptureMode toolMode,
        ContentCaptureMode ctxMode,
        bool legacyInclude,
        bool wantContent)
    {
        var got = Agento11yClient.ShouldIncludeToolContent(
            toolMode,
            ctxMode,
            true,
            ContentCaptureMode.Full,
            legacyInclude);
        Assert.Equal(wantContent, got);
    }

    [Fact]
    public async Task ToolExecution_ClientDefault_LegacyFalse_Suppressed()
    {
        await using var env = new ContentCaptureEnv();
        var recorder = env.Client.StartToolExecution(new ToolExecutionStart
        {
            ToolName = "test_tool",
        });
        recorder.SetResult(new ToolExecutionEnd
        {
            Arguments = "args",
            Result = "result",
        });
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var span = env.ToolSpan();
        Assert.Null(span.GetTagItem(Agento11yClient.SpanAttrToolCallArguments));
    }

    [Fact]
    public async Task ToolExecution_ClientFull_ContentIncluded()
    {
        await using var env = new ContentCaptureEnv(ContentCaptureMode.Full);
        var recorder = env.Client.StartToolExecution(new ToolExecutionStart
        {
            ToolName = "test_tool",
        });
        recorder.SetResult(new ToolExecutionEnd
        {
            Arguments = "args",
            Result = "result",
        });
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var span = env.ToolSpan();
        Assert.NotNull(span.GetTagItem(Agento11yClient.SpanAttrToolCallArguments));
    }

    [Fact]
    public async Task ToolExecution_ClientMetadataOnly_LegacyTrue_Suppressed()
    {
        await using var env = new ContentCaptureEnv(ContentCaptureMode.MetadataOnly);
#pragma warning disable CS0618
        var recorder = env.Client.StartToolExecution(new ToolExecutionStart
        {
            ToolName = "test_tool",
            IncludeContent = true,
        });
#pragma warning restore CS0618
        recorder.SetResult(new ToolExecutionEnd
        {
            Arguments = "args",
            Result = "result",
        });
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var span = env.ToolSpan();
        Assert.Null(span.GetTagItem(Agento11yClient.SpanAttrToolCallArguments));
    }

    [Fact]
    public async Task ToolExecution_ClientMetadataOnly_DoesNotAttachConversationTitleToSpan()
    {
        await using var env = new ContentCaptureEnv(ContentCaptureMode.MetadataOnly);
#pragma warning disable CS0618
        var recorder = env.Client.StartToolExecution(new ToolExecutionStart
        {
            ToolName = "test_tool",
            ToolDescription = "Sensitive internal tool prompt",
            ConversationTitle = "Secret project discussion",
            IncludeContent = true,
        });
#pragma warning restore CS0618
        recorder.SetResult(new ToolExecutionEnd
        {
            Arguments = "args",
            Result = "result",
        });
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var span = env.ToolSpan();
        Assert.Null(span.GetTagItem(Agento11yClient.SpanAttrToolDescription));
        Assert.Null(span.GetTagItem(Agento11yClient.SpanAttrConversationTitle));
        Assert.Null(span.GetTagItem(Agento11yClient.SpanAttrToolCallArguments));
    }

    [Fact]
    public async Task ToolExecution_BackwardCompat_LegacyTrue_Included()
    {
        await using var env = new ContentCaptureEnv();
#pragma warning disable CS0618
        var recorder = env.Client.StartToolExecution(new ToolExecutionStart
        {
            ToolName = "test_tool",
            IncludeContent = true,
        });
#pragma warning restore CS0618
        recorder.SetResult(new ToolExecutionEnd
        {
            Arguments = "args",
            Result = "result",
        });
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var span = env.ToolSpan();
        Assert.NotNull(span.GetTagItem(Agento11yClient.SpanAttrToolCallArguments));
    }

    [Fact]
    public async Task ContextPropagation_GenerationSetsContextForToolExecution()
    {
        await using var env = new ContentCaptureEnv(ContentCaptureMode.MetadataOnly);

        var genRecorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
        });

        // Tool starts while generation is active — should inherit MetadataOnly from context.
#pragma warning disable CS0618
        var toolRecorder = env.Client.StartToolExecution(new ToolExecutionStart
        {
            ToolName = "test_tool",
            IncludeContent = true,
        });
#pragma warning restore CS0618
        toolRecorder.SetResult(new ToolExecutionEnd
        {
            Arguments = "args",
            Result = "result",
        });
        toolRecorder.End();

        genRecorder.SetResult(new Generation
        {
            Input = { Message.UserTextMessage("hello") },
            Output = { Message.AssistantTextMessage("world") },
            Usage = new TokenUsage { InputTokens = 10, OutputTokens = 5 },
        });
        genRecorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var span = env.ToolSpan();
        Assert.Null(span.GetTagItem(Agento11yClient.SpanAttrToolCallArguments));
    }

    [Fact]
    public async Task ContextPropagation_FullOverridesParentMetadataOnly()
    {
        await using var env = new ContentCaptureEnv(ContentCaptureMode.MetadataOnly);

        var genRecorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            ContentCapture = ContentCaptureMode.Full,
        });

        // Tool starts under Full generation context.
#pragma warning disable CS0618
        var toolRecorder = env.Client.StartToolExecution(new ToolExecutionStart
        {
            ToolName = "test_tool",
            IncludeContent = true,
        });
#pragma warning restore CS0618
        toolRecorder.SetResult(new ToolExecutionEnd
        {
            Arguments = "args",
            Result = "result",
        });
        toolRecorder.End();

        genRecorder.SetResult(new Generation
        {
            Input = { Message.UserTextMessage("hello") },
            Output = { Message.AssistantTextMessage("world") },
            Usage = new TokenUsage { InputTokens = 10, OutputTokens = 5 },
        });
        genRecorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var span = env.ToolSpan();
        Assert.NotNull(span.GetTagItem(Agento11yClient.SpanAttrToolCallArguments));
    }

    [Fact]
    public void IsContentStripped_DetectsMarker()
    {
        var gen = new Generation
        {
            Metadata = new Dictionary<string, object?>(StringComparer.Ordinal)
            {
                [Agento11yClient.MetadataKeyContentCaptureMode] = "metadata_only",
            },
        };
        Assert.True(Agento11yClient.IsContentStripped(gen));
    }

    [Fact]
    public void IsContentStripped_FalseWithoutMarker()
    {
        Assert.False(Agento11yClient.IsContentStripped(new Generation()));
    }

    [Fact]
    public void IsContentStripped_FalseForOtherModes()
    {
        var gen = new Generation
        {
            Metadata = new Dictionary<string, object?>(StringComparer.Ordinal)
            {
                [Agento11yClient.MetadataKeyContentCaptureMode] = "full",
            },
        };
        Assert.False(Agento11yClient.IsContentStripped(gen));
    }

    [Fact]
    public async Task ToolExecution_PerToolOverride_Full()
    {
        await using var env = new ContentCaptureEnv(ContentCaptureMode.MetadataOnly);
        var recorder = env.Client.StartToolExecution(new ToolExecutionStart
        {
            ToolName = "test_tool",
            ContentCapture = ContentCaptureMode.Full,
        });
        recorder.SetResult(new ToolExecutionEnd
        {
            Arguments = "args",
            Result = "result",
        });
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var span = env.ToolSpan();
        Assert.NotNull(span.GetTagItem(Agento11yClient.SpanAttrToolCallArguments));
    }

    [Fact]
    public async Task ToolExecution_PerToolOverrideDoesNotInvokeResolver()
    {
        var calls = 0;
        await using var env = new ContentCaptureEnv(
            ContentCaptureMode.Default,
            _ =>
            {
                calls++;
                return ContentCaptureMode.MetadataOnly;
            });
        var recorder = env.Client.StartToolExecution(new ToolExecutionStart
        {
            ToolName = "test_tool",
            ContentCapture = ContentCaptureMode.Full,
        });
        recorder.SetResult(new ToolExecutionEnd
        {
            Arguments = "args",
            Result = "result",
        });
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var span = env.ToolSpan();
        Assert.Equal(0, calls);
        Assert.NotNull(span.GetTagItem(Agento11yClient.SpanAttrToolCallArguments));
    }

    [Fact]
    public async Task ToolExecution_ContextDoesNotInvokeResolver()
    {
        var calls = 0;
        await using var env = new ContentCaptureEnv(
            ContentCaptureMode.Default,
            _ =>
            {
                calls++;
                return ContentCaptureMode.MetadataOnly;
            });
        var genRecorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            ContentCapture = ContentCaptureMode.Full,
        });

        var toolRecorder = env.Client.StartToolExecution(new ToolExecutionStart
        {
            ToolName = "test_tool",
        });
        toolRecorder.SetResult(new ToolExecutionEnd
        {
            Arguments = "args",
            Result = "result",
        });
        toolRecorder.End();

        genRecorder.SetResult(new Generation
        {
            Input = { Message.UserTextMessage("hello") },
            Output = { Message.AssistantTextMessage("world") },
            Usage = new TokenUsage { InputTokens = 10, OutputTokens = 5 },
        });
        genRecorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var span = env.ToolSpan();
        Assert.Equal(0, calls);
        Assert.NotNull(span.GetTagItem(Agento11yClient.SpanAttrToolCallArguments));
    }

    [Fact]
    public void IsContentStripped_DetectsJsonElementMarker()
    {
        // After DeepClone (JSON round-trip), string values in metadata become JsonElement.
        var gen = new Generation
        {
            Metadata = new Dictionary<string, object?>(StringComparer.Ordinal)
            {
                [Agento11yClient.MetadataKeyContentCaptureMode] = JsonDocument.Parse("\"metadata_only\"").RootElement,
            },
        };
        Assert.True(Agento11yClient.IsContentStripped(gen));
    }

    [Fact]
    public void IsContentStripped_FalseForNonMetadataOnlyJsonElement()
    {
        var gen = new Generation
        {
            Metadata = new Dictionary<string, object?>(StringComparer.Ordinal)
            {
                [Agento11yClient.MetadataKeyContentCaptureMode] = JsonDocument.Parse("\"full\"").RootElement,
            },
        };
        Assert.False(Agento11yClient.IsContentStripped(gen));
    }

    [Fact]
    public void CallContentCaptureResolver_PassesNullMetadataThrough()
    {
        IReadOnlyDictionary<string, object?>? received = new Dictionary<string, object?>();
        Agento11yClient.CallContentCaptureResolver(
            metadata =>
            {
                received = metadata;
                return ContentCaptureMode.Full;
            },
            null);
        Assert.Null(received);
    }

    [Fact]
    public void CallContentCaptureResolver_InvalidEnumFailsClosed()
    {
        var got = Agento11yClient.CallContentCaptureResolver(
            _ => (ContentCaptureMode)99,
            null);
        Assert.Equal(ContentCaptureMode.MetadataOnly, got);
    }

    [Fact]
    public void CallContentCaptureResolver_LogsOnException()
    {
        List<string> logged = [];
        Agento11yClient.CallContentCaptureResolver(
            _ => throw new InvalidOperationException("resolver bug"),
            null,
            msg => logged.Add(msg));
        Assert.Single(logged);
        Assert.Contains("resolver bug", logged[0]);
    }

    [Fact]
    public void CallContentCaptureResolver_LogsOnInvalidEnum()
    {
        List<string> logged = [];
        Agento11yClient.CallContentCaptureResolver(
            _ => (ContentCaptureMode)99,
            null,
            msg => logged.Add(msg));
        Assert.Single(logged);
        Assert.Contains("undefined mode", logged[0]);
    }

    [Fact]
    public async Task OverlappingGenerations_NonLifoCompletion()
    {
        // When two generations overlap on the same async flow and the older one
        // ends first, the newer generation's mode should still be active in context.
        await using var env = new ContentCaptureEnv(ContentCaptureMode.Full);

        var genA = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            ContentCapture = ContentCaptureMode.MetadataOnly,
        });

        var genB = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            ContentCapture = ContentCaptureMode.Full,
        });

        // genA ends first (non-LIFO order).
        genA.SetResult(new Generation
        {
            Input = { Message.UserTextMessage("hello") },
            Output = { Message.AssistantTextMessage("world") },
            Usage = new TokenUsage { InputTokens = 10, OutputTokens = 5 },
        });
        genA.End();

        // Tool starts after genA ended but while genB is still active.
        // Should see genB's Full mode, not be wiped by genA's disposal.
        var toolRecorder = env.Client.StartToolExecution(new ToolExecutionStart
        {
            ToolName = "test_tool",
            ContentCapture = ContentCaptureMode.Default,
        });
        toolRecorder.SetResult(new ToolExecutionEnd
        {
            Arguments = "args",
            Result = "result",
        });
        toolRecorder.End();

        genB.SetResult(new Generation
        {
            Input = { Message.UserTextMessage("hello") },
            Output = { Message.AssistantTextMessage("world") },
            Usage = new TokenUsage { InputTokens = 10, OutputTokens = 5 },
        });
        genB.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        // genB was Full, so tool content should have been included.
        var span = env.ToolSpan();
        Assert.NotNull(span.GetTagItem(Agento11yClient.SpanAttrToolCallArguments));
    }

    [Fact]
    public async Task Resolver_ReceivesOriginalMetadataTypes()
    {
        // The resolver should receive original metadata types (string, bool, long),
        // not JsonElement values from DeepClone's JSON round-trip.
        Type? receivedType = null;
        await using var env = new ContentCaptureEnv(
            ContentCaptureMode.Default,
            metadata =>
            {
                if (metadata != null && metadata.TryGetValue("tenant_id", out var val))
                {
                    receivedType = val?.GetType();
                }
                return ContentCaptureMode.Full;
            });

        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            Metadata = new Dictionary<string, object?>(StringComparer.Ordinal)
            {
                ["tenant_id"] = "my-tenant",
            },
        });
        recorder.SetResult(new Generation
        {
            Input = { Message.UserTextMessage("hello") },
            Output = { Message.AssistantTextMessage("world") },
            Usage = new TokenUsage { InputTokens = 10, OutputTokens = 5 },
        });
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        Assert.NotNull(receivedType);
        Assert.Equal(typeof(string), receivedType);
    }

    [Fact]
    public async Task SubmitConversationRating_DoesNotMutateCallerRequest()
    {
        await using var env = new ContentCaptureEnv(ContentCaptureMode.MetadataOnly);

        var request = new SubmitConversationRatingRequest
        {
            RatingId = "r1",
            Rating = ConversationRatingValue.Good,
            Comment = "great answer",
        };

        try
        {
            await env.Client.SubmitConversationRatingAsync("conv-1", request, TestContext.Current.CancellationToken);
        }
        catch (RatingTransportException)
        {
            // Rating submission may fail (no API endpoint) but the mutation
            // test is about the request object, not the HTTP call.
        }

        Assert.Equal("great answer", request.Comment);
    }

    // --- Coverage matrix across every on-the-wire mode ---
    //
    // One full-content fixture run through every mode, expectations driven
    // by the records below. DEFAULT is intentionally absent — it's the
    // resolver fall-through, covered by the resolution tests above.

    public record ModeExpect(
        ContentCaptureMode Mode,
        string Marker,
        bool ProtoContentStripped,
        bool SpanTitlePresent,
        bool ProtoCallErrorRaw,
        bool SpanRawError)
    {
        public override string ToString() => Marker;
    }

    public static TheoryData<ModeExpect> ContentCaptureModeMatrix => new()
    {
        new ModeExpect(ContentCaptureMode.Full, "full", false, true, true, true),
        // NO_TOOL_CONTENT is generation-content-full; only tool spans gate args/results.
        new ModeExpect(ContentCaptureMode.NoToolContent, "no_tool_content", false, true, true, true),
        new ModeExpect(ContentCaptureMode.MetadataOnly, "metadata_only", true, false, false, false),
        new ModeExpect(ContentCaptureMode.FullWithMetadataSpans, "full_with_metadata_spans", false, false, true, false),
    };

    [Theory]
    [MemberData(nameof(ContentCaptureModeMatrix))]
    public async Task ModeMatrix_GenerationProtoAndSpan(ModeExpect expect)
    {
        await using var env = new ContentCaptureEnv(expect.Mode);
        const string title = "Sensitive conversation";
        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "anthropic", Name = "claude-sonnet-4-5" },
            ConversationTitle = title,
            SystemPrompt = "You are helpful.",
        });
        recorder.SetResult(MatrixFixtureGeneration());
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var gen = env.SingleGeneration();
        Assert.Equal(expect.Marker,
            gen.Metadata.Fields[Agento11yClient.MetadataKeyContentCaptureMode].StringValue);

        // Content fields: stripped only under MetadataOnly.
        AssertProtoContent("system_prompt", gen.SystemPrompt, "You are helpful.", expect.ProtoContentStripped);
        AssertProtoContent("input[0].text", gen.Input[0].Parts[0].Text, "What is the weather?", expect.ProtoContentStripped);
        AssertProtoContent("output[0].thinking", gen.Output[0].Parts[0].Thinking, "let me think about weather", expect.ProtoContentStripped);
        AssertProtoContent("output[0].tool_call.input_json", gen.Output[0].Parts[1].ToolCall.InputJson.ToStringUtf8(), "{\"city\":\"Paris\"}", expect.ProtoContentStripped);
        AssertProtoContent("output[0].text", gen.Output[0].Parts[2].Text, "It's 18C and sunny in Paris.", expect.ProtoContentStripped);
        AssertProtoContent("input[1].tool_result.content", gen.Input[1].Parts[0].ToolResult.Content, "sunny 18C", expect.ProtoContentStripped);
        AssertProtoContent("tools[0].description", gen.Tools[0].Description, "Get weather", expect.ProtoContentStripped);
        AssertProtoContent("tools[0].input_schema_json", gen.Tools[0].InputSchemaJson.ToStringUtf8(), "{\"type\":\"object\"}", expect.ProtoContentStripped);

        // Structural fields always preserved.
        Assert.Equal(2, gen.Input.Count);
        Assert.Equal("weather", gen.Output[0].Parts[1].ToolCall.Name);
        Assert.Equal(120L, gen.Usage.InputTokens);

        // Conversation title metadata mirror: present iff the proto keeps it.
        if (expect.ProtoContentStripped)
        {
            Assert.False(gen.Metadata.Fields.ContainsKey(Agento11yClient.SpanAttrConversationTitle));
        }
        else
        {
            Assert.Equal(title, gen.Metadata.Fields[Agento11yClient.SpanAttrConversationTitle].StringValue);
        }

        // Span path: title presence is what the mode advertises.
        var span = env.GenerationSpan();
        var spanTitle = span.GetTagItem(Agento11yClient.SpanAttrConversationTitle)?.ToString();
        if (expect.SpanTitlePresent)
        {
            Assert.Equal(title, spanTitle);
        }
        else
        {
            Assert.Null(spanTitle);
        }
    }

    [Theory]
    [MemberData(nameof(ContentCaptureModeMatrix))]
    public async Task ModeMatrix_GenerationCallError(ModeExpect expect)
    {
        await using var env = new ContentCaptureEnv(expect.Mode);
        var rawError = $"provider returned HTTP 400: blocked content '{LeakMarker}'";

        var recorder = env.Client.StartGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            AgentName = "agent-matrix-error",
        });
        recorder.SetCallError(new InvalidOperationException(rawError));
        recorder.SetResult(new Generation
        {
            Input = { Message.UserTextMessage("x") },
            Output = { Message.AssistantTextMessage("y") },
            Usage = new TokenUsage { InputTokens = 1, OutputTokens = 1 },
        });
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var gen = env.SingleGeneration();
        if (expect.ProtoCallErrorRaw)
        {
            Assert.Equal(rawError, gen.CallError);
            Assert.Equal(rawError, gen.Metadata.Fields["call_error"].StringValue);
        }
        else
        {
            Assert.NotEqual(rawError, gen.CallError);
            Assert.False(string.IsNullOrEmpty(gen.CallError));
            Assert.False(gen.Metadata.Fields.ContainsKey("call_error"));
        }

        var span = env.GenerationSpan();
        if (expect.SpanRawError)
        {
            Assert.Contains(LeakMarker, span.StatusDescription ?? string.Empty);
        }
        else
        {
            AssertSpanErrorRedacted(span, "provider_call_error");
        }
    }

    [Fact]
    public async Task Streaming_FullWithMetadataSpans_ProtoFull_SpanTitleAbsent()
    {
        // Streaming changes the span operation name to streamText but the
        // redaction logic is shared with non-streaming. Catches regressions
        // where the two paths drift apart.
        await using var env = new ContentCaptureEnv(ContentCaptureMode.FullWithMetadataSpans);
        const string title = "Sensitive streaming conversation";
        var recorder = env.Client.StartStreamingGeneration(new GenerationStart
        {
            Model = new ModelRef { Provider = "anthropic", Name = "claude-sonnet-4-5" },
            ConversationTitle = title,
            SystemPrompt = "Be helpful.",
        });
        recorder.SetResult(new Generation
        {
            SystemPrompt = "Be helpful.",
            Input = { Message.UserTextMessage("hello") },
            Output = { Message.AssistantTextMessage("hi") },
            Usage = new TokenUsage { InputTokens = 1, OutputTokens = 1 },
        });
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var gen = env.SingleGeneration();
        Assert.Equal("Be helpful.", gen.SystemPrompt);
        Assert.Equal("hello", gen.Input[0].Parts[0].Text);
        Assert.Equal(title, gen.Metadata.Fields[Agento11yClient.SpanAttrConversationTitle].StringValue);
        Assert.Equal("full_with_metadata_spans",
            gen.Metadata.Fields[Agento11yClient.MetadataKeyContentCaptureMode].StringValue);

        // Span uses streamText operation and still drops the title.
        var streamSpan = env.StreamingGenerationSpan();
        Assert.Null(streamSpan.GetTagItem(Agento11yClient.SpanAttrConversationTitle));
    }

    private static Generation MatrixFixtureGeneration()
    {
        return new Generation
        {
            Input =
            {
                Message.UserTextMessage("What is the weather?"),
                new Message
                {
                    Role = MessageRole.Tool,
                    Parts =
                    {
                        Part.ToolResultPart(new ToolResult
                        {
                            ToolCallId = "call_1",
                            Name = "weather",
                            Content = "sunny 18C",
                        }),
                    },
                },
            },
            Output =
            {
                new Message
                {
                    Role = MessageRole.Assistant,
                    Parts =
                    {
                        Part.ThinkingPart("let me think about weather"),
                        Part.ToolCallPart(new ToolCall
                        {
                            Id = "call_1",
                            Name = "weather",
                            InputJson = Encoding.UTF8.GetBytes("{\"city\":\"Paris\"}"),
                        }),
                        Part.TextPart("It's 18C and sunny in Paris."),
                    },
                },
            },
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
            Usage = new TokenUsage { InputTokens = 120, OutputTokens = 42 },
            StopReason = "end_turn",
        };
    }

    private static void AssertProtoContent(string field, string actual, string expected, bool expectStripped)
    {
        if (expectStripped)
        {
            Assert.True(string.IsNullOrEmpty(actual), $"{field} should be stripped, got {actual}");
        }
        else
        {
            Assert.Equal(expected, actual);
        }
    }

    // Tool span content omission applies to both stripped modes. The
    // proto/span split only matters for generations; tools have no proto
    // export, so MetadataOnly and FullWithMetadataSpans are equivalent for
    // the span path.
    [Theory]
    [InlineData(ContentCaptureMode.MetadataOnly)]
    [InlineData(ContentCaptureMode.FullWithMetadataSpans)]
    public async Task StrippedModes_ToolSpan_OmitsContentAttrs(ContentCaptureMode mode)
    {
        await using var env = new ContentCaptureEnv(mode);
#pragma warning disable CS0618 // IncludeContent is obsolete
        var recorder = env.Client.StartToolExecution(new ToolExecutionStart
        {
            ToolName = "weather",
            ToolCallId = "call_1",
            ConversationTitle = "Sensitive tool title",
            ToolDescription = "Get weather: free-form provider-supplied text",
            IncludeContent = true,
        });
#pragma warning restore CS0618
        recorder.SetResult(new ToolExecutionEnd
        {
            Arguments = new Dictionary<string, object?> { ["city"] = "Paris" },
            Result = new Dictionary<string, object?> { ["temp_c"] = 18 },
        });
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var span = env.ToolSpan();
        Assert.Null(span.GetTagItem("gen_ai.tool.call.arguments"));
        Assert.Null(span.GetTagItem("gen_ai.tool.call.result"));
        Assert.Null(span.GetTagItem(Agento11yClient.SpanAttrConversationTitle));
        Assert.Null(span.GetTagItem("gen_ai.tool.description"));
        // Identity attributes still emitted.
        Assert.Equal("weather", span.GetTagItem("gen_ai.tool.name")?.ToString());
    }

    // Tools have no proto export; under both stripped modes the raw provider
    // error must not echo on the span path.
    [Theory]
    [InlineData(ContentCaptureMode.MetadataOnly)]
    [InlineData(ContentCaptureMode.FullWithMetadataSpans)]
    public async Task StrippedModes_ToolSpan_RedactsCallError(ContentCaptureMode mode)
    {
        await using var env = new ContentCaptureEnv(mode);
        var rawError = $"provider returned HTTP 400: blocked content '{LeakMarker}'";

#pragma warning disable CS0618 // IncludeContent is obsolete
        var recorder = env.Client.StartToolExecution(new ToolExecutionStart
        {
            ToolName = "weather",
            ToolCallId = "call_1",
            IncludeContent = true,
        });
#pragma warning restore CS0618
        recorder.SetExecutionError(new InvalidOperationException(rawError));
        recorder.SetResult(new ToolExecutionEnd
        {
            Arguments = new Dictionary<string, object?> { ["city"] = "Paris" },
            Result = new Dictionary<string, object?> { ["temp_c"] = 18 },
        });
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        AssertSpanErrorRedacted(env.ToolSpan(), "tool_execution_error");
    }

    // Embedding span content omission applies to both stripped modes. The
    // proto/span split only matters for generations; embeddings have no
    // proto export, so MetadataOnly and FullWithMetadataSpans are equivalent
    // for the span path.
    [Theory]
    [InlineData(ContentCaptureMode.MetadataOnly)]
    [InlineData(ContentCaptureMode.FullWithMetadataSpans)]
    public async Task StrippedModes_EmbeddingSpan_OmitsInputTexts(ContentCaptureMode mode)
    {
        await using var env = new ContentCaptureEnv(
            mode,
            embeddingCapture: new EmbeddingCaptureConfig
            {
                CaptureInput = true,
                MaxInputItems = 5,
                MaxTextLength = 100,
            });

        var recorder = env.Client.StartEmbedding(new EmbeddingStart
        {
            Model = new ModelRef { Provider = "openai", Name = "text-embedding-3-small" },
        });
        recorder.SetResult(new EmbeddingResult
        {
            InputCount = 1,
            InputTokens = 10,
            InputTexts = ["sensitive input text"],
            ResponseModel = "text-embedding-3-small",
        });
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        var span = env.EmbeddingSpan();
        Assert.Null(span.GetTagItem("gen_ai.embeddings.input_texts"));
        // Non-content embedding fields remain.
        Assert.Equal(1, Convert.ToInt64(span.GetTagItem("gen_ai.embeddings.input_count")));
        Assert.Equal(10, Convert.ToInt64(span.GetTagItem("gen_ai.usage.input_tokens")));
        Assert.Equal("text-embedding-3-small", span.GetTagItem("gen_ai.response.model")?.ToString());
    }

    // Embedding provider call errors must not echo raw text on the span under
    // either stripped mode. Embeddings have no proto export, so the raw
    // provider error never escapes the span path.
    [Theory]
    [InlineData(ContentCaptureMode.MetadataOnly)]
    [InlineData(ContentCaptureMode.FullWithMetadataSpans)]
    public async Task StrippedModes_EmbeddingProviderCallError_RedactedOnSpan(ContentCaptureMode mode)
    {
        await using var env = new ContentCaptureEnv(
            mode,
            embeddingCapture: new EmbeddingCaptureConfig { CaptureInput = true });
        var rawError = $"provider returned HTTP 400: blocked content '{LeakMarker}'";

        var recorder = env.Client.StartEmbedding(new EmbeddingStart
        {
            Model = new ModelRef { Provider = "openai", Name = "text-embedding-3-small" },
        });
        recorder.SetCallError(new InvalidOperationException(rawError));
        recorder.SetResult(new EmbeddingResult
        {
            InputCount = 1,
            InputTexts = ["sensitive input text"],
        });
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        AssertSpanErrorRedacted(env.EmbeddingSpan(), "provider_call_error");
    }

    [Fact]
    public async Task FullWithMetadataSpans_ResolverHidesEmbeddingInputTexts()
    {
        await using var env = new ContentCaptureEnv(
            ContentCaptureMode.Full,
            resolver: _ => ContentCaptureMode.FullWithMetadataSpans,
            embeddingCapture: new EmbeddingCaptureConfig
            {
                CaptureInput = true,
                MaxInputItems = 5,
                MaxTextLength = 100,
            });

        var recorder = env.Client.StartEmbedding(new EmbeddingStart
        {
            Model = new ModelRef { Provider = "openai", Name = "text-embedding-3-small" },
        });
        recorder.SetResult(new EmbeddingResult
        {
            InputCount = 1,
            InputTexts = ["resolver-gated sensitive text"],
        });
        recorder.End();

        await env.ShutdownAsync(TestContext.Current.CancellationToken);

        Assert.Null(env.EmbeddingSpan().GetTagItem("gen_ai.embeddings.input_texts"));
    }

    [Fact]
    public async Task FullWithMetadataSpans_RatingPreservesComment()
    {
        // Spin up a real HTTP server and assert on the captured request body
        // — asserting on the caller-supplied `request.Comment` would pass for
        // every mode because the SDK strips on a clone, not in place.
        using var server = new RatingCaptureServer((_, _, _) =>
            (
                200,
                "application/json",
                Encoding.UTF8.GetBytes(
                    """
                    {
                      "rating":{"rating_id":"r1","conversation_id":"conv-1","rating":"CONVERSATION_RATING_VALUE_GOOD","created_at":"2026-02-13T12:00:00Z"},
                      "summary":{"total_count":1,"good_count":1,"bad_count":0,"latest_rating":"CONVERSATION_RATING_VALUE_GOOD","latest_rated_at":"2026-02-13T12:00:00Z","has_bad_rating":false}
                    }
                    """
                )
            )
        );

        await using var client = new Agento11yClient(new Agento11yClientConfig
        {
            ContentCapture = ContentCaptureMode.FullWithMetadataSpans,
            Api = new ApiConfig { Endpoint = $"http://127.0.0.1:{server.Port}" },
            GenerationExport = new GenerationExportConfig
            {
                Protocol = GenerationExportProtocol.Http,
                Endpoint = $"http://127.0.0.1:{server.Port}/api/v1/generations:export",
                BatchSize = 1,
                FlushInterval = TimeSpan.FromMinutes(10),
                MaxRetries = 0,
            },
        });

        await client.SubmitConversationRatingAsync(
            "conv-1",
            new SubmitConversationRatingRequest
            {
                RatingId = "r1",
                Rating = ConversationRatingValue.Good,
                Comment = "user-supplied free text",
            },
            TestContext.Current.CancellationToken);

        Assert.True(server.Requests.TryDequeue(out var captured));
        using var body = JsonDocument.Parse(captured.Body);
        Assert.Equal("user-supplied free text", body.RootElement.GetProperty("comment").GetString());
    }

    // Sentinel substring guaranteed not to appear in any error category
    // classifier output. If it leaks onto a span, the redaction is broken.
    private const string LeakMarker = "ignore previous instructions";

    private static void AssertSpanErrorRedacted(Activity span, string expectedErrorType)
    {
        Assert.NotNull(span.GetTagItem("exception.type"));
        Assert.Null(span.GetTagItem("exception.message"));
        Assert.Null(span.GetTagItem("exception.stacktrace"));
        Assert.Equal(ActivityStatusCode.Error, span.Status);
        Assert.DoesNotContain(LeakMarker, span.StatusDescription ?? string.Empty);
        Assert.Equal(expectedErrorType, span.GetTagItem(Agento11yClient.SpanAttrErrorType)?.ToString());
    }

    private static Generation MakeTestGeneration()
    {
        return new Generation
        {
            Id = "gen-1",
            ConversationId = "conv-1",
            AgentName = "test-agent",
            AgentVersion = "1.0",
            Mode = GenerationMode.Sync,
            Model = new ModelRef { Provider = "anthropic", Name = "claude-sonnet-4-5" },
            SystemPrompt = "You are helpful.",
            Input =
            {
                new Message
                {
                    Role = MessageRole.User,
                    Parts = { Part.TextPart("What is the weather?") },
                },
                new Message
                {
                    Role = MessageRole.Tool,
                    Parts =
                    {
                        Part.ToolResultPart(new ToolResult
                        {
                            ToolCallId = "call_1",
                            Name = "weather",
                            Content = "sunny 18C",
                            ContentJson = Encoding.UTF8.GetBytes("{\"temp\":18}"),
                        }),
                    },
                },
            },
            Output =
            {
                new Message
                {
                    Role = MessageRole.Assistant,
                    Parts =
                    {
                        Part.ThinkingPart("let me think about weather"),
                        Part.ToolCallPart(new ToolCall
                        {
                            Id = "call_1",
                            Name = "weather",
                            InputJson = Encoding.UTF8.GetBytes("{\"city\":\"Paris\"}"),
                        }),
                        Part.TextPart("It's 18C and sunny in Paris."),
                    },
                },
            },
            Tools =
            {
                new ToolDefinition
                {
                    Name = "weather",
                    Description = "Get weather info",
                    Type = "function",
                    InputSchemaJson = Encoding.UTF8.GetBytes("{\"type\":\"object\"}"),
                },
            },
            Usage = new TokenUsage { InputTokens = 120, OutputTokens = 42 },
            StopReason = "end_turn",
            CallError = "rate limit exceeded: prompt too long for model",
            Artifacts =
            {
                Artifact.JsonArtifact(ArtifactKind.Request, "request", new { ok = true }),
            },
            Metadata = new Dictionary<string, object?>(StringComparer.Ordinal)
            {
                ["agento11y.sdk.name"] = "sdk-dotnet",
                ["call_error"] = "rate limit exceeded: prompt too long for model",
            },
        };
    }

    private sealed class ContentCaptureEnv : IAsyncDisposable
    {
        private bool _shutdown;
        private readonly ActivityListener _activityListener;

        public GrpcIngestServer Ingest { get; }
        public Agento11yClient Client { get; }
        public ConcurrentQueue<Activity> Spans { get; } = new();

        public ContentCaptureEnv(
            ContentCaptureMode clientMode = ContentCaptureMode.Default,
            Func<IReadOnlyDictionary<string, object?>?, ContentCaptureMode>? resolver = null,
            EmbeddingCaptureConfig? embeddingCapture = null)
        {
            _activityListener = new ActivityListener
            {
                ShouldListenTo = source => source.Name == Agento11yClient.InstrumentationName,
                Sample = static (ref ActivityCreationOptions<ActivityContext> _) => ActivitySamplingResult.AllDataAndRecorded,
                ActivityStopped = activity => Spans.Enqueue(activity),
            };
            ActivitySource.AddActivityListener(_activityListener);

            Ingest = new GrpcIngestServer();
            Client = new Agento11yClient(new Agento11yClientConfig
            {
                ContentCapture = clientMode,
                ContentCaptureResolver = resolver,
                EmbeddingCapture = embeddingCapture ?? new EmbeddingCaptureConfig(),
                GenerationExport = new GenerationExportConfig
                {
                    Protocol = GenerationExportProtocol.Grpc,
                    Endpoint = $"127.0.0.1:{Ingest.Port}",
                    Insecure = true,
                    BatchSize = 1,
                    QueueSize = 10,
                    FlushInterval = TimeSpan.FromHours(1),
                    MaxRetries = 1,
                    InitialBackoff = TimeSpan.FromMilliseconds(1),
                    MaxBackoff = TimeSpan.FromMilliseconds(2),
                },
            });
        }

        public async Task ShutdownAsync(CancellationToken cancellationToken = default)
        {
            if (_shutdown) return;
            _shutdown = true;
            await Client.ShutdownAsync(cancellationToken);
            _activityListener.Dispose();
            Ingest.Dispose();
        }

        public Agento11yProto.Generation SingleGeneration()
        {
            Assert.Single(Ingest.Requests);
            Assert.Single(Ingest.Requests[0].Request.Generations);
            return Ingest.Requests[0].Request.Generations[0];
        }

        public Activity ToolSpan() => SingleSpan("execute_tool");

        public Activity GenerationSpan() => SingleSpan("generateText");

        public Activity StreamingGenerationSpan() => SingleSpan("streamText");

        public Activity EmbeddingSpan() => SingleSpan("embeddings");

        private Activity SingleSpan(string operationName)
        {
            var span = Spans
                .Where(a => a.GetTagItem("gen_ai.operation.name")?.ToString() == operationName)
                .LastOrDefault();
            Assert.NotNull(span);
            return span!;
        }

        public async ValueTask DisposeAsync()
        {
            await ShutdownAsync();
        }
    }
}
