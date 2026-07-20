package com.grafana.agento11y.sdk;

import io.opentelemetry.api.metrics.Meter;
import io.opentelemetry.api.trace.Tracer;
import java.time.Clock;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.Map;
import java.util.logging.Logger;

/** Top-level runtime configuration for {@link Agento11yClient}. */
public final class Agento11yClientConfig {
    private GenerationExportConfig generationExport = new GenerationExportConfig();
    private ApiConfig api = new ApiConfig();
    private EmbeddingCaptureConfig embeddingCapture = new EmbeddingCaptureConfig();
    private ContentCaptureMode contentCapture = ContentCaptureMode.DEFAULT;
    private ContentCaptureResolver contentCaptureResolver;
    private GenerationExporter generationExporter;
    private Tracer tracer;
    private Meter meter;
    private Logger logger = Logger.getLogger("com.grafana.agento11y.sdk");
    private Clock clock = Clock.systemUTC();

    private String agentName = "";
    private String agentVersion = "";
    private String userId = "";
    private Map<String, String> tags = new LinkedHashMap<>();
    private Boolean debug;

    public GenerationExportConfig getGenerationExport() {
        return generationExport;
    }

    public Agento11yClientConfig setGenerationExport(GenerationExportConfig generationExport) {
        this.generationExport = generationExport == null ? new GenerationExportConfig() : generationExport;
        return this;
    }

    public ApiConfig getApi() {
        return api;
    }

    public Agento11yClientConfig setApi(ApiConfig api) {
        this.api = api == null ? new ApiConfig() : api;
        return this;
    }

    public EmbeddingCaptureConfig getEmbeddingCapture() {
        return embeddingCapture;
    }

    public Agento11yClientConfig setEmbeddingCapture(EmbeddingCaptureConfig embeddingCapture) {
        this.embeddingCapture = embeddingCapture == null ? new EmbeddingCaptureConfig() : embeddingCapture;
        return this;
    }

    public ContentCaptureMode getContentCapture() {
        return contentCapture;
    }

    /**
     * Sets the client-level {@link ContentCaptureMode}.
     *
     * <p>Resolution order for each recording: per-recording override on the
     * {@code Start} object &gt; {@link ContentCaptureResolver} result &gt; OTel
     * context inherited from the parent generation (tool executions only) &gt;
     * this client-level mode. {@link ContentCaptureMode#DEFAULT} resolves to
     * {@link ContentCaptureMode#NO_TOOL_CONTENT} for backward compatibility.</p>
     *
     * <p>{@code null} is treated as {@link ContentCaptureMode#DEFAULT}.</p>
     */
    public Agento11yClientConfig setContentCapture(ContentCaptureMode contentCapture) {
        this.contentCapture = contentCapture == null ? ContentCaptureMode.DEFAULT : contentCapture;
        return this;
    }

    public ContentCaptureResolver getContentCaptureResolver() {
        return contentCaptureResolver;
    }

    /**
     * Sets a callback invoked for each generation, tool execution, and conversation
     * rating to dynamically choose a {@link ContentCaptureMode} based on request
     * metadata (e.g. tenant id, feature flag).
     *
     * <p>The resolver fails closed: any thrown exception is logged at WARNING and
     * the request is recorded as {@link ContentCaptureMode#METADATA_ONLY}.</p>
     *
     * <p>Pass {@code null} to clear the resolver.</p>
     */
    public Agento11yClientConfig setContentCaptureResolver(ContentCaptureResolver contentCaptureResolver) {
        this.contentCaptureResolver = contentCaptureResolver;
        return this;
    }

    public Tracer getTracer() {
        return tracer;
    }

    public Meter getMeter() {
        return meter;
    }

    public GenerationExporter getGenerationExporter() {
        return generationExporter;
    }

    public Agento11yClientConfig setGenerationExporter(GenerationExporter generationExporter) {
        this.generationExporter = generationExporter;
        return this;
    }

    public Agento11yClientConfig setTracer(Tracer tracer) {
        this.tracer = tracer;
        return this;
    }

    public Agento11yClientConfig setMeter(Meter meter) {
        this.meter = meter;
        return this;
    }

    public Logger getLogger() {
        return logger;
    }

    public Agento11yClientConfig setLogger(Logger logger) {
        this.logger = logger == null ? Logger.getLogger("com.grafana.agento11y.sdk") : logger;
        return this;
    }

    public Clock getClock() {
        return clock;
    }

    public Agento11yClientConfig setClock(Clock clock) {
        this.clock = clock == null ? Clock.systemUTC() : clock;
        return this;
    }

    /**
     * Default {@code gen_ai.agent.name} for generations that don't supply one
     * per-call. Filled from {@code AGENTO11Y_AGENT_NAME} (legacy
     * {@code SIGIL_AGENT_NAME}) when the caller leaves this empty.
     */
    public String getAgentName() {
        return agentName;
    }

    public Agento11yClientConfig setAgentName(String agentName) {
        this.agentName = agentName == null ? "" : agentName;
        return this;
    }

    /**
     * Default {@code gen_ai.agent.version} for generations that don't supply
     * one per-call. Filled from {@code AGENTO11Y_AGENT_VERSION} (legacy
     * {@code SIGIL_AGENT_VERSION}).
     */
    public String getAgentVersion() {
        return agentVersion;
    }

    public Agento11yClientConfig setAgentVersion(String agentVersion) {
        this.agentVersion = agentVersion == null ? "" : agentVersion;
        return this;
    }

    /**
     * Default {@code user.id} for generations that don't supply one per-call.
     * Filled from {@code AGENTO11Y_USER_ID} (legacy {@code SIGIL_USER_ID}).
     */
    public String getUserId() {
        return userId;
    }

    public Agento11yClientConfig setUserId(String userId) {
        this.userId = userId == null ? "" : userId;
        return this;
    }

    /**
     * Tags merged into every {@link GenerationStart#getTags()}. Per-call tags
     * win on key collision. Filled from {@code AGENTO11Y_TAGS} (legacy
     * {@code SIGIL_TAGS}).
     *
     * <p>The returned map is unmodifiable; use {@link #setTags(Map)} to
     * replace it.</p>
     */
    public Map<String, String> getTags() {
        return Collections.unmodifiableMap(tags);
    }

    public Agento11yClientConfig setTags(Map<String, String> tags) {
        this.tags = tags == null ? new LinkedHashMap<>() : new LinkedHashMap<>(tags);
        return this;
    }

    /**
     * Tri-state debug flag mirroring Go's {@code *bool}. {@code null} means
     * "not set" — filled from {@code AGENTO11Y_DEBUG} (legacy
     * {@code SIGIL_DEBUG}) when the caller hasn't supplied a value. Explicit
     * {@code Boolean.FALSE} overrides {@code AGENTO11Y_DEBUG=true}.
     */
    public Boolean getDebug() {
        return debug;
    }

    public Agento11yClientConfig setDebug(Boolean debug) {
        this.debug = debug;
        return this;
    }

    public Agento11yClientConfig copy() {
        return new Agento11yClientConfig()
                .setGenerationExport(generationExport.copy())
                .setApi(api.copy())
                .setEmbeddingCapture(embeddingCapture.copy())
                .setContentCapture(contentCapture)
                .setContentCaptureResolver(contentCaptureResolver)
                .setGenerationExporter(generationExporter)
                .setTracer(tracer)
                .setMeter(meter)
                .setLogger(logger)
                .setClock(clock)
                .setAgentName(agentName)
                .setAgentVersion(agentVersion)
                .setUserId(userId)
                .setTags(tags)
                .setDebug(debug);
    }
}
