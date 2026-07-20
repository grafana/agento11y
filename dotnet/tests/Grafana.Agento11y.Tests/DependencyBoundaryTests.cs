using System.Reflection;
using Xunit;

namespace Grafana.Agento11y.Tests;

public sealed class DependencyBoundaryTests
{
    [Fact]
    public void CoreProject_DoesNotDependOnProviderSdkPackages()
    {
        var solutionRoot = typeof(DependencyBoundaryTests).Assembly
            .GetCustomAttributes<AssemblyMetadataAttribute>()
            .First((p) => p.Key is "SolutionRoot")
            .Value;

        var projectPath = Path.GetFullPath(
            Path.Combine(solutionRoot!, "src", "Grafana.Agento11y", "Grafana.Agento11y.csproj")
        );
        var content = File.ReadAllText(projectPath);

        Assert.DoesNotContain("PackageReference Include=\"OpenAI\"", content, StringComparison.OrdinalIgnoreCase);
        Assert.DoesNotContain("PackageReference Include=\"Anthropic\"", content, StringComparison.OrdinalIgnoreCase);
        Assert.DoesNotContain("PackageReference Include=\"Google.GenAI\"", content, StringComparison.OrdinalIgnoreCase);
    }
}
