package com.grafana.agento11y.sdk;

import org.junit.jupiter.api.Test;
import java.util.List;
import java.util.Map;
import static org.junit.jupiter.api.Assertions.*;

class ModalityTokenCountsTest {
    @Test void preservesThinkingAndCompleteness() {
        var partition = ModalityTokenCounts.fromProvider(List.of(Map.of("modality", "IMAGE", "tokenCount", 10)), 12, 2, false);
        assertEquals(Map.of("image", 10L, "text", 2L), partition.tokens());
        assertTrue(partition.complete());
        assertNull(ModalityTokenCounts.fromProvider(null, 0, 0, false));
        assertFalse(ModalityTokenCounts.fromProvider(Map.of("image_tokens", 9), 10, 0, true).complete());
    }
}
