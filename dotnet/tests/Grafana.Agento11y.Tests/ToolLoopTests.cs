using System.Text;
using System.Text.Json;
using System.Threading.Tasks;
using Xunit;

namespace Grafana.Sigil.Tests;

public sealed class ToolLoopTests
{
    [Fact]
    public async Task ExecuteToolCalls_HappyPath_TwoTools()
    {
        await using var client = new SigilClient(TestHelpers.TestConfig(new CapturingGenerationExporter()));

        var messages = new List<Message>
        {
            new()
            {
                Role = MessageRole.Assistant,
                Parts =
                [
                    Part.ToolCallPart(
                        new ToolCall
                        {
                            Id = "c1",
                            Name = "weather",
                            InputJson = Encoding.UTF8.GetBytes("""{"city":"Paris"}"""),
                        }
                    ),
                    Part.ToolCallPart(
                        new ToolCall
                        {
                            Id = "c2",
                            Name = "math",
                            InputJson = Encoding.UTF8.GetBytes("""{"a":1,"b":2}"""),
                        }
                    ),
                ],
            },
        };

        var outMsgs = client.ExecuteToolCalls(
            messages,
            (name, args) =>
            {
                if (name == "weather")
                {
                    return new Dictionary<string, object?> { ["temp_c"] = 18 };
                }

                return args;
            },
            new ExecuteToolCallsOptions
            {
                ConversationId = "conv-loop",
                AgentName = "agent-x",
                AgentVersion = "1.0.0",
                RequestModel = "gpt-test",
                RequestProvider = "openai",
            }
        );

        Assert.Equal(2, outMsgs.Count);
        Assert.Equal(MessageRole.Tool, outMsgs[0].Role);
        Assert.Equal("weather", outMsgs[0].Name);
        var tr0 = outMsgs[0].Parts[0].ToolResult!;
        Assert.Equal("c1", tr0.ToolCallId);
        Assert.Equal("weather", tr0.Name);
        using (var doc = JsonDocument.Parse(tr0.ContentJson))
        {
            Assert.Equal(18, doc.RootElement.GetProperty("temp_c").GetInt32());
        }

        var tr1 = outMsgs[1].Parts[0].ToolResult!;
        Assert.Equal("c2", tr1.ToolCallId);
        var roundTrip = JsonSerializer.Deserialize<Dictionary<string, JsonElement>>(tr1.ContentJson);
        Assert.NotNull(roundTrip);
        Assert.Equal(1, roundTrip["a"].GetInt32());
    }

    [Fact]
    public async Task ExecuteToolCalls_ExecutorThrows_MarksError()
    {
        await using var client = new SigilClient(TestHelpers.TestConfig(new CapturingGenerationExporter()));
        var messages = new List<Message>
        {
            new()
            {
                Role = MessageRole.Assistant,
                Parts = [Part.ToolCallPart(new ToolCall { Id = "c1", Name = "boom", InputJson = Encoding.UTF8.GetBytes("{}") })],
            },
        };

        var outMsgs = client.ExecuteToolCalls(
            messages,
            (_, _) => throw new InvalidOperationException("tool failed"),
            new ExecuteToolCallsOptions()
        );

        Assert.Single(outMsgs);
        var tr = outMsgs[0].Parts[0].ToolResult!;
        Assert.True(tr.IsError);
        Assert.Contains("tool failed", tr.Content, StringComparison.Ordinal);
    }

    [Fact]
    public async Task ExecuteToolCalls_NoToolParts_ReturnsEmpty()
    {
        await using var client = new SigilClient(TestHelpers.TestConfig(new CapturingGenerationExporter()));
        var messages = new List<Message>
        {
            new() { Role = MessageRole.Assistant, Parts = [Part.TextPart("hi")] },
        };
        var outMsgs = client.ExecuteToolCalls(messages, (_, _) => null);
        Assert.Empty(outMsgs);
    }

    [Fact]
    public async Task ExecuteToolCalls_SingleTool()
    {
        await using var client = new SigilClient(TestHelpers.TestConfig(new CapturingGenerationExporter()));
        var messages = new List<Message>
        {
            new()
            {
                Role = MessageRole.Assistant,
                Parts =
                [
                    Part.ToolCallPart(
                        new ToolCall { Id = "id1", Name = "echo", InputJson = Encoding.UTF8.GetBytes("""{"x":1}""") }
                    ),
                ],
            },
        };
        var outMsgs = client.ExecuteToolCalls(messages, (_, args) => args);
        Assert.Single(outMsgs);
        Assert.Equal("id1", outMsgs[0].Parts[0].ToolResult!.ToolCallId);
    }

    [Fact]
    public async Task ExecuteToolCalls_NullMessages_ReturnsEmpty()
    {
        await using var client = new SigilClient(TestHelpers.TestConfig(new CapturingGenerationExporter()));
        var outMsgs = client.ExecuteToolCalls(null, (_, _) => null);
        Assert.Empty(outMsgs);
    }

    [Fact]
    public async Task ExecuteToolCalls_SkipsBlankToolName()
    {
        await using var client = new SigilClient(TestHelpers.TestConfig(new CapturingGenerationExporter()));
        var messages = new List<Message>
        {
            new()
            {
                Role = MessageRole.Assistant,
                Parts = [Part.ToolCallPart(new ToolCall { Id = "x", Name = "   ", InputJson = Encoding.UTF8.GetBytes("{}") })],
            },
        };
        var outMsgs = client.ExecuteToolCalls(messages, (_, _) => 1);
        Assert.Empty(outMsgs);
    }

    [Fact]
    public async Task ExecuteToolCalls_NullExecutor_Throws()
    {
        await using var client = new SigilClient(TestHelpers.TestConfig(new CapturingGenerationExporter()));
        Assert.Throws<ArgumentNullException>(() => client.ExecuteToolCalls([], null!));
    }
}
