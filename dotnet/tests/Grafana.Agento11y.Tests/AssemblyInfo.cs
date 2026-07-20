using System.Runtime.CompilerServices;
using Xunit;

[assembly: CollectionBehavior(DisableTestParallelization = true)]

internal static class TestEnvCleanup
{
    [ModuleInitializer]
    internal static void ClearSigilEnvVars()
    {
        // Clear canonical AGENTO11Y_* / SIGIL_* env vars so individual tests
        // don't pick up developer-machine config when they construct a
        // SigilClient. Tests that exercise env layering should pass an
        // explicit lookup to EnvConfig.ResolveFromEnv.
        string[] suffixes =
        {
            "ENDPOINT",
            "PROTOCOL",
            "INSECURE",
            "HEADERS",
            "AUTH_MODE",
            "AUTH_TENANT_ID",
            "AUTH_TOKEN",
            "AGENT_NAME",
            "AGENT_VERSION",
            "USER_ID",
            "TAGS",
            "CONTENT_CAPTURE_MODE",
            "DEBUG",
        };
        foreach (var suffix in suffixes)
        {
            Environment.SetEnvironmentVariable("AGENTO11Y_" + suffix, null);
            Environment.SetEnvironmentVariable("SIGIL_" + suffix, null);
        }
    }
}
