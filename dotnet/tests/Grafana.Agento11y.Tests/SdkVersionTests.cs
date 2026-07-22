using System.Reflection;
using Xunit;

namespace Grafana.Agento11y.Tests;

public sealed class SdkVersionTests
{
    [Fact]
    public void VersionMatchesAssemblyInformationalVersion()
    {
        var informational = typeof(SdkVersion).Assembly
            .GetCustomAttribute<AssemblyInformationalVersionAttribute>()?.InformationalVersion;
        if (string.IsNullOrWhiteSpace(informational))
        {
            Assert.Equal("0.0.0+unknown", SdkVersion.Version);
            return;
        }

        var plus = informational.IndexOf('+');
        var expected = plus > 0 ? informational[..plus] : informational;
        Assert.Equal(expected, SdkVersion.Version);
    }

    [Fact]
    public void UserAgentContainsProductTokenAndVersion()
    {
        var userAgent = SdkVersion.UserAgent();
        Assert.StartsWith("agento11y-sdk-dotnet/", userAgent);

        var version = userAgent["agento11y-sdk-dotnet/".Length..];
        Assert.NotEmpty(version);
        Assert.Equal(SdkVersion.Version, version);

        // The SourceLink "+<sha>" suffix must be stripped; only the fallback
        // legitimately contains '+'.
        if (version != "0.0.0+unknown")
        {
            Assert.DoesNotContain('+', version);
        }
    }
}
