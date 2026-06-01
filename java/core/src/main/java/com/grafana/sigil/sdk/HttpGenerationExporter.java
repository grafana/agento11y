package com.grafana.sigil.sdk;

import com.fasterxml.jackson.databind.JsonNode;
import com.google.protobuf.util.JsonFormat;
import java.net.URI;
import java.net.URISyntaxException;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.time.Duration;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;

/** HTTP exporter for generation ingest parity endpoint. */
public final class HttpGenerationExporter implements GenerationExporter {
    private final URI endpoint;
    private final String userAgent;
    private final Map<String, String> headers;
    private final HttpClient client;

    public HttpGenerationExporter(String endpoint, Map<String, String> headers) {
        this.endpoint = URI.create(normalizeEndpoint(endpoint));
        // Resolve the User-Agent like the gRPC exporter: a non-blank caller
        // override wins, otherwise the SDK default (HttpClient would otherwise
        // send "Java-http-client/<ver>"). Any User-Agent entry is stripped so a
        // blank value can't override the resolved one, and header() can't append
        // a duplicate.
        String resolvedUserAgent = SdkVersion.userAgent();
        Map<String, String> remaining = new LinkedHashMap<>();
        if (headers != null) {
            for (Map.Entry<String, String> entry : headers.entrySet()) {
                if (entry.getKey().equalsIgnoreCase("User-Agent")) {
                    if (entry.getValue() != null && !entry.getValue().isBlank()) {
                        resolvedUserAgent = entry.getValue();
                    }
                    continue;
                }
                remaining.put(entry.getKey(), entry.getValue());
            }
        }
        this.userAgent = resolvedUserAgent;
        this.headers = remaining;
        this.client = HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(10)).build();
    }

    @Override
    public ExportGenerationsResponse exportGenerations(ExportGenerationsRequest request) throws Exception {
        String body = JsonFormat.printer()
                .preservingProtoFieldNames()
                .print(ProtoMapper.toProtoRequest(request));

        HttpRequest.Builder requestBuilder = HttpRequest.newBuilder()
                .uri(endpoint)
                .timeout(Duration.ofSeconds(10))
                .header("Content-Type", "application/json")
                .POST(HttpRequest.BodyPublishers.ofString(body));
        requestBuilder.header("User-Agent", userAgent);
        for (Map.Entry<String, String> entry : headers.entrySet()) {
            requestBuilder.header(entry.getKey(), entry.getValue());
        }

        HttpResponse<String> response = client.send(requestBuilder.build(), HttpResponse.BodyHandlers.ofString());
        if (response.statusCode() < 200 || response.statusCode() >= 300) {
            throw new RuntimeException("http generation export status " + response.statusCode() + ": " + response.body().trim());
        }

        JsonNode payload = Json.MAPPER.readTree(response.body());
        JsonNode resultsNode = payload.path("results");
        if (!resultsNode.isArray()) {
            throw new RuntimeException("invalid generation export response payload");
        }

        List<ExportGenerationResult> results = new ArrayList<>();
        for (int i = 0; i < resultsNode.size(); i++) {
            JsonNode item = resultsNode.get(i);
            String generationId = firstNonBlank(
                    item.path("generation_id").asText(""),
                    item.path("generationId").asText(""),
                    i < request.getGenerations().size() ? request.getGenerations().get(i).getId() : "");
            results.add(new ExportGenerationResult()
                    .setGenerationId(generationId)
                    .setAccepted(item.path("accepted").asBoolean(false))
                    .setError(item.path("error").asText("")));
        }

        return new ExportGenerationsResponse().setResults(results);
    }

    private static final String HTTP_GENERATION_EXPORT_PATH = "/api/v1/generations:export";

    static String normalizeEndpoint(String endpoint) {
        String trimmed = endpoint == null ? "" : endpoint.trim();
        if (trimmed.isEmpty()) {
            throw new IllegalArgumentException("generation export endpoint is required");
        }
        boolean hasScheme = trimmed.regionMatches(true, 0, "http://", 0, 7)
                || trimmed.regionMatches(true, 0, "https://", 0, 8);
        String withScheme = hasScheme ? trimmed : "http://" + trimmed;
        URI uri;
        try {
            uri = new URI(withScheme);
        } catch (URISyntaxException e) {
            throw new IllegalArgumentException(
                    "parse generation export endpoint \"" + endpoint + "\": " + e.getMessage(), e);
        }
        if (uri.getHost() == null || uri.getHost().isEmpty()) {
            throw new IllegalArgumentException(
                    "generation export endpoint \"" + endpoint + "\" has empty host");
        }
        String path = uri.getRawPath();
        if (path != null && !path.isEmpty() && !path.equals("/")) {
            return withLowercaseScheme(uri);
        }

        StringBuilder builder = new StringBuilder();
        builder.append(uri.getScheme().toLowerCase(Locale.ROOT))
                .append("://")
                .append(uri.getRawAuthority())
                .append(HTTP_GENERATION_EXPORT_PATH);
        if (uri.getRawQuery() != null) {
            builder.append('?').append(uri.getRawQuery());
        }
        if (uri.getRawFragment() != null) {
            builder.append('#').append(uri.getRawFragment());
        }
        return builder.toString();
    }

    private static String withLowercaseScheme(URI uri) {
        String ascii = uri.toASCIIString();
        return uri.getScheme().toLowerCase(Locale.ROOT) + ascii.substring(uri.getScheme().length());
    }

    private static String firstNonBlank(String... values) {
        for (String value : values) {
            if (value != null && !value.isBlank()) {
                return value;
            }
        }
        return "";
    }
}
