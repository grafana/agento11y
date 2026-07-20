using System.Threading;

namespace Grafana.Sigil;

public static class SigilContext
{
    private static readonly AsyncLocal<string?> ConversationIdSlot = new();
    private static readonly AsyncLocal<string?> ConversationTitleSlot = new();
    private static readonly AsyncLocal<string?> UserIdSlot = new();
    private static readonly AsyncLocal<string?> AgentNameSlot = new();
    private static readonly AsyncLocal<string?> AgentVersionSlot = new();
    private static readonly AsyncLocal<ContentCaptureMode?> ContentCaptureModeSlot = new();

    // Stack of active generation recorder capture modes, keyed by recorder ID.
    // Handles non-LIFO completion when overlapping generations end out-of-order
    // (matches Python SDK pattern).
    private static readonly AsyncLocal<CaptureStackEntry[]?> CaptureStack = new();
    private static readonly AsyncLocal<ContentCaptureMode?> CaptureStackBase = new();
    private static long _nextRecorderId;

    public static IDisposable WithConversationId(string conversationId)
    {
        return new AsyncLocalScope<string?>(ConversationIdSlot, conversationId);
    }

    public static IDisposable WithConversationTitle(string conversationTitle)
    {
        return new AsyncLocalScope<string?>(ConversationTitleSlot, conversationTitle);
    }

    public static IDisposable WithUserId(string userId)
    {
        return new AsyncLocalScope<string?>(UserIdSlot, userId);
    }

    public static IDisposable WithAgentName(string agentName)
    {
        return new AsyncLocalScope<string?>(AgentNameSlot, agentName);
    }

    public static IDisposable WithAgentVersion(string agentVersion)
    {
        return new AsyncLocalScope<string?>(AgentVersionSlot, agentVersion);
    }

    public static string? ConversationIdFromContext()
    {
        return ConversationIdSlot.Value;
    }

    public static string? ConversationTitleFromContext()
    {
        return ConversationTitleSlot.Value;
    }

    public static string? UserIdFromContext()
    {
        return UserIdSlot.Value;
    }

    public static string? AgentNameFromContext()
    {
        return AgentNameSlot.Value;
    }

    public static string? AgentVersionFromContext()
    {
        return AgentVersionSlot.Value;
    }

    internal static IDisposable PushContentCaptureMode(ContentCaptureMode mode)
    {
        var recorderId = Interlocked.Increment(ref _nextRecorderId);
        var stack = CaptureStack.Value ?? [];
        if (stack.Length == 0)
        {
            CaptureStackBase.Value = ContentCaptureModeSlot.Value;
        }

        var newStack = new CaptureStackEntry[stack.Length + 1];
        Array.Copy(stack, newStack, stack.Length);
        newStack[stack.Length] = new CaptureStackEntry(recorderId, mode);
        CaptureStack.Value = newStack;
        ContentCaptureModeSlot.Value = mode;
        return new CaptureStackPop(recorderId);
    }

    internal static ContentCaptureMode ContentCaptureModeFromContext()
    {
        return ContentCaptureModeSlot.Value ?? ContentCaptureMode.Default;
    }

    internal static bool HasContentCaptureModeInContext()
    {
        return ContentCaptureModeSlot.Value.HasValue;
    }

    private static void PopContentCaptureMode(long recorderId)
    {
        var stack = CaptureStack.Value;
        if (stack == null || stack.Length == 0)
        {
            return;
        }

        var count = 0;
        foreach (var entry in stack)
        {
            if (entry.RecorderId != recorderId)
            {
                count++;
            }
        }

        if (count == stack.Length)
        {
            return;
        }

        var newStack = new CaptureStackEntry[count];
        var idx = 0;
        foreach (var entry in stack)
        {
            if (entry.RecorderId != recorderId)
            {
                newStack[idx++] = entry;
            }
        }

        CaptureStack.Value = newStack;
        ContentCaptureModeSlot.Value = newStack.Length > 0
            ? newStack[newStack.Length - 1].Mode
            : CaptureStackBase.Value;
    }

    private readonly struct CaptureStackEntry
    {
        public readonly long RecorderId;
        public readonly ContentCaptureMode Mode;

        public CaptureStackEntry(long recorderId, ContentCaptureMode mode)
        {
            RecorderId = recorderId;
            Mode = mode;
        }
    }

    private sealed class CaptureStackPop(long recorderId) : IDisposable
    {
        private bool _disposed;

        public void Dispose()
        {
            if (_disposed)
            {
                return;
            }

            _disposed = true;
            PopContentCaptureMode(recorderId);
        }
    }

    private sealed class AsyncLocalScope<T> : IDisposable
    {
        private readonly AsyncLocal<T> _slot;
        private readonly T _previousValue;
        private bool _disposed;

        public AsyncLocalScope(AsyncLocal<T> slot, T value)
        {
            _slot = slot;
            _previousValue = slot.Value!;
            _slot.Value = value;
        }

        public void Dispose()
        {
            if (_disposed)
            {
                return;
            }

            _slot.Value = _previousValue;
            _disposed = true;
        }
    }
}
