using System.Reflection;

namespace Grafana.Agento11y;

/// <summary>
/// SDK version and User-Agent product token.
/// </summary>
/// <remarks>
/// <see cref="Version"/> is stamped into the default generation-export User-Agent
/// (see <see cref="UserAgent"/>). Resolved from the assembly's informational version;
/// <c>0.0.0+unknown</c> when that metadata is unavailable.
/// </remarks>
public static class SdkVersion
{
    /// <summary>Version of the Agento11y .NET SDK.</summary>
    public static readonly string Version = ResolveVersion();

    private const string UserAgentProduct = "agento11y-sdk-dotnet";

    private static string ResolveVersion()
    {
        var informational = typeof(SdkVersion).Assembly
            .GetCustomAttribute<AssemblyInformationalVersionAttribute>()?.InformationalVersion;
        if (informational == null || string.IsNullOrWhiteSpace(informational))
        {
            return "0.0.0+unknown";
        }

        // SourceLink/ContinuousIntegrationBuild appends "+<sha>"; strip it.
        var plus = informational.IndexOf('+');
        return plus > 0 ? informational.Substring(0, plus) : informational;
    }

    /// <summary>
    /// Returns the SDK's default generation-export User-Agent product token,
    /// <c>agento11y-sdk-dotnet/&lt;Version&gt;</c>.
    /// </summary>
    public static string UserAgent() => UserAgentProduct + "/" + Version;
}
