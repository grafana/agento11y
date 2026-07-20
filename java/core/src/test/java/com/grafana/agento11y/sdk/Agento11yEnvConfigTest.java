package com.grafana.agento11y.sdk;

import static org.assertj.core.api.Assertions.assertThat;

import com.grafana.agento11y.sdk.Agento11yEnvConfig.EnvResolveResult;
import java.util.HashMap;
import java.util.LinkedHashMap;
import java.util.Map;
import java.util.function.Function;
import org.junit.jupiter.api.Test;

class Agento11yEnvConfigTest {

    private static Function<String, String> mapLookup(Map<String, String> env) {
        return env::get;
    }

    private static EnvResolveResult resolve(Map<String, String> env) {
        return Agento11yEnvConfig.resolveFromEnv(mapLookup(env), new Agento11yClientConfig());
    }

    @Test
    void noEnvKeepsBaseDefaults() {
        EnvResolveResult result = resolve(Map.of());
        Agento11yClientConfig cfg = result.config();
        assertThat(cfg.getAgentName()).isEmpty();
        assertThat(cfg.getAgentVersion()).isEmpty();
        assertThat(cfg.getUserId()).isEmpty();
        assertThat(cfg.getTags()).isEmpty();
        assertThat(cfg.getDebug()).isNull();
        assertThat(cfg.getGenerationExport().getInsecure()).isNull();
        assertThat(cfg.getGenerationExport().getEndpoint())
                .isEqualTo("http://localhost:8080");
        assertThat(result.warnings()).isEmpty();
    }

    @Test
    void transportFromEnv() {
        Map<String, String> env = Map.of(
                "SIGIL_ENDPOINT", "https://env:4318",
                "SIGIL_PROTOCOL", "http",
                "SIGIL_INSECURE", "true",
                "SIGIL_HEADERS", "X-A=1,X-B=two");
        Agento11yClientConfig cfg = resolve(env).config();
        assertThat(cfg.getGenerationExport().getEndpoint()).isEqualTo("https://env:4318");
        assertThat(cfg.getGenerationExport().getProtocol()).isEqualTo(GenerationExportProtocol.HTTP);
        assertThat(cfg.getGenerationExport().getInsecure()).isTrue();
        assertThat(cfg.getGenerationExport().getHeaders())
                .containsEntry("X-A", "1")
                .containsEntry("X-B", "two");
    }

    @Test
    void grpcProtocolFromEnv() {
        Agento11yClientConfig cfg = resolve(Map.of("SIGIL_PROTOCOL", "grpc")).config();
        assertThat(cfg.getGenerationExport().getProtocol()).isEqualTo(GenerationExportProtocol.GRPC);
    }

    @Test
    void basicAuthFromEnv() {
        Map<String, String> env = Map.of(
                "SIGIL_AUTH_MODE", "basic",
                "SIGIL_AUTH_TENANT_ID", "42",
                "SIGIL_AUTH_TOKEN", "glc_xxx");
        AuthConfig auth = resolve(env).config().getGenerationExport().getAuth();
        assertThat(auth.getMode()).isEqualTo(AuthMode.BASIC);
        assertThat(auth.getTenantId()).isEqualTo("42");
        assertThat(auth.getBasicPassword()).isEqualTo("glc_xxx");
        assertThat(auth.getBasicUser()).isEqualTo("42");
    }

    @Test
    void bearerAuthFromEnv() {
        Map<String, String> env = Map.of(
                "SIGIL_AUTH_MODE", "bearer",
                "SIGIL_AUTH_TOKEN", "tok");
        AuthConfig auth = resolve(env).config().getGenerationExport().getAuth();
        assertThat(auth.getMode()).isEqualTo(AuthMode.BEARER);
        assertThat(auth.getBearerToken()).isEqualTo("tok");
    }

    @Test
    void invalidAuthModeWarnsAndPreservesOtherEnv() {
        Map<String, String> env = new HashMap<>();
        env.put("SIGIL_AUTH_MODE", "Bearrer");
        env.put("SIGIL_ENDPOINT", "valid.example:4318");
        env.put("SIGIL_AGENT_NAME", "valid-agent");
        env.put("SIGIL_USER_ID", "alice");
        EnvResolveResult result = Agento11yEnvConfig.resolveFromEnv(env::get, new Agento11yClientConfig());

        Agento11yClientConfig cfg = result.config();
        assertThat(cfg.getGenerationExport().getEndpoint()).isEqualTo("valid.example:4318");
        assertThat(cfg.getAgentName()).isEqualTo("valid-agent");
        assertThat(cfg.getUserId()).isEqualTo("alice");
        assertThat(cfg.getGenerationExport().getAuth().getMode()).isEqualTo(AuthMode.NONE);
        assertThat(result.warnings()).anySatisfy(w -> assertThat(w).contains("SIGIL_AUTH_MODE"));
    }

    @Test
    void invalidContentCaptureModeWarnsAndPreservesOtherEnv() {
        Map<String, String> env = new HashMap<>();
        env.put("SIGIL_CONTENT_CAPTURE_MODE", "bogus");
        env.put("SIGIL_ENDPOINT", "valid.example:4318");
        EnvResolveResult result = Agento11yEnvConfig.resolveFromEnv(env::get, new Agento11yClientConfig());

        Agento11yClientConfig cfg = result.config();
        assertThat(cfg.getContentCapture()).isEqualTo(ContentCaptureMode.DEFAULT);
        assertThat(cfg.getGenerationExport().getEndpoint()).isEqualTo("valid.example:4318");
        assertThat(result.warnings()).anySatisfy(w -> assertThat(w).contains("SIGIL_CONTENT_CAPTURE_MODE"));
    }

    @Test
    void invalidContentCaptureModeKeepsCallerBaseValue() {
        Agento11yClientConfig base = new Agento11yClientConfig().setContentCapture(ContentCaptureMode.METADATA_ONLY);
        EnvResolveResult result = Agento11yEnvConfig.resolveFromEnv(
                Map.of("SIGIL_CONTENT_CAPTURE_MODE", "bogus")::get, base);
        // Base value should remain because it isn't DEFAULT (caller-set).
        assertThat(result.config().getContentCapture()).isEqualTo(ContentCaptureMode.METADATA_ONLY);
    }

    @Test
    void contentCaptureModeFromEnv() {
        Agento11yClientConfig cfg = resolve(Map.of("SIGIL_CONTENT_CAPTURE_MODE", "metadata_only")).config();
        assertThat(cfg.getContentCapture()).isEqualTo(ContentCaptureMode.METADATA_ONLY);
    }

    @Test
    void contentCaptureModeFullWithMetadataSpansFromEnv() {
        Agento11yClientConfig cfg = resolve(Map.of("SIGIL_CONTENT_CAPTURE_MODE", "full_with_metadata_spans")).config();
        assertThat(cfg.getContentCapture()).isEqualTo(ContentCaptureMode.FULL_WITH_METADATA_SPANS);
    }

    @Test
    void agentUserTagsDebugFromEnv() {
        Map<String, String> env = Map.of(
                "SIGIL_AGENT_NAME", "planner",
                "SIGIL_AGENT_VERSION", "1.2.3",
                "SIGIL_USER_ID", "alice@example.com",
                "SIGIL_TAGS", "service=orchestrator,env=prod",
                "SIGIL_DEBUG", "true");
        Agento11yClientConfig cfg = resolve(env).config();
        assertThat(cfg.getAgentName()).isEqualTo("planner");
        assertThat(cfg.getAgentVersion()).isEqualTo("1.2.3");
        assertThat(cfg.getUserId()).isEqualTo("alice@example.com");
        assertThat(cfg.getTags()).containsEntry("service", "orchestrator").containsEntry("env", "prod");
        assertThat(cfg.getDebug()).isTrue();
    }

    @Test
    void whitespaceOnlyValuesAreIgnored() {
        Map<String, String> env = new HashMap<>();
        env.put("SIGIL_TAGS", "   ");
        env.put("SIGIL_AGENT_NAME", "");
        env.put("SIGIL_USER_ID", "\t \n");
        Agento11yClientConfig cfg = Agento11yEnvConfig.resolveFromEnv(env::get, new Agento11yClientConfig()).config();
        assertThat(cfg.getTags()).isEmpty();
        assertThat(cfg.getAgentName()).isEmpty();
        assertThat(cfg.getUserId()).isEmpty();
    }

    @Test
    void callerEndpointWinsOverEnv() {
        Agento11yClientConfig base = new Agento11yClientConfig();
        base.getGenerationExport().setEndpoint("https://caller-host");
        Agento11yClientConfig cfg = Agento11yEnvConfig.resolveFromEnv(
                Map.of("SIGIL_ENDPOINT", "https://env-host")::get, base).config();
        assertThat(cfg.getGenerationExport().getEndpoint()).isEqualTo("https://caller-host");
    }

    @Test
    void callerInsecureFalseBeatsEnvTrue() {
        Agento11yClientConfig base = new Agento11yClientConfig();
        base.getGenerationExport().setInsecure(Boolean.FALSE);
        Agento11yClientConfig cfg = Agento11yEnvConfig.resolveFromEnv(
                Map.of("SIGIL_INSECURE", "true")::get, base).config();
        assertThat(cfg.getGenerationExport().getInsecure()).isFalse();
        assertThat(cfg.getGenerationExport().isInsecureResolved()).isFalse();
    }

    @Test
    void envInsecureTrueLayersUnderUnsetCaller() {
        Agento11yClientConfig cfg = Agento11yEnvConfig.resolveFromEnv(
                Map.of("SIGIL_INSECURE", "true")::get, new Agento11yClientConfig()).config();
        assertThat(cfg.getGenerationExport().getInsecure()).isTrue();
        assertThat(cfg.getGenerationExport().isInsecureResolved()).isTrue();
    }

    @Test
    void unsetInsecureResolvesToFalse() {
        // SIGIL_INSECURE unset and caller didn't set → resolved boolean is false (TLS on).
        Agento11yClientConfig cfg = Agento11yEnvConfig.resolveFromEnv(Map.<String, String>of()::get, new Agento11yClientConfig())
                .config();
        assertThat(cfg.getGenerationExport().getInsecure()).isNull();
        assertThat(cfg.getGenerationExport().isInsecureResolved()).isFalse();
    }

    @Test
    void authTokenFillsBothBearerAndBasicWhenEmpty() {
        Agento11yClientConfig cfg = Agento11yEnvConfig.resolveFromEnv(
                Map.of("SIGIL_AUTH_TOKEN", "secret")::get, new Agento11yClientConfig()).config();
        AuthConfig auth = cfg.getGenerationExport().getAuth();
        assertThat(auth.getBearerToken()).isEqualTo("secret");
        assertThat(auth.getBasicPassword()).isEqualTo("secret");
    }

    @Test
    void authTokenSkipsAlreadyFilledFields() {
        Agento11yClientConfig base = new Agento11yClientConfig();
        base.getGenerationExport().getAuth().setBearerToken("caller-bearer");
        Agento11yClientConfig cfg = Agento11yEnvConfig.resolveFromEnv(
                Map.of("SIGIL_AUTH_TOKEN", "env-token")::get, base).config();
        AuthConfig auth = cfg.getGenerationExport().getAuth();
        assertThat(auth.getBearerToken()).isEqualTo("caller-bearer");
        assertThat(auth.getBasicPassword()).isEqualTo("env-token");
    }

    @Test
    void basicModeUsesTenantAsBasicUserFallback() {
        Map<String, String> env = Map.of(
                "SIGIL_AUTH_MODE", "basic",
                "SIGIL_AUTH_TENANT_ID", "tenant-a",
                "SIGIL_AUTH_TOKEN", "secret");
        AuthConfig auth = resolve(env).config().getGenerationExport().getAuth();
        assertThat(auth.getMode()).isEqualTo(AuthMode.BASIC);
        assertThat(auth.getBasicUser()).isEqualTo("tenant-a");
        assertThat(auth.getBasicPassword()).isEqualTo("secret");
    }

    @Test
    void strayTenantIdKeepsModeNone() {
        AuthConfig auth = resolve(Map.of("SIGIL_AUTH_TENANT_ID", "42"))
                .config().getGenerationExport().getAuth();
        assertThat(auth.getMode()).isEqualTo(AuthMode.NONE);
        assertThat(auth.getTenantId()).isEqualTo("42");
    }

    @Test
    void parseCsvKvHandlesEdgeCases() {
        Map<String, String> result = Agento11yEnvConfig.parseCsvKv("a=1, b = two ,, =skip,c=");
        assertThat(result).containsExactlyInAnyOrderEntriesOf(Map.of(
                "a", "1",
                "b", "two",
                "c", ""));
    }

    @Test
    void envTagsMergeUnderCallerTags() {
        Agento11yClientConfig base = new Agento11yClientConfig();
        base.setTags(Map.of("team", "ai", "env", "staging"));
        Agento11yClientConfig cfg = Agento11yEnvConfig.resolveFromEnv(
                Map.of("SIGIL_TAGS", "service=orch,env=prod")::get, base).config();
        Map<String, String> tags = cfg.getTags();
        assertThat(tags).containsEntry("service", "orch");      // env-only fills
        assertThat(tags).containsEntry("team", "ai");           // caller-only preserved
        assertThat(tags).containsEntry("env", "staging");       // caller wins on collision
    }

    @Test
    void parseBoolAcceptsCanonicalTrue() {
        assertThat(Agento11yEnvConfig.parseBool("1")).isTrue();
        assertThat(Agento11yEnvConfig.parseBool("true")).isTrue();
        assertThat(Agento11yEnvConfig.parseBool("YES")).isTrue();
        assertThat(Agento11yEnvConfig.parseBool("On")).isTrue();
        assertThat(Agento11yEnvConfig.parseBool("0")).isFalse();
        assertThat(Agento11yEnvConfig.parseBool("false")).isFalse();
        assertThat(Agento11yEnvConfig.parseBool("garbage")).isFalse();
    }

    @Test
    void fromEnvUsesProcessEnv() {
        // Smoke: just verify it doesn't throw and returns a config.
        Agento11yClientConfig cfg = Agento11yEnvConfig.fromEnv();
        assertThat(cfg).isNotNull();
    }

    @Test
    void resolveDoesNotMutateBase() {
        Agento11yClientConfig base = new Agento11yClientConfig().setAgentName("base-agent");
        Agento11yClientConfig resolved = Agento11yEnvConfig.resolveFromEnv(
                Map.of("SIGIL_USER_ID", "alice")::get, base).config();
        assertThat(base.getUserId()).isEmpty();
        assertThat(resolved.getUserId()).isEqualTo("alice");
        assertThat(resolved.getAgentName()).isEqualTo("base-agent");
    }

    @Test
    void parseAuthModeNormalizesCase() {
        assertThat(Agento11yEnvConfig.parseAuthMode("BASIC")).isEqualTo(AuthMode.BASIC);
        assertThat(Agento11yEnvConfig.parseAuthMode("Bearer")).isEqualTo(AuthMode.BEARER);
        assertThat(Agento11yEnvConfig.parseAuthMode("none")).isEqualTo(AuthMode.NONE);
        assertThat(Agento11yEnvConfig.parseAuthMode("garbage")).isNull();
    }

    @Test
    void fluentSetTagsCopiesInput() {
        Map<String, String> source = new LinkedHashMap<>();
        source.put("k", "v");
        Agento11yClientConfig cfg = new Agento11yClientConfig().setTags(source);
        source.put("late", "edit");
        assertThat(cfg.getTags()).containsExactly(Map.entry("k", "v"));
    }

    private static Map<String, String> allFieldsEnv(String prefix) {
        Map<String, String> env = new LinkedHashMap<>();
        env.put(prefix + "ENDPOINT", "https://env:4318");
        env.put(prefix + "PROTOCOL", "http");
        env.put(prefix + "INSECURE", "true");
        env.put(prefix + "HEADERS", "X-A=1,X-B=two");
        env.put(prefix + "AUTH_MODE", "basic");
        env.put(prefix + "AUTH_TENANT_ID", "42");
        env.put(prefix + "AUTH_TOKEN", "glc_xxx");
        env.put(prefix + "AGENT_NAME", "planner");
        env.put(prefix + "AGENT_VERSION", "1.2.3");
        env.put(prefix + "USER_ID", "alice@example.com");
        env.put(prefix + "TAGS", "service=orchestrator,env=prod");
        env.put(prefix + "CONTENT_CAPTURE_MODE", "metadata_only");
        env.put(prefix + "DEBUG", "true");
        return env;
    }

    @Test
    void preferredOnlyMatchesLegacyOnly() {
        Agento11yClientConfig preferred = resolve(allFieldsEnv("AGENTO11Y_")).config();
        Agento11yClientConfig legacy = resolve(allFieldsEnv("SIGIL_")).config();

        GenerationExportConfig pe = preferred.getGenerationExport();
        GenerationExportConfig le = legacy.getGenerationExport();
        assertThat(pe.getEndpoint()).isEqualTo(le.getEndpoint()).isEqualTo("https://env:4318");
        assertThat(pe.getProtocol()).isEqualTo(le.getProtocol()).isEqualTo(GenerationExportProtocol.HTTP);
        assertThat(pe.getInsecure()).isEqualTo(le.getInsecure()).isTrue();
        assertThat(pe.getHeaders()).isEqualTo(le.getHeaders()).containsEntry("X-A", "1");

        AuthConfig pa = pe.getAuth();
        AuthConfig la = le.getAuth();
        assertThat(pa.getMode()).isEqualTo(la.getMode()).isEqualTo(AuthMode.BASIC);
        assertThat(pa.getTenantId()).isEqualTo(la.getTenantId()).isEqualTo("42");
        assertThat(pa.getBasicUser()).isEqualTo(la.getBasicUser()).isEqualTo("42");
        assertThat(pa.getBasicPassword()).isEqualTo(la.getBasicPassword()).isEqualTo("glc_xxx");

        assertThat(preferred.getAgentName()).isEqualTo(legacy.getAgentName()).isEqualTo("planner");
        assertThat(preferred.getAgentVersion()).isEqualTo(legacy.getAgentVersion()).isEqualTo("1.2.3");
        assertThat(preferred.getUserId()).isEqualTo(legacy.getUserId()).isEqualTo("alice@example.com");
        assertThat(preferred.getTags()).isEqualTo(legacy.getTags()).containsEntry("service", "orchestrator");
        assertThat(preferred.getContentCapture()).isEqualTo(legacy.getContentCapture())
                .isEqualTo(ContentCaptureMode.METADATA_ONLY);
        assertThat(preferred.getDebug()).isEqualTo(legacy.getDebug()).isTrue();
    }

    @Test
    void preferredWinsOverLegacyOnConflict() {
        Map<String, String> env = Map.of(
                "AGENTO11Y_ENDPOINT", "preferred.example:4318",
                "SIGIL_ENDPOINT", "legacy.example:4318");
        Agento11yClientConfig cfg = resolve(env).config();
        assertThat(cfg.getGenerationExport().getEndpoint()).isEqualTo("preferred.example:4318");
    }

    @Test
    void blankPreferredFallsThroughToLegacy() {
        Map<String, String> env = Map.of(
                "AGENTO11Y_ENDPOINT", "   ",
                "SIGIL_ENDPOINT", "legacy.example:4318");
        Agento11yClientConfig cfg = resolve(env).config();
        assertThat(cfg.getGenerationExport().getEndpoint()).isEqualTo("legacy.example:4318");
    }

    @Test
    void invalidPreferredContentCaptureModeBlocksValidLegacy() {
        Map<String, String> env = Map.of(
                "AGENTO11Y_CONTENT_CAPTURE_MODE", "bogus",
                "SIGIL_CONTENT_CAPTURE_MODE", "metadata_only");
        EnvResolveResult result = resolve(env);
        assertThat(result.config().getContentCapture()).isEqualTo(ContentCaptureMode.DEFAULT);
        assertThat(result.warnings()).anySatisfy(w -> assertThat(w).contains("AGENTO11Y_CONTENT_CAPTURE_MODE"));
    }

    @Test
    void invalidPreferredAuthModeBlocksValidLegacy() {
        Map<String, String> env = Map.of(
                "AGENTO11Y_AUTH_MODE", "garbage",
                "SIGIL_AUTH_MODE", "bearer");
        EnvResolveResult result = resolve(env);
        assertThat(result.config().getGenerationExport().getAuth().getMode()).isEqualTo(AuthMode.NONE);
        assertThat(result.warnings()).anySatisfy(w -> assertThat(w).contains("AGENTO11Y_AUTH_MODE"));
    }

    @Test
    void callerEndpointBeatsBothPrefixes() {
        Agento11yClientConfig base = new Agento11yClientConfig();
        base.getGenerationExport().setEndpoint("https://caller-host");
        Map<String, String> env = Map.of(
                "AGENTO11Y_ENDPOINT", "https://preferred-host",
                "SIGIL_ENDPOINT", "https://legacy-host");
        Agento11yClientConfig cfg = Agento11yEnvConfig.resolveFromEnv(env::get, base).config();
        assertThat(cfg.getGenerationExport().getEndpoint()).isEqualTo("https://caller-host");
    }

    @Test
    void mixedPrefixAuthResolvesPerField() {
        Map<String, String> env = Map.of(
                "AGENTO11Y_AUTH_MODE", "basic",
                "SIGIL_AUTH_TENANT_ID", "42",
                "SIGIL_AUTH_TOKEN", "glc_xxx");
        AuthConfig auth = resolve(env).config().getGenerationExport().getAuth();
        assertThat(auth.getMode()).isEqualTo(AuthMode.BASIC);
        assertThat(auth.getTenantId()).isEqualTo("42");
        assertThat(auth.getBasicUser()).isEqualTo("42");
        assertThat(auth.getBasicPassword()).isEqualTo("glc_xxx");
    }

    @Test
    void preferredTagsReplaceLegacyTagsWithoutMerging() {
        Map<String, String> env = Map.of(
                "AGENTO11Y_TAGS", "team=ai",
                "SIGIL_TAGS", "service=orch,env=prod");
        Agento11yClientConfig cfg = resolve(env).config();
        assertThat(cfg.getTags()).containsExactlyEntriesOf(Map.of("team", "ai"));
    }

    @Test
    void legacyConstantsKeepAgento11yNames() {
        assertThat(Agento11yEnvConfig.ENV_ENDPOINT).isEqualTo("SIGIL_ENDPOINT");
        assertThat(Agento11yEnvConfig.ENV_PROTOCOL).isEqualTo("SIGIL_PROTOCOL");
        assertThat(Agento11yEnvConfig.ENV_INSECURE).isEqualTo("SIGIL_INSECURE");
        assertThat(Agento11yEnvConfig.ENV_HEADERS).isEqualTo("SIGIL_HEADERS");
        assertThat(Agento11yEnvConfig.ENV_AUTH_MODE).isEqualTo("SIGIL_AUTH_MODE");
        assertThat(Agento11yEnvConfig.ENV_AUTH_TENANT_ID).isEqualTo("SIGIL_AUTH_TENANT_ID");
        assertThat(Agento11yEnvConfig.ENV_AUTH_TOKEN).isEqualTo("SIGIL_AUTH_TOKEN");
        assertThat(Agento11yEnvConfig.ENV_AGENT_NAME).isEqualTo("SIGIL_AGENT_NAME");
        assertThat(Agento11yEnvConfig.ENV_AGENT_VERSION).isEqualTo("SIGIL_AGENT_VERSION");
        assertThat(Agento11yEnvConfig.ENV_USER_ID).isEqualTo("SIGIL_USER_ID");
        assertThat(Agento11yEnvConfig.ENV_TAGS).isEqualTo("SIGIL_TAGS");
        assertThat(Agento11yEnvConfig.ENV_CONTENT_CAPTURE_MODE).isEqualTo("SIGIL_CONTENT_CAPTURE_MODE");
        assertThat(Agento11yEnvConfig.ENV_DEBUG).isEqualTo("SIGIL_DEBUG");
    }

    @Test
    void preferredConstantsUseAgentO11yNames() {
        assertThat(Agento11yEnvConfig.ENV_ENDPOINT_PREFERRED).isEqualTo("AGENTO11Y_ENDPOINT");
        assertThat(Agento11yEnvConfig.ENV_PROTOCOL_PREFERRED).isEqualTo("AGENTO11Y_PROTOCOL");
        assertThat(Agento11yEnvConfig.ENV_INSECURE_PREFERRED).isEqualTo("AGENTO11Y_INSECURE");
        assertThat(Agento11yEnvConfig.ENV_HEADERS_PREFERRED).isEqualTo("AGENTO11Y_HEADERS");
        assertThat(Agento11yEnvConfig.ENV_AUTH_MODE_PREFERRED).isEqualTo("AGENTO11Y_AUTH_MODE");
        assertThat(Agento11yEnvConfig.ENV_AUTH_TENANT_ID_PREFERRED).isEqualTo("AGENTO11Y_AUTH_TENANT_ID");
        assertThat(Agento11yEnvConfig.ENV_AUTH_TOKEN_PREFERRED).isEqualTo("AGENTO11Y_AUTH_TOKEN");
        assertThat(Agento11yEnvConfig.ENV_AGENT_NAME_PREFERRED).isEqualTo("AGENTO11Y_AGENT_NAME");
        assertThat(Agento11yEnvConfig.ENV_AGENT_VERSION_PREFERRED).isEqualTo("AGENTO11Y_AGENT_VERSION");
        assertThat(Agento11yEnvConfig.ENV_USER_ID_PREFERRED).isEqualTo("AGENTO11Y_USER_ID");
        assertThat(Agento11yEnvConfig.ENV_TAGS_PREFERRED).isEqualTo("AGENTO11Y_TAGS");
        assertThat(Agento11yEnvConfig.ENV_CONTENT_CAPTURE_MODE_PREFERRED).isEqualTo("AGENTO11Y_CONTENT_CAPTURE_MODE");
        assertThat(Agento11yEnvConfig.ENV_DEBUG_PREFERRED).isEqualTo("AGENTO11Y_DEBUG");
    }
}
