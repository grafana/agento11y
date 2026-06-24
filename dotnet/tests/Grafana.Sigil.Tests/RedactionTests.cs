using System.Text;
using Xunit;

namespace Grafana.Sigil.Tests;

public sealed class RedactionTests
{
    [Fact]
    public void SecretRedactionSanitizer_RedactsAssistantAndToolContentByDefault()
    {
        var sanitizer = SecretRedactionSanitizer.Create();
        var secretToken = "glc_abcdefghijklmnopqrstuvwxyz1234";
        var envSecret = "DATABASE_PASSWORD=hunter2secret123";
        var bearerToken = new string('a', 30);
        var historicBearer = new string('h', 30);
        var historicEnv = "API_TOKEN=historicvalue9876";

        var generation = sanitizer(new Generation
        {
            Input =
            {
                Message.UserTextMessage("user pasted " + secretToken),
                new Message
                {
                    Role = MessageRole.Assistant,
                    Parts =
                    {
                        Part.TextPart("previous turn mentioned " + secretToken),
                        Part.ToolCallPart(new ToolCall
                        {
                            Id = "prev-call",
                            Name = "bash",
                            InputJson = Encoding.UTF8.GetBytes("{\"header\":\"Bearer " + historicBearer + "\"}"),
                        }),
                    },
                },
                new Message
                {
                    Role = MessageRole.Tool,
                    Parts =
                    {
                        Part.ToolResultPart(new ToolResult
                        {
                            ToolCallId = "prev-call",
                            Name = "bash",
                            Content = "previous output " + historicEnv,
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
                        Part.TextPart("assistant saw " + secretToken),
                        Part.ThinkingPart("thinking about " + secretToken),
                        Part.ToolCallPart(new ToolCall
                        {
                            Id = "call-1",
                            Name = "bash",
                            InputJson = Encoding.UTF8.GetBytes("{\"header\":\"Bearer " + bearerToken + "\",\"env\":\"" + envSecret + "\"}"),
                        }),
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
                            Name = "bash",
                            Content = "output " + envSecret,
                        }),
                    },
                },
            },
        });

        Assert.Contains(secretToken, generation.Input[0].Parts[0].Text);
        Assert.DoesNotContain(secretToken, generation.Input[1].Parts[0].Text);
        Assert.DoesNotContain("Bearer " + historicBearer, Encoding.UTF8.GetString(generation.Input[1].Parts[1].ToolCall!.InputJson));
        Assert.DoesNotContain("historicvalue9876", generation.Input[2].Parts[0].ToolResult!.Content);
        Assert.Contains("[REDACTED:grafana-cloud-token]", generation.Output[0].Parts[0].Text);
        Assert.DoesNotContain(secretToken, generation.Output[0].Parts[1].Thinking);
        Assert.DoesNotContain("hunter2secret123", Encoding.UTF8.GetString(generation.Output[0].Parts[2].ToolCall!.InputJson));
        Assert.DoesNotContain("Bearer " + bearerToken, Encoding.UTF8.GetString(generation.Output[0].Parts[2].ToolCall!.InputJson));
        Assert.Contains("[REDACTED:env-secret-value]", generation.Output[1].Parts[0].ToolResult!.Content);
    }

    [Fact]
    public void SecretRedactionSanitizer_InputRedactionRespectsOptionAndEnv()
    {
        var secretToken = "glc_abcdefghijklmnopqrstuvwxyz1234";

        var defaultSanitized = SecretRedactionSanitizer.Create()(GenerationWithUserInput(secretToken));
        Assert.Contains(secretToken, defaultSanitized.Input[0].Parts[0].Text);

        var optionSanitized = SecretRedactionSanitizer.Create(new SecretRedactionOptions
        {
            RedactInputMessages = true,
        })(GenerationWithUserInput(secretToken));
        Assert.DoesNotContain(secretToken, optionSanitized.Input[0].Parts[0].Text);

        var envSanitized = SecretRedactionSanitizer.Create(
            null,
            key => key == "SIGIL_REDACT_INPUT_MESSAGES" ? "true" : null,
            null
        )(GenerationWithUserInput(secretToken));
        Assert.DoesNotContain(secretToken, envSanitized.Input[0].Parts[0].Text);

        var explicitFalse = SecretRedactionSanitizer.Create(
            new SecretRedactionOptions { RedactInputMessages = false },
            key => key == "SIGIL_REDACT_INPUT_MESSAGES" ? "true" : null,
            null
        )(GenerationWithUserInput(secretToken));
        Assert.Contains(secretToken, explicitFalse.Input[0].Parts[0].Text);
    }

    [Fact]
    public void SecretRedactionSanitizer_EmailToggle()
    {
        const string text = "send mail to example@example.com";

        var defaultSanitized = SecretRedactionSanitizer.Create()(new Generation
        {
            Output = { Message.AssistantTextMessage(text) },
        });
        Assert.Contains("[REDACTED:email]", defaultSanitized.Output[0].Parts[0].Text);
        Assert.DoesNotContain("example@example.com", defaultSanitized.Output[0].Parts[0].Text);

        var disabled = SecretRedactionSanitizer.Create(new SecretRedactionOptions
        {
            RedactEmailAddresses = false,
        })(new Generation
        {
            Output = { Message.AssistantTextMessage(text) },
        });
        Assert.Equal(text, disabled.Output[0].Parts[0].Text);
    }

    [Fact]
    public void SecretRedactionSanitizer_RedactsTier1Patterns()
    {
        var sanitizer = SecretRedactionSanitizer.Create();
        var cases = new (string Id, string Value)[]
        {
            ("grafana-cloud-token", "glc_abcdefghijklmnopqrstuvwxyz1234"),
            ("grafana-service-account-token", "glsa_abcdefghijklmnopqrstuvwxyz1234"),
            ("aws-access-token", "AKIAIOSFODNN7EXAMPLE"),
            ("github-pat", "ghp_" + new string('a', 36)),
            ("github-oauth", "gho_" + new string('a', 36)),
            ("github-app-token", "ghs_" + new string('a', 36)),
            ("github-fine-grained-pat", "github_pat_" + new string('a', 82)),
            ("anthropic-api-key", "sk-ant-api03-" + new string('a', 93) + "AA"),
            ("anthropic-admin-key", "sk-ant-admin01-" + new string('a', 93) + "AA"),
            ("openai-api-key", "sk-" + new string('a', 20) + "T3BlbkFJ" + new string('b', 20)),
            ("openai-project-key", "sk-proj-" + new string('a', 40)),
            ("openai-svcacct-key", "sk-svcacct-" + new string('a', 40)),
            ("gcp-api-key", "AIza" + new string('a', 35)),
            ("private-key", "-----BEGIN RSA PRIVATE KEY-----\nfake-test-body\n-----END RSA PRIVATE KEY-----"),
            ("connection-string", "postgres://user:password@db.example.com:5432/app"),
            ("bearer-token", "Bearer " + new string('a', 30)),
            ("slack-token", "xoxb-" + new string('a', 20)),
            ("stripe-key", "sk_live_" + new string('a', 24)),
            ("sendgrid-api-key", "SG." + new string('a', 22) + "." + new string('b', 43)),
            ("twilio-api-key", "SK" + new string('a', 32)),
            ("npm-token", "npm_" + new string('a', 36)),
            ("pypi-token", "pypi-" + new string('a', 50)),
        };

        foreach (var (id, value) in cases)
        {
            var generation = sanitizer(new Generation
            {
                Output =
                {
                    new Message
                    {
                        Role = MessageRole.Tool,
                        Parts =
                        {
                            Part.ToolResultPart(new ToolResult { Content = "prefix " + value + " suffix" }),
                        },
                    },
                },
            });

            var got = generation.Output[0].Parts[0].ToolResult!.Content;
            Assert.Contains("[REDACTED:" + id + "]", got);
            Assert.DoesNotContain(value, got);
        }
    }

    [Fact]
    public async Task GenerationRecorder_AppliesSanitizerBeforeExport()
    {
        var secret = "glc_abcdefghijklmnopqrstuvwxyz1234";
        var exporter = new CapturingGenerationExporter();
        var config = TestHelpers.TestConfig(exporter);
        config.GenerationSanitizer = SecretRedactionSanitizer.Create();

        await using var client = new SigilClient(config);
        var recorder = client.StartGeneration(new GenerationStart
        {
            Id = "gen-redact",
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            ConversationTitle = "title " + secret,
            SystemPrompt = "SYSTEM_TOKEN=" + secret,
        });
        recorder.SetCallError(new InvalidOperationException("provider failed with " + secret));
        recorder.SetResult(new Generation
        {
            Output = { Message.AssistantTextMessage("assistant saw " + secret) },
            Usage = new TokenUsage { InputTokens = 1, OutputTokens = 1 },
        });
        recorder.End();

        var generation = recorder.LastGeneration!;
        Assert.DoesNotContain(secret, generation.ConversationTitle);
        Assert.DoesNotContain(secret, generation.SystemPrompt);
        Assert.DoesNotContain(secret, generation.CallError);
        Assert.DoesNotContain(secret, generation.Output[0].Parts[0].Text);
        Assert.Equal(generation.ConversationTitle, generation.Metadata[SigilClient.SpanAttrConversationTitle]?.ToString());
        Assert.Equal(generation.CallError, generation.Metadata["call_error"]?.ToString());
    }

    [Fact]
    public async Task GenerationRecorder_SanitizerExceptionDowngradesToMetadataOnly()
    {
        var logs = new List<string>();
        var exporter = new CapturingGenerationExporter();
        var config = TestHelpers.TestConfig(exporter);
        config.ContentCapture = ContentCaptureMode.Full;
        config.Logger = logs.Add;
        config.GenerationSanitizer = _ => throw new InvalidOperationException("boom");

        await using var client = new SigilClient(config);
        var recorder = client.StartGeneration(new GenerationStart
        {
            Id = "gen-sanitizer-fail",
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            ConversationTitle = "sensitive title",
            SystemPrompt = "sensitive system prompt",
        });
        recorder.SetResult(new Generation
        {
            Input = { Message.UserTextMessage("sensitive input") },
            Output = { Message.AssistantTextMessage("sensitive output") },
            Usage = new TokenUsage { InputTokens = 1, OutputTokens = 1 },
        });
        recorder.End();

        var generation = recorder.LastGeneration!;
        Assert.Null(recorder.Error);
        Assert.Equal("metadata_only", generation.Metadata[SigilClient.MetadataKeyContentCaptureMode]?.ToString());
        Assert.Equal(string.Empty, generation.ConversationTitle);
        Assert.Equal(string.Empty, generation.SystemPrompt);
        Assert.Equal(string.Empty, generation.Input[0].Parts[0].Text);
        Assert.Equal(string.Empty, generation.Output[0].Parts[0].Text);
        Assert.Contains(logs, entry => entry.Contains("sigil: generation sanitization failed, falling back to metadata_only"));
    }

    [Fact]
    public async Task GenerationRecorder_SkipsSanitizerInMetadataOnlyMode()
    {
        var calls = 0;
        var exporter = new CapturingGenerationExporter();
        var config = TestHelpers.TestConfig(exporter);
        config.ContentCapture = ContentCaptureMode.MetadataOnly;
        config.GenerationSanitizer = generation =>
        {
            calls++;
            return generation;
        };

        await using var client = new SigilClient(config);
        var recorder = client.StartGeneration(new GenerationStart
        {
            Id = "gen-metadata-only",
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
        });
        recorder.SetResult(new Generation
        {
            Output = { Message.AssistantTextMessage("ok") },
            Usage = new TokenUsage { InputTokens = 1, OutputTokens = 1 },
        });
        recorder.End();

        Assert.Equal(0, calls);
    }

    private static Generation GenerationWithUserInput(string secret)
    {
        return new Generation
        {
            Input = { Message.UserTextMessage("user pasted " + secret) },
        };
    }
}
