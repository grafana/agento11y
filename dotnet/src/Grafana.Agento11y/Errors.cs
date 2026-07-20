namespace Grafana.Sigil;

public class SigilException : Exception
{
    public SigilException(string? message)
        : base(message)
    {
    }

    public SigilException(string? message, Exception? inner)
        : base(message, inner)
    {
    }
}

public sealed class ValidationException : SigilException
{
    public ValidationException(string? message)
        : base(message)
    {
    }

    public ValidationException(string? message, Exception? inner)
        : base(message, inner)
    {
    }
}

public class EnqueueException : SigilException
{
    public EnqueueException(string? message)
        : base(message)
    {
    }

    public EnqueueException(string? message, Exception? inner)
        : base(message, inner)
    {
    }
}

public sealed class QueueFullException : EnqueueException
{
    public QueueFullException(string? message)
        : base(message)
    {
    }

    public QueueFullException(string? message, Exception? inner)
        : base(message, inner)
    {
    }
}

public sealed class ClientShutdownException : EnqueueException
{
    public ClientShutdownException(string? message)
        : base(message)
    {
    }

    public ClientShutdownException(string? message, Exception? inner)
        : base(message, inner)
    {
    }
}

public sealed class MappingException : SigilException
{
    public MappingException(string? message)
        : base(message)
    {
    }

    public MappingException(string? message, Exception? inner)
        : base(message, inner)
    {
    }
}

public sealed class RatingConflictException : SigilException
{
    public RatingConflictException(string? message)
        : base(message)
    {
    }

    public RatingConflictException(string? message, Exception? inner)
        : base(message, inner)
    {
    }
}

public sealed class RatingTransportException : SigilException
{
    public RatingTransportException(string? message)
        : base(message)
    {
    }

    public RatingTransportException(string? message, Exception? inner)
        : base(message, inner)
    {
    }
}
