using Xunit;

namespace Grafana.Sigil.Tests;

public sealed class ValidationTests
{
    [Fact]
    public void ValidateGeneration_RolePartCompatibility()
    {
        var baseGeneration = new Generation
        {
            Model = new ModelRef
            {
                Provider = "anthropic",
                Name = "claude-sonnet-4-5",
            },
            Input =
            {
                new Message
                {
                    Role = MessageRole.Assistant,
                    Parts =
                    {
                        Part.TextPart("ok"),
                    },
                },
            },
        };

        var invalidToolCall = Clone(baseGeneration);
        invalidToolCall.Input.Add(new Message
        {
            Role = MessageRole.User,
            Parts =
            {
                Part.ToolCallPart(new ToolCall { Name = "weather" }),
            },
        });
        Assert.Throws<ArgumentException>(() => GenerationValidator.Validate(invalidToolCall));

        var invalidToolResult = Clone(baseGeneration);
        invalidToolResult.Input.Add(new Message
        {
            Role = MessageRole.Assistant,
            Parts =
            {
                Part.ToolResultPart(new ToolResult { ToolCallId = "toolu_1", Content = "sunny" }),
            },
        });
        Assert.Throws<ArgumentException>(() => GenerationValidator.Validate(invalidToolResult));

        var invalidThinking = Clone(baseGeneration);
        invalidThinking.Output.Add(new Message
        {
            Role = MessageRole.User,
            Parts =
            {
                Part.ThinkingPart("private"),
            },
        });

        var error = Assert.Throws<ArgumentException>(() => GenerationValidator.Validate(invalidThinking));
        Assert.Contains("generation.output[0]", error.Message);
    }

    [Fact]
    public void ValidateGeneration_ArtifactRequiresPayloadOrRecordId()
    {
        var generation = new Generation
        {
            Model = new ModelRef
            {
                Provider = "openai",
                Name = "gpt-5",
            },
            Input =
            {
                Message.UserTextMessage("hello"),
            },
            Output =
            {
                Message.AssistantTextMessage("hi"),
            },
            Artifacts =
            {
                new Artifact
                {
                    Kind = ArtifactKind.Request,
                },
            },
        };

        Assert.Throws<ArgumentException>(() => GenerationValidator.Validate(generation));

        generation.Artifacts[0].RecordId = "rec-1";
        GenerationValidator.Validate(generation);
    }

    [Fact]
    public void ValidateGeneration_AllowsConversationAndResponseIdentityFields()
    {
        var generation = new Generation
        {
            ConversationId = "conv-1",
            Model = new ModelRef
            {
                Provider = "anthropic",
                Name = "claude-sonnet-4-5",
            },
            ResponseId = "resp-1",
            ResponseModel = "claude-sonnet-4-5-20260201",
            Input =
            {
                Message.UserTextMessage("hello"),
            },
            Output =
            {
                Message.AssistantTextMessage("hi"),
            },
        };

        GenerationValidator.Validate(generation);
    }

    [Fact]
    public void ValidateEmbeddingStart_RequiresModelFields()
    {
        var start = new EmbeddingStart
        {
            Model = new ModelRef
            {
                Provider = string.Empty,
                Name = "text-embedding-3-small",
            },
        };
        Assert.Throws<ArgumentException>(() => GenerationValidator.ValidateEmbeddingStart(start));

        start.Model.Provider = "openai";
        start.Model.Name = string.Empty;
        Assert.Throws<ArgumentException>(() => GenerationValidator.ValidateEmbeddingStart(start));
    }

    [Fact]
    public void ValidateEmbeddingResult_RejectsNegativeCounts()
    {
        Assert.Throws<ArgumentException>(() => GenerationValidator.ValidateEmbeddingResult(new EmbeddingResult
        {
            InputCount = -1,
        }));

        Assert.Throws<ArgumentException>(() => GenerationValidator.ValidateEmbeddingResult(new EmbeddingResult
        {
            InputTokens = -1,
        }));
    }

    private static Generation Clone(Generation generation)
    {
        return System.Text.Json.JsonSerializer.Deserialize<Generation>(
            System.Text.Json.JsonSerializer.Serialize(generation)
        )!;
    }
}
