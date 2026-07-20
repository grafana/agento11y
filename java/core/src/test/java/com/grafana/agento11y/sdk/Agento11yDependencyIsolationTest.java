package com.grafana.agento11y.sdk;

import static org.assertj.core.api.Assertions.assertThatThrownBy;

import org.junit.jupiter.api.Test;

class Agento11yDependencyIsolationTest {
    @Test
    void coreModuleDoesNotDependOnProviderAdapters() {
        assertThatThrownBy(() -> Class.forName("com.grafana.agento11y.sdk.providers.openai.OpenAiAdapter"))
                .isInstanceOf(ClassNotFoundException.class);
    }
}
