package com.grafana.agento11y.sdk.frameworks.googleadk;

import com.grafana.agento11y.sdk.GenerationMode;
import com.grafana.agento11y.sdk.GenerationRecorder;
import com.grafana.agento11y.sdk.GenerationResult;
import com.grafana.agento11y.sdk.GenerationStart;
import com.grafana.agento11y.sdk.Message;
import com.grafana.agento11y.sdk.MessagePart;
import com.grafana.agento11y.sdk.MessageRole;
import com.grafana.agento11y.sdk.ModelRef;
import com.grafana.agento11y.sdk.Agento11yClient;
import com.grafana.agento11y.sdk.TokenUsage;
import com.grafana.agento11y.sdk.ToolExecutionRecorder;
import com.grafana.agento11y.sdk.ToolExecutionResult;
import com.grafana.agento11y.sdk.ToolExecutionStart;
import java.time.Instant;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;

/** Google ADK lifecycle adapter for agento11y generation and tool recording. */
public final class Agento11yGoogleAdkAdapter {
    private static final String FRAMEWORK_NAME = "google-adk";
    private static final String FRAMEWORK_SOURCE = "handler";
    private static final String FRAMEWORK_LANGUAGE = "java";
    private static final int MAX_METADATA_DEPTH = 5;
    private static final String EMBEDDINGS_UNSUPPORTED_MESSAGE =
            "google-adk: embeddings are not supported because the Google ADK lifecycle surface does not expose a dedicated embeddings callback";

    static final String META_RUN_ID = "agento11y.framework.run_id";
    static final String META_THREAD_ID = "agento11y.framework.thread_id";
    static final String META_PARENT_RUN_ID = "agento11y.framework.parent_run_id";
    static final String META_COMPONENT_NAME = "agento11y.framework.component_name";
    static final String META_RUN_TYPE = "agento11y.framework.run_type";
    static final String META_TAGS = "agento11y.framework.tags";
    static final String META_RETRY_ATTEMPT = "agento11y.framework.retry_attempt";
    static final String META_EVENT_ID = "agento11y.framework.event_id";

    private final Agento11yClient client;
    private final Options options;
    private final GenerationStarter generationStarter;
    private final ToolStarter toolStarter;
    private final Map<String, RunState> runs = new ConcurrentHashMap<>();
    private final Map<String, ToolRunState> toolRuns = new ConcurrentHashMap<>();

    public Agento11yGoogleAdkAdapter(Agento11yClient client, Options options) {
        this(
                client,
                options,
                (start, stream) -> stream ? client.startStreamingGeneration(start) : client.startGeneration(start),
                client::startToolExecution);
    }

    Agento11yGoogleAdkAdapter(
            Agento11yClient client,
            Options options,
            GenerationStarter generationStarter,
            ToolStarter toolStarter) {
        this.client = client;
        this.options = options == null ? new Options() : options.copy();
        this.generationStarter = generationStarter;
        this.toolStarter = toolStarter;
        if (!this.options.captureInputsSet) {
            this.options.setCaptureInputs(true);
        }
        if (!this.options.captureOutputsSet) {
            this.options.setCaptureOutputs(true);
        }
    }

    /**
     * Returns callback delegates backed by this adapter so callers can wire lifecycle hooks once
     * in runner/app configuration.
     */
    public Callbacks callbacks() {
        return new Callbacks(this);
    }

    /** Creates callback delegates backed by a new adapter instance. */
    public static Callbacks createCallbacks(Agento11yClient client, Options options) {
        return new Agento11yGoogleAdkAdapter(client, options).callbacks();
    }

    /**
     * Reports whether this adapter can observe a native Google ADK embeddings lifecycle.
     * The current lifecycle surface only exposes run and tool callbacks, so embeddings
     * remain unsupported until ADK exposes a dedicated embeddings callback.
     */
    public static void checkEmbeddingsSupport() {
        throw new UnsupportedOperationException(EMBEDDINGS_UNSUPPORTED_MESSAGE);
    }

    public void onRunStart(RunStartEvent event) {
        if (event == null || event.getRunId().isBlank()) {
            return;
        }
        String runId = event.getRunId().trim();
        runs.computeIfAbsent(runId, ignored -> {
            ConversationContext conversation = resolveConversation(event);
            String runType = event.getRunType().isBlank() ? "chat" : event.getRunType().trim();
            String provider = resolveProvider(event);
            String modelName = event.getModelName().isBlank() ? "unknown" : event.getModelName().trim();

            Map<String, Object> metadata = buildFrameworkMetadata(new MetadataInput()
                    .setBase(options.getExtraMetadata())
                    .setEvent(event.getMetadata())
                    .setRunId(runId)
                    .setThreadId(conversation.threadId)
                    .setParentRunId(event.getParentRunId())
                    .setComponentName(event.getComponentName())
                    .setRunType(runType)
                    .setTags(normalizeTags(event.getTags()))
                    .setRetryAttempt(event.getRetryAttempt())
                    .setEventId(event.getEventId()));

            Map<String, String> tags = new LinkedHashMap<>(options.getExtraTags());
            tags.put("agento11y.framework.name", FRAMEWORK_NAME);
            tags.put("agento11y.framework.source", FRAMEWORK_SOURCE);
            tags.put("agento11y.framework.language", FRAMEWORK_LANGUAGE);

            GenerationStart start = new GenerationStart()
                    .setConversationId(conversation.conversationId)
                    .setAgentName(options.getAgentName())
                    .setAgentVersion(options.getAgentVersion())
                    .setMode(event.isStream() ? GenerationMode.STREAM : GenerationMode.SYNC)
                    .setModel(new ModelRef().setProvider(provider).setName(modelName))
                    .setTags(tags)
                    .setMetadata(metadata);

            GenerationRecorder recorder = generationStarter.start(start, event.isStream());

            List<Message> input = new ArrayList<>();
            if (options.isCaptureInputs()) {
                if (!event.getInputMessages().isEmpty()) {
                    for (Message message : event.getInputMessages()) {
                        input.add(message == null ? new Message() : message.copy());
                    }
                } else {
                    for (String prompt : event.getPrompts()) {
                        if (prompt == null || prompt.trim().isBlank()) {
                            continue;
                        }
                        input.add(new Message()
                                .setRole(MessageRole.USER)
                                .setParts(List.of(MessagePart.text(prompt.trim()))));
                    }
                }
            }

            return new RunState(recorder, input, options.isCaptureOutputs());
        });
    }

    public void onRunToken(String runId, String token) {
        if (runId == null || runId.trim().isBlank() || token == null || token.trim().isBlank()) {
            return;
        }
        RunState state = runs.get(runId.trim());
        if (state == null) {
            return;
        }

        synchronized (state.lock) {
            if (state.completed) {
                return;
            }
            if (state.captureOutputs) {
                state.outputChunks.append(token);
            }
            if (!state.firstTokenRecorded) {
                state.firstTokenRecorded = true;
                state.recorder.setFirstTokenAt(Instant.now());
            }
        }
    }

    public void onRunEnd(String runId, RunEndEvent event) {
        if (runId == null || runId.trim().isBlank()) {
            return;
        }
        RunState state = runs.remove(runId.trim());
        if (state == null) {
            return;
        }

        List<Message> output = new ArrayList<>();
        synchronized (state.lock) {
            state.completed = true;
            if (state.captureOutputs) {
                if (event != null && !event.getOutputMessages().isEmpty()) {
                    for (Message message : event.getOutputMessages()) {
                        output.add(message == null ? new Message() : message.copy());
                    }
                } else if (state.outputChunks.length() > 0) {
                    output.add(new Message()
                            .setRole(MessageRole.ASSISTANT)
                            .setParts(List.of(MessagePart.text(state.outputChunks.toString()))));
                }
            }
        }

        GenerationResult result = new GenerationResult()
                .setInput(cloneMessages(state.input))
                .setOutput(output)
                .setUsage(event == null || event.getUsage() == null ? new TokenUsage() : event.getUsage().copy())
                .setResponseModel(event == null ? "" : event.getResponseModel())
                .setStopReason(event == null ? "" : event.getStopReason());

        state.recorder.setResult(result);
        state.recorder.end();
        throwIfRecorderError(state.recorder.error());
    }

    public void onRunError(String runId, Throwable error) {
        if (runId == null || runId.trim().isBlank()) {
            return;
        }
        RunState state = runs.remove(runId.trim());
        if (state == null) {
            return;
        }

        Throwable resolved = error == null ? new RuntimeException("framework callback error") : error;
        String chunkedOutput = "";
        synchronized (state.lock) {
            state.completed = true;
            if (state.captureOutputs && state.outputChunks.length() > 0) {
                chunkedOutput = state.outputChunks.toString();
            }
        }
        state.recorder.setCallError(resolved);
        if (!chunkedOutput.isEmpty()) {
            state.recorder.setResult(new GenerationResult()
                    .setInput(cloneMessages(state.input))
                    .setOutput(List.of(new Message()
                            .setRole(MessageRole.ASSISTANT)
                            .setParts(List.of(MessagePart.text(chunkedOutput))))));
        }
        state.recorder.end();
        throwIfRecorderError(state.recorder.error());
    }

    public void onToolStart(ToolStartEvent event) {
        if (event == null || event.getRunId().isBlank()) {
            return;
        }
        String runId = event.getRunId().trim();
        toolRuns.computeIfAbsent(runId, ignored -> {
            ConversationContext conversation = resolveConversation(new RunStartEvent()
                    .setRunId(runId)
                    .setConversationId(event.getConversationId())
                    .setSessionId(event.getSessionId())
                    .setGroupId(event.getGroupId())
                    .setThreadId(event.getThreadId()));

            ToolExecutionRecorder recorder = toolStarter.start(new ToolExecutionStart()
                    .setToolName(event.getToolName())
                    .setToolDescription(event.getToolDescription())
                    .setConversationId(conversation.conversationId)
                    .setAgentName(options.getAgentName())
                    .setAgentVersion(options.getAgentVersion())
                    .setIncludeContent(options.isCaptureInputs() || options.isCaptureOutputs()));

            Object arguments = options.isCaptureInputs() ? event.getArguments() : null;
            return new ToolRunState(recorder, arguments, options.isCaptureOutputs());
        });
    }

    public void onToolEnd(String runId, ToolEndEvent event) {
        if (runId == null || runId.trim().isBlank()) {
            return;
        }
        ToolRunState state = toolRuns.remove(runId.trim());
        if (state == null) {
            return;
        }

        ToolExecutionResult result = new ToolExecutionResult();
        if (state.arguments != null) {
            result.setArguments(state.arguments);
        }
        if (state.captureOutputs) {
            result.setResult(event == null ? null : event.getResult());
        }
        if (event != null) {
            result.setCompletedAt(event.getCompletedAt());
        }

        state.recorder.setResult(result);
        state.recorder.end();
        throwIfRecorderError(state.recorder.error());
    }

    public void onToolError(String runId, Throwable error) {
        if (runId == null || runId.trim().isBlank()) {
            return;
        }
        ToolRunState state = toolRuns.remove(runId.trim());
        if (state == null) {
            return;
        }

        state.recorder.setCallError(error == null ? new RuntimeException("tool callback error") : error);
        state.recorder.end();
        throwIfRecorderError(state.recorder.error());
    }

    static ConversationContext resolveConversation(RunStartEvent event) {
        if (event == null) {
            return new ConversationContext("agento11y:framework:google-adk:", "");
        }
        if (!event.getConversationId().isBlank()) {
            return new ConversationContext(event.getConversationId().trim(), event.getThreadId().trim());
        }
        if (!event.getSessionId().isBlank()) {
            return new ConversationContext(event.getSessionId().trim(), event.getThreadId().trim());
        }
        if (!event.getGroupId().isBlank()) {
            return new ConversationContext(event.getGroupId().trim(), event.getThreadId().trim());
        }
        if (!event.getThreadId().isBlank()) {
            String threadId = event.getThreadId().trim();
            return new ConversationContext(threadId, threadId);
        }
        return new ConversationContext("agento11y:framework:google-adk:" + event.getRunId().trim(), "");
    }

    private String resolveProvider(RunStartEvent event) {
        String explicit = normalizeProvider(options.getProvider());
        if (!explicit.isBlank()) {
            return explicit;
        }
        String eventProvider = normalizeProvider(event.getProvider());
        if (!eventProvider.isBlank()) {
            return eventProvider;
        }
        if (options.getProviderResolver() != null) {
            String resolved = normalizeProvider(options.getProviderResolver().resolve(event.getModelName(), event));
            if (!resolved.isBlank()) {
                return resolved;
            }
        }
        return inferProvider(event.getModelName());
    }

    static String normalizeProvider(String provider) {
        if (provider == null) {
            return "";
        }
        String normalized = provider.trim().toLowerCase();
        if (normalized.isBlank()) {
            return "";
        }
        return switch (normalized) {
            case "openai", "anthropic", "gemini" -> normalized;
            default -> "custom";
        };
    }

    static String inferProvider(String modelName) {
        String normalized = modelName == null ? "" : modelName.trim().toLowerCase();
        if (normalized.startsWith("gpt-") || normalized.startsWith("o1") || normalized.startsWith("o3") || normalized.startsWith("o4")) {
            return "openai";
        }
        if (normalized.startsWith("claude-")) {
            return "anthropic";
        }
        if (normalized.startsWith("gemini-")) {
            return "gemini";
        }
        return "custom";
    }

    static List<String> normalizeTags(List<String> input) {
        if (input == null || input.isEmpty()) {
            return List.of();
        }
        Set<String> seen = new LinkedHashSet<>();
        for (String raw : input) {
            if (raw == null || raw.trim().isBlank()) {
                continue;
            }
            seen.add(raw.trim());
        }
        return new ArrayList<>(seen);
    }

    static Map<String, Object> buildFrameworkMetadata(MetadataInput input) {
        Map<String, Object> metadata = new LinkedHashMap<>();
        mergeMetadata(metadata, input.getBase());
        mergeMetadata(metadata, input.getEvent());

        metadata.put(META_RUN_ID, input.getRunId().trim());
        metadata.put(META_RUN_TYPE, input.getRunType().trim());
        if (!input.getThreadId().isBlank()) {
            metadata.put(META_THREAD_ID, input.getThreadId().trim());
        }
        if (!input.getParentRunId().isBlank()) {
            metadata.put(META_PARENT_RUN_ID, input.getParentRunId().trim());
        }
        if (!input.getComponentName().isBlank()) {
            metadata.put(META_COMPONENT_NAME, input.getComponentName().trim());
        }
        if (!input.getTags().isEmpty()) {
            metadata.put(META_TAGS, input.getTags());
        }
        if (input.getRetryAttempt() != null) {
            metadata.put(META_RETRY_ATTEMPT, input.getRetryAttempt());
        }
        if (!input.getEventId().isBlank()) {
            metadata.put(META_EVENT_ID, input.getEventId().trim());
        }

        return normalizeMetadata(metadata, 0, new IdentityHashMapSet());
    }

    private static void mergeMetadata(Map<String, Object> destination, Map<String, Object> source) {
        if (source == null || source.isEmpty()) {
            return;
        }
        for (Map.Entry<String, Object> entry : source.entrySet()) {
            if (entry.getKey() == null || entry.getKey().trim().isBlank()) {
                continue;
            }
            destination.put(entry.getKey().trim(), entry.getValue());
        }
    }

    private static Map<String, Object> normalizeMetadata(Map<String, Object> source, int depth, IdentityHashMapSet seen) {
        Map<String, Object> normalized = new LinkedHashMap<>();
        for (Map.Entry<String, Object> entry : source.entrySet()) {
            Object value = normalizeMetadataValue(entry.getValue(), depth, seen);
            if (value == Drop.VALUE) {
                continue;
            }
            normalized.put(entry.getKey(), value);
        }
        return normalized;
    }

    private static Object normalizeMetadataValue(Object value, int depth, IdentityHashMapSet seen) {
        if (depth > MAX_METADATA_DEPTH) {
            return Drop.VALUE;
        }
        if (value == null || value instanceof String || value instanceof Boolean || value instanceof Integer || value instanceof Long) {
            return value;
        }
        if (value instanceof Double d) {
            return Double.isFinite(d) ? d : Drop.VALUE;
        }
        if (value instanceof Float f) {
            return Float.isFinite(f) ? f : Drop.VALUE;
        }
        if (value instanceof Number number) {
            return number.longValue();
        }
        if (value instanceof Instant instant) {
            return instant.toString();
        }
        if (value instanceof List<?> list) {
            List<Object> out = new ArrayList<>();
            for (Object item : list) {
                Object normalized = normalizeMetadataValue(item, depth + 1, seen);
                if (normalized == Drop.VALUE) {
                    continue;
                }
                out.add(normalized);
            }
            return out;
        }
        if (value instanceof Map<?, ?> map) {
            if (seen.contains(value)) {
                return "[circular]";
            }
            seen.add(value);
            Map<String, Object> nested = new LinkedHashMap<>();
            for (Map.Entry<?, ?> entry : map.entrySet()) {
                if (!(entry.getKey() instanceof String key) || key.trim().isBlank()) {
                    continue;
                }
                Object normalized = normalizeMetadataValue(entry.getValue(), depth + 1, seen);
                if (normalized == Drop.VALUE) {
                    continue;
                }
                nested.put(key.trim(), normalized);
            }
            seen.remove(value);
            return nested;
        }
        return Drop.VALUE;
    }

    private static List<Message> cloneMessages(List<Message> source) {
        List<Message> out = new ArrayList<>();
        for (Message message : source) {
            out.add(message == null ? new Message() : message.copy());
        }
        return out;
    }

    private static void throwIfRecorderError(Optional<Throwable> error) {
        error.ifPresent(throwable -> {
            throw new RuntimeException(throwable);
        });
    }

    private static final class RunState {
        private final Object lock = new Object();
        private final GenerationRecorder recorder;
        private final List<Message> input;
        private final boolean captureOutputs;
        private boolean firstTokenRecorded;
        private boolean completed;
        private final StringBuilder outputChunks = new StringBuilder();

        private RunState(GenerationRecorder recorder, List<Message> input, boolean captureOutputs) {
            this.recorder = recorder;
            this.input = input;
            this.captureOutputs = captureOutputs;
        }
    }

    private static final class ToolRunState {
        private final ToolExecutionRecorder recorder;
        private final Object arguments;
        private final boolean captureOutputs;

        private ToolRunState(ToolExecutionRecorder recorder, Object arguments, boolean captureOutputs) {
            this.recorder = recorder;
            this.arguments = arguments;
            this.captureOutputs = captureOutputs;
        }
    }

    static final class ConversationContext {
        final String conversationId;
        final String threadId;

        ConversationContext(String conversationId, String threadId) {
            this.conversationId = conversationId;
            this.threadId = threadId;
        }
    }

    static final class MetadataInput {
        private Map<String, Object> base = new LinkedHashMap<>();
        private Map<String, Object> event = new LinkedHashMap<>();
        private String runId = "";
        private String threadId = "";
        private String parentRunId = "";
        private String componentName = "";
        private String runType = "chat";
        private List<String> tags = List.of();
        private Integer retryAttempt;
        private String eventId = "";

        Map<String, Object> getBase() {
            return base;
        }

        Map<String, Object> getEvent() {
            return event;
        }

        String getRunId() {
            return runId;
        }

        String getThreadId() {
            return threadId;
        }

        String getParentRunId() {
            return parentRunId;
        }

        String getComponentName() {
            return componentName;
        }

        String getRunType() {
            return runType;
        }

        List<String> getTags() {
            return tags;
        }

        Integer getRetryAttempt() {
            return retryAttempt;
        }

        String getEventId() {
            return eventId;
        }

        MetadataInput setBase(Map<String, Object> base) {
            this.base = base == null ? new LinkedHashMap<>() : base;
            return this;
        }

        MetadataInput setEvent(Map<String, Object> event) {
            this.event = event == null ? new LinkedHashMap<>() : event;
            return this;
        }

        MetadataInput setRunId(String runId) {
            this.runId = runId == null ? "" : runId;
            return this;
        }

        MetadataInput setThreadId(String threadId) {
            this.threadId = threadId == null ? "" : threadId;
            return this;
        }

        MetadataInput setParentRunId(String parentRunId) {
            this.parentRunId = parentRunId == null ? "" : parentRunId;
            return this;
        }

        MetadataInput setComponentName(String componentName) {
            this.componentName = componentName == null ? "" : componentName;
            return this;
        }

        MetadataInput setRunType(String runType) {
            this.runType = runType == null ? "" : runType;
            return this;
        }

        MetadataInput setTags(List<String> tags) {
            this.tags = tags == null ? List.of() : tags;
            return this;
        }

        MetadataInput setRetryAttempt(Integer retryAttempt) {
            this.retryAttempt = retryAttempt;
            return this;
        }

        MetadataInput setEventId(String eventId) {
            this.eventId = eventId == null ? "" : eventId;
            return this;
        }
    }

    static final class IdentityHashMapSet {
        private final Map<Object, Boolean> data = new java.util.IdentityHashMap<>();

        boolean contains(Object value) {
            return data.containsKey(value);
        }

        void add(Object value) {
            data.put(value, Boolean.TRUE);
        }

        void remove(Object value) {
            data.remove(value);
        }
    }

    private enum Drop {
        VALUE
    }

    /** Function-style lifecycle callbacks for one-time framework wiring. */
    public static final class Callbacks {
        private final Agento11yGoogleAdkAdapter adapter;

        private Callbacks(Agento11yGoogleAdkAdapter adapter) {
            this.adapter = adapter;
        }

        public void onRunStart(RunStartEvent event) {
            adapter.onRunStart(event);
        }

        public void onRunToken(String runId, String token) {
            adapter.onRunToken(runId, token);
        }

        public void onRunEnd(String runId, RunEndEvent event) {
            adapter.onRunEnd(runId, event);
        }

        public void onRunError(String runId, Throwable error) {
            adapter.onRunError(runId, error);
        }

        public void onToolStart(ToolStartEvent event) {
            adapter.onToolStart(event);
        }

        public void onToolEnd(String runId, ToolEndEvent event) {
            adapter.onToolEnd(runId, event);
        }

        public void onToolError(String runId, Throwable error) {
            adapter.onToolError(runId, error);
        }
    }

    /** Provider resolver callback for custom provider mapping behavior. */
    public interface ProviderResolver {
        String resolve(String modelName, RunStartEvent event);
    }

    @FunctionalInterface
    interface GenerationStarter {
        GenerationRecorder start(GenerationStart start, boolean stream);
    }

    @FunctionalInterface
    interface ToolStarter {
        ToolExecutionRecorder start(ToolExecutionStart start);
    }

    /** Adapter options. */
    public static final class Options {
        private String agentName = "";
        private String agentVersion = "";
        private String provider = "";
        private ProviderResolver providerResolver;
        private boolean captureInputs;
        private boolean captureOutputs;
        private boolean captureInputsSet;
        private boolean captureOutputsSet;
        private final Map<String, String> extraTags = new LinkedHashMap<>();
        private final Map<String, Object> extraMetadata = new LinkedHashMap<>();

        public String getAgentName() {
            return agentName;
        }

        public Options setAgentName(String agentName) {
            this.agentName = agentName == null ? "" : agentName;
            return this;
        }

        public String getAgentVersion() {
            return agentVersion;
        }

        public Options setAgentVersion(String agentVersion) {
            this.agentVersion = agentVersion == null ? "" : agentVersion;
            return this;
        }

        public String getProvider() {
            return provider;
        }

        public Options setProvider(String provider) {
            this.provider = provider == null ? "" : provider;
            return this;
        }

        public ProviderResolver getProviderResolver() {
            return providerResolver;
        }

        public Options setProviderResolver(ProviderResolver providerResolver) {
            this.providerResolver = providerResolver;
            return this;
        }

        public boolean isCaptureInputs() {
            return captureInputs;
        }

        public Options setCaptureInputs(boolean captureInputs) {
            this.captureInputs = captureInputs;
            this.captureInputsSet = true;
            return this;
        }

        public boolean isCaptureOutputs() {
            return captureOutputs;
        }

        public Options setCaptureOutputs(boolean captureOutputs) {
            this.captureOutputs = captureOutputs;
            this.captureOutputsSet = true;
            return this;
        }

        public Map<String, String> getExtraTags() {
            return extraTags;
        }

        public Options putExtraTag(String key, String value) {
            if (key == null || key.trim().isBlank()) {
                return this;
            }
            extraTags.put(key.trim(), value == null ? "" : value);
            return this;
        }

        public Map<String, Object> getExtraMetadata() {
            return extraMetadata;
        }

        public Options putExtraMetadata(String key, Object value) {
            if (key == null || key.trim().isBlank()) {
                return this;
            }
            extraMetadata.put(key.trim(), value);
            return this;
        }

        private Options copy() {
            Options out = new Options();
            out.agentName = this.agentName;
            out.agentVersion = this.agentVersion;
            out.provider = this.provider;
            out.providerResolver = this.providerResolver;
            out.captureInputs = this.captureInputs;
            out.captureOutputs = this.captureOutputs;
            out.captureInputsSet = this.captureInputsSet;
            out.captureOutputsSet = this.captureOutputsSet;
            out.extraTags.putAll(this.extraTags);
            out.extraMetadata.putAll(this.extraMetadata);
            return out;
        }
    }

    /** Run start callback payload. */
    public static final class RunStartEvent {
        private String runId = "";
        private String parentRunId = "";
        private String conversationId = "";
        private String sessionId = "";
        private String groupId = "";
        private String threadId = "";
        private String eventId = "";
        private String componentName = "";
        private String runType = "";
        private Integer retryAttempt;
        private String modelName = "";
        private String provider = "";
        private boolean stream;
        private final List<String> prompts = new ArrayList<>();
        private final List<Message> inputMessages = new ArrayList<>();
        private final List<String> tags = new ArrayList<>();
        private final Map<String, Object> metadata = new LinkedHashMap<>();

        public String getRunId() { return runId == null ? "" : runId; }
        public String getParentRunId() { return parentRunId == null ? "" : parentRunId; }
        public String getConversationId() { return conversationId == null ? "" : conversationId; }
        public String getSessionId() { return sessionId == null ? "" : sessionId; }
        public String getGroupId() { return groupId == null ? "" : groupId; }
        public String getThreadId() { return threadId == null ? "" : threadId; }
        public String getEventId() { return eventId == null ? "" : eventId; }
        public String getComponentName() { return componentName == null ? "" : componentName; }
        public String getRunType() { return runType == null ? "" : runType; }
        public Integer getRetryAttempt() { return retryAttempt; }
        public String getModelName() { return modelName == null ? "" : modelName; }
        public String getProvider() { return provider == null ? "" : provider; }
        public boolean isStream() { return stream; }
        public List<String> getPrompts() { return prompts; }
        public List<Message> getInputMessages() { return inputMessages; }
        public List<String> getTags() { return tags; }
        public Map<String, Object> getMetadata() { return metadata; }

        public RunStartEvent setRunId(String runId) { this.runId = runId; return this; }
        public RunStartEvent setParentRunId(String parentRunId) { this.parentRunId = parentRunId; return this; }
        public RunStartEvent setConversationId(String conversationId) { this.conversationId = conversationId; return this; }
        public RunStartEvent setSessionId(String sessionId) { this.sessionId = sessionId; return this; }
        public RunStartEvent setGroupId(String groupId) { this.groupId = groupId; return this; }
        public RunStartEvent setThreadId(String threadId) { this.threadId = threadId; return this; }
        public RunStartEvent setEventId(String eventId) { this.eventId = eventId; return this; }
        public RunStartEvent setComponentName(String componentName) { this.componentName = componentName; return this; }
        public RunStartEvent setRunType(String runType) { this.runType = runType; return this; }
        public RunStartEvent setRetryAttempt(Integer retryAttempt) { this.retryAttempt = retryAttempt; return this; }
        public RunStartEvent setModelName(String modelName) { this.modelName = modelName; return this; }
        public RunStartEvent setProvider(String provider) { this.provider = provider; return this; }
        public RunStartEvent setStream(boolean stream) { this.stream = stream; return this; }
        public RunStartEvent addPrompt(String prompt) { this.prompts.add(prompt); return this; }
        public RunStartEvent addInputMessage(Message message) { this.inputMessages.add(message); return this; }
        public RunStartEvent addTag(String tag) { this.tags.add(tag); return this; }
        public RunStartEvent putMetadata(String key, Object value) { this.metadata.put(key, value); return this; }
    }

    /** Run end callback payload. */
    public static final class RunEndEvent {
        private final List<Message> outputMessages = new ArrayList<>();
        private String responseModel = "";
        private String stopReason = "";
        private TokenUsage usage = new TokenUsage();

        public List<Message> getOutputMessages() { return outputMessages; }
        public String getResponseModel() { return responseModel == null ? "" : responseModel; }
        public String getStopReason() { return stopReason == null ? "" : stopReason; }
        public TokenUsage getUsage() { return usage; }

        public RunEndEvent addOutputMessage(Message message) { this.outputMessages.add(message); return this; }
        public RunEndEvent setResponseModel(String responseModel) { this.responseModel = responseModel; return this; }
        public RunEndEvent setStopReason(String stopReason) { this.stopReason = stopReason; return this; }
        public RunEndEvent setUsage(TokenUsage usage) { this.usage = usage == null ? new TokenUsage() : usage; return this; }
    }

    /** Tool start callback payload. */
    public static final class ToolStartEvent {
        private String runId = "";
        private String conversationId = "";
        private String sessionId = "";
        private String groupId = "";
        private String threadId = "";
        private String toolName = "";
        private String toolDescription = "";
        private Object arguments;

        public String getRunId() { return runId == null ? "" : runId; }
        public String getConversationId() { return conversationId == null ? "" : conversationId; }
        public String getSessionId() { return sessionId == null ? "" : sessionId; }
        public String getGroupId() { return groupId == null ? "" : groupId; }
        public String getThreadId() { return threadId == null ? "" : threadId; }
        public String getToolName() { return toolName == null ? "" : toolName; }
        public String getToolDescription() { return toolDescription == null ? "" : toolDescription; }
        public Object getArguments() { return arguments; }

        public ToolStartEvent setRunId(String runId) { this.runId = runId; return this; }
        public ToolStartEvent setConversationId(String conversationId) { this.conversationId = conversationId; return this; }
        public ToolStartEvent setSessionId(String sessionId) { this.sessionId = sessionId; return this; }
        public ToolStartEvent setGroupId(String groupId) { this.groupId = groupId; return this; }
        public ToolStartEvent setThreadId(String threadId) { this.threadId = threadId; return this; }
        public ToolStartEvent setToolName(String toolName) { this.toolName = toolName; return this; }
        public ToolStartEvent setToolDescription(String toolDescription) { this.toolDescription = toolDescription; return this; }
        public ToolStartEvent setArguments(Object arguments) { this.arguments = arguments; return this; }
    }

    /** Tool end callback payload. */
    public static final class ToolEndEvent {
        private Object result;
        private Instant completedAt;

        public Object getResult() { return result; }
        public Instant getCompletedAt() { return completedAt; }

        public ToolEndEvent setResult(Object result) { this.result = result; return this; }
        public ToolEndEvent setCompletedAt(Instant completedAt) { this.completedAt = completedAt; return this; }
    }
}
