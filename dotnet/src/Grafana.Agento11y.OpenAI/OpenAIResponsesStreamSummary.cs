using global::OpenAI.Responses;

namespace Grafana.Agento11y.OpenAI;

public sealed class OpenAIResponsesStreamSummary
{
    public List<StreamingResponseUpdate> Events { get; } = [];

    public ResponseResult? FinalResponse { get; set; }

    public DateTimeOffset? FirstChunkAt { get; set; }
}
