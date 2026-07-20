using OpenAI.Chat;
using OpenAI.Embeddings;
using OpenAI.Responses;
using System.ClientModel.Primitives;
using System.Diagnostics;
using System.Reflection;
using Xunit;

namespace Grafana.Sigil.OpenAI.Tests;

public sealed class OpenAIConformanceTests
{
    [Fact]
    public void ChatCompletionsFromRequestResponse_MapsSyncModeAndDefaultsRawArtifactsOff()
    {
        var messages = new List<ChatMessage>
        {
            new SystemChatMessage("You are concise."),
            new UserChatMessage("What is the weather in Paris?"),
            new ToolChatMessage("call_weather", "{\"temp_c\":18}"),
        };

        var options = new ChatCompletionOptions
        {
            MaxOutputTokenCount = 512,
            ToolChoice = ChatToolChoice.CreateFunctionChoice("weather"),
            ReasoningEffortLevel = ChatReasoningEffortLevel.High
        };
        options.Tools.Add(ChatTool.CreateFunctionTool(
            "weather",
            "Get weather",
            BinaryData.FromString("{\"type\":\"object\",\"properties\":{\"city\":{\"type\":\"string\"}}}")
        ));

        var response = OpenAIChatModelFactory.ChatCompletion(
            id: "chatcmpl_1",
            finishReason: ChatFinishReason.ToolCalls,
            content: new ChatMessageContent("calling weather"),
            toolCalls:
            [
                ChatToolCall.CreateFunctionToolCall(
                    "call_weather",
                    "weather",
                    BinaryData.FromString("{\"city\":\"Paris\"}")
                ),
            ],
            role: ChatMessageRole.Assistant,
            model: "gpt-5",
            usage: OpenAIChatModelFactory.ChatTokenUsage(
                outputTokenCount: 42,
                inputTokenCount: 120,
                totalTokenCount: 162,
                outputTokenDetails: OpenAIChatModelFactory.ChatOutputTokenUsageDetails(reasoningTokenCount: 5),
                inputTokenDetails: OpenAIChatModelFactory.ChatInputTokenUsageDetails(cachedTokenCount: 8)
            )
        );

        var generation = OpenAIGenerationMapper.ChatCompletionsFromRequestResponse(
            "gpt-5",
            messages,
            options,
            response,
            new OpenAISigilOptions
            {
                ConversationId = "conv-1",
                AgentName = "agent-openai",
                AgentVersion = "v-openai",
            }
        );

        Assert.Equal(GenerationMode.Sync, generation.Mode);
        Assert.Equal("You are concise.", generation.SystemPrompt);
        Assert.Equal("chatcmpl_1", generation.ResponseId);
        Assert.Equal("tool_calls", generation.StopReason);
        Assert.Equal(512, generation.MaxTokens);
        Assert.Contains("weather", generation.ToolChoice ?? string.Empty, StringComparison.OrdinalIgnoreCase);
        Assert.True(generation.ThinkingEnabled);
        Assert.Equal(162, generation.Usage.TotalTokens);
        Assert.Equal(8, generation.Usage.CacheReadInputTokens);
        Assert.Equal(5, generation.Usage.ReasoningTokens);
        Assert.Empty(generation.Artifacts);
        Assert.Contains(generation.Input, message => message.Role == MessageRole.Tool);
    }

    [Fact]
    public void ChatCompletionsFromRequestResponse_WithRawArtifactsOptIn_IncludesRequestResponseAndToolsArtifacts()
    {
        var messages = new List<ChatMessage>
        {
            new SystemChatMessage("You are concise."),
            new UserChatMessage("hello"),
        };

        var options = new ChatCompletionOptions
        {
            MaxOutputTokenCount = 256,
            ToolChoice = ChatToolChoice.CreateRequiredChoice(),
            ReasoningEffortLevel = ChatReasoningEffortLevel.Medium
        };
        options.Tools.Add(ChatTool.CreateFunctionTool("weather"));

        var response = OpenAIChatModelFactory.ChatCompletion(
            id: "chatcmpl_1",
            finishReason: ChatFinishReason.Stop,
            content: new ChatMessageContent("hi"),
            role: ChatMessageRole.Assistant,
            model: "gpt-5"
        );

        var generation = OpenAIGenerationMapper.ChatCompletionsFromRequestResponse(
            "gpt-5",
            messages,
            options,
            response,
            new OpenAISigilOptions().WithRawArtifacts()
        );

        Assert.Equal(3, generation.Artifacts.Count);
    }

    [Fact]
    public void ResponsesFromRequestResponse_MapsSyncModeAndDefaultsRawArtifactsOff()
    {
        var inputItems = new List<ResponseItem>
        {
            ResponseItem.CreateUserMessageItem("What is the weather in Paris?"),
            ResponseItem.CreateFunctionCallOutputItem("call_weather", "{\"temp_c\":18}"),
        };

        var requestOptions = new CreateResponseOptions
        {
            Instructions = "You are concise.",
            MaxOutputTokenCount = 320,
            Temperature = 0.2f,
            TopP = 0.9f,
            ToolChoice = ResponseToolChoice.CreateFunctionChoice("weather"),
            ReasoningOptions = new ResponseReasoningOptions
            {
                ReasoningEffortLevel = ResponseReasoningEffortLevel.Medium,
            },
        };
        requestOptions.Tools.Add(ResponseTool.CreateFunctionTool(
            "weather",
            BinaryData.FromString("{\"type\":\"object\",\"properties\":{\"city\":{\"type\":\"string\"}}}"),
            strictModeEnabled: true,
            functionDescription: "Get weather"
        ));

        var response = ReadResponse(
            """
            {
              "id": "resp_1",
              "created_at": 1,
              "model": "gpt-5",
              "object": "response",
              "output": [
                {
                  "id": "msg_1",
                  "type": "message",
                  "role": "assistant",
                  "status": "completed",
                  "content": [{"type": "output_text", "text": "Weather is clear."}]
                },
                {
                  "id": "fc_1",
                  "type": "function_call",
                  "call_id": "call_weather",
                  "name": "weather",
                  "arguments": "{\"city\":\"Paris\"}"
                }
              ],
              "parallel_tool_calls": false,
              "tool_choice": "auto",
              "tools": [],
              "status": "completed",
              "usage": {
                "input_tokens": 80,
                "output_tokens": 20,
                "total_tokens": 100,
                "input_tokens_details": {"cached_tokens": 2},
                "output_tokens_details": {"reasoning_tokens": 3}
              }
            }
            """
        );

        var generation = OpenAIGenerationMapper.ResponsesFromRequestResponse(
            "gpt-5",
            inputItems,
            requestOptions,
            response,
            new OpenAISigilOptions
            {
                ConversationId = "conv-resp-1",
                AgentName = "agent-openai",
                AgentVersion = "v-openai",
            }
        );

        Assert.Equal(GenerationMode.Sync, generation.Mode);
        Assert.Equal("resp_1", generation.ResponseId);
        Assert.Equal("You are concise.", generation.SystemPrompt);
        Assert.Equal("stop", generation.StopReason);
        Assert.Equal(320, generation.MaxTokens);
        Assert.Contains("weather", generation.ToolChoice ?? string.Empty, StringComparison.OrdinalIgnoreCase);
        Assert.True(generation.ThinkingEnabled);
        Assert.Equal(100, generation.Usage.TotalTokens);
        Assert.Equal(2, generation.Usage.CacheReadInputTokens);
        Assert.Equal(3, generation.Usage.ReasoningTokens);
        Assert.Empty(generation.Artifacts);
        Assert.Contains(generation.Input, message => message.Role == MessageRole.Tool);
    }

    [Fact]
    public void ResponsesFromRequestResponse_WithRawArtifactsOptIn_IncludesRequestResponseAndToolsArtifacts()
    {
        var inputItems = new List<ResponseItem>
        {
            ResponseItem.CreateUserMessageItem("hello"),
        };

        var requestOptions = new CreateResponseOptions
        {
            Instructions = "Be concise.",
            ToolChoice = ResponseToolChoice.CreateRequiredChoice(),
        };
        requestOptions.Tools.Add(ResponseTool.CreateFunctionTool("weather", BinaryData.FromString("{\"type\":\"object\"}"), null, null));

        var response = ReadResponse(
            """
            {
              "id": "resp_2",
              "created_at": 1,
              "model": "gpt-5",
              "object": "response",
              "output": [
                {
                  "id": "msg_1",
                  "type": "message",
                  "role": "assistant",
                  "status": "completed",
                  "content": [{"type": "output_text", "text": "hi"}]
                }
              ],
              "parallel_tool_calls": false,
              "tool_choice": "required",
              "tools": [],
              "status": "completed",
              "usage": {"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}
            }
            """
        );

        var generation = OpenAIGenerationMapper.ResponsesFromRequestResponse(
            "gpt-5",
            inputItems,
            requestOptions,
            response,
            new OpenAISigilOptions().WithRawArtifacts()
        );

        Assert.Equal(3, generation.Artifacts.Count);
    }

    [Fact]
    public void ResponsesFromStream_MapsStreamModeWhenOnlyEventsPresent()
    {
        var inputItems = new List<ResponseItem>
        {
            ResponseItem.CreateUserMessageItem("hello"),
        };
        var requestOptions = new CreateResponseOptions
        {
            Instructions = "Be concise.",
        };

        var summary = new OpenAIResponsesStreamSummary();
        summary.Events.Add(ReadStreamingEvent(
            """
            {
              "type": "response.output_text.delta",
              "content_index": 0,
              "delta": "hello",
              "item_id": "msg_1",
              "output_index": 0,
              "sequence_number": 1
            }
            """
        ));
        summary.Events.Add(ReadStreamingEvent(
            """
            {
              "type": "response.output_text.done",
              "content_index": 0,
              "text": " world",
              "item_id": "msg_1",
              "output_index": 0,
              "sequence_number": 2
            }
            """
        ));
        summary.Events.Add(ReadStreamingEvent(
            """
            {
              "type": "response.error",
              "code": "server_error",
              "message": "stream broke after final chunk",
              "param": null,
              "sequence_number": 3
            }
            """
        ));

        var generation = OpenAIGenerationMapper.ResponsesFromStream(
            "gpt-5",
            inputItems,
            requestOptions,
            summary,
            new OpenAISigilOptions().WithRawArtifacts()
        );

        Assert.Equal(GenerationMode.Stream, generation.Mode);
        Assert.Equal(string.Empty, generation.StopReason);
        Assert.Null(summary.FinalResponse);
        Assert.Contains(generation.Output, message => message.Parts.Any(part => (part.Text ?? string.Empty).Contains("hello world", StringComparison.Ordinal)));
        Assert.Contains(generation.Artifacts, artifact => artifact.Kind == ArtifactKind.ProviderEvent && artifact.Name == "openai.responses.stream_events");
    }

    [Fact]
    public void MapperRejectsMissingOrMalformedResponses()
    {
        var chatMessages = new List<ChatMessage>
        {
            new UserChatMessage("hello"),
        };
        var responseItems = new List<ResponseItem>
        {
            ResponseItem.CreateUserMessageItem("hello"),
        };

        Assert.Throws<ArgumentNullException>(() => OpenAIGenerationMapper.ChatCompletionsFromRequestResponse(
            "gpt-5",
            chatMessages,
            requestOptions: null,
            response: null!,
            new OpenAISigilOptions()
        ));
        Assert.Throws<ArgumentNullException>(() => OpenAIGenerationMapper.ResponsesFromRequestResponse(
            "gpt-5",
            responseItems,
            requestOptions: null,
            response: null!,
            new OpenAISigilOptions()
        ));
        Assert.Throws<ArgumentException>(() => OpenAIGenerationMapper.ChatCompletionsFromStream(
            "gpt-5",
            chatMessages,
            requestOptions: null,
            new OpenAIChatCompletionsStreamSummary(),
            new OpenAISigilOptions()
        ));
        Assert.Throws<ArgumentException>(() => OpenAIGenerationMapper.ResponsesFromStream(
            "gpt-5",
            responseItems,
            requestOptions: null,
            new OpenAIResponsesStreamSummary(),
            new OpenAISigilOptions()
        ));
    }

    [Fact]
    public async Task Recorder_RecordsChatAndResponsesModesAndPropagatesProviderErrors()
    {
        var exporter = new CapturingExporter();
        var config = new SigilClientConfig
        {
            GenerationExporter = exporter,
            GenerationExport = new GenerationExportConfig
            {
                BatchSize = 1,
                QueueSize = 10,
                FlushInterval = TimeSpan.FromHours(1),
            },
        };

        await using var client = new SigilClient(config);

        var chatMessages = new List<ChatMessage>
        {
            new UserChatMessage("hello"),
        };
        var responseItems = new List<ResponseItem>
        {
            ResponseItem.CreateUserMessageItem("hello"),
        };

        await Assert.ThrowsAsync<InvalidOperationException>(() => OpenAIRecorder.CompleteChatAsync(
            client,
            chatMessages,
            (_, _, _) => throw new InvalidOperationException("chat provider failed"),
            requestOptions: null,
            options: new OpenAISigilOptions
            {
                ModelName = "gpt-5",
            },
            TestContext.Current.CancellationToken
        ));

        var chatStreamSummary = await OpenAIRecorder.CompleteChatStreamingAsync(
            client,
            chatMessages,
            (_, _, _) => StreamChatUpdates(),
            requestOptions: null,
            options: new OpenAISigilOptions
            {
                ModelName = "gpt-5",
            },
            TestContext.Current.CancellationToken
        );

        await Assert.ThrowsAsync<InvalidOperationException>(() => OpenAIRecorder.CreateResponseAsync(
            client,
            responseItems,
            (_, _, _) => throw new InvalidOperationException("responses provider failed"),
            requestOptions: null,
            options: new OpenAISigilOptions
            {
                ModelName = "gpt-5",
            },
            TestContext.Current.CancellationToken
        ));

        var responsesStreamSummary = await OpenAIRecorder.CreateResponseStreamingAsync(
            client,
            responseItems,
            (_, _, _) => StreamResponsesUpdates(),
            requestOptions: new CreateResponseOptions
            {
                Instructions = "Be concise.",
            },
            options: new OpenAISigilOptions
            {
                ModelName = "gpt-5",
            },
            TestContext.Current.CancellationToken
        );

        Assert.NotEmpty(chatStreamSummary.Updates);
        Assert.NotEmpty(responsesStreamSummary.Events);

        await client.FlushAsync(TestContext.Current.CancellationToken);
        await client.ShutdownAsync(TestContext.Current.CancellationToken);

        var generations = exporter.Requests.SelectMany(request => request.Generations).ToList();

        Assert.Contains(generations, generation => generation.Mode == GenerationMode.Sync && generation.CallError.Contains("chat provider failed", StringComparison.Ordinal));
        Assert.Contains(generations, generation => generation.Mode == GenerationMode.Sync && generation.CallError.Contains("responses provider failed", StringComparison.Ordinal));
        Assert.True(generations.Count(generation => generation.Mode == GenerationMode.Stream) >= 2);
    }

    [Fact]
    public async Task Recorder_StreamMappingErrors_PreserveReturnedSummaries_AndMarkSpans()
    {
        var exporter = new CapturingExporter();
        var spans = new List<Activity>();
        using var listener = NewGenerationListener(spans);
        ActivitySource.AddActivityListener(listener);

        await using var client = new SigilClient(new SigilClientConfig
        {
            GenerationExporter = exporter,
            GenerationExport = new GenerationExportConfig
            {
                BatchSize = 1,
                QueueSize = 10,
                FlushInterval = TimeSpan.FromHours(1),
            },
        });

        var chatSummary = await OpenAIRecorder.CompleteChatStreamingAsync(
            client,
            [new UserChatMessage("hello")],
            (_, _, _) => EmptyChatUpdates(),
            requestOptions: null,
            options: new OpenAISigilOptions
            {
                ModelName = "gpt-5",
            },
            TestContext.Current.CancellationToken
        );

        var responsesSummary = await OpenAIRecorder.CreateResponseStreamingAsync(
            client,
            [ResponseItem.CreateUserMessageItem("hello")],
            (_, _, _) => EmptyResponsesUpdates(),
            requestOptions: new CreateResponseOptions(),
            options: new OpenAISigilOptions
            {
                ModelName = "gpt-5",
            },
            TestContext.Current.CancellationToken
        );

        Assert.Empty(chatSummary.Updates);
        Assert.Empty(responsesSummary.Events);

        await client.FlushAsync(TestContext.Current.CancellationToken);
        await client.ShutdownAsync(TestContext.Current.CancellationToken);

        var generations = exporter.Requests.SelectMany(request => request.Generations).ToList();
        Assert.Equal(2, generations.Count);
        Assert.All(generations, generation =>
        {
            Assert.Equal(GenerationMode.Stream, generation.Mode);
            Assert.Equal(string.Empty, generation.CallError);
        });
        Assert.Equal(2, spans.Count(span => span.GetTagItem("error.type")?.ToString() == "mapping_error"));
    }

    [Fact]
    public void EmbeddingsFromRequestResponse_MapsInputCountUsageAndDimensions()
    {
        var inputs = new List<string>
        {
            "alpha",
            "beta",
        };
        var requestOptions = new EmbeddingGenerationOptions
        {
            Dimensions = 256,
        };
        var response = OpenAIEmbeddingsModelFactory.OpenAIEmbeddingCollection(
            [
                OpenAIEmbeddingsModelFactory.OpenAIEmbedding(0, [0.1f, 0.2f, 0.3f]),
            ],
            "text-embedding-3-small",
            OpenAIEmbeddingsModelFactory.EmbeddingTokenUsage(inputTokenCount: 18, totalTokenCount: 18)
        );

        var result = OpenAIGenerationMapper.EmbeddingsFromRequestResponse(
            "text-embedding-3-small",
            inputs,
            requestOptions,
            response
        );

        Assert.Equal(2, result.InputCount);
        Assert.Equal(18, result.InputTokens);
        Assert.Equal("text-embedding-3-small", result.ResponseModel);
        Assert.Equal(3, result.Dimensions);
        Assert.Equal(inputs, result.InputTexts);
    }

    [Fact]
    public async Task Recorder_EmbeddingsWrapper_DoesNotEnqueueAndPropagatesProviderErrors()
    {
        var exporter = new CapturingExporter();
        await using var client = new SigilClient(new SigilClientConfig
        {
            GenerationExporter = exporter,
            GenerationExport = new GenerationExportConfig
            {
                BatchSize = 1,
                QueueSize = 10,
                FlushInterval = TimeSpan.FromHours(1),
            },
        });

        var inputs = new List<string> { "embed this" };
        var requestOptions = new EmbeddingGenerationOptions
        {
            Dimensions = 128,
        };

        await Assert.ThrowsAsync<InvalidOperationException>(() => OpenAIRecorder.GenerateEmbeddingsAsync(
            client,
            inputs,
            (_, _, _) => Task.FromException<OpenAIEmbeddingCollection>(new InvalidOperationException("embedding provider failed")),
            requestOptions,
            new OpenAISigilOptions
            {
                ModelName = "text-embedding-3-small",
            },
            TestContext.Current.CancellationToken
        ));

        var response = OpenAIEmbeddingsModelFactory.OpenAIEmbeddingCollection(
            [
                OpenAIEmbeddingsModelFactory.OpenAIEmbedding(0, [0.1f, 0.2f]),
            ],
            "text-embedding-3-small",
            OpenAIEmbeddingsModelFactory.EmbeddingTokenUsage(inputTokenCount: 6, totalTokenCount: 6)
        );

        var wrapped = await OpenAIRecorder.GenerateEmbeddingsAsync(
            client,
            inputs,
            (_, _, _) => Task.FromResult(response),
            requestOptions,
            new OpenAISigilOptions
            {
                ModelName = "text-embedding-3-small",
            },
            TestContext.Current.CancellationToken
        );

        Assert.Equal("text-embedding-3-small", wrapped.Model);

        await client.FlushAsync(TestContext.Current.CancellationToken);
        await client.ShutdownAsync(TestContext.Current.CancellationToken);

        Assert.Empty(exporter.Requests);
    }

    [Fact]
    public void RemovedRecorderApis_AreNotExposed()
    {
        var publicStaticMethods = typeof(OpenAIRecorder)
            .GetMethods(BindingFlags.Public | BindingFlags.Static)
            .Select(method => method.Name)
            .ToHashSet(StringComparer.Ordinal);

        Assert.DoesNotContain("ChatCompletionAsync", publicStaticMethods);
        Assert.DoesNotContain("ChatCompletionStreamAsync", publicStaticMethods);
    }

    private static ResponseResult ReadResponse(string json)
    {
        return ModelReaderWriter.Read<ResponseResult>(BinaryData.FromString(json))!;
    }

    private static StreamingResponseUpdate ReadStreamingEvent(string json)
    {
        return ModelReaderWriter.Read<StreamingResponseUpdate>(BinaryData.FromString(json))!;
    }

    private static async IAsyncEnumerable<StreamingChatCompletionUpdate> StreamChatUpdates()
    {
        yield return OpenAIChatModelFactory.StreamingChatCompletionUpdate(
            completionId: "chatcmpl_stream_recorder",
            contentUpdate: new ChatMessageContent("hello"),
            finishReason: ChatFinishReason.Stop,
            usage: OpenAIChatModelFactory.ChatTokenUsage(outputTokenCount: 1, inputTokenCount: 1, totalTokenCount: 2),
            model: "gpt-5"
        );

        await Task.CompletedTask;
    }

    private static async IAsyncEnumerable<StreamingResponseUpdate> StreamResponsesUpdates()
    {
        yield return ReadStreamingEvent(
            """
            {
              "type": "response.output_text.delta",
              "content_index": 0,
              "delta": "hello",
              "item_id": "msg_1",
              "output_index": 0,
              "sequence_number": 1
            }
            """
        );
        yield return ReadStreamingEvent(
            """
            {
              "type": "response.output_text.done",
              "content_index": 0,
              "text": " world",
              "item_id": "msg_1",
              "output_index": 0,
              "sequence_number": 2
            }
            """
        );

        await Task.CompletedTask;
    }

    private static async IAsyncEnumerable<StreamingChatCompletionUpdate> EmptyChatUpdates()
    {
        await Task.CompletedTask;
        yield break;
    }

    private static async IAsyncEnumerable<StreamingResponseUpdate> EmptyResponsesUpdates()
    {
        await Task.CompletedTask;
        yield break;
    }

    private static ActivityListener NewGenerationListener(List<Activity> spans)
    {
        return new ActivityListener
        {
            ShouldListenTo = source => source.Name == "github.com/grafana/sigil/sdks/dotnet",
            Sample = static (ref _) => ActivitySamplingResult.AllDataAndRecorded,
            ActivityStopped = activity =>
            {
                if (activity.GetTagItem("gen_ai.operation.name")?.ToString() != "execute_tool")
                {
                    spans.Add(activity);
                }
            },
        };
    }

    private sealed class CapturingExporter : IGenerationExporter
    {
        public List<ExportGenerationsRequest> Requests { get; } = [];

        public Task<ExportGenerationsResponse> ExportGenerationsAsync(ExportGenerationsRequest request, CancellationToken cancellationToken)
        {
            Requests.Add(request);
            return Task.FromResult(new ExportGenerationsResponse
            {
                Results = [.. request.Generations.Select(generation => new ExportGenerationResult
                {
                    GenerationId = generation.Id,
                    Accepted = true,
                })],
            });
        }

        public Task ShutdownAsync(CancellationToken cancellationToken)
        {
            return Task.CompletedTask;
        }
    }
}
