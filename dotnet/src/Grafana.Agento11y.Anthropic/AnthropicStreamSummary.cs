using Anthropic.Models.Messages;
using AnthropicMessage = Anthropic.Models.Messages.Message;

namespace Grafana.Agento11y.Anthropic;

public sealed class AnthropicStreamSummary
{
    public List<RawMessageStreamEvent> Events { get; } = [];

    public AnthropicMessage? FinalMessage { get; set; }

    public DateTimeOffset? FirstChunkAt { get; set; }
}
