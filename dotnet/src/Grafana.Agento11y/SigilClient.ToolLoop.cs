using System.Text;
using System.Text.Json;

namespace Grafana.Sigil;

public sealed partial class SigilClient
{
    /// <summary>
    /// Walks assistant tool-call parts in <paramref name="messages"/>, records <c>execute_tool</c> spans,
    /// and returns tool-role messages with tool-result parts for the next model turn.
    /// </summary>
    public List<Message> ExecuteToolCalls(
        IEnumerable<Message>? messages,
        Func<string, object?, object?> executor,
        ExecuteToolCallsOptions? options = null
    )
    {
        EnsureNotShutdown();
        if (executor is null)
        {
            throw new ArgumentNullException(nameof(executor));
        }

        var opts = options ?? new ExecuteToolCallsOptions();
        var toolType = string.IsNullOrWhiteSpace(opts.ToolType) ? "function" : opts.ToolType.Trim();
        var list = new List<Message>();

        foreach (var msg in messages ?? Array.Empty<Message>())
        {
            foreach (var part in msg.Parts)
            {
                if (part.Kind != PartKind.ToolCall || part.ToolCall is null)
                {
                    continue;
                }

                var name = (part.ToolCall.Name ?? string.Empty).Trim();
                if (name.Length == 0)
                {
                    continue;
                }

                var callId = (part.ToolCall.Id ?? string.Empty).Trim();
                var argsObj = ParseToolCallArguments(part.ToolCall.InputJson);

                var rec = StartToolExecution(
                    new ToolExecutionStart
                    {
                        ToolName = name,
                        ToolCallId = callId,
                        ToolType = toolType,
                        ConversationId = opts.ConversationId,
                        ConversationTitle = opts.ConversationTitle,
                        AgentName = opts.AgentName,
                        AgentVersion = opts.AgentVersion,
                        RequestModel = opts.RequestModel,
                        RequestProvider = opts.RequestProvider,
                        ContentCapture = opts.ContentCapture,
                    }
                );
                try
                {
                    var result = executor(name, argsObj);
                    rec.SetResult(new ToolExecutionEnd { Arguments = argsObj, Result = result });
                    list.Add(BuildToolResultMessageSuccess(name, callId, result));
                }
                catch (Exception ex)
                {
                    rec.SetExecutionError(ex);
                    list.Add(BuildToolResultMessageError(name, callId, ex.Message));
                }
                finally
                {
                    rec.End();
                }
            }
        }

        return list;
    }

    private static object? ParseToolCallArguments(byte[] inputJson)
    {
        if (inputJson.Length == 0)
        {
            return new Dictionary<string, object?>();
        }

        try
        {
            return JsonSerializer.Deserialize<object>(inputJson);
        }
        catch (JsonException)
        {
            return Encoding.UTF8.GetString(inputJson);
        }
    }

    private static Message BuildToolResultMessageSuccess(string toolName, string callId, object? result)
    {
        var tr = new ToolResult { ToolCallId = callId, Name = toolName };
        switch (result)
        {
            case null:
                break;
            case string s:
                tr.Content = s;
                break;
            default:
                tr.ContentJson = JsonSerializer.SerializeToUtf8Bytes(result);
                break;
        }

        return new Message
        {
            Role = MessageRole.Tool,
            Name = toolName,
            Parts = [Part.ToolResultPart(tr)],
        };
    }

    private static Message BuildToolResultMessageError(string toolName, string callId, string errorText)
    {
        return new Message
        {
            Role = MessageRole.Tool,
            Name = toolName,
            Parts =
            [
                Part.ToolResultPart(
                    new ToolResult
                    {
                        ToolCallId = callId,
                        Name = toolName,
                        Content = errorText,
                        IsError = true,
                    }
                ),
            ],
        };
    }
}
