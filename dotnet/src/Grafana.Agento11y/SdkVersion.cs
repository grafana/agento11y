namespace Grafana.Agento11y;

/// <summary>
/// SDK version and User-Agent product token.
/// </summary>
/// <remarks>
/// <see cref="Version"/> is stamped into the default generation-export User-Agent
/// (see <see cref="UserAgent"/>). Keep in sync with the package version on release.
/// </remarks>
public static class SdkVersion
{
    /// <summary>Released version of the Sigil .NET SDK.</summary>
    public const string Version = "0.1.0";

    private const string UserAgentProduct = "agento11y-sdk-dotnet";

    /// <summary>
    /// Returns the SDK's default generation-export User-Agent product token,
    /// <c>agento11y-sdk-dotnet/&lt;Version&gt;</c>.
    /// </summary>
    public static string UserAgent() => UserAgentProduct + "/" + Version;
}
