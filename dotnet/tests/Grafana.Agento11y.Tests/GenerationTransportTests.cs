using Google.Protobuf;
using System.Text;
using Xunit;
using Agento11yProto = Agento11y.V1;

namespace Grafana.Agento11y.Tests;

public sealed class GenerationTransportTests
{
    [Fact]
    public async Task ExportsGenerationOverHttp_AllPropertiesRoundTrip()
    {
        using var server = new HttpCaptureServer((_, body) =>
        {
            var request = Google.Protobuf.JsonParser.Default.Parse<Agento11yProto.ExportGenerationsRequest>(
                Encoding.UTF8.GetString(body)
            );

            var response = new Agento11yProto.ExportGenerationsResponse();
            foreach (var generation in request.Generations)
            {
                response.Results.Add(new Agento11yProto.ExportGenerationResult
                {
                    GenerationId = generation.Id,
                    Accepted = true,
                });
            }

            return Encoding.UTF8.GetBytes(JsonFormatter.Default.Format(response));
        });

        var config = new Agento11yClientConfig
        {
            GenerationExport = new GenerationExportConfig
            {
                Protocol = GenerationExportProtocol.Http,
                Endpoint = $"http://127.0.0.1:{server.Port}",
                BatchSize = 1,
                QueueSize = 10,
                FlushInterval = TimeSpan.FromSeconds(1),
                MaxRetries = 1,
                InitialBackoff = TimeSpan.FromMilliseconds(1),
                MaxBackoff = TimeSpan.FromMilliseconds(2),
            },
        };

        await using var client = new Agento11yClient(config);
        var recorder = client.StartGeneration(TestHelpers.CreateSeedStart("gen-http"));
        recorder.SetResult(TestHelpers.CreateSeedResult("gen-http"));
        recorder.End();

        await client.ShutdownAsync(TestContext.Current.CancellationToken);

        Assert.True(server.Requests.TryDequeue(out var captured));
        var request = Google.Protobuf.JsonParser.Default.Parse<Agento11yProto.ExportGenerationsRequest>(
            Encoding.UTF8.GetString(captured.Body)
        );

        Assert.Single(request.Generations);
        GenerationAssertions.AssertEquivalent(recorder.LastGeneration!, request.Generations[0]);
    }

    [Fact]
    public async Task GenerationHttpTransport_AppliesTenantAuthHeader()
    {
        using var server = new HttpCaptureServer((_, body) =>
        {
            var request = Google.Protobuf.JsonParser.Default.Parse<Agento11yProto.ExportGenerationsRequest>(
                Encoding.UTF8.GetString(body)
            );

            var response = new Agento11yProto.ExportGenerationsResponse();
            foreach (var generation in request.Generations)
            {
                response.Results.Add(new Agento11yProto.ExportGenerationResult
                {
                    GenerationId = generation.Id,
                    Accepted = true,
                });
            }

            return Encoding.UTF8.GetBytes(JsonFormatter.Default.Format(response));
        });

        var config = new Agento11yClientConfig
        {
            GenerationExport = new GenerationExportConfig
            {
                Protocol = GenerationExportProtocol.Http,
                Endpoint = $"http://127.0.0.1:{server.Port}/api/v1/generations:export",
                Auth = new AuthConfig
                {
                    Mode = ExportAuthMode.Tenant,
                    TenantId = "tenant-a",
                },
                BatchSize = 1,
                QueueSize = 10,
                FlushInterval = TimeSpan.FromSeconds(1),
            },
        };

        await using var client = new Agento11yClient(config);
        var recorder = client.StartGeneration(TestHelpers.CreateSeedStart("gen-http-auth"));
        recorder.SetResult(TestHelpers.CreateSeedResult("gen-http-auth"));
        recorder.End();
        await client.ShutdownAsync(TestContext.Current.CancellationToken);

        Assert.True(server.Requests.TryDequeue(out var captured));
        Assert.Equal("tenant-a", captured.Headers["X-Scope-OrgID"]);
        Assert.Equal(SdkVersion.UserAgent(), captured.Headers["User-Agent"]);
    }

    // A non-blank caller User-Agent wins; a blank or whitespace-only one (or no
    // header at all) must fall back to the SDK default, matching gRPC.
    [Theory]
    [InlineData(null)]
    [InlineData("")]
    [InlineData("   ")]
    [InlineData("agento11y-plugin-semantic-kernel/1.2.3")]
    public async Task GenerationHttpTransport_ResolvesUserAgent(string? headerValue)
    {
        var expected = string.IsNullOrWhiteSpace(headerValue) ? SdkVersion.UserAgent() : headerValue;

        using var server = new HttpCaptureServer((_, body) =>
        {
            var request = Google.Protobuf.JsonParser.Default.Parse<Agento11yProto.ExportGenerationsRequest>(
                Encoding.UTF8.GetString(body)
            );

            var response = new Agento11yProto.ExportGenerationsResponse();
            foreach (var generation in request.Generations)
            {
                response.Results.Add(new Agento11yProto.ExportGenerationResult
                {
                    GenerationId = generation.Id,
                    Accepted = true,
                });
            }

            return Encoding.UTF8.GetBytes(JsonFormatter.Default.Format(response));
        });

        var config = new Agento11yClientConfig
        {
            GenerationExport = new GenerationExportConfig
            {
                Protocol = GenerationExportProtocol.Http,
                Endpoint = $"http://127.0.0.1:{server.Port}/api/v1/generations:export",
                BatchSize = 1,
                QueueSize = 10,
                FlushInterval = TimeSpan.FromSeconds(1),
            },
        };
        if (headerValue != null)
        {
            config.GenerationExport.Headers = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase)
            {
                ["User-Agent"] = headerValue,
            };
        }

        await using var client = new Agento11yClient(config);
        var recorder = client.StartGeneration(TestHelpers.CreateSeedStart("gen-http-ua"));
        recorder.SetResult(TestHelpers.CreateSeedResult("gen-http-ua"));
        recorder.End();
        await client.ShutdownAsync(TestContext.Current.CancellationToken);

        Assert.True(server.Requests.TryDequeue(out var captured));
        Assert.Equal(expected, captured.Headers["User-Agent"]);
    }

    [Fact]
    public async Task GenerationHttpTransport_ExplicitHeadersOverrideAuthInjection()
    {
        using var server = new HttpCaptureServer((_, body) =>
        {
            var request = Google.Protobuf.JsonParser.Default.Parse<Agento11yProto.ExportGenerationsRequest>(
                Encoding.UTF8.GetString(body)
            );

            var response = new Agento11yProto.ExportGenerationsResponse();
            foreach (var generation in request.Generations)
            {
                response.Results.Add(new Agento11yProto.ExportGenerationResult
                {
                    GenerationId = generation.Id,
                    Accepted = true,
                });
            }

            return Encoding.UTF8.GetBytes(JsonFormatter.Default.Format(response));
        });

        var config = new Agento11yClientConfig
        {
            GenerationExport = new GenerationExportConfig
            {
                Protocol = GenerationExportProtocol.Http,
                Endpoint = $"http://127.0.0.1:{server.Port}/api/v1/generations:export",
                Headers = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase)
                {
                    ["x-scope-orgid"] = "tenant-override",
                    ["authorization"] = "Bearer override-token",
                },
                Auth = new AuthConfig
                {
                    Mode = ExportAuthMode.Bearer,
                    BearerToken = "token-from-auth",
                },
                BatchSize = 1,
                QueueSize = 10,
                FlushInterval = TimeSpan.FromSeconds(1),
            },
        };

        await using var client = new Agento11yClient(config);
        var recorder = client.StartGeneration(TestHelpers.CreateSeedStart("gen-http-override"));
        recorder.SetResult(TestHelpers.CreateSeedResult("gen-http-override"));
        recorder.End();
        await client.ShutdownAsync(TestContext.Current.CancellationToken);

        Assert.True(server.Requests.TryDequeue(out var captured));
        Assert.Equal("tenant-override", captured.Headers["x-scope-orgid"]);
        Assert.Equal("Bearer override-token", captured.Headers["authorization"]);
    }

    [Fact]
    public async Task GenerationGrpcTransport_SendsDefaultUserAgent()
    {
        using var server = new GrpcIngestServer();

        var config = new Agento11yClientConfig
        {
            GenerationExport = new GenerationExportConfig
            {
                Protocol = GenerationExportProtocol.Grpc,
                Endpoint = $"127.0.0.1:{server.Port}",
                Insecure = true,
                BatchSize = 1,
                QueueSize = 10,
                FlushInterval = TimeSpan.FromSeconds(1),
                MaxRetries = 1,
                InitialBackoff = TimeSpan.FromMilliseconds(1),
                MaxBackoff = TimeSpan.FromMilliseconds(2),
            },
        };

        await using (var client = new Agento11yClient(config))
        {
            var recorder = client.StartGeneration(TestHelpers.CreateSeedStart("gen-grpc-ua"));
            recorder.SetResult(TestHelpers.CreateSeedResult("gen-grpc-ua"));
            recorder.End();
            await client.FlushAsync(TestContext.Current.CancellationToken);
            await client.ShutdownAsync(TestContext.Current.CancellationToken);
        }

        await TestHelpers.WaitForAsync(
            () => server.UserAgents.Count >= 1,
            TimeSpan.FromSeconds(5),
            "expected one gRPC export request"
        );
        var userAgent = server.UserAgents[0];
        // grpc-dotnet appends its own token after ours.
        Assert.Equal(SdkVersion.UserAgent(), userAgent.Split(' ', 2)[0]);
    }

    [Fact]
    public async Task GenerationTransport_NoneProtocol_RecordsWithoutSending()
    {
        var config = new Agento11yClientConfig
        {
            GenerationExport = new GenerationExportConfig
            {
                Protocol = GenerationExportProtocol.None,
                Endpoint = "http://127.0.0.1:1",
                BatchSize = 1,
                QueueSize = 10,
                FlushInterval = TimeSpan.FromSeconds(1),
                MaxRetries = 1,
                InitialBackoff = TimeSpan.FromMilliseconds(1),
                MaxBackoff = TimeSpan.FromMilliseconds(2),
            },
        };

        await using var client = new Agento11yClient(config);
        var recorder = client.StartGeneration(TestHelpers.CreateSeedStart("gen-none"));
        recorder.SetResult(TestHelpers.CreateSeedResult("gen-none"));
        recorder.End();

        await client.FlushAsync(TestContext.Current.CancellationToken);
        await client.ShutdownAsync(TestContext.Current.CancellationToken);

        Assert.Null(recorder.Error);
        Assert.NotNull(recorder.LastGeneration);
    }
}
