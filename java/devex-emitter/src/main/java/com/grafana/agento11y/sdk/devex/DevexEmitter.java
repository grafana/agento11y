package com.grafana.agento11y.sdk.devex;

import com.grafana.agento11y.sdk.AuthConfig;
import com.grafana.agento11y.sdk.AuthMode;
import com.grafana.agento11y.sdk.GenerationExportConfig;
import com.grafana.agento11y.sdk.GenerationExportProtocol;
import com.grafana.agento11y.sdk.GenerationMode;
import com.grafana.agento11y.sdk.GenerationResult;
import com.grafana.agento11y.sdk.GenerationStart;
import com.grafana.agento11y.sdk.Message;
import com.grafana.agento11y.sdk.MessagePart;
import com.grafana.agento11y.sdk.MessageRole;
import com.grafana.agento11y.sdk.ModelRef;
import com.grafana.agento11y.sdk.Agento11yClient;
import com.grafana.agento11y.sdk.Agento11yClientConfig;
import com.grafana.agento11y.sdk.TokenUsage;
import com.grafana.agento11y.sdk.providers.anthropic.AnthropicAdapter;
import com.grafana.agento11y.sdk.providers.anthropic.AnthropicOptions;
import com.grafana.agento11y.sdk.providers.gemini.GeminiAdapter;
import com.grafana.agento11y.sdk.providers.gemini.GeminiOptions;
import com.grafana.agento11y.sdk.providers.openai.OpenAiChatCompletions;
import com.grafana.agento11y.sdk.providers.openai.OpenAiOptions;
import com.grafana.agento11y.sdk.providers.openai.OpenAiResponses;
import com.anthropic.core.http.StreamResponse;
import com.anthropic.models.messages.MessageCreateParams;
import com.anthropic.models.messages.RawMessageStreamEvent;
import com.google.genai.types.Content;
import com.google.genai.types.GenerateContentConfig;
import com.google.genai.types.GenerateContentResponse;
import com.google.genai.types.Part;
import com.openai.core.ObjectMappers;
import com.openai.models.ReasoningEffort;
import com.openai.models.chat.completions.ChatCompletion;
import com.openai.models.chat.completions.ChatCompletionChunk;
import com.openai.models.chat.completions.ChatCompletionCreateParams;
import com.openai.models.responses.Response;
import com.openai.models.responses.ResponseCreateParams;
import com.openai.models.responses.ResponseStreamEvent;
import java.io.IOException;
import java.time.Instant;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ThreadLocalRandom;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.stream.Stream;

/** Continuously emits synthetic multi-provider SDK traffic for compose devex flows. */
public final class DevexEmitter {
    static final String LANGUAGE = "java";
    static final List<String> SOURCES = List.of("openai", "anthropic", "gemini", "mistral");
    static final List<String> PERSONAS = List.of("planner", "retriever", "executor");

    private DevexEmitter() {
    }

    public static void main(String[] args) throws Exception {
        runEmitter(loadConfig());
    }

    static void runEmitter(RuntimeConfig config) throws Exception {
        AtomicBoolean stop = new AtomicBoolean(false);
        Runtime.getRuntime().addShutdownHook(new Thread(() -> stop.set(true)));

        Agento11yClient client = new Agento11yClient(new Agento11yClientConfig()
                .setGenerationExport(new GenerationExportConfig()
                        .setProtocol(GenerationExportProtocol.GRPC)
                        .setEndpoint(config.genGrpcEndpoint)
                        .setAuth(new AuthConfig().setMode(AuthMode.NONE))
                        .setInsecure(true)));

        Map<String, SourceState> sourceState = new LinkedHashMap<>();
        for (String source : SOURCES) {
            sourceState.put(source, new SourceState(config.conversations));
        }

        long cycles = 0;
        System.out.printf(
                "[java-emitter] started interval_ms=%d stream_percent=%d conversations=%d rotate_turns=%d custom_provider=%s%n",
                config.intervalMs,
                config.streamPercent,
                config.conversations,
                config.rotateTurns,
                config.customProvider);

        try {
            while (!stop.get()) {
                for (String source : SOURCES) {
                    SourceState state = sourceState.get(source);
                    int slot = state.cursor % config.conversations;
                    state.cursor += 1;

                    ThreadState thread = resolveThread(state, config.rotateTurns, source, slot);
                    GenerationMode mode = chooseMode(ThreadLocalRandom.current().nextInt(100), config.streamPercent);
                    TagEnvelope envelope = buildTagEnvelope(source, mode, thread.turn, slot);

                    EmitContext context = new EmitContext(
                            thread.conversationId,
                            thread.turn,
                            slot,
                            "devex-" + LANGUAGE + "-" + source + "-" + envelope.agentPersona,
                            "devex-1",
                            envelope.tags,
                            envelope.metadata);

                    emitForSource(client, config, source, mode, context);
                    thread.turn += 1;
                }

                cycles += 1;
                if (config.maxCycles > 0 && cycles >= config.maxCycles) {
                    break;
                }

                int jitter = ThreadLocalRandom.current().nextInt(-200, 201);
                long sleepMs = Math.max(200, config.intervalMs + jitter);
                Thread.sleep(sleepMs);
            }
        } finally {
            client.shutdown();
        }
    }

    static RuntimeConfig loadConfig() {
        return new RuntimeConfig(
                intFromEnv("SIGIL_TRAFFIC_INTERVAL_MS", 2000),
                intFromEnv("SIGIL_TRAFFIC_STREAM_PERCENT", 30),
                intFromEnv("SIGIL_TRAFFIC_CONVERSATIONS", 3),
                intFromEnv("SIGIL_TRAFFIC_ROTATE_TURNS", 24),
                stringFromEnv("SIGIL_TRAFFIC_CUSTOM_PROVIDER", "mistral"),
                stringFromEnv("SIGIL_TRAFFIC_GEN_GRPC_ENDPOINT", "sigil:4317"),
                intFromEnv("SIGIL_TRAFFIC_MAX_CYCLES", 0));
    }

    static int intFromEnv(String key, int defaultValue) {
        String raw = System.getenv(key);
        if (raw == null || raw.trim().isEmpty()) {
            return defaultValue;
        }
        try {
            int parsed = Integer.parseInt(raw.trim());
            if (parsed <= 0) {
                return defaultValue;
            }
            return parsed;
        } catch (NumberFormatException ignored) {
            return defaultValue;
        }
    }

    static String stringFromEnv(String key, String defaultValue) {
        String raw = System.getenv(key);
        if (raw == null || raw.trim().isEmpty()) {
            return defaultValue;
        }
        return raw.trim();
    }

    static String sourceTagFor(String source) {
        return "mistral".equals(source) ? "core_custom" : "provider_wrapper";
    }

    static String providerShapeFor(String source, int turn) {
        return switch (source) {
            case "openai" -> turn % 2 == 0 ? "openai_chat_completions" : "openai_responses";
            case "anthropic" -> "messages";
            case "gemini" -> "generate_content";
            default -> "core_generation";
        };
    }

    static String scenarioFor(String source, int turn) {
        boolean even = (turn % 2) == 0;
        return switch (source) {
            case "openai" -> even ? "openai_plan" : "openai_stream";
            case "anthropic" -> even ? "anthropic_reasoning" : "anthropic_delta";
            case "gemini" -> even ? "gemini_structured" : "gemini_flow";
            default -> even ? "custom_mistral_sync" : "custom_mistral_stream";
        };
    }

    static String personaForTurn(int turn) {
        return PERSONAS.get(turn % PERSONAS.size());
    }

    static GenerationMode chooseMode(int roll, int streamPercent) {
        return roll < streamPercent ? GenerationMode.STREAM : GenerationMode.SYNC;
    }

    static ThreadState resolveThread(SourceState state, int rotateTurns, String source, int slot) {
        ThreadState thread = state.slots.get(slot);
        if (thread.conversationId.isBlank() || thread.turn >= rotateTurns) {
            thread.conversationId = newConversationID(source, slot);
            thread.turn = 0;
        }
        return thread;
    }

    static TagEnvelope buildTagEnvelope(String source, GenerationMode mode, int turn, int slot) {
        String agentPersona = personaForTurn(turn);

        Map<String, String> tags = new LinkedHashMap<>();
        tags.put("agento11y.devex.language", LANGUAGE);
        tags.put("agento11y.devex.provider", source);
        tags.put("agento11y.devex.source", sourceTagFor(source));
        tags.put("agento11y.devex.scenario", scenarioFor(source, turn));
        tags.put("agento11y.devex.mode", mode.name());

        Map<String, Object> metadata = new LinkedHashMap<>();
        metadata.put("turn_index", turn);
        metadata.put("conversation_slot", slot);
        metadata.put("agent_persona", agentPersona);
        metadata.put("emitter", "sdk-traffic");
        metadata.put("provider_shape", providerShapeFor(source, turn));

        return new TagEnvelope(agentPersona, tags, metadata);
    }

    static String newConversationID(String source, int slot) {
        return "devex-" + LANGUAGE + "-" + source + "-" + slot + "-" + Instant.now().toEpochMilli();
    }

    static void emitForSource(Agento11yClient client, RuntimeConfig config, String source, GenerationMode mode, EmitContext context)
            throws Exception {
        if ("openai".equals(source)) {
            String shape = providerShapeFor(source, context.turn);
            boolean useResponses = "openai_responses".equals(shape);
            if (mode == GenerationMode.STREAM) {
                if (useResponses) {
                    emitOpenAiResponsesStream(client, context);
                    return;
                }
                emitOpenAiChatStream(client, context);
                return;
            }

            if (useResponses) {
                emitOpenAiResponsesSync(client, context);
                return;
            }
            emitOpenAiChatSync(client, context);
            return;
        }

        if ("anthropic".equals(source)) {
            if (mode == GenerationMode.STREAM) {
                emitAnthropicStream(client, context);
                return;
            }
            emitAnthropicSync(client, context);
            return;
        }

        if ("gemini".equals(source)) {
            if (mode == GenerationMode.STREAM) {
                emitGeminiStream(client, context);
                return;
            }
            emitGeminiSync(client, context);
            return;
        }

        if (mode == GenerationMode.STREAM) {
            emitCustomStream(client, config, context);
            return;
        }
        emitCustomSync(client, config, context);
    }

    static void emitOpenAiChatSync(Agento11yClient client, EmitContext context) throws Exception {
        ChatCompletionCreateParams request = ChatCompletionCreateParams.builder()
                .model("gpt-5")
                .addSystemMessage("Return concise rollout plans with three bullets.")
                .addUserMessage("Draft rollout plan " + context.turn + ".")
                .maxCompletionTokens(320L)
                .temperature(0.2)
                .topP(0.9)
                .reasoningEffort(ReasoningEffort.MEDIUM)
                .build();

        OpenAiChatCompletions.create(
                client,
                request,
                ignored -> chatResponseFixture(context.turn),
                openAiOptions(context));
    }

    static void emitOpenAiChatStream(Agento11yClient client, EmitContext context) throws Exception {
        ChatCompletionCreateParams request = ChatCompletionCreateParams.builder()
                .model("gpt-5")
                .addSystemMessage("Stream short operational deltas.")
                .addUserMessage("Stream ticket status " + context.turn + ".")
                .maxCompletionTokens(220L)
                .reasoningEffort(ReasoningEffort.MEDIUM)
                .build();

        OpenAiChatCompletions.createStreaming(
                client,
                request,
                ignored -> new FixedOpenAiStreamResponse<>(List.of(chatChunkOne(context.turn), chatChunkTwo(context.turn))),
                openAiOptions(context));
    }

    static void emitOpenAiResponsesSync(Agento11yClient client, EmitContext context) throws Exception {
        ResponseCreateParams request = ResponseCreateParams.builder()
                .model("gpt-5")
                .instructions("Return concise rollout plans with three bullets.")
                .input("Draft rollout plan " + context.turn + ".")
                .maxOutputTokens(320L)
                .temperature(0.2)
                .topP(0.9)
                .build();

        OpenAiResponses.create(
                client,
                request,
                ignored -> responsesResponseFixture(context.turn),
                openAiOptions(context));
    }

    static void emitOpenAiResponsesStream(Agento11yClient client, EmitContext context) throws Exception {
        ResponseCreateParams request = ResponseCreateParams.builder()
                .model("gpt-5")
                .instructions("Stream short operational deltas.")
                .input("Stream ticket status " + context.turn + ".")
                .maxOutputTokens(220L)
                .build();

        OpenAiResponses.createStreaming(
                client,
                request,
                ignored -> new FixedOpenAiStreamResponse<>(List.of(
                        responseTextDeltaEvent("Ticket " + context.turn + ": canary healthy", 1),
                        responseTextDeltaEvent("; production gate passed.", 2),
                        responseCompletedEvent(context.turn))),
                openAiOptions(context));
    }

    static void emitAnthropicSync(Agento11yClient client, EmitContext context) throws Exception {
        MessageCreateParams request = MessageCreateParams.builder()
                .model("claude-sonnet-4-5")
                .system("Summarize with diagnosis and recommendation.")
                .maxTokens(320)
                .temperature(0.3)
                .addUserMessage("Summarize reliability drift " + context.turn + ".")
                .build();

        AnthropicAdapter.completion(
                client,
                request,
                ignored -> anthropicMessageFixture(context.turn),
                anthropicOptions(context));
    }

    static void emitAnthropicStream(Agento11yClient client, EmitContext context) throws Exception {
        MessageCreateParams request = MessageCreateParams.builder()
                .model("claude-sonnet-4-5")
                .system("Emit mitigation status deltas.")
                .maxTokens(280)
                .addUserMessage("Stream mitigation deltas " + context.turn + ".")
                .build();

        AnthropicAdapter.completionStream(
                client,
                request,
                ignored -> new FixedAnthropicStreamResponse<>(List.of()),
                anthropicOptions(context));
    }

    static void emitGeminiSync(Agento11yClient client, EmitContext context) throws Exception {
        List<Content> contents = List.of(
                Content.builder()
                        .role("user")
                        .parts(Part.fromText("Generate launch summary " + context.turn + "."))
                        .build(),
                Content.builder()
                        .role("tool")
                        .parts(Part.fromFunctionResponse("release_metrics", Map.of("status", "green")))
                        .build());
        GenerateContentConfig config = GenerateContentConfig.fromJson("""
                {
                  "systemInstruction": {"parts": [{"text": "Use structured release-note style."}]},
                  "maxOutputTokens": 320,
                  "temperature": 0.2,
                  "topP": 0.9,
                  "thinkingConfig": {"includeThoughts": true, "thinkingBudget": 256}
                }
                """);

        GeminiAdapter.completion(
                client,
                "gemini-2.5-pro",
                contents,
                config,
                (_model, _contents, _config) -> geminiResponseFixture(context.turn),
                geminiOptions(context));
    }

    static void emitGeminiStream(Agento11yClient client, EmitContext context) throws Exception {
        List<Content> contents = List.of(Content.builder()
                .role("user")
                .parts(Part.fromText("Stream migration status " + context.turn + "."))
                .build());
        GenerateContentConfig config = GenerateContentConfig.fromJson("""
                {
                  "systemInstruction": {"parts": [{"text": "Emit staged migration stream language."}]},
                  "maxOutputTokens": 220,
                  "thinkingConfig": {"includeThoughts": false}
                }
                """);

        GeminiAdapter.completionStream(
                client,
                "gemini-2.5-pro",
                contents,
                config,
                (_model, _contents, _config) -> List.of(geminiResponseFixture(context.turn)),
                geminiOptions(context));
    }

    static void emitCustomSync(Agento11yClient client, RuntimeConfig config, EmitContext context) throws Exception {
        client.withGeneration(new GenerationStart()
                        .setConversationId(context.conversationId)
                        .setAgentName(context.agentName)
                        .setAgentVersion(context.agentVersion)
                        .setModel(new ModelRef().setProvider(config.customProvider).setName("mistral-large-devex"))
                        .setTags(context.tags)
                        .setMetadata(context.metadata),
                recorder -> {
                    recorder.setResult(new GenerationResult()
                            .setInput(List.of(new Message()
                                    .setRole(MessageRole.USER)
                                    .setParts(List.of(MessagePart.text("Draft custom checkpoint " + context.turn + ".")))))
                            .setOutput(List.of(new Message()
                                    .setRole(MessageRole.ASSISTANT)
                                    .setParts(List.of(MessagePart.text(
                                            "Custom provider sync " + context.turn + ": all guardrails satisfied.")))))
                            .setUsage(new TokenUsage()
                                    .setInputTokens(30 + (context.turn % 6))
                                    .setOutputTokens(15 + (context.turn % 4))
                                    .setTotalTokens(45 + (context.turn % 7)))
                            .setStopReason("stop"));
                    return null;
                });
    }

    static void emitCustomStream(Agento11yClient client, RuntimeConfig config, EmitContext context) throws Exception {
        client.withStreamingGeneration(new GenerationStart()
                        .setConversationId(context.conversationId)
                        .setAgentName(context.agentName)
                        .setAgentVersion(context.agentVersion)
                        .setModel(new ModelRef().setProvider(config.customProvider).setName("mistral-large-devex"))
                        .setTags(context.tags)
                        .setMetadata(context.metadata),
                recorder -> {
                    recorder.setResult(new GenerationResult()
                            .setInput(List.of(new Message()
                                    .setRole(MessageRole.USER)
                                    .setParts(List.of(MessagePart.text("Stream custom remediation summary " + context.turn + ".")))))
                            .setOutput(List.of(new Message()
                                    .setRole(MessageRole.ASSISTANT)
                                    .setParts(List.of(
                                            MessagePart.thinking("assembling synthetic stream segments"),
                                            MessagePart.text("Custom stream " + context.turn + ": segment A complete; segment B complete.")))))
                            .setUsage(new TokenUsage()
                                    .setInputTokens(24 + (context.turn % 5))
                                    .setOutputTokens(16 + (context.turn % 4))
                                    .setTotalTokens(40 + (context.turn % 6)))
                            .setStopReason("end_turn"));
                    return null;
                });
    }

    static OpenAiOptions openAiOptions(EmitContext context) {
        return new OpenAiOptions()
                .setConversationId(context.conversationId)
                .setAgentName(context.agentName)
                .setAgentVersion(context.agentVersion)
                .setTags(context.tags)
                .setMetadata(context.metadata)
                .setRawArtifacts(false);
    }

    static AnthropicOptions anthropicOptions(EmitContext context) {
        return new AnthropicOptions()
                .setConversationId(context.conversationId)
                .setAgentName(context.agentName)
                .setAgentVersion(context.agentVersion)
                .setTags(context.tags)
                .setMetadata(context.metadata)
                .setRawArtifacts(false);
    }

    static GeminiOptions geminiOptions(EmitContext context) {
        return new GeminiOptions()
                .setConversationId(context.conversationId)
                .setAgentName(context.agentName)
                .setAgentVersion(context.agentVersion)
                .setTags(context.tags)
                .setMetadata(context.metadata)
                .setRawArtifacts(false);
    }

    static ChatCompletion chatResponseFixture(int turn) throws IOException {
        return json(
                """
                {
                  "id": "java-openai-sync-%d",
                  "choices": [
                    {
                      "finish_reason": "stop",
                      "index": 0,
                      "message": {
                        "role": "assistant",
                        "content": "Plan %d: verify canary, assign owner, publish summary."
                      }
                    }
                  ],
                  "created": 1,
                  "model": "gpt-5",
                  "object": "chat.completion",
                  "usage": {
                    "prompt_tokens": %d,
                    "completion_tokens": %d,
                    "total_tokens": %d
                  }
                }
                """.formatted(
                        turn,
                        turn,
                        84 + (turn % 9),
                        24 + (turn % 6),
                        108 + (turn % 12)),
                ChatCompletion.class);
    }

    static com.anthropic.models.messages.Message anthropicMessageFixture(int turn) throws IOException {
        return com.anthropic.core.ObjectMappers.jsonMapper().readValue(
                """
                {
                  "id": "java-anthropic-sync-%d",
                  "content": [
                    {
                      "type": "text",
                      "text": "Diagnosis %d: retry storms in eu-west; rebalance queues."
                    }
                  ],
                  "model": "claude-sonnet-4-5",
                  "stop_reason": "end_turn",
                  "usage": {
                    "input_tokens": %d,
                    "output_tokens": %d,
                    "cache_read_input_tokens": 10
                  }
                }
                """.formatted(
                        turn,
                        turn,
                        72 + (turn % 8),
                        30 + (turn % 5)),
                com.anthropic.models.messages.Message.class);
    }

    static GenerateContentResponse geminiResponseFixture(int turn) {
        return GenerateContentResponse.fromJson(
                """
                {
                  "responseId": "java-gemini-sync-%d",
                  "modelVersion": "gemini-2.5-pro-001",
                  "candidates": [
                    {
                      "finishReason": "STOP",
                      "content": {
                        "role": "model",
                        "parts": [
                          {"text": "Launch %d: all gates green; metrics stable."}
                        ]
                      }
                    }
                  ],
                  "usageMetadata": {
                    "promptTokenCount": %d,
                    "candidatesTokenCount": %d,
                    "totalTokenCount": %d,
                    "thoughtsTokenCount": 6
                  }
                }
                """.formatted(
                        turn,
                        turn,
                        60 + (turn % 7),
                        19 + (turn % 5),
                        79 + (turn % 8)));
    }

    static ChatCompletionChunk chatChunkOne(int turn) throws IOException {
        return json(
                """
                {
                  "id": "java-openai-stream-%d",
                  "choices": [
                    {
                      "delta": {
                        "content": "Ticket %d: canary healthy"
                      },
                      "index": 0
                    }
                  ],
                  "created": 1,
                  "model": "gpt-5",
                  "object": "chat.completion.chunk"
                }
                """.formatted(turn, turn),
                ChatCompletionChunk.class);
    }

    static ChatCompletionChunk chatChunkTwo(int turn) throws IOException {
        return json(
                """
                {
                  "id": "java-openai-stream-%d",
                  "choices": [
                    {
                      "delta": {
                        "content": "; production gate passed."
                      },
                      "finish_reason": "stop",
                      "index": 0
                    }
                  ],
                  "created": 1,
                  "model": "gpt-5",
                  "object": "chat.completion.chunk",
                  "usage": {
                    "prompt_tokens": %d,
                    "completion_tokens": %d,
                    "total_tokens": %d
                  }
                }
                """.formatted(
                        turn,
                        49 + (turn % 5),
                        16 + (turn % 4),
                        65 + (turn % 7)),
                ChatCompletionChunk.class);
    }

    static Response responsesResponseFixture(int turn) throws IOException {
        return json(
                """
                {
                  "id": "java-openai-responses-sync-%d",
                  "created_at": 1,
                  "model": "gpt-5",
                  "object": "response",
                  "output": [
                    {
                      "id": "java-openai-responses-sync-msg-%d",
                      "type": "message",
                      "role": "assistant",
                      "status": "completed",
                      "content": [{"type": "output_text", "text": "Plan %d: verify canary, assign owner, publish summary."}]
                    }
                  ],
                  "parallel_tool_calls": false,
                  "tool_choice": "auto",
                  "tools": [],
                  "status": "completed",
                  "usage": {
                    "input_tokens": %d,
                    "output_tokens": %d,
                    "total_tokens": %d
                  }
                }
                """.formatted(
                        turn,
                        turn,
                        turn,
                        84 + (turn % 9),
                        24 + (turn % 6),
                        108 + (turn % 12)),
                Response.class);
    }

    static ResponseStreamEvent responseTextDeltaEvent(String delta, long sequenceNumber) throws IOException {
        return json(
                """
                {
                  "type": "response.output_text.delta",
                  "content_index": 0,
                  "delta": "%s",
                  "item_id": "java-openai-responses-stream-msg",
                  "output_index": 0,
                  "sequence_number": %d
                }
                """.formatted(delta, sequenceNumber),
                ResponseStreamEvent.class);
    }

    static ResponseStreamEvent responseCompletedEvent(int turn) throws IOException {
        return json(
                """
                {
                  "type": "response.completed",
                  "sequence_number": 3,
                  "response": {
                    "id": "java-openai-responses-stream-%d",
                    "created_at": 1,
                    "model": "gpt-5",
                    "object": "response",
                    "output": [],
                    "parallel_tool_calls": false,
                    "tool_choice": "auto",
                    "tools": [],
                    "status": "completed",
                    "usage": {
                      "input_tokens": %d,
                      "output_tokens": %d,
                      "total_tokens": %d
                    }
                  }
                }
                """.formatted(
                        turn,
                        49 + (turn % 5),
                        16 + (turn % 4),
                        65 + (turn % 7)),
                ResponseStreamEvent.class);
    }

    static <T> T json(String value, Class<T> type) throws IOException {
        return ObjectMappers.jsonMapper().readValue(value, type);
    }

    static final class FixedOpenAiStreamResponse<T> implements com.openai.core.http.StreamResponse<T> {
        private final List<T> values;

        FixedOpenAiStreamResponse(List<T> values) {
            this.values = values;
        }

        @Override
        public Stream<T> stream() {
            return values.stream();
        }

        @Override
        public void close() {
        }
    }

    static final class FixedAnthropicStreamResponse<T> implements StreamResponse<T> {
        private final List<T> values;

        FixedAnthropicStreamResponse(List<T> values) {
            this.values = values;
        }

        @Override
        public Stream<T> stream() {
            return values.stream();
        }

        @Override
        public void close() {
        }
    }

    record RuntimeConfig(
            int intervalMs,
            int streamPercent,
            int conversations,
            int rotateTurns,
            String customProvider,
            String genGrpcEndpoint,
            int maxCycles) {
    }

    static final class SourceState {
        int cursor;
        final List<ThreadState> slots;

        SourceState(int conversations) {
            this.slots = new ArrayList<>(conversations);
            for (int i = 0; i < conversations; i++) {
                this.slots.add(new ThreadState());
            }
        }
    }

    static final class ThreadState {
        String conversationId = "";
        int turn;
    }

    record EmitContext(
            String conversationId,
            int turn,
            int slot,
            String agentName,
            String agentVersion,
            Map<String, String> tags,
            Map<String, Object> metadata) {
    }

    record TagEnvelope(String agentPersona, Map<String, String> tags, Map<String, Object> metadata) {
    }
}
