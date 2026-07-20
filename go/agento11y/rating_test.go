package agento11y

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace/noop"
)

func TestSubmitConversationRatingOverHTTP(t *testing.T) {
	var capturedPath string
	var capturedMethod string
	var capturedHeaders http.Header
	var capturedBody ConversationRatingInput

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		capturedPath = req.URL.Path
		capturedMethod = req.Method
		capturedHeaders = req.Header.Clone()

		if err := json.NewDecoder(req.Body).Decode(&capturedBody); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"rating":{
				"rating_id":"rat-1",
				"conversation_id":"conv-1",
				"rating":"CONVERSATION_RATING_VALUE_BAD",
				"created_at":"2026-02-13T12:00:00Z"
			},
			"summary":{
				"total_count":1,
				"good_count":0,
				"bad_count":1,
				"latest_rating":"CONVERSATION_RATING_VALUE_BAD",
				"latest_rated_at":"2026-02-13T12:00:00Z",
				"has_bad_rating":true
			}
		}`))
	}))
	defer server.Close()

	client := newRatingTestClient(t, ratingTestClientOptions{
		generationEndpoint: server.URL + "/api/v1/generations:export",
		apiEndpoint:        server.URL,
		auth: AuthConfig{
			Mode:     ExportAuthModeTenant,
			TenantID: "tenant-a",
		},
	})
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	response, err := client.SubmitConversationRating(context.Background(), "conv-1", ConversationRatingInput{
		RatingID: "rat-1",
		Rating:   ConversationRatingValueBad,
		Comment:  "wrong answer",
		Metadata: map[string]any{"channel": "assistant"},
	})
	if err != nil {
		t.Fatalf("submit rating: %v", err)
	}

	if capturedMethod != http.MethodPost {
		t.Fatalf("expected method POST, got %s", capturedMethod)
	}
	if capturedPath != "/api/v1/conversations/conv-1/ratings" {
		t.Fatalf("unexpected request path: %s", capturedPath)
	}
	if capturedHeaders.Get("X-Scope-OrgID") != "tenant-a" {
		t.Fatalf("expected tenant header, got %q", capturedHeaders.Get("X-Scope-OrgID"))
	}
	if capturedBody.RatingID != "rat-1" || capturedBody.Rating != ConversationRatingValueBad {
		t.Fatalf("unexpected request body: %#v", capturedBody)
	}
	if response == nil || response.Rating.RatingID != "rat-1" {
		t.Fatalf("unexpected response: %#v", response)
	}
	if !response.Summary.HasBadRating {
		t.Fatalf("expected has_bad_rating=true in summary")
	}
}

func TestSubmitConversationRatingMapsConflict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "idempotency conflict", http.StatusConflict)
	}))
	defer server.Close()

	client := newRatingTestClient(t, ratingTestClientOptions{
		generationEndpoint: server.URL + "/api/v1/generations:export",
		apiEndpoint:        server.URL,
		auth: AuthConfig{
			Mode:     ExportAuthModeTenant,
			TenantID: "tenant-a",
		},
	})
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	_, err := client.SubmitConversationRating(context.Background(), "conv-1", ConversationRatingInput{
		RatingID: "rat-1",
		Rating:   ConversationRatingValueGood,
	})
	if !errors.Is(err, ErrRatingConflict) {
		t.Fatalf("expected ErrRatingConflict, got %v", err)
	}
}

func TestSubmitConversationRatingValidation(t *testing.T) {
	client := newRatingTestClient(t, ratingTestClientOptions{
		generationEndpoint: "http://example.invalid/api/v1/generations:export",
		apiEndpoint:        "http://example.invalid",
	})
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	_, err := client.SubmitConversationRating(context.Background(), " ", ConversationRatingInput{
		RatingID: "rat-1",
		Rating:   ConversationRatingValueGood,
	})
	if !errors.Is(err, ErrRatingValidationFailed) {
		t.Fatalf("expected validation error for empty conversation id, got %v", err)
	}

	_, err = client.SubmitConversationRating(context.Background(), "conv-1", ConversationRatingInput{
		RatingID: "",
		Rating:   ConversationRatingValueGood,
	})
	if !errors.Is(err, ErrRatingValidationFailed) {
		t.Fatalf("expected validation error for empty rating id, got %v", err)
	}
}

func TestSubmitConversationRatingUsesBearerHeader(t *testing.T) {
	var authorizationHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		authorizationHeader = req.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rating":{"rating_id":"rat-1","conversation_id":"conv-1","rating":"CONVERSATION_RATING_VALUE_GOOD","created_at":"2026-02-13T12:00:00Z"},"summary":{"total_count":1,"good_count":1,"bad_count":0,"latest_rating":"CONVERSATION_RATING_VALUE_GOOD","latest_rated_at":"2026-02-13T12:00:00Z","has_bad_rating":false}}`))
	}))
	defer server.Close()

	client := newRatingTestClient(t, ratingTestClientOptions{
		generationEndpoint: strings.TrimPrefix(server.URL, "http://") + "/api/v1/generations:export",
		apiEndpoint:        strings.TrimPrefix(server.URL, "http://"),
		auth: AuthConfig{
			Mode:        ExportAuthModeBearer,
			BearerToken: "token-a",
		},
	})
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	_, err := client.SubmitConversationRating(context.Background(), "conv-1", ConversationRatingInput{
		RatingID: "rat-1",
		Rating:   ConversationRatingValueGood,
	})
	if err != nil {
		t.Fatalf("submit rating with bearer auth: %v", err)
	}
	if authorizationHeader != "Bearer token-a" {
		t.Fatalf("expected bearer authorization header, got %q", authorizationHeader)
	}
}

func TestSubmitConversationRatingUsesAPIEndpointWhenGenerationExportIsGRPC(t *testing.T) {
	var capturedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		capturedPath = req.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rating":{"rating_id":"rat-1","conversation_id":"conv-1","rating":"CONVERSATION_RATING_VALUE_GOOD","created_at":"2026-02-13T12:00:00Z"},"summary":{"total_count":1,"good_count":1,"bad_count":0,"latest_rating":"CONVERSATION_RATING_VALUE_GOOD","latest_rated_at":"2026-02-13T12:00:00Z","has_bad_rating":false}}`))
	}))
	defer server.Close()

	client := newRatingTestClient(t, ratingTestClientOptions{
		generationProtocol: GenerationExportProtocolGRPC,
		generationEndpoint: "localhost:4317",
		apiEndpoint:        server.URL,
		auth: AuthConfig{
			Mode:     ExportAuthModeTenant,
			TenantID: "tenant-a",
		},
	})
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	_, err := client.SubmitConversationRating(context.Background(), "conv-1", ConversationRatingInput{
		RatingID: "rat-1",
		Rating:   ConversationRatingValueGood,
	})
	if err != nil {
		t.Fatalf("submit rating: %v", err)
	}
	if capturedPath != "/api/v1/conversations/conv-1/ratings" {
		t.Fatalf("unexpected request path: %s", capturedPath)
	}
}

type ratingTestClientOptions struct {
	generationProtocol GenerationExportProtocol
	generationEndpoint string
	apiEndpoint        string
	auth               AuthConfig
}

func newRatingTestClient(t *testing.T, options ratingTestClientOptions) *Client {
	t.Helper()
	if options.generationProtocol == "" {
		options.generationProtocol = GenerationExportProtocolHTTP
	}

	return NewClient(Config{
		Tracer: noop.NewTracerProvider().Tracer("agento11y-go-rating-test"),
		GenerationExport: GenerationExportConfig{
			Protocol:        options.generationProtocol,
			Endpoint:        options.generationEndpoint,
			Auth:            options.auth,
			Insecure:        BoolPtr(true),
			BatchSize:       1,
			FlushInterval:   time.Hour,
			QueueSize:       1,
			MaxRetries:      1,
			InitialBackoff:  time.Millisecond,
			MaxBackoff:      time.Millisecond,
			PayloadMaxBytes: 1 << 20,
		},
		API: APIConfig{
			Endpoint: options.apiEndpoint,
		},
		testGenerationExporter: newNoopGenerationExporter(nil),
		testDisableWorker:      true,
	})
}
