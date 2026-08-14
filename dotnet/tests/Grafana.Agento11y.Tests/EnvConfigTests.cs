using Xunit;

namespace Grafana.Agento11y.Tests;

public sealed class EnvConfigTests
{
    private static Func<string, string?> MapLookup(IDictionary<string, string?> env)
    {
        return key => env.TryGetValue(key, out var value) ? value : null;
    }

    private static Func<string, string?> EmptyLookup => _ => null;

    /// <summary>
    /// Values <c>AGENTO11Y_EXPORT_TIMEOUT_MS</c> / <c>SIGIL_EXPORT_TIMEOUT_MS</c>
    /// must reject: the parser takes base-10 integer milliseconds in the
    /// inclusive range 1..2147483647 only. Shared with
    /// <see cref="EnvIntegrationTests"/> so the env layer and
    /// <c>ConfigResolver</c> are exercised against the same table.
    /// </summary>
    public static TheoryData<string> InvalidExportTimeoutValues() => new()
    {
        "0",           // below the inclusive 1 ms floor
        "-1",          // negative
        "1.5",         // fractional
        "abc",         // non-numeric
        "2147483648",  // one past int.MaxValue
        "+5",          // signs are not part of the grammar
        "1_000",       // digit separators are a C# literal feature, not env syntax
        "0x10",        // hex
        "1e3",         // exponent
        "1 000",       // embedded whitespace
    };

    /// <summary>Accepted export-timeout env values and their millisecond result.</summary>
    public static TheoryData<string, int> ValidExportTimeoutValues() => new()
    {
        { "1", 1 },                      // inclusive lower bound
        { "250", 250 },
        { "1500", 1500 },
        { "  2500  ", 2500 },            // surrounding whitespace is trimmed first
        { "2147483647", int.MaxValue },  // inclusive upper bound
    };

    [Fact]
    public void NoEnvKeepsBaseDefaults()
    {
        var (cfg, warnings) = EnvConfig.ResolveFromEnv(EmptyLookup, new Agento11yClientConfig());

        Assert.Equal(string.Empty, cfg.AgentName);
        Assert.Equal(string.Empty, cfg.AgentVersion);
        Assert.Equal(string.Empty, cfg.UserId);
        Assert.Empty(cfg.Tags);
        Assert.Null(cfg.Debug);
        Assert.Null(cfg.GenerationExport.Insecure);
        Assert.Equal("localhost:4317", cfg.GenerationExport.Endpoint);
        Assert.Equal(TimeSpan.FromSeconds(30), cfg.GenerationExport.ExportTimeout);
        Assert.False(cfg.GenerationExport.HasExplicitExportTimeout);
        Assert.Empty(warnings);
    }

    [Fact]
    public void TransportFromEnv()
    {
        var env = new Dictionary<string, string?>
        {
            ["SIGIL_ENDPOINT"] = "https://env:4318",
            ["SIGIL_PROTOCOL"] = "http",
            ["SIGIL_INSECURE"] = "true",
            ["SIGIL_HEADERS"] = "X-A=1,X-B=two",
        };

        var (cfg, _) = EnvConfig.ResolveFromEnv(MapLookup(env), new Agento11yClientConfig());

        Assert.Equal("https://env:4318", cfg.GenerationExport.Endpoint);
        Assert.Equal(GenerationExportProtocol.Http, cfg.GenerationExport.Protocol);
        Assert.True(cfg.GenerationExport.Insecure);
        Assert.Equal("1", cfg.GenerationExport.Headers["X-A"]);
        Assert.Equal("two", cfg.GenerationExport.Headers["X-B"]);
    }

    [Theory]
    [MemberData(nameof(ValidExportTimeoutValues))]
    public void ExportTimeoutFromLegacyEnv(string raw, int expectedMs)
    {
        var env = new Dictionary<string, string?> { ["SIGIL_EXPORT_TIMEOUT_MS"] = raw };

        var (cfg, warnings) = EnvConfig.ResolveFromEnv(MapLookup(env), new Agento11yClientConfig());

        Assert.Equal(TimeSpan.FromMilliseconds(expectedMs), cfg.GenerationExport.ExportTimeout);
        Assert.Empty(warnings);
    }

    [Theory]
    [MemberData(nameof(ValidExportTimeoutValues))]
    public void ExportTimeoutFromPreferredEnv(string raw, int expectedMs)
    {
        var env = new Dictionary<string, string?> { ["AGENTO11Y_EXPORT_TIMEOUT_MS"] = raw };

        var (cfg, warnings) = EnvConfig.ResolveFromEnv(MapLookup(env), new Agento11yClientConfig());

        Assert.Equal(TimeSpan.FromMilliseconds(expectedMs), cfg.GenerationExport.ExportTimeout);
        Assert.Empty(warnings);
    }

    [Theory]
    [MemberData(nameof(InvalidExportTimeoutValues))]
    public void InvalidExportTimeoutWarnsAndPreservesOtherEnv(string raw)
    {
        var env = new Dictionary<string, string?>
        {
            ["AGENTO11Y_EXPORT_TIMEOUT_MS"] = raw,
            ["AGENTO11Y_ENDPOINT"] = "valid.example:4318",
            ["AGENTO11Y_AGENT_NAME"] = "valid-agent",
            ["AGENTO11Y_INSECURE"] = "true",
        };

        var (cfg, warnings) = EnvConfig.ResolveFromEnv(MapLookup(env), new Agento11yClientConfig());

        Assert.Equal(TimeSpan.FromSeconds(30), cfg.GenerationExport.ExportTimeout);
        Assert.False(cfg.GenerationExport.HasExplicitExportTimeout);
        Assert.Contains(warnings, w => w.Contains("AGENTO11Y_EXPORT_TIMEOUT_MS") && w.Contains(raw.Trim()));
        // A bad timeout must not discard the valid sibling env values.
        Assert.Equal("valid.example:4318", cfg.GenerationExport.Endpoint);
        Assert.Equal("valid-agent", cfg.AgentName);
        Assert.True(cfg.GenerationExport.Insecure);
    }

    [Theory]
    [MemberData(nameof(InvalidExportTimeoutValues))]
    public void InvalidLegacyExportTimeoutWarnsWithLegacyKey(string raw)
    {
        var env = new Dictionary<string, string?> { ["SIGIL_EXPORT_TIMEOUT_MS"] = raw };

        var (cfg, warnings) = EnvConfig.ResolveFromEnv(MapLookup(env), new Agento11yClientConfig());

        Assert.Equal(TimeSpan.FromSeconds(30), cfg.GenerationExport.ExportTimeout);
        Assert.Contains(warnings, w => w.Contains("SIGIL_EXPORT_TIMEOUT_MS"));
    }

    [Fact]
    public void InvalidPreferredExportTimeoutBlocksValidLegacy()
    {
        var env = new Dictionary<string, string?>
        {
            ["AGENTO11Y_EXPORT_TIMEOUT_MS"] = "2147483648",
            ["SIGIL_EXPORT_TIMEOUT_MS"] = "1500",
        };

        var (cfg, warnings) = EnvConfig.ResolveFromEnv(MapLookup(env), new Agento11yClientConfig());

        Assert.Equal(TimeSpan.FromSeconds(30), cfg.GenerationExport.ExportTimeout);
        Assert.Contains(warnings, w => w.Contains("AGENTO11Y_EXPORT_TIMEOUT_MS"));
    }

    [Fact]
    public void PreferredExportTimeoutWinsOverLegacy()
    {
        var env = new Dictionary<string, string?>
        {
            ["AGENTO11Y_EXPORT_TIMEOUT_MS"] = "250",
            ["SIGIL_EXPORT_TIMEOUT_MS"] = "1500",
        };

        var (cfg, _) = EnvConfig.ResolveFromEnv(MapLookup(env), new Agento11yClientConfig());

        Assert.Equal(TimeSpan.FromMilliseconds(250), cfg.GenerationExport.ExportTimeout);
    }

    [Fact]
    public void CallerExportTimeoutWinsOverEnv()
    {
        var baseConfig = new Agento11yClientConfig();
        baseConfig.GenerationExport.ExportTimeout = TimeSpan.FromSeconds(5);
        var env = new Dictionary<string, string?>
        {
            ["AGENTO11Y_EXPORT_TIMEOUT_MS"] = "1500",
            ["SIGIL_EXPORT_TIMEOUT_MS"] = "1600",
        };

        var (cfg, _) = EnvConfig.ResolveFromEnv(MapLookup(env), baseConfig);

        Assert.Equal(TimeSpan.FromSeconds(5), cfg.GenerationExport.ExportTimeout);
    }

    [Fact]
    public void CallerExportTimeoutEqualToDefaultStillWinsOverEnv()
    {
        // Tri-state backing field: an explicit 30s is "set", not "unset", so env
        // must not overwrite it.
        var baseConfig = new Agento11yClientConfig();
        baseConfig.GenerationExport.ExportTimeout = TimeSpan.FromSeconds(30);
        var env = new Dictionary<string, string?> { ["AGENTO11Y_EXPORT_TIMEOUT_MS"] = "1500" };

        var (cfg, _) = EnvConfig.ResolveFromEnv(MapLookup(env), baseConfig);

        Assert.True(cfg.GenerationExport.HasExplicitExportTimeout);
        Assert.Equal(TimeSpan.FromSeconds(30), cfg.GenerationExport.ExportTimeout);
    }

    [Theory]
    [MemberData(nameof(InvalidExportTimeoutValues))]
    public void ParseExportTimeoutMsRejectsInvalidValues(string raw)
    {
        Assert.Null(EnvConfig.ParseExportTimeoutMs(raw));
    }

    [Theory]
    [MemberData(nameof(ValidExportTimeoutValues))]
    public void ParseExportTimeoutMsAcceptsValidValues(string raw, int expectedMs)
    {
        Assert.Equal(expectedMs, EnvConfig.ParseExportTimeoutMs(raw));
    }

    [Fact]
    public void BasicAuthFromEnv()
    {
        var env = new Dictionary<string, string?>
        {
            ["SIGIL_AUTH_MODE"] = "basic",
            ["SIGIL_AUTH_TENANT_ID"] = "42",
            ["SIGIL_AUTH_TOKEN"] = "glc_xxx",
        };

        var (cfg, _) = EnvConfig.ResolveFromEnv(MapLookup(env), new Agento11yClientConfig());
        var auth = cfg.GenerationExport.Auth;

        Assert.Equal(ExportAuthMode.Basic, auth.Mode);
        Assert.Equal("42", auth.TenantId);
        Assert.Equal("glc_xxx", auth.BasicPassword);
        Assert.Equal("42", auth.BasicUser);
    }

    [Fact]
    public void BearerAuthFromEnv()
    {
        var env = new Dictionary<string, string?>
        {
            ["SIGIL_AUTH_MODE"] = "bearer",
            ["SIGIL_AUTH_TOKEN"] = "tok",
        };

        var (cfg, _) = EnvConfig.ResolveFromEnv(MapLookup(env), new Agento11yClientConfig());
        var auth = cfg.GenerationExport.Auth;

        Assert.Equal(ExportAuthMode.Bearer, auth.Mode);
        Assert.Equal("tok", auth.BearerToken);
    }

    [Fact]
    public void InvalidAuthModeWarnsAndPreservesOtherEnv()
    {
        var env = new Dictionary<string, string?>
        {
            ["SIGIL_AUTH_MODE"] = "Bearrer",
            ["SIGIL_ENDPOINT"] = "valid.example:4318",
            ["SIGIL_AGENT_NAME"] = "valid-agent",
            ["SIGIL_USER_ID"] = "alice",
        };

        var (cfg, warnings) = EnvConfig.ResolveFromEnv(MapLookup(env), new Agento11yClientConfig());

        Assert.Equal("valid.example:4318", cfg.GenerationExport.Endpoint);
        Assert.Equal("valid-agent", cfg.AgentName);
        Assert.Equal("alice", cfg.UserId);
        Assert.Equal(ExportAuthMode.None, cfg.GenerationExport.Auth.Mode);
        Assert.Contains(warnings, w => w.Contains("SIGIL_AUTH_MODE"));
    }

    [Fact]
    public void InvalidContentCaptureModeWarnsAndPreservesOtherEnv()
    {
        var env = new Dictionary<string, string?>
        {
            ["SIGIL_CONTENT_CAPTURE_MODE"] = "bogus",
            ["SIGIL_ENDPOINT"] = "valid.example:4318",
        };

        var (cfg, warnings) = EnvConfig.ResolveFromEnv(MapLookup(env), new Agento11yClientConfig());

        Assert.Equal(ContentCaptureMode.Default, cfg.ContentCapture);
        Assert.Equal("valid.example:4318", cfg.GenerationExport.Endpoint);
        Assert.Contains(warnings, w => w.Contains("SIGIL_CONTENT_CAPTURE_MODE"));
    }

    [Fact]
    public void InvalidContentCaptureModeKeepsCallerBaseValue()
    {
        var baseConfig = new Agento11yClientConfig { ContentCapture = ContentCaptureMode.MetadataOnly };
        var env = new Dictionary<string, string?> { ["SIGIL_CONTENT_CAPTURE_MODE"] = "bogus" };

        var (cfg, _) = EnvConfig.ResolveFromEnv(MapLookup(env), baseConfig);

        Assert.Equal(ContentCaptureMode.MetadataOnly, cfg.ContentCapture);
    }

    [Fact]
    public void ContentCaptureModeFromEnv()
    {
        var env = new Dictionary<string, string?> { ["SIGIL_CONTENT_CAPTURE_MODE"] = "metadata_only" };
        var (cfg, _) = EnvConfig.ResolveFromEnv(MapLookup(env), new Agento11yClientConfig());
        Assert.Equal(ContentCaptureMode.MetadataOnly, cfg.ContentCapture);
    }

    [Fact]
    public void ContentCaptureModeFullWithMetadataSpansFromEnv()
    {
        var env = new Dictionary<string, string?>
        {
            ["SIGIL_CONTENT_CAPTURE_MODE"] = "full_with_metadata_spans",
        };
        var (cfg, _) = EnvConfig.ResolveFromEnv(MapLookup(env), new Agento11yClientConfig());
        Assert.Equal(ContentCaptureMode.FullWithMetadataSpans, cfg.ContentCapture);
    }

    [Fact]
    public void AgentUserTagsDebugFromEnv()
    {
        var env = new Dictionary<string, string?>
        {
            ["SIGIL_AGENT_NAME"] = "planner",
            ["SIGIL_AGENT_VERSION"] = "1.2.3",
            ["SIGIL_USER_ID"] = "alice@example.com",
            ["SIGIL_TAGS"] = "service=orchestrator,env=prod",
            ["SIGIL_DEBUG"] = "true",
        };

        var (cfg, _) = EnvConfig.ResolveFromEnv(MapLookup(env), new Agento11yClientConfig());

        Assert.Equal("planner", cfg.AgentName);
        Assert.Equal("1.2.3", cfg.AgentVersion);
        Assert.Equal("alice@example.com", cfg.UserId);
        Assert.Equal("orchestrator", cfg.Tags["service"]);
        Assert.Equal("prod", cfg.Tags["env"]);
        Assert.True(cfg.Debug);
    }

    [Fact]
    public void WhitespaceOnlyValuesAreIgnored()
    {
        var env = new Dictionary<string, string?>
        {
            ["SIGIL_TAGS"] = "   ",
            ["SIGIL_AGENT_NAME"] = "",
            ["SIGIL_USER_ID"] = "\t \n",
        };

        var (cfg, _) = EnvConfig.ResolveFromEnv(MapLookup(env), new Agento11yClientConfig());

        Assert.Empty(cfg.Tags);
        Assert.Equal(string.Empty, cfg.AgentName);
        Assert.Equal(string.Empty, cfg.UserId);
    }

    [Fact]
    public void CallerEndpointWinsOverEnv()
    {
        var baseConfig = new Agento11yClientConfig();
        baseConfig.GenerationExport.Endpoint = "https://caller-host";
        var env = new Dictionary<string, string?> { ["SIGIL_ENDPOINT"] = "https://env-host" };

        var (cfg, _) = EnvConfig.ResolveFromEnv(MapLookup(env), baseConfig);

        Assert.Equal("https://caller-host", cfg.GenerationExport.Endpoint);
    }

    [Fact]
    public void CallerInsecureFalseBeatsEnvTrue()
    {
        var baseConfig = new Agento11yClientConfig();
        baseConfig.GenerationExport.Insecure = false;
        var env = new Dictionary<string, string?> { ["SIGIL_INSECURE"] = "true" };

        var (cfg, _) = EnvConfig.ResolveFromEnv(MapLookup(env), baseConfig);

        Assert.False(cfg.GenerationExport.Insecure);
    }

    [Fact]
    public void EnvInsecureTrueLayersUnderUnsetCaller()
    {
        var env = new Dictionary<string, string?> { ["SIGIL_INSECURE"] = "true" };
        var (cfg, _) = EnvConfig.ResolveFromEnv(MapLookup(env), new Agento11yClientConfig());
        Assert.True(cfg.GenerationExport.Insecure);
    }

    [Fact]
    public void UnsetInsecureRemainsNull()
    {
        var (cfg, _) = EnvConfig.ResolveFromEnv(EmptyLookup, new Agento11yClientConfig());
        Assert.Null(cfg.GenerationExport.Insecure);
    }

    [Fact]
    public void AuthTokenFillsBothBearerAndBasicWhenEmpty()
    {
        var env = new Dictionary<string, string?> { ["SIGIL_AUTH_TOKEN"] = "secret" };
        var (cfg, _) = EnvConfig.ResolveFromEnv(MapLookup(env), new Agento11yClientConfig());
        var auth = cfg.GenerationExport.Auth;
        Assert.Equal("secret", auth.BearerToken);
        Assert.Equal("secret", auth.BasicPassword);
    }

    [Fact]
    public void AuthTokenSkipsAlreadyFilledFields()
    {
        var baseConfig = new Agento11yClientConfig();
        baseConfig.GenerationExport.Auth.BearerToken = "caller-bearer";
        var env = new Dictionary<string, string?> { ["SIGIL_AUTH_TOKEN"] = "env-token" };

        var (cfg, _) = EnvConfig.ResolveFromEnv(MapLookup(env), baseConfig);
        var auth = cfg.GenerationExport.Auth;

        Assert.Equal("caller-bearer", auth.BearerToken);
        Assert.Equal("env-token", auth.BasicPassword);
    }

    [Fact]
    public void BasicModeUsesTenantAsBasicUserFallback()
    {
        var env = new Dictionary<string, string?>
        {
            ["SIGIL_AUTH_MODE"] = "basic",
            ["SIGIL_AUTH_TENANT_ID"] = "tenant-a",
            ["SIGIL_AUTH_TOKEN"] = "secret",
        };

        var (cfg, _) = EnvConfig.ResolveFromEnv(MapLookup(env), new Agento11yClientConfig());
        var auth = cfg.GenerationExport.Auth;

        Assert.Equal(ExportAuthMode.Basic, auth.Mode);
        Assert.Equal("tenant-a", auth.BasicUser);
        Assert.Equal("secret", auth.BasicPassword);
    }

    [Fact]
    public void StrayTenantIdKeepsModeNone()
    {
        var env = new Dictionary<string, string?> { ["SIGIL_AUTH_TENANT_ID"] = "42" };
        var (cfg, _) = EnvConfig.ResolveFromEnv(MapLookup(env), new Agento11yClientConfig());
        Assert.Equal(ExportAuthMode.None, cfg.GenerationExport.Auth.Mode);
        Assert.Equal("42", cfg.GenerationExport.Auth.TenantId);
    }

    [Fact]
    public void ParseCsvKvHandlesEdgeCases()
    {
        var result = EnvConfig.ParseCsvKv("a=1, b = two ,, =skip,c=");
        Assert.Equal("1", result["a"]);
        Assert.Equal("two", result["b"]);
        Assert.Equal(string.Empty, result["c"]);
        Assert.False(result.ContainsKey(""));
        Assert.Equal(3, result.Count);
    }

    [Fact]
    public void EnvTagsMergeUnderCallerTags()
    {
        var baseConfig = new Agento11yClientConfig
        {
            Tags = new Dictionary<string, string>
            {
                ["team"] = "ai",
                ["env"] = "staging",
            },
        };
        var env = new Dictionary<string, string?> { ["SIGIL_TAGS"] = "service=orch,env=prod" };

        var (cfg, _) = EnvConfig.ResolveFromEnv(MapLookup(env), baseConfig);

        Assert.Equal("orch", cfg.Tags["service"]);   // env-only fills
        Assert.Equal("ai", cfg.Tags["team"]);        // caller-only preserved
        Assert.Equal("staging", cfg.Tags["env"]);    // caller wins on collision
    }

    [Fact]
    public void ParseBoolAcceptsCanonicalTrue()
    {
        Assert.True(EnvConfig.ParseBool("1"));
        Assert.True(EnvConfig.ParseBool("true"));
        Assert.True(EnvConfig.ParseBool("YES"));
        Assert.True(EnvConfig.ParseBool("On"));
        Assert.False(EnvConfig.ParseBool("0"));
        Assert.False(EnvConfig.ParseBool("false"));
        Assert.False(EnvConfig.ParseBool("garbage"));
    }

    [Fact]
    public void FromEnvUsesProcessEnv()
    {
        // Smoke: just verify it doesn't throw and returns a config.
        var cfg = EnvConfig.FromEnv();
        Assert.NotNull(cfg);
    }

    [Fact]
    public void ResolveMutatesBaseInPlace()
    {
        // .NET's ConfigResolver mutates in-place to preserve the existing
        // contract where callers can read back the resolved Headers from the
        // config object they supplied.
        var baseConfig = new Agento11yClientConfig { AgentName = "base-agent" };
        var env = new Dictionary<string, string?> { ["SIGIL_USER_ID"] = "alice" };

        var (resolved, _) = EnvConfig.ResolveFromEnv(MapLookup(env), baseConfig);

        Assert.Same(baseConfig, resolved);
        Assert.Equal("alice", baseConfig.UserId);
        Assert.Equal("base-agent", baseConfig.AgentName);
    }

    [Fact]
    public void PreferredOnlyEnvMatchesLegacyOnlyResolution()
    {
        var values = new Dictionary<string, string?>
        {
            ["ENDPOINT"] = "https://env:4318",
            ["PROTOCOL"] = "http",
            ["INSECURE"] = "true",
            ["HEADERS"] = "X-A=1,X-B=two",
            ["EXPORT_TIMEOUT_MS"] = "1500",
            ["AUTH_MODE"] = "basic",
            ["AUTH_TENANT_ID"] = "42",
            ["AUTH_TOKEN"] = "glc_xxx",
            ["AGENT_NAME"] = "planner",
            ["AGENT_VERSION"] = "1.2.3",
            ["USER_ID"] = "alice@example.com",
            ["TAGS"] = "service=orchestrator,env=prod",
            ["CONTENT_CAPTURE_MODE"] = "metadata_only",
            ["DEBUG"] = "true",
        };
        var preferredEnv = values.ToDictionary(kv => "AGENTO11Y_" + kv.Key, kv => kv.Value);
        var legacyEnv = values.ToDictionary(kv => "SIGIL_" + kv.Key, kv => kv.Value);

        var (preferred, preferredWarnings) = EnvConfig.ResolveFromEnv(MapLookup(preferredEnv), new Agento11yClientConfig());
        var (legacy, legacyWarnings) = EnvConfig.ResolveFromEnv(MapLookup(legacyEnv), new Agento11yClientConfig());

        Assert.Empty(preferredWarnings);
        Assert.Empty(legacyWarnings);
        Assert.Equal("https://env:4318", preferred.GenerationExport.Endpoint);
        Assert.Equal(legacy.GenerationExport.Endpoint, preferred.GenerationExport.Endpoint);
        Assert.Equal(legacy.GenerationExport.Protocol, preferred.GenerationExport.Protocol);
        Assert.Equal(legacy.GenerationExport.Insecure, preferred.GenerationExport.Insecure);
        Assert.Equal(legacy.GenerationExport.Headers, preferred.GenerationExport.Headers);
        Assert.Equal(TimeSpan.FromMilliseconds(1500), preferred.GenerationExport.ExportTimeout);
        Assert.Equal(legacy.GenerationExport.ExportTimeout, preferred.GenerationExport.ExportTimeout);
        Assert.Equal(legacy.GenerationExport.Auth.Mode, preferred.GenerationExport.Auth.Mode);
        Assert.Equal(legacy.GenerationExport.Auth.TenantId, preferred.GenerationExport.Auth.TenantId);
        Assert.Equal(legacy.GenerationExport.Auth.BasicUser, preferred.GenerationExport.Auth.BasicUser);
        Assert.Equal(legacy.GenerationExport.Auth.BasicPassword, preferred.GenerationExport.Auth.BasicPassword);
        Assert.Equal(legacy.GenerationExport.Auth.BearerToken, preferred.GenerationExport.Auth.BearerToken);
        Assert.Equal(legacy.AgentName, preferred.AgentName);
        Assert.Equal(legacy.AgentVersion, preferred.AgentVersion);
        Assert.Equal(legacy.UserId, preferred.UserId);
        Assert.Equal(legacy.Tags, preferred.Tags);
        Assert.Equal(legacy.ContentCapture, preferred.ContentCapture);
        Assert.Equal(legacy.Debug, preferred.Debug);
    }

    [Fact]
    public void PreferredWinsOverLegacyOnConflict()
    {
        var env = new Dictionary<string, string?>
        {
            ["AGENTO11Y_ENDPOINT"] = "preferred.example:4318",
            ["SIGIL_ENDPOINT"] = "legacy.example:4318",
        };

        var (cfg, _) = EnvConfig.ResolveFromEnv(MapLookup(env), new Agento11yClientConfig());

        Assert.Equal("preferred.example:4318", cfg.GenerationExport.Endpoint);
    }

    [Fact]
    public void BlankPreferredFallsThroughToLegacy()
    {
        var env = new Dictionary<string, string?>
        {
            ["AGENTO11Y_ENDPOINT"] = "   ",
            ["SIGIL_ENDPOINT"] = "legacy.example:4318",
        };

        var (cfg, _) = EnvConfig.ResolveFromEnv(MapLookup(env), new Agento11yClientConfig());

        Assert.Equal("legacy.example:4318", cfg.GenerationExport.Endpoint);
    }

    [Fact]
    public void InvalidPreferredContentCaptureModeBlocksValidLegacy()
    {
        var env = new Dictionary<string, string?>
        {
            ["AGENTO11Y_CONTENT_CAPTURE_MODE"] = "bogus",
            ["SIGIL_CONTENT_CAPTURE_MODE"] = "metadata_only",
        };

        var (cfg, warnings) = EnvConfig.ResolveFromEnv(MapLookup(env), new Agento11yClientConfig());

        Assert.Equal(ContentCaptureMode.Default, cfg.ContentCapture);
        Assert.Contains(warnings, w => w.Contains("AGENTO11Y_CONTENT_CAPTURE_MODE"));
    }

    [Fact]
    public void InvalidPreferredAuthModeBlocksValidLegacy()
    {
        var env = new Dictionary<string, string?>
        {
            ["AGENTO11Y_AUTH_MODE"] = "garbage",
            ["SIGIL_AUTH_MODE"] = "bearer",
        };

        var (cfg, warnings) = EnvConfig.ResolveFromEnv(MapLookup(env), new Agento11yClientConfig());

        Assert.Equal(ExportAuthMode.None, cfg.GenerationExport.Auth.Mode);
        Assert.Contains(warnings, w => w.Contains("AGENTO11Y_AUTH_MODE"));
    }

    [Fact]
    public void CallerValueBeatsBothSpellings()
    {
        var baseConfig = new Agento11yClientConfig();
        baseConfig.GenerationExport.Endpoint = "https://caller-host";
        var env = new Dictionary<string, string?>
        {
            ["AGENTO11Y_ENDPOINT"] = "preferred.example:4318",
            ["SIGIL_ENDPOINT"] = "legacy.example:4318",
        };

        var (cfg, _) = EnvConfig.ResolveFromEnv(MapLookup(env), baseConfig);

        Assert.Equal("https://caller-host", cfg.GenerationExport.Endpoint);
    }

    [Fact]
    public void MixedPrefixAuthFieldsResolvePerField()
    {
        var env = new Dictionary<string, string?>
        {
            ["AGENTO11Y_AUTH_MODE"] = "basic",
            ["SIGIL_AUTH_TENANT_ID"] = "42",
            ["SIGIL_AUTH_TOKEN"] = "glc_xxx",
        };

        var (cfg, _) = EnvConfig.ResolveFromEnv(MapLookup(env), new Agento11yClientConfig());
        var auth = cfg.GenerationExport.Auth;

        Assert.Equal(ExportAuthMode.Basic, auth.Mode);
        Assert.Equal("42", auth.TenantId);
        Assert.Equal("42", auth.BasicUser);
        Assert.Equal("glc_xxx", auth.BasicPassword);
    }

    [Fact]
    public void PreferredTagsReplaceLegacyTagsWithoutMerging()
    {
        var env = new Dictionary<string, string?>
        {
            ["AGENTO11Y_TAGS"] = "team=ai",
            ["SIGIL_TAGS"] = "service=orch,env=prod",
        };

        var (cfg, _) = EnvConfig.ResolveFromEnv(MapLookup(env), new Agento11yClientConfig());

        Assert.Equal("ai", cfg.Tags["team"]);
        Assert.False(cfg.Tags.ContainsKey("service"));
        Assert.Single(cfg.Tags);
    }

    [Fact]
    public void LegacyConstantsKeepAgento11yValues()
    {
        Assert.Equal("SIGIL_ENDPOINT", EnvConfig.EnvEndpoint);
        Assert.Equal("SIGIL_PROTOCOL", EnvConfig.EnvProtocol);
        Assert.Equal("SIGIL_INSECURE", EnvConfig.EnvInsecure);
        Assert.Equal("SIGIL_HEADERS", EnvConfig.EnvHeaders);
        Assert.Equal("SIGIL_EXPORT_TIMEOUT_MS", EnvConfig.EnvExportTimeoutMs);
        Assert.Equal("SIGIL_AUTH_MODE", EnvConfig.EnvAuthMode);
        Assert.Equal("SIGIL_AUTH_TENANT_ID", EnvConfig.EnvAuthTenantId);
        Assert.Equal("SIGIL_AUTH_TOKEN", EnvConfig.EnvAuthToken);
        Assert.Equal("SIGIL_AGENT_NAME", EnvConfig.EnvAgentName);
        Assert.Equal("SIGIL_AGENT_VERSION", EnvConfig.EnvAgentVersion);
        Assert.Equal("SIGIL_USER_ID", EnvConfig.EnvUserId);
        Assert.Equal("SIGIL_TAGS", EnvConfig.EnvTags);
        Assert.Equal("SIGIL_CONTENT_CAPTURE_MODE", EnvConfig.EnvContentCaptureMode);
        Assert.Equal("SIGIL_DEBUG", EnvConfig.EnvDebug);
    }

    [Fact]
    public void PreferredConstantsUseAgentO11yValues()
    {
        Assert.Equal("AGENTO11Y_ENDPOINT", EnvConfig.PreferredEnvEndpoint);
        Assert.Equal("AGENTO11Y_PROTOCOL", EnvConfig.PreferredEnvProtocol);
        Assert.Equal("AGENTO11Y_INSECURE", EnvConfig.PreferredEnvInsecure);
        Assert.Equal("AGENTO11Y_HEADERS", EnvConfig.PreferredEnvHeaders);
        Assert.Equal("AGENTO11Y_EXPORT_TIMEOUT_MS", EnvConfig.PreferredEnvExportTimeoutMs);
        Assert.Equal("AGENTO11Y_AUTH_MODE", EnvConfig.PreferredEnvAuthMode);
        Assert.Equal("AGENTO11Y_AUTH_TENANT_ID", EnvConfig.PreferredEnvAuthTenantId);
        Assert.Equal("AGENTO11Y_AUTH_TOKEN", EnvConfig.PreferredEnvAuthToken);
        Assert.Equal("AGENTO11Y_AGENT_NAME", EnvConfig.PreferredEnvAgentName);
        Assert.Equal("AGENTO11Y_AGENT_VERSION", EnvConfig.PreferredEnvAgentVersion);
        Assert.Equal("AGENTO11Y_USER_ID", EnvConfig.PreferredEnvUserId);
        Assert.Equal("AGENTO11Y_TAGS", EnvConfig.PreferredEnvTags);
        Assert.Equal("AGENTO11Y_CONTENT_CAPTURE_MODE", EnvConfig.PreferredEnvContentCaptureMode);
        Assert.Equal("AGENTO11Y_DEBUG", EnvConfig.PreferredEnvDebug);
    }
}
