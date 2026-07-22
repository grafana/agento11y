package com.grafana.agento11y.sdk;

import static org.assertj.core.api.Assertions.assertThat;

import org.junit.jupiter.api.Test;

class SdkVersionTest {
    @Test
    void versionMatchesRuntimePackageMetadataOrFallback() {
        String implementationVersion = SdkVersion.class.getPackage().getImplementationVersion();
        if (implementationVersion == null || implementationVersion.isBlank()) {
            // Tests run from the classes directory, so manifest metadata is
            // unavailable and the fallback applies.
            assertThat(SdkVersion.VERSION).isEqualTo("0.0.0+unknown");
        } else {
            assertThat(SdkVersion.VERSION).isEqualTo(implementationVersion);
        }
    }

    @Test
    void userAgentContainsProductTokenAndVersion() {
        assertThat(SdkVersion.VERSION).isNotBlank();
        assertThat(SdkVersion.userAgent()).isEqualTo("agento11y-sdk-java/" + SdkVersion.VERSION);
    }
}
