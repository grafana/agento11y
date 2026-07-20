package com.grafana.agento11y.sdk;

import java.util.Map;

/**
 * Callback for dynamic per-request {@link ContentCaptureMode} resolution.
 *
 * <p>Configured via {@link Agento11yClientConfig#setContentCaptureResolver(ContentCaptureResolver)}.
 * The resolver is invoked for each generation, tool execution, and conversation rating
 * with the request metadata; its result feeds the resolution chain alongside the
 * client-level mode.</p>
 *
 * <p>If the resolver throws, the SDK fails closed to {@link ContentCaptureMode#METADATA_ONLY}
 * and logs a warning via the configured logger.</p>
 */
@FunctionalInterface
public interface ContentCaptureResolver {
    /**
     * @param metadata Request metadata. May be {@code null} (e.g. for tool executions
     *                 where no metadata is attached). Implementations should treat the
     *                 map as read-only.
     */
    ContentCaptureMode resolve(Map<String, Object> metadata);
}
