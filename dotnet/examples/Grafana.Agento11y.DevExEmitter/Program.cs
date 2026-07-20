using System.ClientModel;
using System.ClientModel.Primitives;
using System.Text.Json;
using AnthropicDelta = Anthropic.Models.Messages.Delta;
using AnthropicMessage = Anthropic.Models.Messages.Message;
using AnthropicMessageCreateParams = Anthropic.Models.Messages.MessageCreateParams;
using AnthropicMessageDeltaUsage = Anthropic.Models.Messages.MessageDeltaUsage;
using AnthropicMessageParam = Anthropic.Models.Messages.MessageParam;
using AnthropicModel = Anthropic.Models.Messages.Model;
using AnthropicRawMessageDeltaEvent = Anthropic.Models.Messages.RawMessageDeltaEvent;
using AnthropicRawMessageStartEvent = Anthropic.Models.Messages.RawMessageStartEvent;
using AnthropicRawMessageStreamEvent = Anthropic.Models.Messages.RawMessageStreamEvent;
using AnthropicRole = Anthropic.Models.Messages.Role;
using AnthropicStopReason = Anthropic.Models.Messages.StopReason;
using AnthropicTextBlock = Anthropic.Models.Messages.TextBlock;
using AnthropicUsage = Anthropic.Models.Messages.Usage;
using GPart = Google.GenAI.Types.Part;
using GeminiCandidate = Google.GenAI.Types.Candidate;
using GeminiContent = Google.GenAI.Types.Content;
using GeminiFinishReason = Google.GenAI.Types.FinishReason;
using GeminiFunctionResponse = Google.GenAI.Types.FunctionResponse;
using GeminiGenerateContentConfig = Google.GenAI.Types.GenerateContentConfig;
using GeminiGenerateContentResponse = Google.GenAI.Types.GenerateContentResponse;
using GeminiGenerateContentResponseUsageMetadata = Google.GenAI.Types.GenerateContentResponseUsageMetadata;
using Grafana.Agento11y;
using Grafana.Agento11y.Anthropic;
using Grafana.Agento11y.Gemini;
using Grafana.Agento11y.OpenAI;
using OpenAI.Chat;
using OpenAI.Responses;

internal static class Program
{
    private const string Language = "dotnet";
    private static readonly string[] Sources = ["openai", "anthropic", "gemini", "mistral"];
    private static readonly string[] Personas = ["planner", "retriever", "executor"];

    private static async Task<int> Main()
    {
        var config = RuntimeConfig.Load();
        using var cts = new CancellationTokenSource();

        Console.CancelKeyPress += (_, eventArgs) =>
        {
            eventArgs.Cancel = true;
            cts.Cancel();
        };

        AppDomain.CurrentDomain.ProcessExit += (_, _) =>
        {
            try
            {
                cts.Cancel();
            }
            catch (ObjectDisposedException)
            {
                // Process-exit handlers may run after scoped disposals.
            }
        };

        var client = new Agento11yClient(new Agento11yClientConfig
        {
            GenerationExport = new GenerationExportConfig
            {
                Protocol = GenerationExportProtocol.Grpc,
                Endpoint = config.GenGrpcEndpoint,
                Auth = new Grafana.Agento11y.AuthConfig
                {
                    Mode = ExportAuthMode.None,
                },
                Insecure = true,
            },
            Logger = message => Console.Error.WriteLine(message),
        });

        try
        {
            await RunEmitterAsync(client, config, cts.Token).ConfigureAwait(false);
            return 0;
        }
        catch (OperationCanceledException)
        {
            return 0;
        }
        catch (Exception exception)
        {
            Console.Error.WriteLine($"[dotnet-emitter] fatal error: {exception}");
            return 1;
        }
        finally
        {
            await client.ShutdownAsync(CancellationToken.None).ConfigureAwait(false);
        }
    }

    private static async Task RunEmitterAsync(Agento11yClient client, RuntimeConfig config, CancellationToken cancellationToken)
    {
        var sourceStates = Sources.ToDictionary(
            source => source,
            _ => new SourceState(config.Conversations),
            StringComparer.Ordinal
        );

        long cycles = 0;
        Console.WriteLine(
            $"[dotnet-emitter] started interval_ms={config.IntervalMs} stream_percent={config.StreamPercent} conversations={config.Conversations} rotate_turns={config.RotateTurns} custom_provider={config.CustomProvider}"
        );

        while (!cancellationToken.IsCancellationRequested)
        {
            foreach (var source in Sources)
            {
                var state = sourceStates[source];
                var slot = state.Cursor % config.Conversations;
                state.Cursor += 1;

                var thread = ResolveThread(state, config.RotateTurns, source, slot);
                var mode = ChooseMode(config.StreamPercent);
                var envelope = BuildTagEnvelope(source, mode, thread.Turn, slot);

                var context = new EmitContext(
                    ConversationId: thread.ConversationId,
                    Turn: thread.Turn,
                    Slot: slot,
                    AgentName: $"devex-{Language}-{source}-{envelope.AgentPersona}",
                    AgentVersion: "devex-1",
                    Tags: envelope.Tags,
                    Metadata: envelope.Metadata
                );

                await EmitSourceAsync(client, config, source, mode, context, cancellationToken).ConfigureAwait(false);
                thread.Turn += 1;
            }

            cycles += 1;
            if (config.MaxCycles > 0 && cycles >= config.MaxCycles)
            {
                break;
            }

            var jitterMs = Random.Shared.Next(-200, 201);
            var sleepMs = Math.Max(200, config.IntervalMs + jitterMs);
            await Task.Delay(TimeSpan.FromMilliseconds(sleepMs), cancellationToken).ConfigureAwait(false);
        }
    }

    private static GenerationMode ChooseMode(int streamPercent)
    {
        return Random.Shared.Next(0, 100) < streamPercent ? GenerationMode.Stream : GenerationMode.Sync;
    }

    private static ThreadState ResolveThread(SourceState state, int rotateTurns, string source, int slot)
    {
        var thread = state.Slots[slot];
        if (string.IsNullOrWhiteSpace(thread.ConversationId) || thread.Turn >= rotateTurns)
        {
            thread.ConversationId = NewConversationId(source, slot);
            thread.Turn = 0;
        }

        return thread;
    }

    private static string NewConversationId(string source, int slot)
    {
        return $"devex-{Language}-{source}-{slot}-{DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()}";
    }

    private static async Task EmitSourceAsync(
        Agento11yClient client,
        RuntimeConfig config,
        string source,
        GenerationMode mode,
        EmitContext context,
        CancellationToken cancellationToken
    )
    {
        if (source == "openai")
        {
            var shape = ProviderShapeFor(source, context.Turn);
            var useResponses = string.Equals(shape, "openai_responses", StringComparison.Ordinal);

            if (mode == GenerationMode.Stream)
            {
                if (useResponses)
                {
                    await EmitOpenAiResponsesStreamAsync(client, context, cancellationToken).ConfigureAwait(false);
                    return;
                }

                await EmitOpenAiChatStreamAsync(client, context, cancellationToken).ConfigureAwait(false);
                return;
            }

            if (useResponses)
            {
                await EmitOpenAiResponsesSyncAsync(client, context, cancellationToken).ConfigureAwait(false);
                return;
            }

            await EmitOpenAiChatSyncAsync(client, context, cancellationToken).ConfigureAwait(false);
            return;
        }

        if (source == "anthropic")
        {
            if (mode == GenerationMode.Stream)
            {
                await EmitAnthropicStreamAsync(client, context, cancellationToken).ConfigureAwait(false);
                return;
            }

            await EmitAnthropicSyncAsync(client, context, cancellationToken).ConfigureAwait(false);
            return;
        }

        if (source == "gemini")
        {
            if (mode == GenerationMode.Stream)
            {
                await EmitGeminiStreamAsync(client, context, cancellationToken).ConfigureAwait(false);
                return;
            }

            await EmitGeminiSyncAsync(client, context, cancellationToken).ConfigureAwait(false);
            return;
        }

        if (mode == GenerationMode.Stream)
        {
            EmitCustomStream(client, config, context);
            return;
        }

        EmitCustomSync(client, config, context);
    }

    private static async Task EmitOpenAiChatSyncAsync(Agento11yClient client, EmitContext context, CancellationToken cancellationToken)
    {
        var messages = new List<ChatMessage>
        {
            new SystemChatMessage("Return concise rollout planning bullets."),
            new UserChatMessage($"Draft rollout plan {context.Turn}."),
        };

        var requestOptions = new ChatCompletionOptions();
        requestOptions.Tools.Add(ChatTool.CreateFunctionTool(
            "release_gate",
            "Check release gate",
            BinaryData.FromString("{\"type\":\"object\",\"properties\":{\"release\":{\"type\":\"string\"}}}")
        ));

        await OpenAIRecorder.CompleteChatAsync(
            client,
            messages,
            (_, _, _) => Task.FromResult(OpenAIChatModelFactory.ChatCompletion(
                id: $"dotnet-openai-sync-{context.Turn}",
                finishReason: ChatFinishReason.Stop,
                content: new ChatMessageContent($"Plan {context.Turn}: validate canary, assign owner, publish summary."),
                role: ChatMessageRole.Assistant,
                model: "gpt-5",
                usage: OpenAIChatModelFactory.ChatTokenUsage(
                    inputTokenCount: 86 + (context.Turn % 9),
                    outputTokenCount: 25 + (context.Turn % 6),
                    totalTokenCount: 111 + (context.Turn % 11)
                )
            )),
            requestOptions: requestOptions,
            options: new OpenAIAgento11yOptions
            {
                ProviderName = "openai",
                ModelName = "gpt-5",
                ConversationId = context.ConversationId,
                AgentName = context.AgentName,
                AgentVersion = context.AgentVersion,
                Tags = context.Tags,
                Metadata = context.Metadata,
            },
            cancellationToken: cancellationToken
        ).ConfigureAwait(false);
    }

    private static async Task EmitOpenAiChatStreamAsync(Agento11yClient client, EmitContext context, CancellationToken cancellationToken)
    {
        var messages = new List<ChatMessage>
        {
            new UserChatMessage($"Stream ticket status {context.Turn}."),
        };

        await OpenAIRecorder.CompleteChatStreamingAsync(
            client,
            messages,
            (_, _, ct) => StreamOpenAiUpdates(context.Turn, ct),
            requestOptions: null,
            options: new OpenAIAgento11yOptions
            {
                ProviderName = "openai",
                ModelName = "gpt-5",
                ConversationId = context.ConversationId,
                AgentName = context.AgentName,
                AgentVersion = context.AgentVersion,
                Tags = context.Tags,
                Metadata = context.Metadata,
            },
            cancellationToken: cancellationToken
        ).ConfigureAwait(false);
    }

    private static async IAsyncEnumerable<StreamingChatCompletionUpdate> StreamOpenAiUpdates(
        int turn,
        [System.Runtime.CompilerServices.EnumeratorCancellation] CancellationToken cancellationToken
    )
    {
        yield return OpenAIChatModelFactory.StreamingChatCompletionUpdate(
            completionId: $"dotnet-openai-stream-{turn}",
            contentUpdate: new ChatMessageContent($"Ticket {turn}: canary healthy"),
            model: "gpt-5"
        );

        cancellationToken.ThrowIfCancellationRequested();

        yield return OpenAIChatModelFactory.StreamingChatCompletionUpdate(
            completionId: $"dotnet-openai-stream-{turn}",
            contentUpdate: new ChatMessageContent("; production gate passed."),
            finishReason: ChatFinishReason.Stop,
            usage: OpenAIChatModelFactory.ChatTokenUsage(
                inputTokenCount: 49 + (turn % 5),
                outputTokenCount: 16 + (turn % 4),
                totalTokenCount: 65 + (turn % 7)
            ),
            model: "gpt-5"
        );

        await Task.CompletedTask;
    }

    private static async Task EmitOpenAiResponsesSyncAsync(Agento11yClient client, EmitContext context, CancellationToken cancellationToken)
    {
        var inputItems = new List<ResponseItem>
        {
            ResponseItem.CreateUserMessageItem($"Draft rollout plan {context.Turn}."),
        };

        var requestOptions = new CreateResponseOptions
        {
            Instructions = "Return concise rollout planning bullets.",
            MaxOutputTokenCount = 320,
            Temperature = 0.2f,
            TopP = 0.9f,
            ToolChoice = ResponseToolChoice.CreateFunctionChoice("release_gate"),
        };
        requestOptions.Tools.Add(ResponseTool.CreateFunctionTool(
            "release_gate",
            BinaryData.FromString("{\"type\":\"object\",\"properties\":{\"release\":{\"type\":\"string\"}}}"),
            strictModeEnabled: true,
            functionDescription: "Check release gate"
        ));

        await OpenAIRecorder.CreateResponseAsync(
            client,
            inputItems,
            (_, _, _) => Task.FromResult(ReadOpenAIResponse(
                $$"""
                {
                  "id": "dotnet-openai-responses-sync-{{context.Turn}}",
                  "created_at": 1,
                  "model": "gpt-5",
                  "object": "response",
                  "output": [
                    {
                      "id": "dotnet-openai-responses-sync-msg-{{context.Turn}}",
                      "type": "message",
                      "role": "assistant",
                      "status": "completed",
                      "content": [{"type": "output_text", "text": "Plan {{context.Turn}}: validate canary, assign owner, publish summary."}]
                    }
                  ],
                  "parallel_tool_calls": false,
                  "tool_choice": "auto",
                  "tools": [],
                  "status": "completed",
                  "usage": {
                    "input_tokens": {{86 + (context.Turn % 9)}},
                    "output_tokens": {{25 + (context.Turn % 6)}},
                    "total_tokens": {{111 + (context.Turn % 11)}}
                  }
                }
                """
            )),
            requestOptions: requestOptions,
            options: new OpenAIAgento11yOptions
            {
                ProviderName = "openai",
                ModelName = "gpt-5",
                ConversationId = context.ConversationId,
                AgentName = context.AgentName,
                AgentVersion = context.AgentVersion,
                Tags = context.Tags,
                Metadata = context.Metadata,
            },
            cancellationToken: cancellationToken
        ).ConfigureAwait(false);
    }

    private static async Task EmitOpenAiResponsesStreamAsync(Agento11yClient client, EmitContext context, CancellationToken cancellationToken)
    {
        var inputItems = new List<ResponseItem>
        {
            ResponseItem.CreateUserMessageItem($"Stream ticket status {context.Turn}."),
        };

        var requestOptions = new CreateResponseOptions
        {
            Instructions = "Stream short operational deltas.",
            MaxOutputTokenCount = 220,
        };

        await OpenAIRecorder.CreateResponseStreamingAsync(
            client,
            inputItems,
            (_, _, ct) => StreamOpenAiResponseUpdates(context.Turn, ct),
            requestOptions: requestOptions,
            options: new OpenAIAgento11yOptions
            {
                ProviderName = "openai",
                ModelName = "gpt-5",
                ConversationId = context.ConversationId,
                AgentName = context.AgentName,
                AgentVersion = context.AgentVersion,
                Tags = context.Tags,
                Metadata = context.Metadata,
            },
            cancellationToken: cancellationToken
        ).ConfigureAwait(false);
    }

    private static async IAsyncEnumerable<StreamingResponseUpdate> StreamOpenAiResponseUpdates(
        int turn,
        [System.Runtime.CompilerServices.EnumeratorCancellation] CancellationToken cancellationToken
    )
    {
        yield return ReadStreamingResponseUpdate(
            $$"""
            {
              "type": "response.output_text.delta",
              "content_index": 0,
              "delta": "Ticket {{turn}}: canary healthy",
              "item_id": "dotnet-openai-responses-stream-msg-{{turn}}",
              "output_index": 0,
              "sequence_number": 1
            }
            """
        );

        cancellationToken.ThrowIfCancellationRequested();

        yield return ReadStreamingResponseUpdate(
            $$"""
            {
              "type": "response.output_text.done",
              "content_index": 0,
              "text": "; production gate passed.",
              "item_id": "dotnet-openai-responses-stream-msg-{{turn}}",
              "output_index": 0,
              "sequence_number": 2
            }
            """
        );

        yield return ReadStreamingResponseUpdate(
            $$"""
            {
              "type": "response.completed",
              "sequence_number": 3,
              "response": {
                "id": "dotnet-openai-responses-stream-{{turn}}",
                "created_at": 1,
                "model": "gpt-5",
                "object": "response",
                "output": [],
                "parallel_tool_calls": false,
                "tool_choice": "auto",
                "tools": [],
                "status": "completed",
                "usage": {
                  "input_tokens": {{49 + (turn % 5)}},
                  "output_tokens": {{16 + (turn % 4)}},
                  "total_tokens": {{65 + (turn % 7)}}
                }
              }
            }
            """
        );

        await Task.CompletedTask;
    }

    private static async Task EmitAnthropicSyncAsync(Agento11yClient client, EmitContext context, CancellationToken cancellationToken)
    {
        var request = new AnthropicMessageCreateParams
        {
            Model = AnthropicModel.ClaudeSonnet4_5,
            MaxTokens = 512,
            System = "Summarize with diagnosis and recommendation.",
            Messages =
            [
                new AnthropicMessageParam
                {
                    Role = AnthropicRole.User,
                    Content = $"Summarize reliability drift {context.Turn}.",
                },
            ],
        };

        await AnthropicRecorder.MessageAsync(
            client,
            request,
            (_, _) => Task.FromResult<AnthropicMessage>(new AnthropicMessage
            {
                Container = default!,
                ID = $"dotnet-anthropic-sync-{context.Turn}",
                Model = AnthropicModel.ClaudeSonnet4_5,
                Content =
                [
                    new AnthropicTextBlock
                    {
                        Type = JsonSerializer.SerializeToElement("text"),
                        Text = $"Diagnosis {context.Turn}: retry storms in eu-west; rebalance queues.",
                        Citations = null,
                    },
                ],
                StopReason = AnthropicStopReason.EndTurn,
                StopSequence = null,
                Usage = new AnthropicUsage
                {
                    InputTokens = 72 + (context.Turn % 8),
                    OutputTokens = 30 + (context.Turn % 5),
                    CacheReadInputTokens = 10,
                    CacheCreationInputTokens = 0,
                    InferenceGeo = "us",
                    CacheCreation = null,
                    ServerToolUse = null,
                    ServiceTier = null,
                },
            }),
            options: new AnthropicAgento11yOptions
            {
                ProviderName = "anthropic",
                ModelName = "claude-sonnet-4-5",
                ConversationId = context.ConversationId,
                AgentName = context.AgentName,
                AgentVersion = context.AgentVersion,
                Tags = context.Tags,
                Metadata = context.Metadata,
            },
            cancellationToken: cancellationToken
        ).ConfigureAwait(false);
    }

    private static async Task EmitAnthropicStreamAsync(Agento11yClient client, EmitContext context, CancellationToken cancellationToken)
    {
        var request = new AnthropicMessageCreateParams
        {
            Model = AnthropicModel.ClaudeSonnet4_5,
            MaxTokens = 512,
            System = "Emit mitigation status deltas.",
            Messages =
            [
                new AnthropicMessageParam
                {
                    Role = AnthropicRole.User,
                    Content = $"Stream mitigation delta {context.Turn}.",
                },
            ],
        };

        await AnthropicRecorder.MessageStreamAsync(
            client,
            request,
            (_, ct) => StreamAnthropicEvents(context.Turn, ct),
            options: new AnthropicAgento11yOptions
            {
                ProviderName = "anthropic",
                ModelName = "claude-sonnet-4-5",
                ConversationId = context.ConversationId,
                AgentName = context.AgentName,
                AgentVersion = context.AgentVersion,
                Tags = context.Tags,
                Metadata = context.Metadata,
            },
            cancellationToken: cancellationToken
        ).ConfigureAwait(false);
    }

    private static async IAsyncEnumerable<AnthropicRawMessageStreamEvent> StreamAnthropicEvents(
        int turn,
        [System.Runtime.CompilerServices.EnumeratorCancellation] CancellationToken cancellationToken
    )
    {
        yield return new AnthropicRawMessageStreamEvent(new AnthropicRawMessageStartEvent
        {
            Type = JsonSerializer.SerializeToElement("message_start"),
            Message = new AnthropicMessage
            {
                Container = default!,
                ID = $"dotnet-anthropic-stream-{turn}",
                Model = AnthropicModel.ClaudeSonnet4_5,
                Content =
                [
                    new AnthropicTextBlock
                    {
                        Type = JsonSerializer.SerializeToElement("text"),
                        Text = $"Change {turn}: guard enabled",
                        Citations = null,
                    },
                ],
                StopReason = null,
                StopSequence = null,
                Usage = new AnthropicUsage
                {
                    InputTokens = 0,
                    OutputTokens = 0,
                    CacheReadInputTokens = null,
                    CacheCreationInputTokens = null,
                    InferenceGeo = "us",
                    CacheCreation = null,
                    ServerToolUse = null,
                    ServiceTier = null,
                },
            },
        });

        cancellationToken.ThrowIfCancellationRequested();

        yield return new AnthropicRawMessageStreamEvent(new AnthropicRawMessageDeltaEvent
        {
            Type = JsonSerializer.SerializeToElement("message_delta"),
            Delta = new AnthropicDelta
            {
                Container = default!,
                StopReason = AnthropicStopReason.EndTurn,
                StopSequence = null,
            },
            Usage = new AnthropicMessageDeltaUsage
            {
                InputTokens = 45 + (turn % 6),
                OutputTokens = 16 + (turn % 4),
                CacheReadInputTokens = null,
                CacheCreationInputTokens = null,
                ServerToolUse = null,
            },
        });

        await Task.CompletedTask;
    }

    private static async Task EmitGeminiSyncAsync(Agento11yClient client, EmitContext context, CancellationToken cancellationToken)
    {
        var model = "gemini-2.5-pro";
        var contents = new List<GeminiContent>
        {
            new GeminiContent
            {
                Role = "user",
                Parts = [new GPart { Text = $"Generate launch summary {context.Turn}." }],
            },
            new GeminiContent
            {
                Role = "user",
                Parts =
                [
                    new GPart
                    {
                        FunctionResponse = new GeminiFunctionResponse
                        {
                            Id = "release_metrics",
                            Name = "release_metrics",
                            Response = new Dictionary<string, object>
                            {
                                ["status"] = "green",
                            },
                        },
                    },
                ],
            },
        };
        var config = new GeminiGenerateContentConfig
        {
            SystemInstruction = new GeminiContent
            {
                Role = "user",
                Parts = [new GPart { Text = "Use structured release-note style." }],
            },
        };

        await GeminiRecorder.GenerateContentAsync(
            client,
            model,
            contents,
            (_, _, _, _) => Task.FromResult(new GeminiGenerateContentResponse
            {
                ResponseId = $"dotnet-gemini-sync-{context.Turn}",
                ModelVersion = "gemini-2.5-pro-001",
                Candidates =
                [
                    new GeminiCandidate
                    {
                        FinishReason = GeminiFinishReason.Stop,
                        Content = new GeminiContent
                        {
                            Role = "model",
                            Parts = [new GPart { Text = $"Launch {context.Turn}: all gates green; metrics stable." }],
                        },
                    },
                ],
                UsageMetadata = new GeminiGenerateContentResponseUsageMetadata
                {
                    PromptTokenCount = 60 + (context.Turn % 7),
                    CandidatesTokenCount = 19 + (context.Turn % 5),
                    TotalTokenCount = 79 + (context.Turn % 8),
                    ThoughtsTokenCount = 6,
                },
            }),
            config,
            options: new GeminiAgento11yOptions
            {
                ProviderName = "gemini",
                ModelName = "gemini-2.5-pro",
                ConversationId = context.ConversationId,
                AgentName = context.AgentName,
                AgentVersion = context.AgentVersion,
                Tags = context.Tags,
                Metadata = context.Metadata,
            },
            cancellationToken: cancellationToken
        ).ConfigureAwait(false);
    }

    private static async Task EmitGeminiStreamAsync(Agento11yClient client, EmitContext context, CancellationToken cancellationToken)
    {
        var model = "gemini-2.5-pro";
        var contents = new List<GeminiContent>
        {
            new GeminiContent
            {
                Role = "user",
                Parts = [new GPart { Text = $"Stream migration status {context.Turn}." }],
            },
        };

        await GeminiRecorder.GenerateContentStreamAsync(
            client,
            model,
            contents,
            (_, _, _, ct) => StreamGeminiResponses(context.Turn, ct),
            options: new GeminiAgento11yOptions
            {
                ProviderName = "gemini",
                ModelName = "gemini-2.5-pro",
                ConversationId = context.ConversationId,
                AgentName = context.AgentName,
                AgentVersion = context.AgentVersion,
                Tags = context.Tags,
                Metadata = context.Metadata,
            },
            cancellationToken: cancellationToken
        ).ConfigureAwait(false);
    }

    private static async IAsyncEnumerable<GeminiGenerateContentResponse> StreamGeminiResponses(
        int turn,
        [System.Runtime.CompilerServices.EnumeratorCancellation] CancellationToken cancellationToken
    )
    {
        yield return new GeminiGenerateContentResponse
        {
            ResponseId = $"dotnet-gemini-stream-{turn}",
            ModelVersion = "gemini-2.5-pro-001",
            Candidates =
            [
                new GeminiCandidate
                {
                    Content = new GeminiContent
                    {
                        Role = "model",
                        Parts = [new GPart { Text = $"Wave {turn}: shard sync complete" }],
                    },
                },
            ],
        };

        cancellationToken.ThrowIfCancellationRequested();

        yield return new GeminiGenerateContentResponse
        {
            ResponseId = $"dotnet-gemini-stream-{turn}",
            ModelVersion = "gemini-2.5-pro-001",
            Candidates =
            [
                new GeminiCandidate
                {
                    FinishReason = GeminiFinishReason.Stop,
                    Content = new GeminiContent
                    {
                        Role = "model",
                        Parts = [new GPart { Text = "; promotion done." }],
                    },
                },
            ],
            UsageMetadata = new GeminiGenerateContentResponseUsageMetadata
            {
                PromptTokenCount = 46 + (turn % 5),
                CandidatesTokenCount = 17 + (turn % 4),
                TotalTokenCount = 63 + (turn % 7),
            },
        };

        await Task.CompletedTask;
    }

    private static void EmitCustomSync(Agento11yClient client, RuntimeConfig config, EmitContext context)
    {
        var recorder = client.StartGeneration(new GenerationStart
        {
            ConversationId = context.ConversationId,
            AgentName = context.AgentName,
            AgentVersion = context.AgentVersion,
            Model = new ModelRef
            {
                Provider = config.CustomProvider,
                Name = "mistral-large-devex",
            },
            Tags = new Dictionary<string, string>(context.Tags, StringComparer.Ordinal),
            Metadata = new Dictionary<string, object?>(context.Metadata, StringComparer.Ordinal),
        });

        try
        {
            recorder.SetResult(new Generation
            {
                Input =
                [
                    Message.UserTextMessage($"Draft custom checkpoint {context.Turn}."),
                ],
                Output =
                [
                    Message.AssistantTextMessage($"Custom provider sync {context.Turn}: all guardrails satisfied."),
                ],
                Usage = new TokenUsage
                {
                    InputTokens = 30 + (context.Turn % 6),
                    OutputTokens = 15 + (context.Turn % 4),
                    TotalTokens = 45 + (context.Turn % 7),
                },
                StopReason = "stop",
            });
        }
        finally
        {
            recorder.End();
        }

        if (recorder.Error is not null)
        {
            throw recorder.Error;
        }
    }

    private static void EmitCustomStream(Agento11yClient client, RuntimeConfig config, EmitContext context)
    {
        var recorder = client.StartStreamingGeneration(new GenerationStart
        {
            ConversationId = context.ConversationId,
            AgentName = context.AgentName,
            AgentVersion = context.AgentVersion,
            Model = new ModelRef
            {
                Provider = config.CustomProvider,
                Name = "mistral-large-devex",
            },
            Tags = new Dictionary<string, string>(context.Tags, StringComparer.Ordinal),
            Metadata = new Dictionary<string, object?>(context.Metadata, StringComparer.Ordinal),
        });

        try
        {
            recorder.SetResult(new Generation
            {
                Input =
                [
                    Message.UserTextMessage($"Stream custom remediation summary {context.Turn}."),
                ],
                Output =
                [
                    new Message
                    {
                        Role = Grafana.Agento11y.MessageRole.Assistant,
                        Parts =
                        [
                            Part.ThinkingPart("assembling synthetic stream segments"),
                            Part.TextPart($"Custom stream {context.Turn}: segment A complete; segment B complete."),
                        ],
                    },
                ],
                Usage = new TokenUsage
                {
                    InputTokens = 24 + (context.Turn % 5),
                    OutputTokens = 16 + (context.Turn % 4),
                    TotalTokens = 40 + (context.Turn % 6),
                },
                StopReason = "end_turn",
            });
        }
        finally
        {
            recorder.End();
        }

        if (recorder.Error is not null)
        {
            throw recorder.Error;
        }
    }

    private static TagEnvelope BuildTagEnvelope(string source, GenerationMode mode, int turn, int slot)
    {
        var persona = PersonaForTurn(turn);

        var tags = new Dictionary<string, string>(StringComparer.Ordinal)
        {
            ["agento11y.devex.language"] = Language,
            ["agento11y.devex.provider"] = source,
            ["agento11y.devex.source"] = SourceTagFor(source),
            ["agento11y.devex.scenario"] = ScenarioFor(source, turn),
            ["agento11y.devex.mode"] = mode == GenerationMode.Stream ? "STREAM" : "SYNC",
        };

        var metadata = new Dictionary<string, object?>(StringComparer.Ordinal)
        {
            ["turn_index"] = turn,
            ["conversation_slot"] = slot,
            ["agent_persona"] = persona,
            ["emitter"] = "sdk-traffic",
            ["provider_shape"] = ProviderShapeFor(source, turn),
        };

        return new TagEnvelope(persona, tags, metadata);
    }

    private static string PersonaForTurn(int turn)
    {
        return Personas[turn % Personas.Length];
    }

    private static string ScenarioFor(string source, int turn)
    {
        var even = (turn % 2) == 0;
        return source switch
        {
            "openai" => even ? "openai_plan" : "openai_stream",
            "anthropic" => even ? "anthropic_reasoning" : "anthropic_delta",
            "gemini" => even ? "gemini_structured" : "gemini_flow",
            _ => even ? "custom_mistral_sync" : "custom_mistral_stream",
        };
    }

    private static string ProviderShapeFor(string source, int turn)
    {
        return source switch
        {
            "openai" => turn % 2 == 0 ? "openai_chat_completions" : "openai_responses",
            "anthropic" => "messages",
            "gemini" => "generate_content",
            _ => "core_generation",
        };
    }

    private static string SourceTagFor(string source)
    {
        return source == "mistral" ? "core_custom" : "provider_wrapper";
    }

    private static ResponseResult ReadOpenAIResponse(string json)
    {
        return ModelReaderWriter.Read<ResponseResult>(BinaryData.FromString(json));
    }

    private static StreamingResponseUpdate ReadStreamingResponseUpdate(string json)
    {
        return ModelReaderWriter.Read<StreamingResponseUpdate>(BinaryData.FromString(json));
    }

    private sealed record RuntimeConfig(
        int IntervalMs,
        int StreamPercent,
        int Conversations,
        int RotateTurns,
        string CustomProvider,
        string GenGrpcEndpoint,
        int MaxCycles
    )
    {
        public static RuntimeConfig Load()
        {
            return new RuntimeConfig(
                IntervalMs: IntFromEnv("SIGIL_TRAFFIC_INTERVAL_MS", 2000),
                StreamPercent: IntFromEnv("SIGIL_TRAFFIC_STREAM_PERCENT", 30),
                Conversations: IntFromEnv("SIGIL_TRAFFIC_CONVERSATIONS", 3),
                RotateTurns: IntFromEnv("SIGIL_TRAFFIC_ROTATE_TURNS", 24),
                CustomProvider: StringFromEnv("SIGIL_TRAFFIC_CUSTOM_PROVIDER", "mistral"),
                GenGrpcEndpoint: StringFromEnv("SIGIL_TRAFFIC_GEN_GRPC_ENDPOINT", "sigil:4317"),
                MaxCycles: IntFromEnv("SIGIL_TRAFFIC_MAX_CYCLES", 0)
            );
        }

        private static int IntFromEnv(string key, int defaultValue)
        {
            var raw = System.Environment.GetEnvironmentVariable(key);
            if (string.IsNullOrWhiteSpace(raw))
            {
                return defaultValue;
            }

            return int.TryParse(raw.Trim(), out var parsed) && parsed > 0 ? parsed : defaultValue;
        }

        private static string StringFromEnv(string key, string defaultValue)
        {
            var raw = System.Environment.GetEnvironmentVariable(key);
            return string.IsNullOrWhiteSpace(raw) ? defaultValue : raw.Trim();
        }
    }

    private sealed class SourceState
    {
        public SourceState(int conversations)
        {
            Slots = Enumerable.Range(0, conversations).Select(_ => new ThreadState()).ToList();
        }

        public int Cursor { get; set; }

        public List<ThreadState> Slots { get; }
    }

    private sealed class ThreadState
    {
        public string ConversationId { get; set; } = string.Empty;

        public int Turn { get; set; }
    }

    private sealed record EmitContext(
        string ConversationId,
        int Turn,
        int Slot,
        string AgentName,
        string AgentVersion,
        IReadOnlyDictionary<string, string> Tags,
        IReadOnlyDictionary<string, object?> Metadata
    );

    private sealed record TagEnvelope(
        string AgentPersona,
        IReadOnlyDictionary<string, string> Tags,
        IReadOnlyDictionary<string, object?> Metadata
    );
}
