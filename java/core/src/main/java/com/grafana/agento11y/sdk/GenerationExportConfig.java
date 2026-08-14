package com.grafana.agento11y.sdk;

import java.time.Duration;
import java.util.LinkedHashMap;
import java.util.Map;

/** Generation ingest export settings. */
public final class GenerationExportConfig {
    /**
     * Default per-attempt export timeout applied when neither the caller nor
     * {@code AGENTO11Y_EXPORT_TIMEOUT_MS} (legacy {@code SIGIL_EXPORT_TIMEOUT_MS})
     * supplies one.
     */
    public static final Duration DEFAULT_EXPORT_TIMEOUT = Duration.ofSeconds(30);

    /**
     * Export protocol. {@code null} means "not set" — env layer or
     * {@link Agento11yClient} resolves it to {@link GenerationExportProtocol#HTTP}.
     * An explicit {@code setProtocol(...)} call is preserved (caller-wins) and
     * not overridden by {@code AGENTO11Y_PROTOCOL} (legacy
     * {@code SIGIL_PROTOCOL}).
     */
    private GenerationExportProtocol protocol;
    /**
     * Export endpoint. Empty string means "not set" — env layer or
     * {@link Agento11yClient} resolves it from {@code AGENTO11Y_ENDPOINT} (legacy
     * {@code SIGIL_ENDPOINT}) when configured.
     * The HTTP exporter auto-appends {@code /api/v1/generations:export}
     * when the URL has no path. An explicit non-empty value is preserved
     * (caller-wins) and not overridden by env.
     */
    private String endpoint = "";
    private final Map<String, String> headers = new LinkedHashMap<>();
    private AuthConfig auth = new AuthConfig();
    private Boolean insecure;

    private int batchSize = 100;
    private Duration flushInterval = Duration.ofSeconds(1);
    private int queueSize = 2000;
    private int maxRetries = 5;
    private Duration initialBackoff = Duration.ofMillis(100);
    private Duration maxBackoff = Duration.ofSeconds(5);
    private int payloadMaxBytes = 4 << 20;
    private Duration exportTimeout = DEFAULT_EXPORT_TIMEOUT;
    /**
     * Tracks whether {@link #setExportTimeout(Duration)} was called with a
     * non-null value, so a caller value equal to
     * {@link #DEFAULT_EXPORT_TIMEOUT} still wins over the env layer. Preserved
     * by {@link #copy()} because {@link Agento11yEnvConfig#resolveFromEnv} works
     * on a copy of the caller config.
     */
    private boolean exportTimeoutExplicit;

    public GenerationExportProtocol getProtocol() {
        return protocol;
    }

    public GenerationExportConfig setProtocol(GenerationExportProtocol protocol) {
        this.protocol = protocol;
        return this;
    }

    public String getEndpoint() {
        return endpoint;
    }

    public GenerationExportConfig setEndpoint(String endpoint) {
        this.endpoint = endpoint == null ? "" : endpoint;
        return this;
    }

    public Map<String, String> getHeaders() {
        return headers;
    }

    public GenerationExportConfig setHeaders(Map<String, String> headers) {
        this.headers.clear();
        if (headers != null) {
            this.headers.putAll(headers);
        }
        return this;
    }

    public AuthConfig getAuth() {
        return auth;
    }

    public GenerationExportConfig setAuth(AuthConfig auth) {
        this.auth = auth == null ? new AuthConfig() : auth;
        return this;
    }

    /**
     * Returns the tri-state insecure flag. {@code null} means "not set" — the
     * resolved value is {@code false} (TLS on) unless {@code AGENTO11Y_INSECURE}
     * (legacy {@code SIGIL_INSECURE}) provides a value or the caller explicitly
     * sets one.
     *
     * <p>Use {@link #isInsecureResolved()} to read the boolean for transport
     * decisions.</p>
     */
    public Boolean getInsecure() {
        return insecure;
    }

    public GenerationExportConfig setInsecure(Boolean insecure) {
        this.insecure = insecure;
        return this;
    }

    /**
     * Returns the resolved boolean value with {@code null} treated as
     * {@code false} (TLS on by default — matches Go/JS/Python SDKs after
     * PR #103).
     */
    public boolean isInsecureResolved() {
        return Boolean.TRUE.equals(insecure);
    }

    public int getBatchSize() {
        return batchSize;
    }

    public GenerationExportConfig setBatchSize(int batchSize) {
        this.batchSize = batchSize;
        return this;
    }

    public Duration getFlushInterval() {
        return flushInterval;
    }

    public GenerationExportConfig setFlushInterval(Duration flushInterval) {
        this.flushInterval = flushInterval == null ? Duration.ZERO : flushInterval;
        return this;
    }

    public int getQueueSize() {
        return queueSize;
    }

    public GenerationExportConfig setQueueSize(int queueSize) {
        this.queueSize = queueSize;
        return this;
    }

    public int getMaxRetries() {
        return maxRetries;
    }

    public GenerationExportConfig setMaxRetries(int maxRetries) {
        this.maxRetries = maxRetries;
        return this;
    }

    public Duration getInitialBackoff() {
        return initialBackoff;
    }

    public GenerationExportConfig setInitialBackoff(Duration initialBackoff) {
        this.initialBackoff = initialBackoff == null ? Duration.ZERO : initialBackoff;
        return this;
    }

    public Duration getMaxBackoff() {
        return maxBackoff;
    }

    public GenerationExportConfig setMaxBackoff(Duration maxBackoff) {
        this.maxBackoff = maxBackoff == null ? Duration.ZERO : maxBackoff;
        return this;
    }

    public int getPayloadMaxBytes() {
        return payloadMaxBytes;
    }

    public GenerationExportConfig setPayloadMaxBytes(int payloadMaxBytes) {
        this.payloadMaxBytes = payloadMaxBytes;
        return this;
    }

    /**
     * Per-attempt timeout for a single generation export call (HTTP request or
     * gRPC deadline). Defaults to {@link #DEFAULT_EXPORT_TIMEOUT} (30s) and can
     * be filled from {@code AGENTO11Y_EXPORT_TIMEOUT_MS} (legacy
     * {@code SIGIL_EXPORT_TIMEOUT_MS}) when the caller leaves it unset.
     *
     * <p>The value bounds one attempt, not the whole retry loop. Each retry from
     * {@link #getMaxRetries()} gets a fresh timeout.</p>
     */
    public Duration getExportTimeout() {
        return exportTimeout;
    }

    /**
     * Sets the per-attempt export timeout. The value is stored as it is, like
     * the other duration fields in this class. The transports reject or
     * immediately expire a non-positive value.
     *
     * <p>{@code null} resets the field to {@link #DEFAULT_EXPORT_TIMEOUT} and
     * marks it unset again, so the env layer can still fill it.</p>
     */
    public GenerationExportConfig setExportTimeout(Duration exportTimeout) {
        this.exportTimeout = exportTimeout == null ? DEFAULT_EXPORT_TIMEOUT : exportTimeout;
        this.exportTimeoutExplicit = exportTimeout != null;
        return this;
    }

    /** Returns whether the caller or the env layer set an export timeout. */
    boolean isExportTimeoutExplicit() {
        return exportTimeoutExplicit;
    }

    public GenerationExportConfig copy() {
        GenerationExportConfig copy = new GenerationExportConfig()
                .setProtocol(protocol)
                .setEndpoint(endpoint)
                .setHeaders(headers)
                .setAuth(auth.copy())
                .setInsecure(insecure)
                .setBatchSize(batchSize)
                .setFlushInterval(flushInterval)
                .setQueueSize(queueSize)
                .setMaxRetries(maxRetries)
                .setInitialBackoff(initialBackoff)
                .setMaxBackoff(maxBackoff)
                .setPayloadMaxBytes(payloadMaxBytes);
        // Copied directly instead of through the setter so "unset" survives the
        // copy that env resolution takes of the caller config.
        copy.exportTimeout = exportTimeout;
        copy.exportTimeoutExplicit = exportTimeoutExplicit;
        return copy;
    }
}
