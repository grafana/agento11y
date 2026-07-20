using Google.GenAI.Types;

namespace Grafana.Agento11y.Gemini;

public sealed class GeminiStreamSummary
{
    public List<GenerateContentResponse> Responses { get; } = [];

    public DateTimeOffset? FirstChunkAt { get; set; }
}
