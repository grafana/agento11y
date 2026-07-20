namespace Grafana.Agento11y;

public class Agento11yException : Exception
{
    public Agento11yException(string? message)
        : base(message)
    {
    }

    public Agento11yException(string? message, Exception? inner)
        : base(message, inner)
    {
    }
}

public sealed class ValidationException : Agento11yException
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

public class EnqueueException : Agento11yException
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

public sealed class MappingException : Agento11yException
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

public sealed class RatingConflictException : Agento11yException
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

public sealed class RatingTransportException : Agento11yException
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
