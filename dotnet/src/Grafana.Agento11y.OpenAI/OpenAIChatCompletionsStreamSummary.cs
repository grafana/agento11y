using OpenAI.Chat;

namespace Grafana.Agento11y.OpenAI;

public sealed class OpenAIChatCompletionsStreamSummary
{
    public List<StreamingChatCompletionUpdate> Updates { get; } = [];

    public ChatCompletion? FinalResponse { get; set; }

    public DateTimeOffset? FirstChunkAt { get; set; }
}
