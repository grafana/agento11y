namespace Grafana.Sigil.OpenAI;

public sealed record OpenAISigilOptions
{
    public string ProviderName { get; init; } = "openai";
    public string ModelName { get; init; } = string.Empty;
    public string ConversationId { get; init; } = string.Empty;
    public string AgentName { get; init; } = string.Empty;
    public string AgentVersion { get; init; } = string.Empty;
    public IReadOnlyDictionary<string, string> Tags { get; init; } = new Dictionary<string, string>();
    public IReadOnlyDictionary<string, object?> Metadata { get; init; } = new Dictionary<string, object?>();

    public bool IncludeRequestArtifact { get; init; }
    public bool IncludeResponseArtifact { get; init; }
    public bool IncludeToolsArtifact { get; init; }
    public bool IncludeEventsArtifact { get; init; }

    public OpenAISigilOptions WithRawArtifacts()
    {
        return this with
        {
            IncludeRequestArtifact = true,
            IncludeResponseArtifact = true,
            IncludeToolsArtifact = true,
            IncludeEventsArtifact = true,
        };
    }
}
