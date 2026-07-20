namespace Grafana.Sigil;

/// <summary>
/// One logical config field readable under the preferred <c>AGENTO11Y_*</c>
/// name with a <c>SIGIL_*</c> legacy fallback. Selection happens before
/// parsing: a nonblank preferred value always wins, even when it later fails
/// validation, so stale legacy config cannot silently resurface.
/// </summary>
internal readonly record struct EnvPair(string Preferred, string Legacy);

/// <summary>
/// Reads canonical <c>AGENTO11Y_*</c> environment variables (with
/// <c>SIGIL_*</c> legacy fallbacks) and layers them under caller-supplied
/// <see cref="SigilClientConfig"/> values.
/// </summary>
/// <remarks>
/// <para>Resolution order (highest precedence first):</para>
/// <list type="number">
///   <item><description>Caller-supplied <see cref="SigilClientConfig"/> field
///       (when not at its default/unset state).</description></item>
///   <item><description>Preferred <c>AGENTO11Y_*</c> env var, then the
///       <c>SIGIL_*</c> legacy spelling.</description></item>
///   <item><description>SDK schema default.</description></item>
/// </list>
/// <para>Mirrors the Go reference implementation in
/// <c>go/sigil/env.go</c>. Invalid env values are skipped with a warning so
/// a single typo does not discard the rest of the env layer.</para>
/// </remarks>
public static class EnvConfig
{
    // Legacy SIGIL_* spellings. Kept with their original names and values
    // because they may be compiled into consumer binaries.
    public const string EnvEndpoint = "SIGIL_ENDPOINT";
    public const string EnvProtocol = "SIGIL_PROTOCOL";
    public const string EnvInsecure = "SIGIL_INSECURE";
    public const string EnvHeaders = "SIGIL_HEADERS";
    public const string EnvAuthMode = "SIGIL_AUTH_MODE";
    public const string EnvAuthTenantId = "SIGIL_AUTH_TENANT_ID";
    public const string EnvAuthToken = "SIGIL_AUTH_TOKEN";
    public const string EnvAgentName = "SIGIL_AGENT_NAME";
    public const string EnvAgentVersion = "SIGIL_AGENT_VERSION";
    public const string EnvUserId = "SIGIL_USER_ID";
    public const string EnvTags = "SIGIL_TAGS";
    public const string EnvContentCaptureMode = "SIGIL_CONTENT_CAPTURE_MODE";
    public const string EnvDebug = "SIGIL_DEBUG";

    // Preferred AGENTO11Y_* spellings.
    public const string PreferredEnvEndpoint = "AGENTO11Y_ENDPOINT";
    public const string PreferredEnvProtocol = "AGENTO11Y_PROTOCOL";
    public const string PreferredEnvInsecure = "AGENTO11Y_INSECURE";
    public const string PreferredEnvHeaders = "AGENTO11Y_HEADERS";
    public const string PreferredEnvAuthMode = "AGENTO11Y_AUTH_MODE";
    public const string PreferredEnvAuthTenantId = "AGENTO11Y_AUTH_TENANT_ID";
    public const string PreferredEnvAuthToken = "AGENTO11Y_AUTH_TOKEN";
    public const string PreferredEnvAgentName = "AGENTO11Y_AGENT_NAME";
    public const string PreferredEnvAgentVersion = "AGENTO11Y_AGENT_VERSION";
    public const string PreferredEnvUserId = "AGENTO11Y_USER_ID";
    public const string PreferredEnvTags = "AGENTO11Y_TAGS";
    public const string PreferredEnvContentCaptureMode = "AGENTO11Y_CONTENT_CAPTURE_MODE";
    public const string PreferredEnvDebug = "AGENTO11Y_DEBUG";

    private static readonly EnvPair EndpointPair = new(PreferredEnvEndpoint, EnvEndpoint);
    private static readonly EnvPair ProtocolPair = new(PreferredEnvProtocol, EnvProtocol);
    private static readonly EnvPair InsecurePair = new(PreferredEnvInsecure, EnvInsecure);
    private static readonly EnvPair HeadersPair = new(PreferredEnvHeaders, EnvHeaders);
    private static readonly EnvPair AuthModePair = new(PreferredEnvAuthMode, EnvAuthMode);
    private static readonly EnvPair AuthTenantIdPair = new(PreferredEnvAuthTenantId, EnvAuthTenantId);
    private static readonly EnvPair AuthTokenPair = new(PreferredEnvAuthToken, EnvAuthToken);
    private static readonly EnvPair AgentNamePair = new(PreferredEnvAgentName, EnvAgentName);
    private static readonly EnvPair AgentVersionPair = new(PreferredEnvAgentVersion, EnvAgentVersion);
    private static readonly EnvPair UserIdPair = new(PreferredEnvUserId, EnvUserId);
    private static readonly EnvPair TagsPair = new(PreferredEnvTags, EnvTags);
    private static readonly EnvPair ContentCaptureModePair = new(PreferredEnvContentCaptureMode, EnvContentCaptureMode);
    private static readonly EnvPair DebugPair = new(PreferredEnvDebug, EnvDebug);

    internal const string DefaultEndpoint = "localhost:4317";

    /// <summary>
    /// Returns a config built from process env vars layered onto a fresh
    /// <see cref="SigilClientConfig"/>. Convenience helper; most callers should
    /// let <see cref="SigilClient"/> construction perform the same resolution
    /// internally. Warnings for invalid env values are written to stderr.
    /// </summary>
    public static SigilClientConfig FromEnv()
    {
        var (cfg, warnings) = ResolveFromEnv(Environment.GetEnvironmentVariable, new SigilClientConfig());
        LogWarnings(Console.Error.WriteLine, warnings);
        return cfg;
    }

    /// <summary>
    /// Applies canonical <c>AGENTO11Y_*</c> env values (with <c>SIGIL_*</c>
    /// legacy fallbacks) onto <paramref name="base"/>, preserving
    /// caller-supplied fields. Mutates <paramref name="base"/> in place to
    /// match the existing <c>ConfigResolver.Resolve</c> semantics so callers
    /// can read back resolved auth headers and other normalised fields from
    /// the config object they supplied.
    /// </summary>
    /// <returns>
    /// Tuple of the resolved config (same reference as <paramref name="base"/>)
    /// and a list of warnings emitted for invalid env values (e.g. unknown
    /// <c>AGENTO11Y_AUTH_MODE</c>), naming the env var the value came from.
    /// </returns>
    internal static (SigilClientConfig config, IReadOnlyList<string> warnings) ResolveFromEnv(
        Func<string, string?> lookup,
        SigilClientConfig @base
    )
    {
        var src = lookup ?? Environment.GetEnvironmentVariable;
        var cfg = @base ?? new SigilClientConfig();
        EnsureNonNullNested(cfg);
        var warnings = new List<string>();

        var export = cfg.GenerationExport;

        var endpoint = EnvTrimmed(src, EndpointPair);
        if (endpoint != null && string.IsNullOrEmpty(export.Endpoint))
        {
            export.Endpoint = endpoint;
        }

        var protocol = EnvTrimmed(src, ProtocolPair, out var protocolKey);
        if (protocol != null && export.Protocol == null)
        {
            var parsed = ParseProtocol(protocol);
            if (parsed.HasValue)
            {
                export.Protocol = parsed.Value;
            }
            else
            {
                warnings.Add($"sigil: ignoring invalid {protocolKey} {protocol}");
            }
        }

        var insecureRaw = EnvTrimmed(src, InsecurePair);
        if (insecureRaw != null && export.Insecure == null)
        {
            export.Insecure = ParseBool(insecureRaw);
        }

        var headersRaw = EnvTrimmed(src, HeadersPair);
        if (headersRaw != null && (export.Headers == null || export.Headers.Count == 0))
        {
            // Headers are HTTP-style and case-insensitive at the consumer; the
            // env-derived map mirrors that. Built element-by-element so case-mixed
            // duplicates (x-a=1,X-A=2) collapse to last-wins instead of throwing.
            var headers = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
            foreach (var kv in ParseCsvKv(headersRaw))
            {
                headers[kv.Key] = kv.Value;
            }
            export.Headers = headers;
        }

        var auth = export.Auth ??= new AuthConfig();
        var authModeRaw = EnvTrimmed(src, AuthModePair, out var authModeKey);
        if (authModeRaw != null && auth.Mode == null)
        {
            var parsed = ParseAuthMode(authModeRaw);
            if (parsed.HasValue)
            {
                auth.Mode = parsed.Value;
            }
            else
            {
                warnings.Add($"sigil: ignoring invalid {authModeKey} {authModeRaw}");
            }
        }

        var tenantId = EnvTrimmed(src, AuthTenantIdPair);
        if (tenantId != null && string.IsNullOrEmpty(auth.TenantId))
        {
            auth.TenantId = tenantId;
        }

        var token = EnvTrimmed(src, AuthTokenPair);
        if (token != null)
        {
            // Set both fields when empty; ResolveHeadersWithAuth uses only the
            // one matching the final mode. Lets env's token populate a
            // caller-set mode without env declaring AGENTO11Y_AUTH_MODE.
            if (string.IsNullOrEmpty(auth.BearerToken))
            {
                auth.BearerToken = token;
            }
            if (string.IsNullOrEmpty(auth.BasicPassword))
            {
                auth.BasicPassword = token;
            }
        }
        if (auth.Mode == ExportAuthMode.Basic
            && string.IsNullOrEmpty(auth.BasicUser)
            && !string.IsNullOrEmpty(auth.TenantId))
        {
            auth.BasicUser = auth.TenantId;
        }

        // Finalize tri-state defaults after env layering: null/empty means
        // "no caller value and no env value", so apply the schema default.
        export.Protocol ??= GenerationExportProtocol.Grpc;
        if (string.IsNullOrEmpty(export.Endpoint))
        {
            export.Endpoint = DefaultEndpoint;
        }
        auth.Mode ??= ExportAuthMode.None;

        var agentName = EnvTrimmed(src, AgentNamePair);
        if (agentName != null && string.IsNullOrEmpty(cfg.AgentName))
        {
            cfg.AgentName = agentName;
        }
        var agentVersion = EnvTrimmed(src, AgentVersionPair);
        if (agentVersion != null && string.IsNullOrEmpty(cfg.AgentVersion))
        {
            cfg.AgentVersion = agentVersion;
        }
        var userId = EnvTrimmed(src, UserIdPair);
        if (userId != null && string.IsNullOrEmpty(cfg.UserId))
        {
            cfg.UserId = userId;
        }

        var tagsRaw = EnvTrimmed(src, TagsPair);
        if (tagsRaw != null)
        {
            var envTags = ParseCsvKv(tagsRaw);
            // Env tags act as a base layer; caller tags win on collision.
            var merged = new Dictionary<string, string>(envTags);
            if (cfg.Tags != null)
            {
                foreach (var kv in cfg.Tags)
                {
                    merged[kv.Key] = kv.Value;
                }
            }
            cfg.Tags = merged;
        }

        var ccmRaw = EnvTrimmed(src, ContentCaptureModePair, out var ccmKey);
        if (ccmRaw != null && cfg.ContentCapture == ContentCaptureMode.Default)
        {
            var parsed = ParseContentCaptureMode(ccmRaw);
            if (parsed.HasValue)
            {
                cfg.ContentCapture = parsed.Value;
            }
            else
            {
                warnings.Add($"sigil: ignoring invalid {ccmKey} {ccmRaw}");
            }
        }

        var debugRaw = EnvTrimmed(src, DebugPair);
        if (debugRaw != null && cfg.Debug == null)
        {
            cfg.Debug = ParseBool(debugRaw);
        }

        return (cfg, warnings);
    }

    private static void EnsureNonNullNested(SigilClientConfig cfg)
    {
        cfg.Api ??= new ApiConfig();
        cfg.GenerationExport ??= new GenerationExportConfig();
        cfg.GenerationExport.Auth ??= new AuthConfig();
        cfg.GenerationExport.Headers ??= new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
        cfg.Tags ??= new Dictionary<string, string>();
    }

    internal static string? EnvTrimmed(Func<string, string?> lookup, EnvPair pair)
    {
        return EnvTrimmed(lookup, pair, out _);
    }

    /// <summary>
    /// Selects the pair's first nonblank value (preferred, then legacy) and
    /// reports the env-var name it came from via <paramref name="key"/>, so
    /// validation warnings can name the key the user actually set.
    /// </summary>
    internal static string? EnvTrimmed(Func<string, string?> lookup, EnvPair pair, out string key)
    {
        var preferred = EnvTrimmed(lookup, pair.Preferred);
        if (preferred != null)
        {
            key = pair.Preferred;
            return preferred;
        }
        key = pair.Legacy;
        return EnvTrimmed(lookup, pair.Legacy);
    }

    internal static string? EnvTrimmed(Func<string, string?> lookup, string key)
    {
        string? raw;
        try
        {
            raw = lookup(key);
        }
        catch (System.Security.SecurityException)
        {
            return null;
        }
        if (raw == null)
        {
            return null;
        }
        var v = raw.Trim();
        return v.Length == 0 ? null : v;
    }

    internal static bool ParseBool(string? raw)
    {
        if (raw == null)
        {
            return false;
        }
        return raw.Trim().ToLowerInvariant() switch
        {
            "1" or "true" or "yes" or "on" => true,
            _ => false,
        };
    }

    internal static Dictionary<string, string> ParseCsvKv(string? raw)
    {
        // Case-sensitive to match Go/Java: AGENTO11Y_TAGS=env=prod,Env=staging
        // yields two distinct keys. Headers consumers wrap the result in an
        // OrdinalIgnoreCase dictionary at the use site.
        var out_ = new Dictionary<string, string>();
        if (raw == null)
        {
            return out_;
        }
        foreach (var part in raw.Split(','))
        {
            var trimmed = part.Trim();
            if (trimmed.Length == 0)
            {
                continue;
            }
            var idx = trimmed.IndexOf('=');
            if (idx <= 0)
            {
                continue;
            }
            var key = trimmed.Substring(0, idx).Trim();
            var value = trimmed.Substring(idx + 1).Trim();
            if (key.Length > 0)
            {
                out_[key] = value;
            }
        }
        return out_;
    }

    internal static ContentCaptureMode? ParseContentCaptureMode(string? raw)
    {
        if (raw == null)
        {
            return null;
        }
        return raw.Trim().ToLowerInvariant() switch
        {
            "full" => ContentCaptureMode.Full,
            "no_tool_content" => ContentCaptureMode.NoToolContent,
            "metadata_only" => ContentCaptureMode.MetadataOnly,
            "full_with_metadata_spans" => ContentCaptureMode.FullWithMetadataSpans,
            _ => null,
        };
    }

    internal static ExportAuthMode? ParseAuthMode(string? raw)
    {
        if (raw == null)
        {
            return null;
        }
        return raw.Trim().ToLowerInvariant() switch
        {
            "none" => ExportAuthMode.None,
            "tenant" => ExportAuthMode.Tenant,
            "bearer" => ExportAuthMode.Bearer,
            "basic" => ExportAuthMode.Basic,
            _ => null,
        };
    }

    internal static GenerationExportProtocol? ParseProtocol(string? raw)
    {
        if (raw == null)
        {
            return null;
        }
        return raw.Trim().ToLowerInvariant() switch
        {
            "grpc" => GenerationExportProtocol.Grpc,
            "http" => GenerationExportProtocol.Http,
            "none" => GenerationExportProtocol.None,
            _ => null,
        };
    }

    internal static void LogWarnings(Action<string>? log, IReadOnlyList<string> warnings)
    {
        if (log == null || warnings == null)
        {
            return;
        }
        foreach (var w in warnings)
        {
            log(w);
        }
    }
}
