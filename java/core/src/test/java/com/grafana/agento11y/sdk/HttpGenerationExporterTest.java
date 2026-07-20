package com.grafana.agento11y.sdk;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.Arguments;
import org.junit.jupiter.params.provider.MethodSource;
import org.junit.jupiter.api.Test;

import java.util.stream.Stream;

class HttpGenerationExporterTest {

    static Stream<Arguments> normalizeCases() {
        return Stream.of(
                Arguments.of(
                        "missing path appends default ingest path",
                        "http://localhost:8080",
                        "http://localhost:8080/api/v1/generations:export"),
                Arguments.of(
                        "trailing slash treated as missing path",
                        "http://localhost:8080/",
                        "http://localhost:8080/api/v1/generations:export"),
                Arguments.of(
                        "explicit ingest path is preserved",
                        "http://localhost:8080/api/v1/generations:export",
                        "http://localhost:8080/api/v1/generations:export"),
                Arguments.of(
                        "custom path is preserved",
                        "http://localhost:8080/custom/ingest",
                        "http://localhost:8080/custom/ingest"),
                Arguments.of(
                        "no scheme defaults to http and appends path",
                        "localhost:8080",
                        "http://localhost:8080/api/v1/generations:export"),
                Arguments.of(
                        "https with no path appends default ingest path",
                        "https://stack.grafana.net",
                        "https://stack.grafana.net/api/v1/generations:export"),
                Arguments.of(
                        "uppercase scheme normalized to lowercase",
                        "HTTPS://stack.grafana.net",
                        "https://stack.grafana.net/api/v1/generations:export"),
                Arguments.of(
                        "query string preserved when path appended",
                        "http://localhost:8080?token=abc",
                        "http://localhost:8080/api/v1/generations:export?token=abc"),
                Arguments.of(
                        "encoded query string preserved when path appended",
                        "http://localhost:8080?token=a%26b",
                        "http://localhost:8080/api/v1/generations:export?token=a%26b"),
                Arguments.of(
                        "encoded custom path and query preserved",
                        "http://localhost:8080/custom%2Fingest?token=a%26b#frag%2Fment",
                        "http://localhost:8080/custom%2Fingest?token=a%26b#frag%2Fment"),
                Arguments.of(
                        "fragment preserved when path appended",
                        "http://localhost:8080#section",
                        "http://localhost:8080/api/v1/generations:export#section"),
                Arguments.of(
                        "encoded fragment preserved when path appended",
                        "http://localhost:8080#frag%2Fment",
                        "http://localhost:8080/api/v1/generations:export#frag%2Fment"));
    }

    @ParameterizedTest(name = "{0}")
    @MethodSource("normalizeCases")
    void normalizeEndpoint(String name, String input, String want) {
        assertThat(HttpGenerationExporter.normalizeEndpoint(input)).isEqualTo(want);
    }

    @Test
    void emptyInputThrows() {
        assertThatThrownBy(() -> HttpGenerationExporter.normalizeEndpoint(""))
                .isInstanceOf(IllegalArgumentException.class)
                .hasMessageContaining("endpoint is required");
        assertThatThrownBy(() -> HttpGenerationExporter.normalizeEndpoint("   "))
                .isInstanceOf(IllegalArgumentException.class)
                .hasMessageContaining("endpoint is required");
        assertThatThrownBy(() -> HttpGenerationExporter.normalizeEndpoint(null))
                .isInstanceOf(IllegalArgumentException.class)
                .hasMessageContaining("endpoint is required");
    }
}
