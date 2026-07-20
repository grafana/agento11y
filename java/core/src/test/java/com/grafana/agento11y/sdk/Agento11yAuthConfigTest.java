package com.grafana.agento11y.sdk;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

import java.nio.charset.StandardCharsets;
import java.util.Base64;
import java.util.LinkedHashMap;
import java.util.Map;
import org.junit.jupiter.api.Test;

class Agento11yAuthConfigTest {
    @Test
    void requiresModeSpecificCredentials() {
        assertThatThrownBy(() -> AuthHeaders.resolve(Map.of(), new AuthConfig().setMode(AuthMode.TENANT), "generation export"))
                .isInstanceOf(IllegalArgumentException.class)
                .hasMessageContaining("requires tenantId");

        assertThatThrownBy(() -> AuthHeaders.resolve(Map.of(), new AuthConfig().setMode(AuthMode.BEARER), "generation export"))
                .isInstanceOf(IllegalArgumentException.class)
                .hasMessageContaining("requires bearerToken");
    }

    @Test
    void modeNoneIgnoresIrrelevantCredentialFields() {
        // env layering can populate cross-mode fields without explicit SIGIL_AUTH_MODE.
        Map<String, String> headers = AuthHeaders.resolve(
                Map.of(),
                new AuthConfig()
                        .setMode(AuthMode.NONE)
                        .setTenantId("42")
                        .setBearerToken("tok")
                        .setBasicUser("user")
                        .setBasicPassword("pass"),
                "trace");
        assertThat(headers).isEmpty();
    }

    @Test
    void modeBearerIgnoresIrrelevantTenantField() {
        Map<String, String> headers = AuthHeaders.resolve(
                Map.of(),
                new AuthConfig().setMode(AuthMode.BEARER).setBearerToken("tok").setTenantId("42"),
                "generation export");
        assertThat(headers).containsEntry("Authorization", "Bearer tok");
        assertThat(headers).doesNotContainKey("X-Scope-OrgID");
    }

    @Test
    void explicitHeadersOverrideInjectedAuthHeaders() {
        Map<String, String> trace = AuthHeaders.resolve(
                Map.of("Authorization", "Bearer override"),
                new AuthConfig().setMode(AuthMode.BEARER).setBearerToken("injected"),
                "trace");
        assertThat(trace.get("Authorization")).isEqualTo("Bearer override");

        Map<String, String> generation = AuthHeaders.resolve(
                Map.of("x-scope-orgid", "tenant-override"),
                new AuthConfig().setMode(AuthMode.TENANT).setTenantId("tenant-injected"),
                "generation export");
        assertThat(generation.get("x-scope-orgid")).isEqualTo("tenant-override");
    }

    @Test
    void basicAuthWithTenantId() {
        Map<String, String> headers = AuthHeaders.resolve(
                Map.of(),
                new AuthConfig().setMode(AuthMode.BASIC).setTenantId("42").setBasicPassword("secret"),
                "generation export");
        String expected = "Basic " + Base64.getEncoder()
                .encodeToString("42:secret".getBytes(StandardCharsets.UTF_8));
        assertThat(headers.get("Authorization")).isEqualTo(expected);
        assertThat(headers.get("X-Scope-OrgID")).isEqualTo("42");
    }

    @Test
    void basicAuthWithExplicitUser() {
        Map<String, String> headers = AuthHeaders.resolve(
                Map.of(),
                new AuthConfig().setMode(AuthMode.BASIC).setTenantId("42")
                        .setBasicUser("probe-user").setBasicPassword("secret"),
                "generation export");
        String expected = "Basic " + Base64.getEncoder()
                .encodeToString("probe-user:secret".getBytes(StandardCharsets.UTF_8));
        assertThat(headers.get("Authorization")).isEqualTo(expected);
        assertThat(headers.get("X-Scope-OrgID")).isEqualTo("42");
    }

    @Test
    void basicAuthExplicitHeaderWins() {
        Map<String, String> input = new LinkedHashMap<>();
        input.put("Authorization", "Basic override");
        input.put("X-Scope-OrgID", "override-tenant");
        Map<String, String> headers = AuthHeaders.resolve(
                input,
                new AuthConfig().setMode(AuthMode.BASIC).setTenantId("42").setBasicPassword("secret"),
                "generation export");
        assertThat(headers.get("Authorization")).isEqualTo("Basic override");
        assertThat(headers.get("X-Scope-OrgID")).isEqualTo("override-tenant");
    }

    @Test
    void basicAuthRejectsInvalidConfig() {
        assertThatThrownBy(() -> AuthHeaders.resolve(Map.of(),
                new AuthConfig().setMode(AuthMode.BASIC), "generation export"))
                .isInstanceOf(IllegalArgumentException.class)
                .hasMessageContaining("requires basicPassword");

        assertThatThrownBy(() -> AuthHeaders.resolve(Map.of(),
                new AuthConfig().setMode(AuthMode.BASIC).setBasicPassword("secret"), "generation export"))
                .isInstanceOf(IllegalArgumentException.class)
                .hasMessageContaining("requires basicUser or tenantId");
    }

    @Test
    void basicAuthCopy() {
        AuthConfig original = new AuthConfig()
                .setMode(AuthMode.BASIC)
                .setTenantId("42")
                .setBasicUser("user")
                .setBasicPassword("pass");
        AuthConfig copy = original.copy();
        assertThat(copy.getMode()).isEqualTo(AuthMode.BASIC);
        assertThat(copy.getTenantId()).isEqualTo("42");
        assertThat(copy.getBasicUser()).isEqualTo("user");
        assertThat(copy.getBasicPassword()).isEqualTo("pass");
    }
}
