using System.Text;
using Xunit;

namespace Grafana.Sigil.Tests;

public sealed class AuthConfigTests
{
    public static TheoryData<AuthConfig, string> InvalidGenerationAuthConfigs =>
        new()
        {
            {
                new AuthConfig
                {
                    Mode = ExportAuthMode.Tenant,
                },
                "generation auth mode 'tenant' requires tenant_id"
            },
            {
                new AuthConfig
                {
                    Mode = ExportAuthMode.Bearer,
                },
                "generation auth mode 'bearer' requires bearer_token"
            },
            {
                new AuthConfig
                {
                    Mode = (ExportAuthMode)99,
                },
                "unsupported generation auth mode"
            },
            {
                new AuthConfig
                {
                    Mode = ExportAuthMode.Basic,
                },
                "generation auth mode 'basic' requires basic_password"
            },
            {
                new AuthConfig
                {
                    Mode = ExportAuthMode.Basic,
                    BasicPassword = "secret",
                },
                "generation auth mode 'basic' requires basic_user or tenant_id"
            },
        };

    [Fact]
    public void ModeNoneIgnoresIrrelevantCredentialFields()
    {
        // env layering can populate cross-mode fields without explicit SIGIL_AUTH_MODE.
        var auth = new AuthConfig
        {
            Mode = ExportAuthMode.None,
            TenantId = "42",
            BearerToken = "tok",
            BasicUser = "user",
            BasicPassword = "pass",
        };
        var headers = ConfigResolver.ResolveHeadersWithAuth(
            new Dictionary<string, string>(),
            auth,
            "generation"
        );
        Assert.Empty(headers);
    }

    [Fact]
    public void ModeBearerIgnoresIrrelevantTenantField()
    {
        var auth = new AuthConfig
        {
            Mode = ExportAuthMode.Bearer,
            BearerToken = "tok",
            TenantId = "42",
        };
        var headers = ConfigResolver.ResolveHeadersWithAuth(
            new Dictionary<string, string>(),
            auth,
            "generation"
        );
        Assert.Equal("Bearer tok", headers["Authorization"]);
        Assert.False(headers.ContainsKey("X-Scope-OrgID"));
    }

    [Theory]
#pragma warning disable xUnit1044 // Avoid using TheoryData type arguments that are not serializable
    [MemberData(nameof(InvalidGenerationAuthConfigs))]
#pragma warning restore xUnit1044 // Avoid using TheoryData type arguments that are not serializable
    public void Constructor_RejectsInvalidGenerationAuthConfig(AuthConfig auth, string expected)
    {
        var config = TestHelpers.TestConfig(new CapturingGenerationExporter());
        config.GenerationExport.Auth = auth;

        var error = Assert.Throws<ArgumentException>(() => new SigilClient(config));

        Assert.Contains(expected, error.Message);
    }

    [Fact]
    public async Task Constructor_AppliesGenerationBearerHeaderFromAuth()
    {
        var config = TestHelpers.TestConfig(new CapturingGenerationExporter());
        config.GenerationExport.Auth = new AuthConfig
        {
            Mode = ExportAuthMode.Bearer,
            BearerToken = "token-a",
        };

        await using var client = new SigilClient(config);

        Assert.Equal("Bearer token-a", config.GenerationExport.Headers["Authorization"]);
    }

    [Fact]
    public async Task Constructor_PreservesExplicitGenerationAuthorizationHeader()
    {
        var config = TestHelpers.TestConfig(new CapturingGenerationExporter());
        config.GenerationExport.Headers["authorization"] = "Bearer override-token";
        config.GenerationExport.Auth = new AuthConfig
        {
            Mode = ExportAuthMode.Bearer,
            BearerToken = "token-from-auth",
        };

        await using var client = new SigilClient(config);

        Assert.Equal("Bearer override-token", config.GenerationExport.Headers["authorization"]);
    }

    [Fact]
    public async Task Constructor_AppliesBasicAuthWithTenantId()
    {
        var config = TestHelpers.TestConfig(new CapturingGenerationExporter());
        config.GenerationExport.Auth = new AuthConfig
        {
            Mode = ExportAuthMode.Basic,
            TenantId = "42",
            BasicPassword = "secret",
        };

        await using var client = new SigilClient(config);

        var expected = "Basic " + Convert.ToBase64String(Encoding.UTF8.GetBytes("42:secret"));
        Assert.Equal(expected, config.GenerationExport.Headers["Authorization"]);
        Assert.Equal("42", config.GenerationExport.Headers["X-Scope-OrgID"]);
    }

    [Fact]
    public async Task Constructor_AppliesBasicAuthWithExplicitUser()
    {
        var config = TestHelpers.TestConfig(new CapturingGenerationExporter());
        config.GenerationExport.Auth = new AuthConfig
        {
            Mode = ExportAuthMode.Basic,
            TenantId = "42",
            BasicUser = "probe-user",
            BasicPassword = "secret",
        };

        await using var client = new SigilClient(config);

        var expected = "Basic " + Convert.ToBase64String(Encoding.UTF8.GetBytes("probe-user:secret"));
        Assert.Equal(expected, config.GenerationExport.Headers["Authorization"]);
        Assert.Equal("42", config.GenerationExport.Headers["X-Scope-OrgID"]);
    }

    [Fact]
    public async Task Constructor_BasicAuthExplicitHeaderWins()
    {
        var config = TestHelpers.TestConfig(new CapturingGenerationExporter());
        config.GenerationExport.Headers["Authorization"] = "Basic override";
        config.GenerationExport.Headers["X-Scope-OrgID"] = "override-tenant";
        config.GenerationExport.Auth = new AuthConfig
        {
            Mode = ExportAuthMode.Basic,
            TenantId = "42",
            BasicPassword = "secret",
        };

        await using var client = new SigilClient(config);

        Assert.Equal("Basic override", config.GenerationExport.Headers["Authorization"]);
        Assert.Equal("override-tenant", config.GenerationExport.Headers["X-Scope-OrgID"]);
    }

}
