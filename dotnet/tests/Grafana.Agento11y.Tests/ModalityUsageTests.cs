using System.Text.Json;
using Xunit;

namespace Grafana.Agento11y.Tests;

public sealed class ModalityUsageTests
{
    [Fact]
    public void PreservesThinkingAndCompleteness()
    {
        using var doc = JsonDocument.Parse("[{\"modality\":\"IMAGE\",\"tokenCount\":10}]");
        var partition = ModalityUsage.Partition(doc.RootElement, 12, 2)!;
        Assert.True(partition.Complete);
        Assert.Equal(10, partition.Tokens["image"]);
        Assert.Equal(2, partition.Tokens["text"]);
        Assert.Null(ModalityUsage.Partition(default, 0));
    }
}
