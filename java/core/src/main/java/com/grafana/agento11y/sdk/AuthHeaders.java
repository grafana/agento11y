package com.grafana.agento11y.sdk;

import java.nio.charset.StandardCharsets;
import java.util.Base64;
import java.util.LinkedHashMap;
import java.util.Map;

final class AuthHeaders {
    static final String TENANT_HEADER = "X-Scope-OrgID";
    static final String AUTHORIZATION_HEADER = "Authorization";

    private AuthHeaders() {
    }

    /**
     * Builds the auth-related headers for {@code auth.mode}. Mode-irrelevant
     * fields (e.g. {@code tenantId} on a bearer-mode config) are silently
     * ignored — env layering can populate any field independently of mode,
     * and rejecting cross-mode mixes only forced extra cleanup upstream.
     */
    static Map<String, String> resolve(Map<String, String> headers, AuthConfig auth, String label) {
        Map<String, String> out = new LinkedHashMap<>();
        if (headers != null) {
            out.putAll(headers);
        }

        AuthMode mode = auth == null || auth.getMode() == null ? AuthMode.NONE : auth.getMode();
        String tenantId = auth == null ? "" : auth.getTenantId().trim();
        String bearer = auth == null ? "" : auth.getBearerToken().trim();

        if (mode == AuthMode.NONE) {
            return out;
        }

        if (mode == AuthMode.TENANT) {
            if (tenantId.isEmpty()) {
                throw new IllegalArgumentException(label + " auth mode 'tenant' requires tenantId");
            }
            if (!hasHeader(out, TENANT_HEADER)) {
                out.put(TENANT_HEADER, tenantId);
            }
            return out;
        }

        if (mode == AuthMode.BEARER) {
            if (bearer.isEmpty()) {
                throw new IllegalArgumentException(label + " auth mode 'bearer' requires bearerToken");
            }
            if (!hasHeader(out, AUTHORIZATION_HEADER)) {
                out.put(AUTHORIZATION_HEADER, formatBearer(bearer));
            }
            return out;
        }

        if (mode == AuthMode.BASIC) {
            String password = auth == null ? "" : auth.getBasicPassword().trim();
            if (password.isEmpty()) {
                throw new IllegalArgumentException(label + " auth mode 'basic' requires basicPassword");
            }
            String user = auth == null ? "" : auth.getBasicUser().trim();
            if (user.isEmpty()) {
                user = tenantId;
            }
            if (user.isEmpty()) {
                throw new IllegalArgumentException(label + " auth mode 'basic' requires basicUser or tenantId");
            }
            if (!hasHeader(out, AUTHORIZATION_HEADER)) {
                String encoded = Base64.getEncoder().encodeToString(
                        (user + ":" + password).getBytes(StandardCharsets.UTF_8));
                out.put(AUTHORIZATION_HEADER, "Basic " + encoded);
            }
            if (!tenantId.isEmpty() && !hasHeader(out, TENANT_HEADER)) {
                out.put(TENANT_HEADER, tenantId);
            }
            return out;
        }

        throw new IllegalArgumentException("unsupported " + label + " auth mode " + mode);
    }

    private static boolean hasHeader(Map<String, String> headers, String key) {
        String target = key.toLowerCase();
        return headers.keySet().stream().anyMatch(existing -> existing != null && existing.toLowerCase().equals(target));
    }

    private static String formatBearer(String token) {
        String value = token.trim();
        if (value.regionMatches(true, 0, "Bearer ", 0, 7)) {
            value = value.substring(7).trim();
        }
        return "Bearer " + value;
    }
}
