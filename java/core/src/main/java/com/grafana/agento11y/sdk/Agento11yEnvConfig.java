package com.grafana.agento11y.sdk;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.function.Function;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Reads canonical {@code AGENTO11Y_*} environment variables (with legacy
 * {@code SIGIL_*} fallbacks) and layers them under caller-supplied
 * {@link Agento11yClientConfig} values.
 *
 * <p>Resolution order (highest precedence first):
 *
 * <ol>
 *   <li>Caller-supplied {@code Agento11yClientConfig} field (when not at its
 *       default/unset state).</li>
 *   <li>Canonical {@code AGENTO11Y_*} env var, falling back to the legacy
 *       {@code SIGIL_*} spelling when the preferred one is unset or blank.</li>
 *   <li>SDK schema default (the field initializer on
 *       {@link Agento11yClientConfig} / {@link GenerationExportConfig} /
 *       {@link AuthConfig}).</li>
 * </ol>
 *
 * <p>Per field, the first nonblank value of the preferred then legacy name is
 * selected before parsing: a nonblank preferred value always wins, even when
 * it later fails validation, so stale legacy config cannot silently
 * resurface.</p>
 *
 * <p>Mirrors the Go reference implementation in {@code go/agento11y/env.go}.
 * Invalid env values are skipped with a warning so a single typo does not
 * discard the rest of the env layer.</p>
 */
public final class Agento11yEnvConfig {
    public static final String ENV_ENDPOINT_PREFERRED = "AGENTO11Y_ENDPOINT";
    public static final String ENV_PROTOCOL_PREFERRED = "AGENTO11Y_PROTOCOL";
    public static final String ENV_INSECURE_PREFERRED = "AGENTO11Y_INSECURE";
    public static final String ENV_HEADERS_PREFERRED = "AGENTO11Y_HEADERS";
    public static final String ENV_AUTH_MODE_PREFERRED = "AGENTO11Y_AUTH_MODE";
    public static final String ENV_AUTH_TENANT_ID_PREFERRED = "AGENTO11Y_AUTH_TENANT_ID";
    public static final String ENV_AUTH_TOKEN_PREFERRED = "AGENTO11Y_AUTH_TOKEN";
    public static final String ENV_AGENT_NAME_PREFERRED = "AGENTO11Y_AGENT_NAME";
    public static final String ENV_AGENT_VERSION_PREFERRED = "AGENTO11Y_AGENT_VERSION";
    public static final String ENV_USER_ID_PREFERRED = "AGENTO11Y_USER_ID";
    public static final String ENV_TAGS_PREFERRED = "AGENTO11Y_TAGS";
    public static final String ENV_CONTENT_CAPTURE_MODE_PREFERRED = "AGENTO11Y_CONTENT_CAPTURE_MODE";
    public static final String ENV_DEBUG_PREFERRED = "AGENTO11Y_DEBUG";

    // Legacy SIGIL_* spellings, still honored as fallbacks.
    public static final String ENV_ENDPOINT = "SIGIL_ENDPOINT";
    public static final String ENV_PROTOCOL = "SIGIL_PROTOCOL";
    public static final String ENV_INSECURE = "SIGIL_INSECURE";
    public static final String ENV_HEADERS = "SIGIL_HEADERS";
    public static final String ENV_AUTH_MODE = "SIGIL_AUTH_MODE";
    public static final String ENV_AUTH_TENANT_ID = "SIGIL_AUTH_TENANT_ID";
    public static final String ENV_AUTH_TOKEN = "SIGIL_AUTH_TOKEN";
    public static final String ENV_AGENT_NAME = "SIGIL_AGENT_NAME";
    public static final String ENV_AGENT_VERSION = "SIGIL_AGENT_VERSION";
    public static final String ENV_USER_ID = "SIGIL_USER_ID";
    public static final String ENV_TAGS = "SIGIL_TAGS";
    public static final String ENV_CONTENT_CAPTURE_MODE = "SIGIL_CONTENT_CAPTURE_MODE";
    public static final String ENV_DEBUG = "SIGIL_DEBUG";

    private Agento11yEnvConfig() {
    }

    /** Result of {@link #resolveFromEnv} — resolved config plus any non-fatal warnings. */
    public record EnvResolveResult(Agento11yClientConfig config, List<String> warnings) {
        public EnvResolveResult {
            warnings = warnings == null ? List.of() : List.copyOf(warnings);
        }
    }

    /**
     * Returns a config built from process env vars layered onto a fresh
     * {@link Agento11yClientConfig}. Convenience helper; most callers should let
     * {@link Agento11yClient} construction perform the same resolution internally.
     * Warnings for invalid env values are logged to {@code com.grafana.agento11y.sdk}.
     */
    public static Agento11yClientConfig fromEnv() {
        EnvResolveResult result = resolveFromEnv(System::getenv, new Agento11yClientConfig());
        logWarnings(Logger.getLogger("com.grafana.agento11y.sdk"), result.warnings());
        return result.config();
    }

    /**
     * Applies canonical {@code AGENTO11Y_*} env values (with legacy
     * {@code SIGIL_*} fallbacks) onto {@code base}, preserving caller-supplied
     * fields. The returned config is a fresh copy; {@code base} is not
     * mutated.
     *
     * <p>Invalid auth-mode, protocol, or content-capture-mode values are
     * skipped — the base value is kept and a warning naming the selected env
     * var is appended to the result so other valid env vars still apply.</p>
     */
    public static EnvResolveResult resolveFromEnv(Function<String, String> lookup, Agento11yClientConfig base) {
        Function<String, String> source = lookup == null ? System::getenv : lookup;
        Agento11yClientConfig cfg = base == null ? new Agento11yClientConfig() : base.copy();
        List<String> warnings = new ArrayList<>();

        GenerationExportConfig export = cfg.getGenerationExport();

        EnvValue endpoint = envTrimmed(source, ENV_ENDPOINT_PREFERRED, ENV_ENDPOINT);
        if (endpoint != null && export.getEndpoint().isEmpty()) {
            export.setEndpoint(endpoint.value());
        }

        EnvValue protocol = envTrimmed(source, ENV_PROTOCOL_PREFERRED, ENV_PROTOCOL);
        if (protocol != null && export.getProtocol() == null) {
            GenerationExportProtocol parsed = parseProtocol(protocol.value());
            if (parsed != null) {
                export.setProtocol(parsed);
            } else {
                warnings.add("agento11y: ignoring invalid " + protocol.key() + " " + protocol.value());
            }
        }

        EnvValue insecureRaw = envTrimmed(source, ENV_INSECURE_PREFERRED, ENV_INSECURE);
        if (insecureRaw != null && export.getInsecure() == null) {
            export.setInsecure(parseBool(insecureRaw.value()));
        }

        EnvValue headersRaw = envTrimmed(source, ENV_HEADERS_PREFERRED, ENV_HEADERS);
        if (headersRaw != null && export.getHeaders().isEmpty()) {
            export.setHeaders(parseCsvKv(headersRaw.value()));
        }

        AuthConfig auth = export.getAuth();
        EnvValue authModeRaw = envTrimmed(source, ENV_AUTH_MODE_PREFERRED, ENV_AUTH_MODE);
        if (authModeRaw != null && auth.getMode() == null) {
            AuthMode parsed = parseAuthMode(authModeRaw.value());
            if (parsed != null) {
                auth.setMode(parsed);
            } else {
                warnings.add("agento11y: ignoring invalid " + authModeRaw.key() + " " + authModeRaw.value());
            }
        }

        EnvValue tenantId = envTrimmed(source, ENV_AUTH_TENANT_ID_PREFERRED, ENV_AUTH_TENANT_ID);
        if (tenantId != null && auth.getTenantId().isEmpty()) {
            auth.setTenantId(tenantId.value());
        }

        EnvValue token = envTrimmed(source, ENV_AUTH_TOKEN_PREFERRED, ENV_AUTH_TOKEN);
        if (token != null) {
            // Set both fields when empty; AuthHeaders.resolve uses only the one
            // matching the final mode. Lets env's token populate a caller-set
            // mode without env declaring an AUTH_MODE.
            if (auth.getBearerToken().isEmpty()) {
                auth.setBearerToken(token.value());
            }
            if (auth.getBasicPassword().isEmpty()) {
                auth.setBasicPassword(token.value());
            }
        }
        if (auth.getMode() == AuthMode.BASIC && auth.getBasicUser().isEmpty() && !auth.getTenantId().isEmpty()) {
            auth.setBasicUser(auth.getTenantId());
        }

        // Finalize tri-state defaults after env layering: null/empty means
        // "no caller value and no env value", so apply the schema default.
        if (export.getProtocol() == null) {
            export.setProtocol(GenerationExportProtocol.HTTP);
        }
        if (export.getEndpoint().isEmpty()) {
            export.setEndpoint("http://localhost:8080");
        }
        if (auth.getMode() == null) {
            auth.setMode(AuthMode.NONE);
        }

        EnvValue agentName = envTrimmed(source, ENV_AGENT_NAME_PREFERRED, ENV_AGENT_NAME);
        if (agentName != null && cfg.getAgentName().isEmpty()) {
            cfg.setAgentName(agentName.value());
        }
        EnvValue agentVersion = envTrimmed(source, ENV_AGENT_VERSION_PREFERRED, ENV_AGENT_VERSION);
        if (agentVersion != null && cfg.getAgentVersion().isEmpty()) {
            cfg.setAgentVersion(agentVersion.value());
        }
        EnvValue userId = envTrimmed(source, ENV_USER_ID_PREFERRED, ENV_USER_ID);
        if (userId != null && cfg.getUserId().isEmpty()) {
            cfg.setUserId(userId.value());
        }

        EnvValue tagsRaw = envTrimmed(source, ENV_TAGS_PREFERRED, ENV_TAGS);
        if (tagsRaw != null) {
            Map<String, String> envTags = parseCsvKv(tagsRaw.value());
            // Env tags act as a base layer; caller tags win on collision.
            Map<String, String> merged = new LinkedHashMap<>(envTags);
            merged.putAll(cfg.getTags());
            cfg.setTags(merged);
        }

        EnvValue ccmRaw = envTrimmed(source, ENV_CONTENT_CAPTURE_MODE_PREFERRED, ENV_CONTENT_CAPTURE_MODE);
        if (ccmRaw != null && cfg.getContentCapture() == ContentCaptureMode.DEFAULT) {
            ContentCaptureMode parsed = parseContentCaptureMode(ccmRaw.value());
            if (parsed != null) {
                cfg.setContentCapture(parsed);
            } else {
                warnings.add("agento11y: ignoring invalid " + ccmRaw.key() + " " + ccmRaw.value());
            }
        }

        EnvValue debugRaw = envTrimmed(source, ENV_DEBUG_PREFERRED, ENV_DEBUG);
        if (debugRaw != null && cfg.getDebug() == null) {
            cfg.setDebug(parseBool(debugRaw.value()));
        }

        return new EnvResolveResult(cfg, warnings);
    }

    /** Logs each warning at WARNING level on {@code logger} (no-op when null). */
    public static void logWarnings(Logger logger, List<String> warnings) {
        if (logger == null || warnings == null) {
            return;
        }
        for (String w : warnings) {
            logger.log(Level.WARNING, w);
        }
    }

    /** A trimmed env value together with the env-var name it was read from. */
    private record EnvValue(String value, String key) {
    }

    /**
     * Selects the first nonblank value of {@code preferred} then {@code legacy}
     * and returns it with the env-var name it came from, so validation warnings
     * can name the key the user actually set. Returns {@code null} when both
     * are unset or blank.
     */
    private static EnvValue envTrimmed(Function<String, String> lookup, String preferred, String legacy) {
        for (String key : new String[] {preferred, legacy}) {
            String raw;
            try {
                raw = lookup.apply(key);
            } catch (SecurityException ex) {
                continue;
            }
            if (raw == null) {
                continue;
            }
            String v = raw.trim();
            if (!v.isEmpty()) {
                return new EnvValue(v, key);
            }
        }
        return null;
    }

    static boolean parseBool(String raw) {
        if (raw == null) {
            return false;
        }
        switch (raw.trim().toLowerCase(Locale.ROOT)) {
            case "1":
            case "true":
            case "yes":
            case "on":
                return true;
            default:
                return false;
        }
    }

    static Map<String, String> parseCsvKv(String raw) {
        Map<String, String> out = new LinkedHashMap<>();
        if (raw == null) {
            return out;
        }
        for (String part : raw.split(",", -1)) {
            String trimmed = part.trim();
            if (trimmed.isEmpty()) {
                continue;
            }
            int idx = trimmed.indexOf('=');
            if (idx <= 0) {
                continue;
            }
            String key = trimmed.substring(0, idx).trim();
            String value = trimmed.substring(idx + 1).trim();
            if (!key.isEmpty()) {
                out.put(key, value);
            }
        }
        return out;
    }

    static ContentCaptureMode parseContentCaptureMode(String raw) {
        if (raw == null) {
            return null;
        }
        switch (raw.trim().toLowerCase(Locale.ROOT)) {
            case "full":
                return ContentCaptureMode.FULL;
            case "no_tool_content":
                return ContentCaptureMode.NO_TOOL_CONTENT;
            case "metadata_only":
                return ContentCaptureMode.METADATA_ONLY;
            case "full_with_metadata_spans":
                return ContentCaptureMode.FULL_WITH_METADATA_SPANS;
            default:
                return null;
        }
    }

    static AuthMode parseAuthMode(String raw) {
        if (raw == null) {
            return null;
        }
        switch (raw.trim().toLowerCase(Locale.ROOT)) {
            case "none":
                return AuthMode.NONE;
            case "tenant":
                return AuthMode.TENANT;
            case "bearer":
                return AuthMode.BEARER;
            case "basic":
                return AuthMode.BASIC;
            default:
                return null;
        }
    }

    static GenerationExportProtocol parseProtocol(String raw) {
        if (raw == null) {
            return null;
        }
        switch (raw.trim().toLowerCase(Locale.ROOT)) {
            case "grpc":
                return GenerationExportProtocol.GRPC;
            case "http":
                return GenerationExportProtocol.HTTP;
            case "none":
                return GenerationExportProtocol.NONE;
            default:
                return null;
        }
    }

}
