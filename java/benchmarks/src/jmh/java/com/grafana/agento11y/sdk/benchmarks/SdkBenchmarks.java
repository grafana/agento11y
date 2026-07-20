package com.grafana.agento11y.sdk.benchmarks;

import com.grafana.agento11y.sdk.ExportGenerationResult;
import com.grafana.agento11y.sdk.ExportGenerationsRequest;
import com.grafana.agento11y.sdk.ExportGenerationsResponse;
import com.grafana.agento11y.sdk.GenerationExportConfig;
import com.grafana.agento11y.sdk.GenerationExporter;
import com.grafana.agento11y.sdk.GenerationStart;
import com.grafana.agento11y.sdk.Message;
import com.grafana.agento11y.sdk.MessagePart;
import com.grafana.agento11y.sdk.MessageRole;
import com.grafana.agento11y.sdk.ModelRef;
import com.grafana.agento11y.sdk.Agento11yClient;
import com.grafana.agento11y.sdk.Agento11yClientConfig;
import com.grafana.agento11y.sdk.providers.openai.OpenAiChatCompletions;
import com.grafana.agento11y.sdk.providers.openai.OpenAiOptions;
import com.openai.core.ObjectMappers;
import com.openai.models.chat.completions.ChatCompletion;
import com.openai.models.chat.completions.ChatCompletionCreateParams;
import io.opentelemetry.api.GlobalOpenTelemetry;
import java.io.IOException;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.TimeUnit;
import org.openjdk.jmh.annotations.Benchmark;
import org.openjdk.jmh.annotations.BenchmarkMode;
import org.openjdk.jmh.annotations.Level;
import org.openjdk.jmh.annotations.Mode;
import org.openjdk.jmh.annotations.OutputTimeUnit;
import org.openjdk.jmh.annotations.Scope;
import org.openjdk.jmh.annotations.Setup;
import org.openjdk.jmh.annotations.State;

@State(Scope.Benchmark)
public class SdkBenchmarks {
    private ChatCompletionCreateParams request;
    private ChatCompletion response;
    private Agento11yClient client;

    @Setup(Level.Trial)
    public void setup() throws IOException {
        request = ChatCompletionCreateParams.builder()
                .model("gpt-5")
                .addUserMessage("hello")
                .maxCompletionTokens(128L)
                .build();
        response = ObjectMappers.jsonMapper().readValue(
                """
                {
                  "id": "chatcmpl_1",
                  "choices": [
                    {
                      "finish_reason": "stop",
                      "index": 0,
                      "message": {
                        "role": "assistant",
                        "content": "hello"
                      }
                    }
                  ],
                  "created": 1,
                  "model": "gpt-5"
                }
                """,
                ChatCompletion.class);

        client = new Agento11yClient(new Agento11yClientConfig()
                .setTracer(GlobalOpenTelemetry.getTracer("bench"))
                .setGenerationExporter(new NoopExporter())
                .setGenerationExport(new GenerationExportConfig()
                        .setBatchSize(1)
                        .setQueueSize(10000)
                        .setFlushInterval(Duration.ofMinutes(10))
                        .setMaxRetries(0)));
    }

    @Benchmark
    @BenchmarkMode(Mode.Throughput)
    @OutputTimeUnit(TimeUnit.SECONDS)
    public Object mapOpenAiSync() {
        return OpenAiChatCompletions.fromRequestResponse(request, response, new OpenAiOptions());
    }

    @Benchmark
    @BenchmarkMode(Mode.Throughput)
    @OutputTimeUnit(TimeUnit.SECONDS)
    public void recordGenerationHotPath() {
        GenerationStart start = new GenerationStart()
                .setModel(new ModelRef().setProvider("openai").setName("gpt-5"))
                .setOperationName("generateText");
        var recorder = client.startGeneration(start);
        recorder.setResult(new com.grafana.agento11y.sdk.GenerationResult().setOutput(List.of(
                new Message().setRole(MessageRole.ASSISTANT).setParts(List.of(MessagePart.text("hello"))))));
        recorder.end();
    }

    private static final class NoopExporter implements GenerationExporter {
        @Override
        public ExportGenerationsResponse exportGenerations(ExportGenerationsRequest request) {
            List<ExportGenerationResult> results = new ArrayList<>();
            request.getGenerations().forEach(g -> results.add(new ExportGenerationResult().setGenerationId(g.getId()).setAccepted(true)));
            return new ExportGenerationsResponse().setResults(results);
        }
    }
}
