using System.Collections.Concurrent;
using System.Net;
using System.Text;
using System.Text.Json;
using Xunit;

namespace Grafana.Sigil.Tests;

public sealed class RatingTransportTests
{
    [Fact]
    public async Task SubmitConversationRatingAsync_OverHttpSendsRequestAndMapsResponse()
    {
        using var server = new RatingCaptureServer((_, _, _) =>
            (
                200,
                "application/json",
                Encoding.UTF8.GetBytes(
                    """
                    {
                      "rating":{
                        "rating_id":"rat-1",
                        "conversation_id":"conv-1",
                        "rating":"CONVERSATION_RATING_VALUE_BAD",
                        "comment":"wrong answer",
                        "created_at":"2026-02-13T12:00:00Z"
                      },
                      "summary":{
                        "total_count":1,
                        "good_count":0,
                        "bad_count":1,
                        "latest_rating":"CONVERSATION_RATING_VALUE_BAD",
                        "latest_rated_at":"2026-02-13T12:00:00Z",
                        "has_bad_rating":true
                      }
                    }
                    """
                )
            )
        );

        var config = new SigilClientConfig
        {
            Api = new ApiConfig
            {
                Endpoint = $"http://127.0.0.1:{server.Port}",
            },
            GenerationExport = new GenerationExportConfig
            {
                Protocol = GenerationExportProtocol.Grpc,
                Endpoint = "localhost:4317",
                Auth = new AuthConfig
                {
                    Mode = ExportAuthMode.Tenant,
                    TenantId = "tenant-a",
                },
                BatchSize = 1,
                FlushInterval = TimeSpan.FromMinutes(10),
                MaxRetries = 0,
            },
        };

        await using var client = new SigilClient(config);
        var response = await client.SubmitConversationRatingAsync(
            "conv-1",
            new SubmitConversationRatingRequest
            {
                RatingId = "rat-1",
                Rating = ConversationRatingValue.Bad,
                Comment = "wrong answer",
                Metadata = new Dictionary<string, object?>(StringComparer.Ordinal)
                {
                    ["channel"] = "assistant",
                },
            },
            TestContext.Current.CancellationToken
        );

        Assert.Equal("rat-1", response.Rating.RatingId);
        Assert.True(response.Summary.HasBadRating);

        Assert.True(server.Requests.TryDequeue(out var captured));
        Assert.Equal("/api/v1/conversations/conv-1/ratings", captured.Path);
        Assert.Equal("tenant-a", captured.Headers["X-Scope-OrgID"]);

        using var body = JsonDocument.Parse(captured.Body);
        Assert.Equal("rat-1", body.RootElement.GetProperty("rating_id").GetString());
        Assert.Equal("CONVERSATION_RATING_VALUE_BAD", body.RootElement.GetProperty("rating").GetString());
        Assert.Equal("wrong answer", body.RootElement.GetProperty("comment").GetString());
    }

    [Fact]
    public async Task SubmitConversationRatingAsync_MapsConflictResponse()
    {
        using var server = new RatingCaptureServer((_, _, _) =>
            (409, "text/plain", Encoding.UTF8.GetBytes("idempotency conflict"))
        );

        var config = new SigilClientConfig
        {
            Api = new ApiConfig
            {
                Endpoint = $"http://127.0.0.1:{server.Port}",
            },
            GenerationExport = new GenerationExportConfig
            {
                Protocol = GenerationExportProtocol.Http,
                Endpoint = $"http://127.0.0.1:{server.Port}/api/v1/generations:export",
                BatchSize = 1,
                FlushInterval = TimeSpan.FromMinutes(10),
                MaxRetries = 0,
            },
        };

        await using var client = new SigilClient(config);
        var error = await Assert.ThrowsAsync<RatingConflictException>(() =>
            client.SubmitConversationRatingAsync(
                "conv-1",
                new SubmitConversationRatingRequest
                {
                    RatingId = "rat-1",
                    Rating = ConversationRatingValue.Good,
                },
                TestContext.Current.CancellationToken
            )
        );

        Assert.Contains("idempotency conflict", error.Message, StringComparison.Ordinal);
    }

    [Fact]
    public async Task SubmitConversationRatingAsync_ValidatesInputBeforeTransport()
    {
        var config = new SigilClientConfig
        {
            Api = new ApiConfig
            {
                Endpoint = "http://127.0.0.1:8080",
            },
            GenerationExport = new GenerationExportConfig
            {
                Protocol = GenerationExportProtocol.Http,
                Endpoint = "http://127.0.0.1:8080/api/v1/generations:export",
                BatchSize = 1,
                FlushInterval = TimeSpan.FromMinutes(10),
                MaxRetries = 0,
            },
        };

        await using var client = new SigilClient(config);

        await Assert.ThrowsAsync<ValidationException>(() =>
            client.SubmitConversationRatingAsync(
                " ",
                new SubmitConversationRatingRequest
                {
                    RatingId = "rat-1",
                    Rating = ConversationRatingValue.Good,
                },
                TestContext.Current.CancellationToken
            )
        );

        await Assert.ThrowsAsync<ValidationException>(() =>
            client.SubmitConversationRatingAsync(
                "conv-1",
                new SubmitConversationRatingRequest
                {
                    RatingId = " ",
                    Rating = ConversationRatingValue.Good,
                },
                TestContext.Current.CancellationToken
            )
        );
    }

    [Fact]
    public async Task SubmitConversationRatingAsync_AppliesBearerAuthHeaderFromConfig()
    {
        using var server = new RatingCaptureServer((_, _, _) =>
            (
                200,
                "application/json",
                Encoding.UTF8.GetBytes(
                    """
                    {
                      "rating":{
                        "rating_id":"rat-1",
                        "conversation_id":"conv-1",
                        "rating":"CONVERSATION_RATING_VALUE_GOOD",
                        "created_at":"2026-02-13T12:00:00Z"
                      },
                      "summary":{
                        "total_count":1,
                        "good_count":1,
                        "bad_count":0,
                        "latest_rating":"CONVERSATION_RATING_VALUE_GOOD",
                        "latest_rated_at":"2026-02-13T12:00:00Z",
                        "has_bad_rating":false
                      }
                    }
                    """
                )
            )
        );

        var config = new SigilClientConfig
        {
            Api = new ApiConfig
            {
                Endpoint = $"127.0.0.1:{server.Port}",
            },
            GenerationExport = new GenerationExportConfig
            {
                Protocol = GenerationExportProtocol.Http,
                Endpoint = $"127.0.0.1:{server.Port}/api/v1/generations:export",
                Insecure = true,
                Auth = new AuthConfig
                {
                    Mode = ExportAuthMode.Bearer,
                    BearerToken = "token-a",
                },
                BatchSize = 1,
                FlushInterval = TimeSpan.FromMinutes(10),
                MaxRetries = 0,
            },
        };

        await using var client = new SigilClient(config);
        await client.SubmitConversationRatingAsync(
            "conv-1",
            new SubmitConversationRatingRequest
            {
                RatingId = "rat-1",
                Rating = ConversationRatingValue.Good,
            },
            TestContext.Current.CancellationToken
        );

        Assert.True(server.Requests.TryDequeue(out var captured));
        Assert.Equal("Bearer token-a", captured.Headers["Authorization"]);
    }
}

internal sealed class RatingCaptureServer : IDisposable
{
    private readonly HttpListener _listener;
    private readonly CancellationTokenSource _cts = new();
    private readonly Task _loop;

    public int Port { get; }

    public ConcurrentQueue<(string Path, Dictionary<string, string> Headers, byte[] Body)> Requests { get; } = new();

    public RatingCaptureServer(
        Func<string, Dictionary<string, string>, byte[], (int StatusCode, string ContentType, byte[] Body)> responseFactory
    )
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

                    var path = context.Request.Url?.AbsolutePath ?? string.Empty;
                    Requests.Enqueue((path, headers, body));

                    var (statusCode, contentType, response) = responseFactory(path, headers, body);
                    context.Response.StatusCode = statusCode;
                    context.Response.ContentType = contentType;
                    await context.Response.OutputStream.WriteAsync(response).ConfigureAwait(false);
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
        _listener.Close();
        try
        {
            _loop.GetAwaiter().GetResult();
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
