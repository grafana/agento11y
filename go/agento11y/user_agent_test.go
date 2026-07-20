package agento11y

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agento11yv1 "github.com/grafana/agento11y/go/proto/agento11y/v1"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestSDKExportSendsDefaultUserAgent(t *testing.T) {
	want := "agento11y-sdk-go/" + Version

	t.Run("http", func(t *testing.T) {
		if got := exportAndCaptureHTTPUserAgent(t, nil); got != want {
			t.Fatalf("http User-Agent = %q, want %q", got, want)
		}
	})

	t.Run("grpc", func(t *testing.T) {
		// grpc-go appends its own token (grpc-go/<ver>) after ours.
		got := exportAndCaptureGRPCUserAgent(t, nil)
		if first := firstToken(got); first != want {
			t.Fatalf("grpc first token = %q, want %q (full %q)", first, want, got)
		}
	})
}

func TestSDKExportUserAgentOverride(t *testing.T) {
	defaultUA := "agento11y-sdk-go/" + Version
	override := "agento11y-plugin-claude-code/1.2.3 " + defaultUA

	// A non-blank caller User-Agent wins; a blank or whitespace-only one must
	// not blank out the default. HTTP and gRPC must agree.
	cases := []struct {
		name   string
		header string
		wantUA string
	}{
		{name: "non-blank", header: override, wantUA: override},
		{name: "empty", header: "", wantUA: defaultUA},
		{name: "whitespace", header: "   ", wantUA: defaultUA},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{"User-Agent": tc.header}

			t.Run("http", func(t *testing.T) {
				if got := exportAndCaptureHTTPUserAgent(t, headers); got != tc.wantUA {
					t.Fatalf("http User-Agent = %q, want %q", got, tc.wantUA)
				}
			})

			t.Run("grpc", func(t *testing.T) {
				// grpc-go appends its own token after ours, so compare first tokens.
				got := exportAndCaptureGRPCUserAgent(t, headers)
				if first, want := firstToken(got), firstToken(tc.wantUA); first != want {
					t.Fatalf("grpc first token = %q, want %q (full %q)", first, want, got)
				}
			})
		})
	}
}

func userAgentTestConfig(headers map[string]string) Config {
	return Config{
		Tracer: noop.NewTracerProvider().Tracer("test"),
		GenerationExport: GenerationExportConfig{
			Headers:        headers,
			BatchSize:      1,
			QueueSize:      10,
			FlushInterval:  time.Second,
			MaxRetries:     1,
			InitialBackoff: time.Millisecond,
			MaxBackoff:     10 * time.Millisecond,
		},
	}
}

func runOneExport(t *testing.T, client *Client) {
	t.Helper()

	_, rec := client.StartGeneration(context.Background(), GenerationStart{
		Model: ModelRef{Provider: "openai", Name: "gpt-5"},
	})
	rec.SetResult(Generation{
		Input:  []Message{UserTextMessage("hello")},
		Output: []Message{AssistantTextMessage("hi")},
	}, nil)
	rec.End()
	if err := rec.Err(); err != nil {
		t.Fatalf("recorder error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func exportAndCaptureHTTPUserAgent(t *testing.T, headers map[string]string) string {
	t.Helper()

	captured := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case captured <- r.Header.Get("User-Agent"):
		default:
		}
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		request := &agento11yv1.ExportGenerationsRequest{}
		if err := protojson.Unmarshal(payload, request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		response := acceptanceResponse(request)
		encoded, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(response)
		if err != nil {
			http.Error(w, "marshal response", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(encoded)
	}))
	t.Cleanup(srv.Close)

	cfg := userAgentTestConfig(headers)
	cfg.GenerationExport.Protocol = GenerationExportProtocolHTTP
	cfg.GenerationExport.Endpoint = srv.URL + "/api/v1/generations:export"
	runOneExport(t, NewClient(cfg))

	select {
	case ua := <-captured:
		return ua
	default:
		t.Fatal("no export request reached the server")
		return ""
	}
}

func exportAndCaptureGRPCUserAgent(t *testing.T, headers map[string]string) string {
	t.Helper()

	ingest := &capturingIngestServer{}
	grpcServer := grpc.NewServer()
	agento11yv1.RegisterGenerationIngestServiceServer(grpcServer, ingest)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen grpc: %v", err)
	}
	go func() {
		_ = grpcServer.Serve(listener)
	}()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = listener.Close()
	})

	cfg := userAgentTestConfig(headers)
	cfg.GenerationExport.Protocol = GenerationExportProtocolGRPC
	cfg.GenerationExport.Endpoint = listener.Addr().String()
	cfg.GenerationExport.Insecure = BoolPtr(true)
	runOneExport(t, NewClient(cfg))

	return ingest.singleUserAgent(t)
}

func firstToken(userAgent string) string {
	token, _, _ := strings.Cut(userAgent, " ")
	return token
}
