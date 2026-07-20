using Google.GenAI.Types;

namespace Grafana.Sigil.Gemini;

public sealed class GeminiStreamSummary
{
    public List<GenerateContentResponse> Responses { get; } = [];

    public DateTimeOffset? FirstChunkAt { get; set; }
}
