using System.Reflection;
using System.Text;
using System.Text.Json;
using System.Text.Json.Serialization;
using Xunit;

namespace Grafana.Agento11y.Tests;

/// <summary>
/// Runs the shared fixtures in <c>redaction/fixtures/</c> through the .NET
/// engine. Every redaction engine loads the same files, so a fixture change
/// fails all of them at once instead of letting one SDK drift.
/// </summary>
public sealed class RedactionConformanceTests
{
    /// <summary>
    /// Resolves the repository root from assembly metadata. mise runs
    /// <c>dotnet test</c> from <c>dotnet/</c> and CI runs it from the repository
    /// root, so the current working directory cannot be trusted.
    /// </summary>
    internal static string FixturesDirectory()
    {
        var solutionRoot = typeof(RedactionConformanceTests).Assembly
            .GetCustomAttributes<AssemblyMetadataAttribute>()
            .First((p) => p.Key is "SolutionRoot")
            .Value;

        return Path.GetFullPath(Path.Combine(solutionRoot!, "..", "redaction", "fixtures"));
    }

    internal static T LoadFixture<T>(string name)
    {
        var path = Path.Combine(FixturesDirectory(), name);
        var loaded = JsonSerializer.Deserialize<T>(File.ReadAllText(path));
        Assert.NotNull(loaded);
        return loaded!;
    }

    public static TheoryData<StringCase> StringCases()
    {
        var data = new TheoryData<StringCase>();
        var fixtures = LoadFixture<StringFixtures>("strings.json");
        Assert.NotEmpty(fixtures.Cases);
        foreach (var stringCase in fixtures.Cases)
        {
            data.Add(stringCase);
        }

        return data;
    }

    [Theory]
    [MemberData(nameof(StringCases))]
    public void ConformanceRedactionStrings(StringCase testCase)
    {
        var actual = testCase.Mode switch
        {
            "full" => SecretRedactionSanitizer.RedactFull(testCase.Input, testCase.Emails),
            "light" => SecretRedactionSanitizer.RedactLight(testCase.Input, testCase.Emails),
            _ => throw new InvalidOperationException($"unknown mode {testCase.Mode}"),
        };

        Assert.Equal(testCase.Expected, actual);
    }

    public static TheoryData<GenerationCase> GenerationCases()
    {
        var data = new TheoryData<GenerationCase>();
        var fixtures = LoadFixture<GenerationFixtures>("generations.json");
        Assert.NotEmpty(fixtures.Cases);
        foreach (var generationCase in fixtures.Cases)
        {
            generationCase.Probe = fixtures.Probe;
            data.Add(generationCase);
        }

        return data;
    }

    [Theory]
    [MemberData(nameof(GenerationCases))]
    public void ConformanceRedactionGenerationSlots(GenerationCase testCase)
    {
        var sanitize = SecretRedactionSanitizer.Create(
            new SecretRedactionOptions
            {
                RedactInputMessages = testCase.RedactInputMessages,
                RedactEmailAddresses = testCase.RedactEmailAddresses,
            },
            (_) => null,
            null
        );

        var actual = SlotValues(sanitize(BuildProbeGeneration(testCase.Probe["input"])));

        var expected = testCase.Slots.ToDictionary((slot) => slot.Key, (slot) => testCase.Probe[slot.Value]);

        Assert.Equal(expected.Keys.OrderBy((k) => k), actual.Keys.OrderBy((k) => k));
        Assert.Equal(expected, actual);
    }

    /// <summary>Fills every slot in the matrix with the same probe.</summary>
    private static Generation BuildProbeGeneration(string probe)
    {
        List<Part> AssistantParts() =>
        [
            Part.TextPart(probe),
            Part.ThinkingPart(probe),
            Part.ToolCallPart(new ToolCall { Name = "bash", InputJson = Encoding.UTF8.GetBytes(probe) }),
        ];

        List<Part> ToolParts() =>
        [
            Part.TextPart(probe),
            Part.ToolResultPart(
                new ToolResult
                {
                    Name = "bash",
                    Content = probe,
                    ContentJson = Encoding.UTF8.GetBytes(probe),
                }
            ),
        ];

        return new Generation
        {
            Id = "gen-conformance",
            Model = new ModelRef { Provider = "openai", Name = "gpt-5" },
            SystemPrompt = probe,
            ConversationTitle = probe,
            CallError = probe,
            Input =
            [
                new Message { Role = MessageRole.User, Parts = [Part.TextPart(probe)] },
                new Message { Role = MessageRole.Assistant, Parts = AssistantParts() },
                new Message { Role = MessageRole.Tool, Parts = ToolParts() },
            ],
            Output =
            [
                new Message { Role = MessageRole.Assistant, Parts = AssistantParts() },
                new Message { Role = MessageRole.Tool, Parts = ToolParts() },
            ],
        };
    }

    private static Dictionary<string, string> SlotValues(Generation generation)
    {
        return new Dictionary<string, string>
        {
            ["systemPrompt"] = generation.SystemPrompt,
            ["conversationTitle"] = generation.ConversationTitle,
            ["callError"] = generation.CallError,
            ["input.user.text"] = generation.Input[0].Parts[0].Text,
            ["input.assistant.text"] = generation.Input[1].Parts[0].Text,
            ["input.assistant.thinking"] = generation.Input[1].Parts[1].Thinking,
            ["input.assistant.toolCallInputJson"] = Encoding.UTF8.GetString(
                generation.Input[1].Parts[2].ToolCall!.InputJson
            ),
            ["input.tool.text"] = generation.Input[2].Parts[0].Text,
            ["input.tool.toolResultContent"] = generation.Input[2].Parts[1].ToolResult!.Content,
            ["input.tool.toolResultContentJson"] = Encoding.UTF8.GetString(
                generation.Input[2].Parts[1].ToolResult!.ContentJson
            ),
            ["output.assistant.text"] = generation.Output[0].Parts[0].Text,
            ["output.assistant.thinking"] = generation.Output[0].Parts[1].Thinking,
            ["output.assistant.toolCallInputJson"] = Encoding.UTF8.GetString(
                generation.Output[0].Parts[2].ToolCall!.InputJson
            ),
            ["output.tool.text"] = generation.Output[1].Parts[0].Text,
            ["output.tool.toolResultContent"] = generation.Output[1].Parts[1].ToolResult!.Content,
            ["output.tool.toolResultContentJson"] = Encoding.UTF8.GetString(
                generation.Output[1].Parts[1].ToolResult!.ContentJson
            ),
        };
    }

    public sealed class GenerationFixtures
    {
        [JsonPropertyName("probe")]
        public Dictionary<string, string> Probe { get; set; } = [];

        [JsonPropertyName("cases")]
        public List<GenerationCase> Cases { get; set; } = [];
    }

    public sealed class GenerationCase
    {
        [JsonPropertyName("id")]
        public string Id { get; set; } = string.Empty;

        [JsonPropertyName("redactInputMessages")]
        public bool RedactInputMessages { get; set; }

        [JsonPropertyName("redactEmailAddresses")]
        public bool RedactEmailAddresses { get; set; }

        [JsonPropertyName("slots")]
        public Dictionary<string, string> Slots { get; set; } = [];

        [JsonIgnore]
        public Dictionary<string, string> Probe { get; set; } = [];

        public override string ToString() => Id;
    }

    public sealed class StringFixtures
    {
        [JsonPropertyName("cases")]
        public List<StringCase> Cases { get; set; } = [];
    }

    public sealed class StringCase
    {
        [JsonPropertyName("id")]
        public string Id { get; set; } = string.Empty;

        [JsonPropertyName("mode")]
        public string Mode { get; set; } = string.Empty;

        [JsonPropertyName("emails")]
        public bool Emails { get; set; }

        [JsonPropertyName("input")]
        public string Input { get; set; } = string.Empty;

        [JsonPropertyName("expected")]
        public string Expected { get; set; } = string.Empty;

        public override string ToString() => Id;
    }
}
