package emit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

func TestExportEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		want     string
	}{
		{"no trailing slash", "http://localhost:8080", "http://localhost:8080/api/v1/generations:export"},
		{"trailing slash trimmed", "http://localhost:8080/", "http://localhost:8080/api/v1/generations:export"},
		{"multiple trailing slashes trimmed", "http://localhost:8080///", "http://localhost:8080/api/v1/generations:export"},
		{"empty endpoint", "", "/api/v1/generations:export"},
		{"pasted full export URL", "http://localhost:8080/api/v1/generations:export", "http://localhost:8080/api/v1/generations:export"},
		{"pasted full export URL with trailing slash", "http://localhost:8080/api/v1/generations:export/", "http://localhost:8080/api/v1/generations:export"},
		{"pasted full export URL under a path prefix", "https://host.example/prefix/api/v1/generations:export", "https://host.example/prefix/api/v1/generations:export"},
		{"surrounding whitespace trimmed", "  http://localhost:8080  ", "http://localhost:8080/api/v1/generations:export"},
		{"path prefix kept", "https://host.example/prefix", "https://host.example/prefix/api/v1/generations:export"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SIGIL_ENDPOINT", tc.endpoint)
			if got := ExportEndpoint(); got != tc.want {
				t.Fatalf("ExportEndpoint() = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestExportConfig(t *testing.T) {
	t.Setenv("SIGIL_ENDPOINT", "http://localhost:8080")

	t.Run("empty user agent leaves headers unset", func(t *testing.T) {
		if got := exportConfig("").Headers; got != nil {
			t.Fatalf("Headers = %v; want nil", got)
		}
	})

	t.Run("user agent sets header", func(t *testing.T) {
		got := exportConfig("agento11y-plugin-codex/1.2.3").Headers["User-Agent"]
		if got != "agento11y-plugin-codex/1.2.3" {
			t.Fatalf("User-Agent = %q; want %q", got, "agento11y-plugin-codex/1.2.3")
		}
	})
}

func TestNewClientUsesSDKEnvResolution(t *testing.T) {
	t.Setenv("SIGIL_ENDPOINT", "http://localhost:8080")
	// The adapters leave basic-auth credentials to the SDK's env resolution; the
	// SDK validates that basic_password is present, so provide them.
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")
	client := NewClient(ClientOptions{InstrumentationName: "sigil.test"})
	if client == nil {
		t.Fatal("NewClient returned nil")
	}
	_ = client.Shutdown(t.Context())
}

// TestNewClientAppliesClientTags proves the ClientOptions.Tags wiring end to
// end: the tags an adapter passes reach agento11y.Config.Tags, so the SDK
// merges them into every generation export (and, with a meter configured,
// emits them as metric attributes).
func TestNewClientAppliesClientTags(t *testing.T) {
	type exported struct {
		Generations []struct {
			ID   string            `json:"id"`
			Tags map[string]string `json:"tags"`
		} `json:"generations"`
	}
	var got exported
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		if err := json.Unmarshal(body, &got); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		results := make([]map[string]any, 0, len(got.Generations))
		for _, g := range got.Generations {
			results = append(results, map[string]any{"generation_id": g.ID, "accepted": true})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	defer server.Close()
	// Pin every branded family blank first: the SDK prefers the AGENTO11Y_
	// spelling, so a machine with AGENTO11Y_ENDPOINT and AGENTO11Y_AUTH_TOKEN
	// exported would send this generation to that endpoint instead of the test
	// server.
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("SIGIL_ENDPOINT", server.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")

	client := NewClient(ClientOptions{
		InstrumentationName: "agento11y.test",
		Tags:                map[string]string{"user": "alice@example.com", "repo": "grafana/agento11y"},
	})
	if client == nil {
		t.Fatal("NewClient returned nil")
	}
	ctx := context.Background()
	if err := Record(ctx, client,
		agento11y.GenerationStart{Model: agento11y.ModelRef{Provider: "openai", Name: "gpt-5"}},
		agento11y.Generation{
			ID:             "gen-1",
			ConversationID: "conv-1",
			Model:          agento11y.ModelRef{Provider: "openai", Name: "gpt-5"},
			CompletedAt:    time.Now(),
		}, nil, nil); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := client.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	_ = client.Shutdown(ctx)

	if len(got.Generations) != 1 {
		t.Fatalf("exported %d generations; want 1", len(got.Generations))
	}
	tags := got.Generations[0].Tags
	if tags["user"] != "alice@example.com" || tags["repo"] != "grafana/agento11y" {
		t.Fatalf("exported tags = %v; want user and repo client tags", tags)
	}
}

func TestToolSpanWindow(t *testing.T) {
	genEnd := time.Date(2026, 4, 28, 12, 0, 30, 0, time.UTC)
	dur := func(ms float64) *float64 { return &ms }

	cases := []struct {
		name          string
		completedAt   string
		duration      *float64
		wantStarted   time.Time
		wantCompleted time.Time
	}{
		{
			name:          "completedAt minus duration",
			completedAt:   "2026-04-28T12:00:10.500Z",
			duration:      dur(2500),
			wantStarted:   time.Date(2026, 4, 28, 12, 0, 8, 0, time.UTC),
			wantCompleted: time.Date(2026, 4, 28, 12, 0, 10, 500_000_000, time.UTC),
		},
		{
			name:          "no duration → started equals completed",
			completedAt:   "2026-04-28T12:00:10Z",
			duration:      nil,
			wantStarted:   time.Date(2026, 4, 28, 12, 0, 10, 0, time.UTC),
			wantCompleted: time.Date(2026, 4, 28, 12, 0, 10, 0, time.UTC),
		},
		{
			name:          "missing completedAt falls back to genCompletedAt",
			completedAt:   "",
			duration:      dur(1000),
			wantStarted:   genEnd.Add(-1000 * time.Millisecond),
			wantCompleted: genEnd,
		},
		{
			name:          "unparseable completedAt falls back to genCompletedAt",
			completedAt:   "not-a-timestamp",
			duration:      dur(500),
			wantStarted:   genEnd.Add(-500 * time.Millisecond),
			wantCompleted: genEnd,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotStart, gotEnd := ToolSpanWindow(tc.completedAt, tc.duration, genEnd)
			if !gotStart.Equal(tc.wantStarted) {
				t.Errorf("startedAt = %s; want %s", gotStart, tc.wantStarted)
			}
			if !gotEnd.Equal(tc.wantCompleted) {
				t.Errorf("completedAt = %s; want %s", gotEnd, tc.wantCompleted)
			}
		})
	}
}

func TestToolError(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want string
	}{
		{"empty message → sentinel", "", "tool returned error"},
		{"non-empty message preserved", "boom", "boom"},
		{"whitespace message preserved unchanged", "   ", "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ToolError(tc.msg).Error(); got != tc.want {
				t.Errorf("ToolError(%q) = %q; want %q", tc.msg, got, tc.want)
			}
		})
	}
}
