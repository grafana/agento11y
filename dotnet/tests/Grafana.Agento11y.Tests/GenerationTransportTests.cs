using Google.Protobuf;
using Grpc.Core;
using System.Diagnostics;
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

    // A single export attempt is bounded by the resolved ExportTimeout, and the
    // bound is created inside the exporter so it also applies to the background
    // flush path, which exports with CancellationToken.None.
    [Fact]
    public async Task GenerationHttpTransport_BoundsRequestByExportTimeout()
    {
        using var server = new HttpCaptureServer(
            (_, _) => Encoding.UTF8.GetBytes("{}"),
            responseDelay: TimeSpan.FromSeconds(3)
        );

        var exporter = new HttpGenerationExporter(
            $"http://127.0.0.1:{server.Port}/api/v1/generations:export",
            new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase),
            TimeSpan.FromMilliseconds(250)
        );

        try
        {
            var stopwatch = Stopwatch.StartNew();
            await Assert.ThrowsAnyAsync<OperationCanceledException>(
                () => exporter.ExportGenerationsAsync(
                    new ExportGenerationsRequest { Generations = [TestHelpers.CreateSeedResult("gen-http-timeout")] },
                    CancellationToken.None
                )
            );
            stopwatch.Stop();

            // Well below the 3s server delay and the 30s default budget.
            Assert.True(
                stopwatch.Elapsed < TimeSpan.FromSeconds(2),
                $"expected the export to abort on its own timeout, took {stopwatch.Elapsed}"
            );
        }
        finally
        {
            await exporter.ShutdownAsync(CancellationToken.None);
        }
    }

    [Theory]
    [InlineData(5000)]
    [InlineData(0)]
    public async Task GenerationHttpTransport_CompletesWithConfiguredAndClampedTimeout(int exportTimeoutMs)
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

        var exporter = new HttpGenerationExporter(
            $"http://127.0.0.1:{server.Port}/api/v1/generations:export",
            new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase),
            TimeSpan.FromMilliseconds(exportTimeoutMs)
        );

        try
        {
            var response = await exporter.ExportGenerationsAsync(
                new ExportGenerationsRequest { Generations = [TestHelpers.CreateSeedResult("gen-http-fast")] },
                CancellationToken.None
            );

            Assert.Single(response.Results);
            Assert.True(response.Results[0].Accepted);
        }
        finally
        {
            await exporter.ShutdownAsync(CancellationToken.None);
        }
    }

    // Config-level wiring: a short ExportTimeout on the client config reaches the
    // HTTP exporter, so a stalled collector surfaces as a flush error quickly
    // instead of hanging for the 30s default.
    [Fact]
    public async Task GenerationHttpTransport_ExportTimeoutFlowsFromClientConfig()
    {
        using var server = new HttpCaptureServer(
            (_, _) => Encoding.UTF8.GetBytes("{}"),
            responseDelay: TimeSpan.FromSeconds(3)
        );

        var config = new Agento11yClientConfig
        {
            GenerationExport = new GenerationExportConfig
            {
                Protocol = GenerationExportProtocol.Http,
                Endpoint = $"http://127.0.0.1:{server.Port}/api/v1/generations:export",
                // Larger than the single pending generation so only the explicit
                // FlushAsync below performs the export and observes its failure.
                BatchSize = 10,
                QueueSize = 10,
                FlushInterval = TimeSpan.FromHours(1),
                MaxRetries = 0,
                InitialBackoff = TimeSpan.FromMilliseconds(1),
                MaxBackoff = TimeSpan.FromMilliseconds(2),
                ExportTimeout = TimeSpan.FromMilliseconds(250),
            },
        };

        await using var client = new Agento11yClient(config);
        var recorder = client.StartGeneration(TestHelpers.CreateSeedStart("gen-http-config-timeout"));
        recorder.SetResult(TestHelpers.CreateSeedResult("gen-http-config-timeout"));
        recorder.End();

        var stopwatch = Stopwatch.StartNew();
        await Assert.ThrowsAnyAsync<OperationCanceledException>(
            () => client.FlushAsync(TestContext.Current.CancellationToken)
        );
        stopwatch.Stop();

        Assert.True(
            stopwatch.Elapsed < TimeSpan.FromSeconds(2),
            $"expected the flush to abort on the configured timeout, took {stopwatch.Elapsed}"
        );

        await client.ShutdownAsync(TestContext.Current.CancellationToken);
    }

    // The gRPC deadline is built inside the exporter from ExportTimeout, so it
    // bounds the call even when the caller passes CancellationToken.None (which
    // is exactly what the background flush does).
    [Fact]
    public async Task GenerationGrpcTransport_DeadlineAppliesWithoutCallerToken()
    {
        using var server = new GrpcIngestServer(responseDelay: TimeSpan.FromSeconds(5));

        using var exporter = new GrpcGenerationExporter(
            $"127.0.0.1:{server.Port}",
            insecure: true,
            new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase),
            TimeSpan.FromMilliseconds(500)
        );

        var stopwatch = Stopwatch.StartNew();
        var error = await Assert.ThrowsAsync<RpcException>(
            () => exporter.ExportGenerationsAsync(
                new ExportGenerationsRequest { Generations = [TestHelpers.CreateSeedResult("gen-grpc-deadline")] },
                CancellationToken.None
            )
        );
        stopwatch.Stop();

        Assert.Equal(StatusCode.DeadlineExceeded, error.StatusCode);
        Assert.True(
            stopwatch.Elapsed < TimeSpan.FromSeconds(3),
            $"expected the call to hit its deadline, took {stopwatch.Elapsed}"
        );
        await TestHelpers.WaitForAsync(
            () => server.Requests.Count >= 1,
            TimeSpan.FromSeconds(5),
            "expected the server to have received the export"
        );
    }

    [Fact]
    public async Task GenerationGrpcTransport_CompletesWithinExportTimeout()
    {
        using var server = new GrpcIngestServer();

        using var exporter = new GrpcGenerationExporter(
            $"127.0.0.1:{server.Port}",
            insecure: true,
            new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase),
            TimeSpan.FromSeconds(5)
        );

        var response = await exporter.ExportGenerationsAsync(
            new ExportGenerationsRequest { Generations = [TestHelpers.CreateSeedResult("gen-grpc-fast")] },
            CancellationToken.None
        );

        Assert.Single(response.Results);
        Assert.True(response.Results[0].Accepted);
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
