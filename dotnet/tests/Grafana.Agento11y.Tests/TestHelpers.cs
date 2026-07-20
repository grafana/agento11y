using Grpc.Core;
using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Hosting;
using Microsoft.AspNetCore.Server.Kestrel.Core;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Hosting;
using System.Collections.Concurrent;
using System.Net;
using System.Text;
using IngestProto = global::Agento11y.V1;

namespace Grafana.Agento11y.Tests;

internal static class TestHelpers
{
    public static async Task WaitForAsync(Func<bool> condition, TimeSpan timeout, string message)
    {
        var deadline = DateTime.UtcNow + timeout;
        while (DateTime.UtcNow < deadline)
        {
            if (condition())
            {
                return;
            }

            await Task.Delay(10).ConfigureAwait(false);
        }

        throw new Xunit.Sdk.XunitException(message);
    }

    public static GenerationStart CreateSeedStart(string id = "gen-seed")
    {
        return new GenerationStart
        {
            Id = id,
            ConversationId = "conv-seed",
            AgentName = "agent-seed",
            AgentVersion = "1.2.3",
            Mode = GenerationMode.Sync,
            Model = new ModelRef
            {
                Provider = "openai",
                Name = "gpt-5",
            },
            SystemPrompt = "be concise",
            MaxTokens = 1024,
            Temperature = 0.7,
            TopP = 0.9,
            ToolChoice = "auto",
            ThinkingEnabled = true,
            Tools =
            {
                new ToolDefinition
                {
                    Name = "weather",
                    Description = "Get weather",
                    Type = "function",
                    InputSchemaJson = Encoding.UTF8.GetBytes("{\"type\":\"object\"}"),
                },
            },
            Tags =
            {
                ["tenant"] = "dev",
            },
            Metadata =
            {
                ["seed"] = 42,
                ["agento11y.gen_ai.request.thinking.budget_tokens"] = 4096L,
            },
            StartedAt = new DateTimeOffset(2026, 02, 13, 10, 00, 00, TimeSpan.Zero),
        };
    }

    public static Generation CreateSeedResult(string id = "gen-seed")
    {
        return new Generation
        {
            Id = id,
            ConversationId = "conv-seed",
            AgentName = "agent-seed",
            AgentVersion = "1.2.3",
            Mode = GenerationMode.Sync,
            OperationName = "generateText",
            Model = new ModelRef
            {
                Provider = "openai",
                Name = "gpt-5",
            },
            ResponseId = "resp-seed",
            ResponseModel = "gpt-5-2026",
            SystemPrompt = "be concise",
            MaxTokens = 256,
            Temperature = 0.25,
            TopP = 0.85,
            ToolChoice = "required",
            ThinkingEnabled = false,
            Input =
            {
                new Message
                {
                    Role = MessageRole.User,
                    Parts =
                    {
                        Part.TextPart("hello"),
                    },
                },
            },
            Output =
            {
                new Message
                {
                    Role = MessageRole.Assistant,
                    Parts =
                    {
                        Part.ThinkingPart("reasoning"),
                        Part.ToolCallPart(new ToolCall
                        {
                            Id = "call-1",
                            Name = "weather",
                            InputJson = Encoding.UTF8.GetBytes("{\"city\":\"Paris\"}"),
                        }),
                        Part.TextPart("It is sunny"),
                    },
                },
                new Message
                {
                    Role = MessageRole.Tool,
                    Parts =
                    {
                        Part.ToolResultPart(new ToolResult
                        {
                            ToolCallId = "call-1",
                            Name = "weather",
                            Content = "sunny",
                            ContentJson = Encoding.UTF8.GetBytes("{\"temp_c\":18}"),
                        }),
                    },
                },
            },
            Tools =
            {
                new ToolDefinition
                {
                    Name = "weather",
                    Description = "Get weather",
                    Type = "function",
                    InputSchemaJson = Encoding.UTF8.GetBytes("{\"type\":\"object\"}"),
                },
            },
            Usage = new TokenUsage
            {
                InputTokens = 120,
                OutputTokens = 42,
                TotalTokens = 162,
                CacheReadInputTokens = 8,
                CacheWriteInputTokens = 1,
                ReasoningTokens = 5,
            },
            StopReason = "stop",
            StartedAt = new DateTimeOffset(2026, 02, 13, 10, 00, 00, TimeSpan.Zero),
            CompletedAt = new DateTimeOffset(2026, 02, 13, 10, 00, 02, TimeSpan.Zero),
            Tags =
            {
                ["tenant"] = "dev",
            },
            Metadata =
            {
                ["seed"] = 42,
                ["agento11y.gen_ai.request.thinking.budget_tokens"] = 2048L,
            },
            Artifacts =
            {
                Artifact.JsonArtifact(ArtifactKind.Request, "request", new { ok = true }),
            },
        };
    }

    public static Agento11yClientConfig TestConfig(IGenerationExporter exporter)
    {
        return new Agento11yClientConfig
        {
            GenerationExporter = exporter,
            GenerationExport = new GenerationExportConfig
            {
                BatchSize = 10,
                QueueSize = 100,
                FlushInterval = TimeSpan.FromHours(1),
                MaxRetries = 1,
                InitialBackoff = TimeSpan.FromMilliseconds(1),
                MaxBackoff = TimeSpan.FromMilliseconds(2),
            },
        };
    }
}

internal sealed class CapturingGenerationExporter : IGenerationExporter
{
#if NET10_0_OR_GREATER
    private readonly Lock _gate = new();
#else
    private readonly object _gate = new();
#endif

    private readonly List<ExportGenerationsRequest> _requests = [];

    public int Calls { get; private set; }

    public int FailuresBeforeSuccess { get; set; }

    public IReadOnlyList<ExportGenerationsRequest> Requests
    {
        get
        {
            lock (_gate)
            {
                return [.. _requests];
            }
        }
    }

    public Task<ExportGenerationsResponse> ExportGenerationsAsync(
        ExportGenerationsRequest request,
        CancellationToken cancellationToken
    )
    {
        lock (_gate)
        {
            Calls++;
            if (Calls <= FailuresBeforeSuccess)
            {
                throw new InvalidOperationException("forced export failure");
            }

            _requests.Add(request);
        }

        var response = new ExportGenerationsResponse
        {
            Results = [.. request.Generations.Select(g => new ExportGenerationResult
            {
                GenerationId = g.Id,
                Accepted = true,
            })],
        };

        return Task.FromResult(response);
    }

    public Task ShutdownAsync(CancellationToken cancellationToken)
    {
        return Task.CompletedTask;
    }
}

internal sealed class HttpCaptureServer : IDisposable
{
    private readonly HttpListener _listener;
    private readonly CancellationTokenSource _cts = new();
    private readonly Task _loop;

    public int Port { get; }

    public ConcurrentQueue<(Dictionary<string, string> Headers, byte[] Body)> Requests { get; } = new();

    public HttpCaptureServer(Func<Dictionary<string, string>, byte[], byte[]> responseFactory)
    {
        Port = ReservePort();
        _listener = new HttpListener();
        _listener.Prefixes.Add($"http://127.0.0.1:{Port}/");
        _listener.Start();

        _loop = Task.Run(async () =>
        {
            while (!_cts.IsCancellationRequested)
            {
                HttpListenerContext context;
                try
                {
                    context = await _listener.GetContextAsync().ConfigureAwait(false);
                }
                catch
                {
                    if (_cts.IsCancellationRequested)
                    {
                        break;
                    }

                    throw;
                }

                try
                {
                    using var stream = new MemoryStream();
                    await context.Request.InputStream.CopyToAsync(stream).ConfigureAwait(false);
                    var body = stream.ToArray();

                    var headers = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
                    foreach (var key in context.Request.Headers.AllKeys)
                    {
                        if (key != null)
                        {
                            headers[key] = context.Request.Headers[key] ?? string.Empty;
                        }
                    }

                    Requests.Enqueue((headers, body));

                    var responsePayload = responseFactory(headers, body);
                    context.Response.StatusCode = 202;
                    context.Response.ContentType = "application/json";
                    await context.Response.OutputStream.WriteAsync(responsePayload).ConfigureAwait(false);
                }
                finally
                {
                    context.Response.Close();
                }
            }
        });
    }

    public void Dispose()
    {
        _cts.Cancel();
        try
        {
            _listener.Stop();
        }
        catch
        {
            // Ignore shutdown exceptions.
        }

        _listener.Close();
        try
        {
            _loop.Wait(TimeSpan.FromSeconds(2));
        }
        catch
        {
            // Ignore shutdown exceptions.
        }
    }

    private static int ReservePort()
    {
        var listener = new System.Net.Sockets.TcpListener(System.Net.IPAddress.Loopback, 0);
        listener.Start();
        var port = ((System.Net.IPEndPoint)listener.LocalEndpoint).Port;
        listener.Stop();
        return port;
    }
}

internal sealed class GrpcIngestServer : IDisposable
{
    private readonly IHost _host;
    private readonly RequestStore _store = new();

    public int Port { get; }

    public IReadOnlyList<(IngestProto.ExportGenerationsRequest Request, Metadata Headers)> Requests => _store.Snapshot();

    public IReadOnlyList<string> UserAgents => _store.UserAgentSnapshot();

    public GrpcIngestServer()
    {
        Port = ReservePort();
        _host = Host.CreateDefaultBuilder()
            .ConfigureWebHostDefaults(webBuilder =>
            {
                webBuilder.UseKestrel(options =>
                {
                    options.ListenLocalhost(Port, listen => listen.Protocols = HttpProtocols.Http2);
                });
                webBuilder.ConfigureServices(services =>
                {
                    services.AddSingleton(_store);
                    services.AddGrpc();
                });
                webBuilder.Configure(app =>
                {
                    app.UseRouting();
                    app.UseEndpoints(endpoints =>
                    {
                        endpoints.MapGrpcService<IngestService>();
                    });
                });
            })
            .Build();

        _host.Start();
    }

    public void Dispose()
    {
        using var cts = new CancellationTokenSource(TimeSpan.FromSeconds(2));
        try
        {
            _host.StopAsync(cts.Token).GetAwaiter().GetResult();
        }
        catch
        {
            // Ignore shutdown exceptions.
        }
        _host.Dispose();
    }

    private sealed class RequestStore
    {
#if NET10_0_OR_GREATER
        private readonly Lock _gate = new();
#else
        private readonly object _gate = new();
#endif

        private readonly List<(IngestProto.ExportGenerationsRequest Request, Metadata Headers)> _requests = [];
        private readonly List<string> _userAgents = [];

        public void Add(IngestProto.ExportGenerationsRequest request, Metadata headers, string userAgent)
        {
            lock (_gate)
            {
                _requests.Add((request, headers));
                _userAgents.Add(userAgent);
            }
        }

        public IReadOnlyList<(IngestProto.ExportGenerationsRequest Request, Metadata Headers)> Snapshot()
        {
            lock (_gate)
            {
                return [.. _requests];
            }
        }

        public IReadOnlyList<string> UserAgentSnapshot()
        {
            lock (_gate)
            {
                return [.. _userAgents];
            }
        }
    }

    private sealed class IngestService : IngestProto.GenerationIngestService.GenerationIngestServiceBase
    {
        private readonly RequestStore _store;

        public IngestService(RequestStore store) => _store = store;

        public override Task<IngestProto.ExportGenerationsResponse> ExportGenerations(
            IngestProto.ExportGenerationsRequest request,
            ServerCallContext context
        )
        {
            var userAgent = context.GetHttpContext().Request.Headers.UserAgent.ToString();
            _store.Add(request, context.RequestHeaders, userAgent);

            var response = new IngestProto.ExportGenerationsResponse();
            foreach (var generation in request.Generations)
            {
                response.Results.Add(new IngestProto.ExportGenerationResult
                {
                    GenerationId = generation.Id,
                    Accepted = true,
                });
            }

            return Task.FromResult(response);
        }
    }

    private static int ReservePort()
    {
        var listener = new System.Net.Sockets.TcpListener(System.Net.IPAddress.Loopback, 0);
        listener.Start();
        var port = ((System.Net.IPEndPoint)listener.LocalEndpoint).Port;
        listener.Stop();
        return port;
    }
}

internal static class GenerationAssertions
{
    public static void AssertEquivalent(Generation expected, IngestProto.Generation actual)
    {
        Xunit.Assert.Equal(expected.Id, actual.Id);
        Xunit.Assert.Equal(expected.ConversationId, actual.ConversationId);
        Xunit.Assert.Equal(expected.AgentName, actual.AgentName);
        Xunit.Assert.Equal(expected.AgentVersion, actual.AgentVersion);
        Xunit.Assert.Equal(expected.Model.Provider, actual.Model.Provider);
        Xunit.Assert.Equal(expected.Model.Name, actual.Model.Name);
        Xunit.Assert.Equal(expected.ResponseId, actual.ResponseId);
        Xunit.Assert.Equal(expected.ResponseModel, actual.ResponseModel);
        Xunit.Assert.Equal(expected.SystemPrompt, actual.SystemPrompt);
        Xunit.Assert.Equal(expected.MaxTokens, actual.HasMaxTokens ? actual.MaxTokens : null);
        Xunit.Assert.Equal(expected.Temperature, actual.HasTemperature ? actual.Temperature : null);
        Xunit.Assert.Equal(expected.TopP, actual.HasTopP ? actual.TopP : null);
        Xunit.Assert.Equal(expected.ToolChoice ?? string.Empty, actual.HasToolChoice ? actual.ToolChoice : string.Empty);
        Xunit.Assert.Equal(expected.ThinkingEnabled, actual.HasThinkingEnabled ? actual.ThinkingEnabled : null);
        Xunit.Assert.Equal(expected.StopReason, actual.StopReason);
        Xunit.Assert.Equal(expected.Usage.InputTokens, actual.Usage.InputTokens);
        Xunit.Assert.Equal(expected.Usage.OutputTokens, actual.Usage.OutputTokens);
        Xunit.Assert.Equal(expected.Usage.TotalTokens, actual.Usage.TotalTokens);
        Xunit.Assert.Equal(expected.Input.Count, actual.Input.Count);
        Xunit.Assert.Equal(expected.Output.Count, actual.Output.Count);
        Xunit.Assert.Equal(expected.Tools.Count, actual.Tools.Count);
        Xunit.Assert.Equal(expected.Artifacts.Count, actual.RawArtifacts.Count);
        Xunit.Assert.True(actual.Metadata.Fields.ContainsKey("agento11y.sdk.name"));
        Xunit.Assert.Equal(
            expected.Metadata["agento11y.sdk.name"]?.ToString(),
            actual.Metadata.Fields["agento11y.sdk.name"].StringValue
        );

        Xunit.Assert.Contains(actual.Input.SelectMany(m => m.Parts), p => p.PayloadCase == IngestProto.Part.PayloadOneofCase.Text);
        Xunit.Assert.Contains(actual.Output.SelectMany(m => m.Parts), p => p.PayloadCase == IngestProto.Part.PayloadOneofCase.Thinking);
        Xunit.Assert.Contains(actual.Output.SelectMany(m => m.Parts), p => p.PayloadCase == IngestProto.Part.PayloadOneofCase.ToolCall);
        Xunit.Assert.Contains(actual.Output.SelectMany(m => m.Parts), p => p.PayloadCase == IngestProto.Part.PayloadOneofCase.ToolResult);
    }
}

internal static class TraceAssertions
{
    public static Dictionary<string, object?> AttributeValueMap(IEnumerable<OpenTelemetry.Proto.Common.V1.KeyValue> attributes)
    {
        var map = new Dictionary<string, object?>(StringComparer.Ordinal);
        foreach (var attribute in attributes)
        {
            if (attribute?.Value == null)
            {
                continue;
            }

            map[attribute.Key] = DecodeAnyValue(attribute.Value);
        }

        return map;
    }

    private static object? DecodeAnyValue(OpenTelemetry.Proto.Common.V1.AnyValue value)
    {
        return value.ValueCase switch
        {
            OpenTelemetry.Proto.Common.V1.AnyValue.ValueOneofCase.StringValue => value.StringValue,
            OpenTelemetry.Proto.Common.V1.AnyValue.ValueOneofCase.IntValue => value.IntValue,
            OpenTelemetry.Proto.Common.V1.AnyValue.ValueOneofCase.DoubleValue => value.DoubleValue,
            OpenTelemetry.Proto.Common.V1.AnyValue.ValueOneofCase.BoolValue => value.BoolValue,
            OpenTelemetry.Proto.Common.V1.AnyValue.ValueOneofCase.ArrayValue => DecodeArray(value.ArrayValue.Values),
            _ => null,
        };
    }

    private static List<object?> DecodeArray(IEnumerable<OpenTelemetry.Proto.Common.V1.AnyValue> values)
    {
        var decoded = new List<object?>();
        foreach (var value in values)
        {
            decoded.Add(DecodeAnyValue(value));
        }

        return decoded;
    }

    public static OpenTelemetry.Proto.Trace.V1.Span FindFirstSpan(OpenTelemetry.Proto.Collector.Trace.V1.ExportTraceServiceRequest request)
    {
        var span = request.ResourceSpans
            .SelectMany(resource => resource.ScopeSpans)
            .SelectMany(scope => scope.Spans)
            .FirstOrDefault();

        return span ?? throw new Xunit.Sdk.XunitException("no span found in trace export request");
    }
}
