using System.Text;
using System.Text.Json;

namespace Grafana.Sigil.OpenAI;

internal static class OpenAIJsonHelpers
{
    public static byte[] ParseJsonOrString(string? value)
    {
        if (string.IsNullOrWhiteSpace(value))
        {
            return [];
        }

        try
        {
            using var doc = JsonDocument.Parse(value);
            return Encoding.UTF8.GetBytes(doc.RootElement.GetRawText());
        }
        catch
        {
            return JsonSerializer.SerializeToUtf8Bytes(value);
        }
    }

    public static byte[] ToBytes(BinaryData? data)
    {
        if (data == null)
        {
            return [];
        }

        return data.ToArray();
    }

    public static string NormalizeStopReason(string? reason)
    {
        var normalized = (reason ?? string.Empty).Trim();
        if (normalized.Length == 0)
        {
            return string.Empty;
        }

        return normalized switch
        {
            "Stop" or "stop" => "stop",
            "Length" or "length" => "length",
            "ContentFilter" or "content_filter" or "contentfilter" => "content_filter",
            "ToolCalls" or "tool_calls" or "toolcalls" => "tool_calls",
            "FunctionCall" or "function_call" or "functioncall" => "function_call",
            _ => normalized,
        };
    }

    public static string MergeSystemPrompt(List<string> chunks)
    {
        return string.Join("\n\n", chunks.Where(chunk => !string.IsNullOrWhiteSpace(chunk)));
    }
}
