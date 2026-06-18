namespace Grafana.Sigil;

public enum GenerationExportProtocol
{
    Grpc,
    Http,
    None
}

public enum ExportAuthMode
{
    None,
    Tenant,
    Bearer,
    Basic
}

public sealed class AuthConfig
{
    /// <summary>
    /// Auth mode. <c>null</c> means "not set" — the env layer or
    /// <c>ConfigResolver</c> resolves it to <see cref="ExportAuthMode.None"/>.
    /// Explicit <c>Mode = ExportAuthMode.None</c> is preserved (caller-wins)
    /// and not overridden by <c>SIGIL_AUTH_MODE</c>.
    /// </summary>
    public ExportAuthMode? Mode { get; set; }
    public string TenantId { get; set; } = string.Empty;
    public string BearerToken { get; set; } = string.Empty;
    /// <summary>Username for basic auth. When empty, TenantId is used.</summary>
    public string BasicUser { get; set; } = string.Empty;
    /// <summary>Password/token for basic auth.</summary>
    public string BasicPassword { get; set; } = string.Empty;
}

public sealed class GenerationExportConfig
{
    /// <summary>
    /// Export protocol. <c>null</c> means "not set" — the env layer or
    /// <c>ConfigResolver</c> resolves it to
    /// <see cref="GenerationExportProtocol.Grpc"/>. An explicit
    /// <c>Protocol = ...</c> assignment is preserved (caller-wins) and not
    /// overridden by <c>SIGIL_PROTOCOL</c>.
    /// </summary>
    public GenerationExportProtocol? Protocol { get; set; }
    /// <summary>
    /// Export endpoint. Empty string means "not set" — env layer or
    /// <c>ConfigResolver</c> resolves it to <c>localhost:4317</c>. An explicit
    /// non-empty value is preserved (caller-wins) and not overridden by
    /// <c>SIGIL_ENDPOINT</c>.
    /// </summary>
    public string Endpoint { get; set; } = "";
    public Dictionary<string, string> Headers { get; set; } = new(StringComparer.OrdinalIgnoreCase);
    public AuthConfig Auth { get; set; } = new();

    /// <summary>
    /// Tri-state insecure flag. <c>null</c> means "not set" — the resolved
    /// value is <c>false</c> (TLS on) unless <c>SIGIL_INSECURE</c> provides a
    /// value or the caller explicitly sets one. Matches Go's <c>*bool</c>
    /// semantics so explicit <c>false</c> overrides <c>SIGIL_INSECURE=true</c>.
    /// </summary>
    public bool? Insecure { get; set; }
    public int BatchSize { get; set; } = 100;
    public TimeSpan FlushInterval { get; set; } = TimeSpan.FromSeconds(1);
    public int QueueSize { get; set; } = 2000;
    public int MaxRetries { get; set; } = 5;
    public TimeSpan InitialBackoff { get; set; } = TimeSpan.FromMilliseconds(100);
    public TimeSpan MaxBackoff { get; set; } = TimeSpan.FromSeconds(5);
    public int PayloadMaxBytes { get; set; } = 4 << 20;
}

public sealed class ApiConfig
{
    public string Endpoint { get; set; } = "http://localhost:8080";
}

public sealed class SigilClientConfig
{
    public GenerationExportConfig GenerationExport { get; set; } = new();
    public ApiConfig Api { get; set; } = new();
    public EmbeddingCaptureConfig EmbeddingCapture { get; set; } = new();
    public ContentCaptureMode ContentCapture { get; set; } = ContentCaptureMode.Default;
    public Func<IReadOnlyDictionary<string, object?>?, ContentCaptureMode>? ContentCaptureResolver { get; set; }
    /// <summary>
    /// Optional hook invoked for each generation after normalization and before
    /// content-capture stripping/export. Use <see cref="SecretRedactionSanitizer"/>
    /// to create the built-in regex-based secrets redactor.
    /// </summary>
    public GenerationSanitizer? GenerationSanitizer { get; set; }
    public Action<string>? Logger { get; set; }
    public Func<DateTimeOffset>? UtcNow { get; set; }
    public Func<TimeSpan, CancellationToken, Task>? SleepAsync { get; set; }
    public IGenerationExporter? GenerationExporter { get; set; }

    /// <summary>
    /// Default <c>gen_ai.agent.name</c> for generations that don't supply one
    /// per-call. Filled from <c>SIGIL_AGENT_NAME</c> when the caller leaves
    /// this empty.
    /// </summary>
    public string AgentName { get; set; } = string.Empty;

    /// <summary>
    /// Default <c>gen_ai.agent.version</c>. Filled from <c>SIGIL_AGENT_VERSION</c>.
    /// </summary>
    public string AgentVersion { get; set; } = string.Empty;

    /// <summary>
    /// Default <c>user.id</c>. Filled from <c>SIGIL_USER_ID</c>.
    /// </summary>
    public string UserId { get; set; } = string.Empty;

    private Dictionary<string, string> _tags = new();

    /// <summary>
    /// Tags merged into every <see cref="GenerationStart"/>'s tags. Per-call
    /// tags win on key collision. Filled from <c>SIGIL_TAGS</c>.
    /// </summary>
    /// <remarks>
    /// The setter takes a defensive copy so caller-side mutations after
    /// assignment cannot reach the SDK. The getter returns the live
    /// internal map so the env resolver and client code can populate it.
    /// </remarks>
    public Dictionary<string, string> Tags
    {
        get => _tags;
        set => _tags = value == null ? new Dictionary<string, string>() : new Dictionary<string, string>(value);
    }

    /// <summary>
    /// Tri-state debug flag mirroring Go's <c>*bool</c>. <c>null</c> means
    /// "not set" — filled from <c>SIGIL_DEBUG</c> when the caller hasn't
    /// supplied a value. Explicit <c>false</c> overrides
    /// <c>SIGIL_DEBUG=true</c>.
    /// </summary>
    public bool? Debug { get; set; }
}

public sealed class EmbeddingCaptureConfig
{
    public bool CaptureInput { get; set; }
    public int MaxInputItems { get; set; } = 20;
    public int MaxTextLength { get; set; } = 1024;
}

internal static class ConfigResolver
{
    internal const string TenantHeaderName = "X-Scope-OrgID";
    internal const string AuthorizationHeaderName = "Authorization";

    public static SigilClientConfig Resolve(SigilClientConfig? config)
    {
        return Resolve(config, Environment.GetEnvironmentVariable);
    }

    internal static SigilClientConfig Resolve(SigilClientConfig? config, Func<string, string?> envLookup)
    {
        var (resolved, warnings) = EnvConfig.ResolveFromEnv(envLookup, config ?? new SigilClientConfig());

        var callerLogger = resolved.Logger;
        resolved.Logger ??= _ => { };
        // Always surface env-resolve warnings to stderr so a typo in
        // SIGIL_AUTH_MODE / SIGIL_PROTOCOL / SIGIL_CONTENT_CAPTURE_MODE has
        // operator-visible signal even when the caller didn't supply a Logger.
        // When the caller provided a Logger, route warnings through it as well.
        EnvConfig.LogWarnings(callerLogger ?? Console.Error.WriteLine, warnings);

#if NET8_0_OR_GREATER
        resolved.UtcNow ??= TimeProvider.System.GetUtcNow;
#else
        resolved.UtcNow ??= () => DateTimeOffset.UtcNow;
#endif

        resolved.SleepAsync ??= static (delay, ct) => Task.Delay(delay, ct);

        // After env layering, null Insecure resolves to false (TLS on).
        resolved.GenerationExport.Insecure ??= false;

        resolved.GenerationExport.Headers = ResolveHeadersWithAuth(
            resolved.GenerationExport.Headers,
            resolved.GenerationExport.Auth,
            "generation"
        );
        if (string.IsNullOrWhiteSpace(resolved.Api.Endpoint))
        {
            resolved.Api.Endpoint = "http://localhost:8080";
        }

        if (resolved.GenerationExport.BatchSize <= 0)
        {
            resolved.GenerationExport.BatchSize = 1;
        }

        if (resolved.GenerationExport.QueueSize <= 0)
        {
            resolved.GenerationExport.QueueSize = 1;
        }

        if (resolved.GenerationExport.FlushInterval <= TimeSpan.Zero)
        {
            resolved.GenerationExport.FlushInterval = TimeSpan.FromMilliseconds(1);
        }

        if (resolved.GenerationExport.MaxRetries < 0)
        {
            resolved.GenerationExport.MaxRetries = 0;
        }

        if (resolved.GenerationExport.InitialBackoff <= TimeSpan.Zero)
        {
            resolved.GenerationExport.InitialBackoff = TimeSpan.FromMilliseconds(100);
        }

        if (resolved.GenerationExport.MaxBackoff <= TimeSpan.Zero)
        {
            resolved.GenerationExport.MaxBackoff = TimeSpan.FromMilliseconds(100);
        }

        resolved.EmbeddingCapture ??= new EmbeddingCaptureConfig();

        if (resolved.EmbeddingCapture.MaxInputItems <= 0)
        {
            resolved.EmbeddingCapture.MaxInputItems = 20;
        }
        if (resolved.EmbeddingCapture.MaxTextLength <= 0)
        {
            resolved.EmbeddingCapture.MaxTextLength = 1024;
        }

        return resolved;
    }

    /// <summary>
    /// Builds the auth-related headers for <paramref name="auth"/>.Mode.
    /// Mode-irrelevant fields (e.g. <c>TenantId</c> on a bearer-mode config)
    /// are silently ignored — env layering can populate any field independently
    /// of mode, and rejecting cross-mode mixes only forced extra cleanup
    /// upstream. Callers who want strict validation should check their
    /// <see cref="AuthConfig"/> before constructing the client.
    /// </summary>
    public static Dictionary<string, string> ResolveHeadersWithAuth(
        Dictionary<string, string> headers,
        AuthConfig auth,
        string label
    )
    {
        var resolved = new Dictionary<string, string>(headers, StringComparer.OrdinalIgnoreCase);

        var tenantId = auth.TenantId?.Trim() ?? string.Empty;
        var bearerToken = auth.BearerToken?.Trim() ?? string.Empty;

        // Default null Mode to None so direct callers (without ConfigResolver)
        // get the same fallback behavior as env-resolved configs.
        switch (auth.Mode ?? ExportAuthMode.None)
        {
            case ExportAuthMode.None:
                return resolved;
            case ExportAuthMode.Tenant:
                if (tenantId.Length == 0)
                {
                    throw new ArgumentException($"{label} auth mode 'tenant' requires tenant_id");
                }

                if (!resolved.ContainsKey(TenantHeaderName))
                {
                    resolved[TenantHeaderName] = tenantId;
                }

                return resolved;
            case ExportAuthMode.Bearer:
                if (bearerToken.Length == 0)
                {
                    throw new ArgumentException($"{label} auth mode 'bearer' requires bearer_token");
                }

                if (!resolved.ContainsKey(AuthorizationHeaderName))
                {
                    resolved[AuthorizationHeaderName] = FormatBearerTokenValue(bearerToken);
                }

                return resolved;
            case ExportAuthMode.Basic:
                var basicPassword = auth.BasicPassword?.Trim() ?? string.Empty;
                if (basicPassword.Length == 0)
                {
                    throw new ArgumentException($"{label} auth mode 'basic' requires basic_password");
                }

                var basicUser = auth.BasicUser?.Trim() ?? string.Empty;
                if (basicUser.Length == 0)
                {
                    basicUser = tenantId;
                }

                if (basicUser.Length == 0)
                {
                    throw new ArgumentException($"{label} auth mode 'basic' requires basic_user or tenant_id");
                }

                if (!resolved.ContainsKey(AuthorizationHeaderName))
                {
                    var encoded = Convert.ToBase64String(
                        System.Text.Encoding.UTF8.GetBytes($"{basicUser}:{basicPassword}"));
                    resolved[AuthorizationHeaderName] = $"Basic {encoded}";
                }

                if (tenantId.Length > 0 && !resolved.ContainsKey(TenantHeaderName))
                {
                    resolved[TenantHeaderName] = tenantId;
                }

                return resolved;
            default:
                throw new ArgumentException($"unsupported {label} auth mode '{auth.Mode}'");
        }
    }

    private static string FormatBearerTokenValue(string token)
    {
        var value = token.Trim();
        if (value.StartsWith("Bearer ", StringComparison.OrdinalIgnoreCase))
        {
#if NET
            value = value["Bearer ".Length..].Trim();
#else
            value = value.Substring("Bearer ".Length).Trim();
#endif
        }

        return $"Bearer {value}";
    }
}
