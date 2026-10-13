using System.Text.Json;

namespace Grafana.Agento11y;

/// <summary>Maps provider-reported modality partitions without inferring media tokens.</summary>
public static class ModalityUsage
{
    public static ModalityTokenCounts? Partition(JsonElement raw, long total, long thinking = 0, bool openai = false)
    {
        if (raw.ValueKind is JsonValueKind.Undefined or JsonValueKind.Null) { return null; }
        var tokens = new Dictionary<string, long>();
        if (openai && raw.ValueKind == JsonValueKind.Object)
        {
            foreach (var modality in new[] { "text", "image", "audio", "video" })
            {
                if (raw.TryGetProperty(modality + "_tokens", out var value) && value.ValueKind == JsonValueKind.Number && value.TryGetInt64(out var count)) { tokens[modality] = count; }
            }
        }
        else if (raw.ValueKind == JsonValueKind.Array)
        {
            foreach (var row in raw.EnumerateArray())
            {
                var modality = Property(row, "modality").ToString().ToLowerInvariant();
                var value = Property(row, "tokenCount", "token_count", "tokens");
                if (value.ValueKind == JsonValueKind.Number && value.TryGetInt64(out var count)) { tokens[modality] = (tokens.TryGetValue(modality, out var existing) ? existing : 0) + count; }
            }
        }
        if (thinking != 0) { tokens["text"] = (tokens.TryGetValue("text", out var text) ? text : 0) + thinking; }
        return new ModalityTokenCounts { Tokens = tokens, Complete = tokens.Values.Sum() == total };
    }

    public static JsonElement Property(JsonElement raw, params string[] names)
    {
        foreach (var name in names) { if (raw.ValueKind == JsonValueKind.Object && raw.TryGetProperty(name, out var value)) { return value; } }
        return default;
    }
}
