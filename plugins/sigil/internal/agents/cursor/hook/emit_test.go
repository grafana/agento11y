package hook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/grafana/sigil-sdk/go/sigil"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/cursor/config"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/cursor/fragment"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/cursor/mapper"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/sigilemit"
)

// buildClient passes the cursor User-Agent token to sigilemit; this guards the
// wiring end to end through a real export request.
func TestEmitGenerationSendsCursorUserAgent(t *testing.T) {
	var gotUA string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		var request struct {
			Generations []struct {
				ID string `json:"id"`
			} `json:"generations"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		results := make([]map[string]any, 0, len(request.Generations))
		for _, g := range request.Generations {
			results = append(results, map[string]any{"generation_id": g.ID, "accepted": true})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	defer server.Close()
	t.Setenv("SIGIL_ENDPOINT", server.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")

	frag := &fragment.Fragment{ConversationID: "conv", GenerationID: "gen-1", Model: "gpt-5"}
	mapped := mapper.MapFragment(mapper.Inputs{
		Fragment:       frag,
		Stop:           &mapper.StopInput{Status: "completed"},
		ContentCapture: sigil.ContentCaptureModeMetadataOnly,
		Now:            time.Now(),
	})

	ctx := context.Background()
	client := buildClient(config.Config{ContentCapture: sigil.ContentCaptureModeMetadataOnly}, nil)
	if err := emitGeneration(ctx, client, frag, mapped, nil); err != nil {
		t.Fatalf("emitGeneration: %v", err)
	}
	if err := client.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	_ = client.Shutdown(ctx)

	if !strings.HasPrefix(gotUA, "sigil-plugin-cursor/") {
		t.Fatalf("User-Agent = %q, want sigil-plugin-cursor/ prefix", gotUA)
	}
}

// Two tool records with different completedAt timestamps must produce
// distinct, non-overlapping windows so the UI can show the real
// CALL→TOOL→CALL→TOOL interleaving instead of stacking spans at the end.
// The window math lives in sigilemit.ToolSpanWindow; this guards cursor's
// reliance on it.
func TestToolSpanWindow_PreservesInterleaving(t *testing.T) {
	genEnd := time.Date(2026, 4, 28, 12, 0, 30, 0, time.UTC)
	dur := func(ms float64) *float64 { return &ms }

	_, firstEnd := sigilemit.ToolSpanWindow("2026-04-28T12:00:05Z", dur(1000), genEnd)
	secondStart, _ := sigilemit.ToolSpanWindow("2026-04-28T12:00:20Z", dur(1000), genEnd)
	if !firstEnd.Before(secondStart) {
		t.Errorf("first.completedAt (%s) should precede second.startedAt (%s)", firstEnd, secondStart)
	}
}
