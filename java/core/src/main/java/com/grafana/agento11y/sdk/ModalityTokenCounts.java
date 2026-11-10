package com.grafana.agento11y.sdk;

import java.util.LinkedHashMap;
import java.util.Locale;
import java.util.Map;

/** A provider-reported partition; absent entries mean zero only if complete. */
public record ModalityTokenCounts(Map<String, Long> tokens, boolean complete) {
    public ModalityTokenCounts { tokens = Map.copyOf(tokens); }

    public static ModalityTokenCounts fromProvider(Object raw, long total, long thinking, boolean openai) {
        if (raw == null) { return null; }
        Map<String, Long> tokens = new LinkedHashMap<>();
        if (openai && raw instanceof Map<?, ?> details) {
            for (String modality : new String[]{"text", "image", "audio", "video"}) {
                if (details.get(modality + "_tokens") instanceof Number count) {
                    tokens.put(modality, count.longValue());
                }
            }
        } else if (raw instanceof Iterable<?> rows) {
            for (Object row : rows) {
                if (row instanceof Map<?, ?> detail) {
                    Object count = detail.containsKey("tokenCount") ? detail.get("tokenCount") :
                            detail.containsKey("token_count") ? detail.get("token_count") : detail.get("tokens");
                    String modality = String.valueOf(detail.get("modality")).toLowerCase(Locale.ROOT);
                    if (count instanceof Number n) { tokens.merge(modality, n.longValue(), Long::sum); }
                }
            }
        }
        if (thinking != 0) { tokens.merge("text", thinking, Long::sum); }
        long sum = tokens.values().stream().mapToLong(Long::longValue).sum();
        return new ModalityTokenCounts(tokens, sum == total);
    }
}
