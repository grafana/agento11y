package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	agento11yv1 "github.com/grafana/agento11y/go/proto/agento11y/v1"
	"github.com/grafana/agento11y/go/proto/agento11y/wire"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/opencode/sessiondb/sessiondbtest"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/protobuf/proto"
)

type capturedRecord struct {
	start agento11y.GenerationStart
	gen   agento11y.Generation
	err   error
}

// stubExporter builds an Exporter whose Record captures instead of exporting.
func stubExporter() (*Exporter, *[]capturedRecord) {
	var captured []capturedRecord
	e := &Exporter{
		Record: func(_ context.Context, start agento11y.GenerationStart, gen agento11y.Generation, callErr error) error {
			captured = append(captured, capturedRecord{start: start, gen: gen, err: callErr})
			return nil
		},
	}
	return e, &captured
}

func exportOne(t *testing.T, e *Exporter, g HistoricalGeneration) {
	t.Helper()
	if err := e.ExportHistoricalGeneration(context.Background(), g); err != nil {
		t.Fatalf("ExportHistoricalGeneration: %v", err)
	}
}

func TestExporterPreparesGeneration(t *testing.T) {
	e, captured := stubExporter()
	src := SourceRef{Agent: AgentClaudeCode, SessionID: "s1", SourcePath: "/a.jsonl", TurnID: "t1"}
	exportOne(t, e, HistoricalGeneration{Source: src})

	got := (*captured)[0].gen
	if got.ID != src.GenerationID() {
		t.Fatalf("ID = %q, want the deterministic %q", got.ID, src.GenerationID())
	}
	if got.AgentName != string(AgentClaudeCode) {
		t.Fatalf("AgentName = %q", got.AgentName)
	}
	if got.Mode != agento11y.GenerationModeSync || got.OperationName != "generateText" {
		t.Fatalf("mode/operation = %q/%q", got.Mode, got.OperationName)
	}
	if got.Model.Name != placeholderModelName || got.Model.Provider != string(AgentClaudeCode) {
		t.Fatalf("model = %+v", got.Model)
	}
	if (*captured)[0].start.ID != got.ID || !(*captured)[0].start.StartedAt.Equal(got.StartedAt) {
		t.Fatalf("recorder seed does not match the generation: %+v", (*captured)[0].start)
	}
}

func TestExporterKeepsExplicitIdentity(t *testing.T) {
	e, captured := stubExporter()
	exportOne(t, e, HistoricalGeneration{
		Source: SourceRef{Agent: AgentCodex, SessionID: "s1"},
		Gen: agento11y.Generation{
			ID:            "explicit-id",
			AgentName:     "codex/subagent",
			OperationName: "chat",
			Model:         agento11y.ModelRef{Provider: "openai", Name: "gpt-5"},
		},
	})
	got := (*captured)[0].gen
	if got.ID != "explicit-id" || got.AgentName != "codex/subagent" || got.OperationName != "chat" {
		t.Fatalf("prepare overwrote importer-supplied identity: %+v", got)
	}
	if got.Model.Name != "gpt-5" || got.Model.Provider != "openai" {
		t.Fatalf("model = %+v", got.Model)
	}
	if _, ok := got.Metadata[MetaMissingModel]; ok {
		t.Fatal("a turn with a model was flagged as missing one")
	}
}

func TestExporterStampsQualityMetadata(t *testing.T) {
	completed := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		gen        HistoricalGeneration
		wantKeys   []string
		absentKeys []string
	}{
		{
			name: "a complete turn is only marked backfilled",
			gen: HistoricalGeneration{
				Gen: agento11y.Generation{
					Model:       agento11y.ModelRef{Provider: "anthropic", Name: "claude"},
					Usage:       agento11y.TokenUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
					StartedAt:   completed.Add(-time.Second),
					CompletedAt: completed,
				},
			},
			wantKeys:   []string{MetaBackfill},
			absentKeys: []string{MetaApproximate, MetaApproxUsage, MetaMissingModel, MetaApproxStartedAt},
		},
		{
			name: "missing usage and model are flagged",
			gen: HistoricalGeneration{
				Gen: agento11y.Generation{StartedAt: completed, CompletedAt: completed},
			},
			wantKeys: []string{MetaBackfill, MetaApproximate, MetaApproxUsage, MetaMissingModel},
		},
		{
			name: "truncation reported by the sanitizer is carried through",
			gen: HistoricalGeneration{
				Gen: agento11y.Generation{
					Model:       agento11y.ModelRef{Provider: "anthropic", Name: "claude"},
					Usage:       agento11y.TokenUsage{TotalTokens: 1},
					StartedAt:   completed,
					CompletedAt: completed,
				},
				Quality: QualityReport{Truncated: true},
			},
			wantKeys: []string{MetaBackfill, MetaApproximate, MetaTruncated},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, captured := stubExporter()
			exportOne(t, e, tc.gen)
			meta := (*captured)[0].gen.Metadata
			for _, key := range tc.wantKeys {
				if meta[key] != true {
					t.Fatalf("metadata %q = %v, want true (all: %v)", key, meta[key], meta)
				}
			}
			for _, key := range tc.absentKeys {
				if _, ok := meta[key]; ok {
					t.Fatalf("metadata %q was set for a turn that does not need it", key)
				}
			}
		})
	}
}

func TestExporterMirrorsConversationTitle(t *testing.T) {
	e, captured := stubExporter()
	exportOne(t, e, HistoricalGeneration{
		Gen: agento11y.Generation{ConversationTitle: "  add a retry  "},
	})
	if got := (*captured)[0].gen.Metadata[MetaConversationTitle]; got != "add a retry" {
		t.Fatalf("metadata %q = %v, want the trimmed title", MetaConversationTitle, got)
	}
}

func TestExporterConversationTitleKeyIsNamespaced(t *testing.T) {
	if MetaConversationTitle != "agento11y.conversation.title" {
		t.Fatalf("MetaConversationTitle = %q; the viewer reads agento11y.conversation.title", MetaConversationTitle)
	}
	for _, key := range []string{
		MetaBackfill, MetaApproximate, MetaApproxStartedAt,
		MetaApproxCompleted, MetaApproxUsage, MetaMissingModel, MetaTruncated,
	} {
		if !strings.HasPrefix(key, "agento11y.import.") {
			t.Fatalf("import metadata key %q is not namespaced under agento11y.import.", key)
		}
	}
}

func TestExporterApplyTimestamps(t *testing.T) {
	known := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name          string
		started       time.Time
		completed     time.Time
		wantStarted   time.Time
		wantCompleted time.Time
	}{
		{
			name:        "both ends present are left alone",
			started:     known.Add(-time.Second),
			completed:   known,
			wantStarted: known.Add(-time.Second), wantCompleted: known,
		},
		{
			name:        "a missing start anchors to completion",
			completed:   known,
			wantStarted: known, wantCompleted: known,
		},
		{
			name:        "a missing completion anchors to the start",
			started:     known,
			wantStarted: known, wantCompleted: known,
		},
		{
			name: "both missing stays zero for the SDK to fill",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, captured := stubExporter()
			exportOne(t, e, HistoricalGeneration{
				Gen: agento11y.Generation{StartedAt: tc.started, CompletedAt: tc.completed},
			})
			got := (*captured)[0].gen
			if !got.StartedAt.Equal(tc.wantStarted) || !got.CompletedAt.Equal(tc.wantCompleted) {
				t.Fatalf("timestamps = (%v, %v), want (%v, %v)",
					got.StartedAt, got.CompletedAt, tc.wantStarted, tc.wantCompleted)
			}
			if got.CompletedAt.Before(got.StartedAt) {
				t.Fatal("span has a negative duration")
			}
		})
	}
}

func TestExporterSurfacesACallError(t *testing.T) {
	e, captured := stubExporter()
	exportOne(t, e, HistoricalGeneration{
		Gen: agento11y.Generation{CallError: "the model returned 500"},
	})
	if !errors.Is((*captured)[0].err, errHistoricalCall) {
		t.Fatalf("call error = %v, want the coarse sentinel", (*captured)[0].err)
	}
	if got := (*captured)[0].gen.CallError; got != "the model returned 500" {
		t.Fatalf("CallError = %q; the payload keeps the sanitized message", got)
	}
}

// TestExporterDoesNotFlushPerTurn pins the batching contract: handing a turn
// over must not wait for a round trip, or an import of the 277,625 turns on the
// development machine makes that many sequential requests.
func TestExporterDoesNotFlushPerTurn(t *testing.T) {
	records, confirms := 0, 0
	e := &Exporter{
		Record: func(context.Context, agento11y.GenerationStart, agento11y.Generation, error) error {
			records++
			return nil
		},
		Confirm: func(context.Context) error {
			confirms++
			return nil
		},
	}
	for range 10 {
		if err := e.ExportHistoricalGeneration(context.Background(), HistoricalGeneration{}); err != nil {
			t.Fatalf("ExportHistoricalGeneration: %v", err)
		}
	}
	if records != 10 || confirms != 0 {
		t.Fatalf("records = %d, confirms = %d; want 10 records and no confirmation until Flush", records, confirms)
	}
	if err := e.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if confirms != 1 {
		t.Fatalf("confirms = %d, want 1", confirms)
	}
}

// TestExporterSurfacesFailures pins which call reports which failure. A record
// failure and a cancelled context stop the handover; a delivery failure only
// shows up at the confirmation, because the handover does not wait for one.
func TestExporterSurfacesFailures(t *testing.T) {
	recordErr := errors.New("still broken")
	confirmErr := errors.New("transport down")
	tests := []struct {
		name         string
		recordErr    error
		confirmErr   error
		cancelled    bool
		wantOnExport error
		wantOnFlush  error
	}{
		{
			name:         "a record failure stops the handover",
			recordErr:    recordErr,
			wantOnExport: recordErr,
		},
		{
			name:        "a delivery failure surfaces at the confirmation",
			confirmErr:  confirmErr,
			wantOnFlush: confirmErr,
		},
		{
			name:         "a cancelled context stops the handover",
			cancelled:    true,
			wantOnExport: context.Canceled,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &Exporter{
				Record: func(context.Context, agento11y.GenerationStart, agento11y.Generation, error) error {
					return tt.recordErr
				},
				Confirm: func(context.Context) error { return tt.confirmErr },
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancelled {
				cancel()
			}

			err := e.ExportHistoricalGeneration(ctx, HistoricalGeneration{})
			if tt.wantOnExport != nil {
				if !errors.Is(err, tt.wantOnExport) {
					t.Fatalf("ExportHistoricalGeneration error = %v, want %v", err, tt.wantOnExport)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExportHistoricalGeneration: %v", err)
			}
			if err := e.Flush(ctx); !errors.Is(err, tt.wantOnFlush) {
				t.Fatalf("Flush error = %v, want %v", err, tt.wantOnFlush)
			}
		})
	}
}

// TestNewTargetExporterUsesExplicitEndpointAndHeaders pins the rule that an
// import target is passed explicitly rather than through the process
// environment: the local daemon resolves its Cloud forwarding from the same
// variables, so mutating them during an in-process import would change what
// the daemon forwards.
func TestNewTargetExporterUsesExplicitEndpointAndHeaders(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("AGENTO11Y_ENDPOINT", "https://cloud.example.invalid")

	var mu sync.Mutex
	var gotHeader string
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotHeader = r.Header.Get("X-Test-Marker")
		gotPath = r.URL.Path
		mu.Unlock()
		_, _ = io.WriteString(w, acceptAllGenerations(r))
	}))
	defer srv.Close()

	target := Target{
		Endpoint:    srv.URL,
		ContentMode: "full",
		Headers:     map[string]string{"X-Test-Marker": "1"},
	}
	exp, cleanup, err := NewTargetExporter(context.Background(), target, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewTargetExporter: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = cleanup(ctx)
	}()

	err = exp.ExportHistoricalGeneration(context.Background(), HistoricalGeneration{
		Source: SourceRef{Agent: AgentClaudeCode, SessionID: "s1", SourcePath: "/a.jsonl"},
		Gen: agento11y.Generation{
			ConversationID: "s1",
			Model:          agento11y.ModelRef{Provider: "anthropic", Name: "claude"},
			Input:          []agento11y.Message{agento11y.UserTextMessage("hello")},
			StartedAt:      time.Now().Add(-time.Minute),
			CompletedAt:    time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("ExportHistoricalGeneration: %v", err)
	}
	if err := exp.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotHeader != "1" {
		t.Fatalf("target header was not sent (got %q)", gotHeader)
	}
	if gotPath != "/api/v1/generations:export" {
		t.Fatalf("export path = %q", gotPath)
	}
	// The explicit endpoint must win over the configured Cloud one.
	if strings.Contains(srv.URL, "cloud.example.invalid") {
		t.Fatal("test setup error")
	}
}

func TestNewTargetExporterFallsBackToTheConfiguredEndpoint(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("AGENTO11Y_ENDPOINT", "https://cloud.example.invalid")
	t.Setenv("AGENTO11Y_AUTH_TENANT_ID", "tenant")
	t.Setenv("AGENTO11Y_AUTH_TOKEN", "token")

	exp, cleanup, err := NewTargetExporter(context.Background(), Target{}, nil)
	if err != nil {
		t.Fatalf("NewTargetExporter: %v", err)
	}
	if exp == nil {
		t.Fatal("no exporter returned")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = cleanup(ctx)
}

func TestNewTargetExporterRejectsAnIncompleteTarget(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*testing.T)
		target Target
		want   string
	}{
		{
			name:   "no endpoint at all",
			target: Target{},
			want:   "no endpoint configured",
		},
		{
			name:   "a Cloud endpoint with no credentials",
			target: Target{Endpoint: "https://cloud.example.invalid"},
			want:   "no credentials configured",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			if tc.setup != nil {
				tc.setup(t)
			}
			_, _, err := NewTargetExporter(context.Background(), tc.target, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

// TestLocalTargetNeedsNoCredentials pins the local placeholder path: a
// loopback import must work on a machine that has never run `agento11y login`.
func TestLocalTargetNeedsNoCredentials(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, acceptAllGenerations(r))
	}))
	defer srv.Close()

	_, cleanup, err := NewTargetExporter(context.Background(), Target{
		Endpoint: srv.URL,
		Headers:  map[string]string{"X-Test-Marker": "1"},
	}, nil)
	if err != nil {
		t.Fatalf("NewTargetExporter: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = cleanup(ctx)
}

// TestNewTargetExporterIgnoresAutoTags pins the one client path that must not
// take AGENTO11Y_AUTO_CODING_AGENT_TAGS. The importer replays sessions recorded earlier,
// possibly in another checkout, so the machine running the import does not
// describe them: stamping the current user, repository, or branch on a
// replayed turn would be wrong.
func TestNewTargetExporterIgnoresAutoTags(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("AGENTO11Y_AUTO_CODING_AGENT_TAGS", "true")

	var mu sync.Mutex
	var gotTags map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		var req struct {
			Generations []struct {
				ID   string            `json:"id"`
				Tags map[string]string `json:"tags"`
			} `json:"generations"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		results := make([]map[string]any, len(req.Generations))
		mu.Lock()
		for i, g := range req.Generations {
			gotTags = g.Tags
			results[i] = map[string]any{"generation_id": g.ID, "accepted": true}
		}
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	defer srv.Close()

	exp, cleanup, err := NewTargetExporter(context.Background(),
		Target{Endpoint: srv.URL, ContentMode: "full"}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewTargetExporter: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = cleanup(ctx)
	}()

	if err := exp.ExportHistoricalGeneration(context.Background(), HistoricalGeneration{
		Source: SourceRef{Agent: AgentClaudeCode, SessionID: "s1", SourcePath: "/a.jsonl"},
		Gen: agento11y.Generation{
			ConversationID: "s1",
			Model:          agento11y.ModelRef{Provider: "anthropic", Name: "claude"},
			Input:          []agento11y.Message{agento11y.UserTextMessage("hello")},
			StartedAt:      time.Now().Add(-time.Minute),
			CompletedAt:    time.Now(),
		},
	}); err != nil {
		t.Fatalf("ExportHistoricalGeneration: %v", err)
	}
	if err := exp.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, key := range []string{"user", "repo", "git.branch"} {
		if v, ok := gotTags[key]; ok {
			t.Errorf("imported generation carries auto-tag %s=%q", key, v)
		}
	}
}

// acceptAllGenerations answers an export request the way the local receiver
// does: one accepted result per generation, keyed by its ID.
func acceptAllGenerations(r *http.Request) string {
	var req struct {
		Generations []struct {
			ID string `json:"id"`
		} `json:"generations"`
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return `{"results":[]}`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return `{"results":[]}`
	}
	results := make([]map[string]any, len(req.Generations))
	for i, g := range req.Generations {
		results[i] = map[string]any{"generation_id": g.ID, "accepted": true}
	}
	encoded, err := json.Marshal(map[string]any{"results": results})
	if err != nil {
		return `{"results":[]}`
	}
	return string(encoded)
}

func TestOTLPHeaders(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		wantNil bool
	}{
		{name: "no headers keeps the environment set", wantNil: true},
		{
			name:    "explicit headers replace the environment set",
			headers: map[string]string{"X-Marker": "1"},
		},
		{
			name:    "an empty non-nil map sends no headers",
			headers: map[string]string{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := otlpHeaders(tc.headers)
			if (got == nil) != tc.wantNil {
				t.Fatalf("otlpHeaders() = %v, want nil = %v", got, tc.wantNil)
			}
		})
	}
}

func TestGenerationStartCarriesRequestFields(t *testing.T) {
	started := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	gen := agento11y.Generation{
		ID:                  "gen-1",
		ConversationID:      "conv-1",
		ConversationTitle:   "title",
		UserID:              "u@example.com",
		AgentName:           "claude-code",
		AgentVersion:        "1.2.3",
		Mode:                agento11y.GenerationModeSync,
		OperationName:       "generateText",
		Model:               agento11y.ModelRef{Provider: "anthropic", Name: "claude"},
		SystemPrompt:        "be brief",
		ParentGenerationIDs: []string{"parent"},
		EffectiveVersion:    "1.2.3",
		Tags:                map[string]string{"cwd": "/src"},
		Metadata:            map[string]any{"k": "v"},
		StartedAt:           started,
	}
	start := generationStart(gen)

	fields := map[string]struct{ got, want any }{
		"ID":                {start.ID, gen.ID},
		"ConversationID":    {start.ConversationID, gen.ConversationID},
		"ConversationTitle": {start.ConversationTitle, gen.ConversationTitle},
		"UserID":            {start.UserID, gen.UserID},
		"AgentName":         {start.AgentName, gen.AgentName},
		"AgentVersion":      {start.AgentVersion, gen.AgentVersion},
		"Mode":              {start.Mode, gen.Mode},
		"OperationName":     {start.OperationName, gen.OperationName},
		"Model":             {start.Model, gen.Model},
		"SystemPrompt":      {start.SystemPrompt, gen.SystemPrompt},
		"EffectiveVersion":  {start.EffectiveVersion, gen.EffectiveVersion},
	}
	for name, f := range fields {
		if f.got != f.want {
			t.Fatalf("recorder seed %s = %v, want %v", name, f.got, f.want)
		}
	}
	if !equalStrings(start.ParentGenerationIDs, gen.ParentGenerationIDs) {
		t.Fatalf("seed ParentGenerationIDs = %v", start.ParentGenerationIDs)
	}
	if start.Tags["cwd"] != "/src" || start.Metadata["k"] != "v" {
		t.Fatalf("seed tags/metadata = %v / %v", start.Tags, start.Metadata)
	}
	if !start.StartedAt.Equal(started) {
		t.Fatalf("seed StartedAt = %v, want %v; backdating depends on it", start.StartedAt, started)
	}
}

// TestLoopbackEndpointAlwaysCarriesTheForwardMarker covers the case a caller
// cannot see: an import the user asked to send to Grafana Cloud, on a machine
// whose configured endpoint is the local daemon. Without the marker the daemon
// relays the whole backfill to Cloud, so the rule follows the resolved
// endpoint rather than the caller's intent.
//
// Regression: an endpoint that reached the daemon under another spelling,
// LOCALHOST or a scheme left to AGENTO11Y_INSECURE, was taken for Cloud, and a
// user with Cloud credentials sent its import out unmarked.
func TestLoopbackEndpointAlwaysCarriesTheForwardMarker(t *testing.T) {
	var mu sync.Mutex
	var marker string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		marker = r.Header.Get(ForwardMarkerHeader)
		mu.Unlock()
		_, _ = io.WriteString(w, acceptAllGenerations(r))
	}))
	defer srv.Close()
	port := strings.TrimPrefix(srv.URL, "http://127.0.0.1:")

	for _, tt := range []struct {
		name     string
		endpoint string
		insecure bool
	}{
		{name: "127.0.0.1", endpoint: srv.URL},
		{name: "upper-case localhost", endpoint: "http://LOCALHOST:" + port},
		{name: "no scheme, insecure", endpoint: "127.0.0.1:" + port, insecure: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			// Go only skips a proxy for a lower-case localhost, so a proxy in
			// the environment would carry the LOCALHOST case away.
			for _, key := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"} {
				t.Setenv(key, "")
			}
			mu.Lock()
			marker = ""
			mu.Unlock()

			// The Cloud path: no endpoint on the target and no headers, with
			// Cloud credentials saved. AGENTO11Y_ENDPOINT happens to be the
			// local daemon.
			t.Setenv("AGENTO11Y_ENDPOINT", tt.endpoint)
			t.Setenv("AGENTO11Y_AUTH_TENANT_ID", "123")
			t.Setenv("AGENTO11Y_AUTH_TOKEN", "token")
			if tt.insecure {
				t.Setenv("AGENTO11Y_INSECURE", "true")
			}
			if !(Target{}).Local() {
				t.Errorf("Target.Local() = false for %q, which reaches the local daemon", tt.endpoint)
			}
			exp, cleanup, err := NewTargetExporter(context.Background(), Target{}, log.New(io.Discard, "", 0))
			if err != nil {
				t.Fatalf("NewTargetExporter: %v", err)
			}
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_ = cleanup(ctx)
			}()

			err = exp.ExportHistoricalGeneration(context.Background(), HistoricalGeneration{
				Source: SourceRef{Agent: AgentClaudeCode, SessionID: "s1", SourcePath: "/a.jsonl"},
				Gen: agento11y.Generation{
					ConversationID: "s1",
					Model:          agento11y.ModelRef{Provider: "anthropic", Name: "claude"},
					Input:          []agento11y.Message{agento11y.UserTextMessage("hello")},
					StartedAt:      time.Now().Add(-time.Minute),
					CompletedAt:    time.Now(),
				},
			})
			if err != nil {
				t.Fatalf("ExportHistoricalGeneration: %v", err)
			}
			if err := exp.Flush(context.Background()); err != nil {
				t.Fatalf("Flush: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if marker == "" {
				t.Fatal("an import that reached the local daemon carried no forward marker, so the daemon would relay it to Cloud")
			}
		})
	}
}

const openCodeV2ExportSecret = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij"

func openCodeV2ExportFixture(t *testing.T) *sessiondbtest.Builder {
	t.Helper()
	db := sessiondbtest.New(t, sessiondbtest.WithoutV1(), sessiondbtest.WithSessionV2())
	db.AddSessionV2("root", "title "+openCodeV2ExportSecret, "/synthetic/work", 1000, 9000)
	db.AddV2Message("user", "root", "user", 1, 1000, 1000, fmt.Sprintf(`{
		"text":"prompt %s", "files":[{"uri":"FILE-MUST-NOT-EXPORT"}],
		"metadata":{"private":"USER-METADATA-MUST-NOT-EXPORT"}
	}`, openCodeV2ExportSecret))
	db.AddV2Message("first", "root", "assistant", 3, 1000, 3000, strings.ReplaceAll(`{
		"agent":"build", "model":{"id":"test-model","providerID":"test-provider"},
		"time":{"created":1000,"streamed":2000,"completed":3000}, "cost":0.125,
		"tokens":{"input":200,"output":50,"reasoning":10,"cache":{"read":30,"write":20}},
		"providerState":{"private":"PROVIDER-MUST-NOT-EXPORT"},
		"metadata":{"private":"ASSISTANT-METADATA-MUST-NOT-EXPORT"},
		"snapshot":{"private":"SNAPSHOT-MUST-NOT-EXPORT"},
		"content":[
			{"type":"text","text":"answer SECRET","state":{"signature":"SIGNATURE-MUST-NOT-EXPORT"}},
			{"type":"reasoning","text":"thought SECRET","state":{"private":"REASONING-STATE-MUST-NOT-EXPORT"}},
			{"type":"tool","id":"completed","name":"read","state":{"status":"completed",
				"input":{"query":"argument SECRET"},
				"content":[{"type":"text","text":"result SECRET"},{"type":"file","uri":"TOOL-FILE-MUST-NOT-EXPORT"}],
				"metadata":{"private":"TOOL-METADATA-MUST-NOT-EXPORT"}}},
			{"type":"tool","id":"failed","name":"fetch","state":{"status":"error","input":{},
				"error":{"type":"network","message":"fallback SECRET","response":{"body":"TOOL-ERROR-BODY-MUST-NOT-EXPORT"}}}},
			{"type":"tool","id":"streaming","name":"read","state":{"status":"streaming","input":"{\"query\":\"stream SECRET\""}},
			{"type":"file","uri":"ASSISTANT-FILE-MUST-NOT-EXPORT"}
		]
	}`, "SECRET", openCodeV2ExportSecret))
	db.AddV2Message("zero", "root", "assistant", 5, 4000, 6000, `{
		"model":{"id":"test-model","providerID":"test-provider"},
		"time":{"created":4000,"completed":6000}, "cost":0,
		"tokens":{"input":0,"output":0,"reasoning":0,"cache":{"read":0,"write":0}}, "content":[]
	}`)
	db.AddV2Message("failed", "root", "assistant", 8, 7000, 9000, `{
		"model":{"id":"test-model","providerID":"test-provider"},
		"time":{"created":7000,"completed":9000},
		"error":{"type":"provider.failure","message":"ASSISTANT-ERROR-MUST-NOT-EXPORT",
			"response":{"body":"ASSISTANT-ERROR-BODY-MUST-NOT-EXPORT"}}, "content":[]
	}`)
	return db
}

// NewTargetExporter forces full capture on loopback. Configure the SDK directly
// to test Cloud capture modes without sending data outside the test receiver.
func sdkHistoryExporter(t *testing.T, endpoint string, mode agento11y.ContentCaptureMode) (*Exporter, *tracetest.SpanRecorder) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	client := agento11y.NewClient(agento11y.Config{
		ContentCapture: mode,
		Tracer:         provider.Tracer("agento11y.history.test"),
		Logger:         log.New(io.Discard, "", 0),
		GenerationExport: agento11y.GenerationExportConfig{
			Protocol: agento11y.GenerationExportProtocolHTTP,
			Endpoint: endpoint + "/api/v1/generations:export",
			Auth: agento11y.AuthConfig{
				Mode: agento11y.ExportAuthModeBasic, TenantID: "synthetic-tenant", BasicPassword: "synthetic-token",
			},
		},
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := client.Shutdown(ctx); err != nil {
			t.Errorf("SDK shutdown: %v", err)
		}
	})
	return NewExporter(client), recorder
}

func TestOpenCodeV2SDKLoopback(t *testing.T) {
	for _, tc := range []struct {
		name     string
		local    bool
		metadata bool
	}{
		{name: "local forces full capture", local: true},
		{name: "Cloud-shaped full capture"},
		{name: "Cloud-shaped metadata only", metadata: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			db := openCodeV2ExportFixture(t)
			var mu sync.Mutex
			var generations []*agento11yv1.Generation
			var requestBodies []byte
			var markers []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/generations:export" {
					t.Errorf("unexpected export path %q", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read export: %v", err)
					http.Error(w, "read failed", http.StatusBadRequest)
					return
				}
				req, err := wire.UnmarshalExportGenerationsJSON(body)
				if err != nil {
					t.Errorf("decode export: %v", err)
					http.Error(w, "decode failed", http.StatusBadRequest)
					return
				}
				mu.Lock()
				generations = append(generations, req.Generations...)
				requestBodies = append(requestBodies, body...)
				markers = append(markers, r.Header.Get(ForwardMarkerHeader))
				mu.Unlock()
				results := make([]map[string]any, len(req.Generations))
				for i, gen := range req.Generations {
					results[i] = map[string]any{"generation_id": gen.Id, "accepted": true}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
			}))
			t.Cleanup(srv.Close)

			var exp *Exporter
			var spans *tracetest.SpanRecorder
			if tc.local {
				var cleanup func(context.Context) error
				var err error
				exp, cleanup, err = NewTargetExporter(context.Background(), Target{
					Endpoint: srv.URL, ContentMode: "metadata_only",
				}, log.New(io.Discard, "", 0))
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					if err := cleanup(ctx); err != nil {
						t.Errorf("target shutdown: %v", err)
					}
				}()
			} else {
				mode := agento11y.ContentCaptureModeFull
				if tc.metadata {
					mode = agento11y.ContentCaptureModeMetadataOnly
				}
				exp, spans = sdkHistoryExporter(t, srv.URL, mode)
			}

			imp, ok := NewImporter(AgentOpenCode)
			if !ok {
				t.Fatal("OpenCode importer is not registered")
			}
			sessions, err := imp.(MultiSessionImporter).Previews(context.Background(), db.Path)
			if err != nil || len(sessions) != 1 || sessions[0].TurnCount != 3 {
				t.Fatalf("previews = %+v, error %v", sessions, err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			result, err := RunImport(ctx, ImportOptions{
				Agent: AgentOpenCode, Sessions: sessions, Exporter: exp,
				Ledger: openTestLedger(t, filepath.Join(t.TempDir(), "ledger.jsonl")),
			})
			if err != nil || result.Imported != 3 || result.Failed != 0 || len(result.Warnings) != 0 {
				t.Fatalf("RunImport = %+v, error %v", result, err)
			}

			mu.Lock()
			got := append([]*agento11yv1.Generation(nil), generations...)
			raw := string(requestBodies)
			gotMarkers := append([]string(nil), markers...)
			mu.Unlock()
			if len(got) != 3 || len(gotMarkers) == 0 {
				t.Fatalf("received %d generations in %d requests", len(got), len(gotMarkers))
			}
			for _, marker := range gotMarkers {
				if tc.local && marker != "1" {
					t.Errorf("local export forward marker = %q, want 1", marker)
				}
			}
			if strings.Contains(raw, openCodeV2ExportSecret) || strings.Contains(raw, "MUST-NOT-EXPORT") {
				t.Fatal("export leaked a secret, opaque state, file or raw error")
			}
			byID := make(map[string]*agento11yv1.Generation)
			for _, gen := range got {
				byID[gen.Id] = gen
			}
			id := func(turn string) string {
				return (SourceRef{Agent: AgentOpenCode, SessionID: "root", SourcePath: db.Path, TurnID: turn, TurnIDStable: true}).GenerationID()
			}
			first, zero, failed := byID[id("first")], byID[id("zero")], byID[id("failed")]
			if first == nil || zero == nil || failed == nil {
				t.Fatal("export lost native generation identities")
			}
			firstMeta, zeroMeta, failedMeta := first.Metadata.AsMap(), zero.Metadata.AsMap(), failed.Metadata.AsMap()
			wantUsage := &agento11yv1.TokenUsage{
				InputTokens: 250, OutputTokens: 60, TotalTokens: 310,
				CacheReadInputTokens: 30, CacheWriteInputTokens: 20, ReasoningTokens: 10,
				InputSemantics: agento11yv1.TokenInputSemantics_TOKEN_INPUT_SEMANTICS_INCLUSIVE,
			}
			if !proto.Equal(first.Usage, wantUsage) || firstMeta["cost_usd"] != 0.125 || firstMeta[MetaApproxUsage] != nil {
				t.Errorf("first usage/cost/quality = %+v / %v", first.Usage, firstMeta)
			}
			if !proto.Equal(zero.Usage, &agento11yv1.TokenUsage{InputSemantics: agento11yv1.TokenInputSemantics_TOKEN_INPUT_SEMANTICS_INCLUSIVE}) || zeroMeta["cost_usd"] != float64(0) || zeroMeta[MetaApproxUsage] != nil {
				t.Errorf("present-zero usage/cost/quality = %+v / %v", zero.Usage, zeroMeta)
			}
			wantCallError := "provider.failure"
			if tc.metadata {
				wantCallError = "sdk_error"
			}
			if _, present := failedMeta["cost_usd"]; present || failedMeta[MetaApproxUsage] != true || failed.CallError != wantCallError {
				t.Errorf("failed generation invented cost/usage or lost safe error: %+v", failed)
			}
			for _, gen := range got {
				meta := gen.Metadata.AsMap()
				if meta[MetaBackfill] != true || meta["opencode.settlement"] != "history" || meta["opencode.input_source"] != "persisted_user_delta" {
					t.Errorf("history metadata = %v", meta)
				}
				wantMode := "full"
				if tc.metadata {
					wantMode = "metadata_only"
				}
				if meta["agento11y.sdk.content_capture_mode"] != wantMode {
					t.Errorf("SDK capture mode = %v, want %s", meta["agento11y.sdk.content_capture_mode"], wantMode)
				}
				if tc.metadata {
					if len(gen.RawArtifacts) != 0 || gen.SystemPrompt != "" || meta[MetaConversationTitle] != nil {
						t.Errorf("metadata-only export retained artifacts, system prompt or title: %+v", gen)
					}
					for _, messages := range [][]*agento11yv1.Message{gen.Input, gen.Output} {
						for _, message := range messages {
							for _, part := range message.Parts {
								if part.GetText() != "" || part.GetThinking() != "" || len(part.GetToolCall().GetInputJson()) != 0 || part.GetToolResult().GetContent() != "" || len(part.GetToolResult().GetContentJson()) != 0 || part.GetMedia() != nil {
									t.Errorf("metadata-only export retained a content body: %+v", part)
								}
							}
						}
					}
					for _, tool := range gen.Tools {
						if tool.Description != "" || len(tool.InputSchemaJson) != 0 {
							t.Errorf("metadata-only export retained tool content: %+v", tool)
						}
					}
				}
			}
			if !tc.metadata {
				if len(first.Input) != 1 || len(first.Output) != 7 {
					t.Fatalf("full export input/output counts = %d/%d, want 1/7", len(first.Input), len(first.Output))
				}
				for field, text := range map[string]string{
					"title":               fmt.Sprint(firstMeta[MetaConversationTitle]),
					"input":               first.Input[0].Parts[0].GetText(),
					"text":                first.Output[0].Parts[0].GetText(),
					"reasoning":           first.Output[1].Parts[0].GetThinking(),
					"arguments":           string(first.Output[2].Parts[0].GetToolCall().GetInputJson()),
					"result":              first.Output[3].Parts[0].GetToolResult().GetContent(),
					"error fallback":      first.Output[5].Parts[0].GetToolResult().GetContent(),
					"streaming arguments": string(first.Output[6].Parts[0].GetToolCall().GetInputJson()),
				} {
					if !strings.Contains(text, "REDACTED") || strings.Contains(text, openCodeV2ExportSecret) {
						t.Errorf("%s did not retain redacted content: %q", field, text)
					}
				}
				if !first.Output[5].Parts[0].GetToolResult().GetIsError() || first.CallError != "" {
					t.Error("tool failure lost its error flag or failed the generation")
				}
			}
			if !first.StartedAt.AsTime().Equal(time.UnixMilli(1000)) || !first.CompletedAt.AsTime().Equal(time.UnixMilli(2000)) || firstMeta[MetaApproxCompleted] != nil {
				t.Errorf("streamed timestamps/quality = %v / %v / %v", first.StartedAt, first.CompletedAt, first.Metadata)
			}
			if !zero.CompletedAt.AsTime().Equal(time.UnixMilli(6000)) || zeroMeta[MetaApproxCompleted] != true || zeroMeta["opencode.completion_time_approximate"] != true {
				t.Errorf("completed fallback timestamps/quality = %v / %v", zero.CompletedAt, zero.Metadata)
			}
			if spans == nil {
				return
			}
			ended := spans.Ended()
			if len(ended) != 3 {
				t.Fatalf("SDK recorded %d spans, want 3", len(ended))
			}
			for _, span := range ended {
				attrs := make(map[string]attribute.Value)
				for _, attr := range span.Attributes() {
					attrs[string(attr.Key)] = attr.Value
					if strings.Contains(attr.Value.Emit(), openCodeV2ExportSecret) || strings.Contains(attr.Value.Emit(), "MUST-NOT-EXPORT") {
						t.Errorf("span attribute %s leaked source content", attr.Key)
					}
				}
				events, err := json.Marshal(span.Events())
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(events), openCodeV2ExportSecret) || strings.Contains(string(events), "MUST-NOT-EXPORT") {
					t.Error("span event leaked source content")
				}
				gen, ok := byID[attrs["agento11y.generation.id"].AsString()]
				if !ok {
					t.Fatalf("span has unknown generation ID %q", attrs["agento11y.generation.id"].AsString())
				}
				if !span.StartTime().Equal(gen.StartedAt.AsTime()) || !span.EndTime().Equal(gen.CompletedAt.AsTime()) {
					t.Errorf("span timestamps = %v/%v, generation %+v", span.StartTime(), span.EndTime(), gen)
				}
				if gen.Id == first.Id {
					for key, want := range map[string]int64{
						"gen_ai.usage.input_tokens": 250, "gen_ai.usage.output_tokens": 60,
						"gen_ai.usage.cache_read_input_tokens": 30, "gen_ai.usage.cache_write_input_tokens": 20,
					} {
						if attrs[key].AsInt64() != want {
							t.Errorf("span %s = %v, want %d", key, attrs[key], want)
						}
					}
					if attrs["gen_ai.token.semantics"].AsString() != "inclusive" || span.Status().Code != codes.Ok {
						t.Errorf("span semantics/status = %v/%v", attrs["gen_ai.token.semantics"], span.Status())
					}
				}
				if gen.Id == failed.Id && span.Status().Code != codes.Error {
					t.Errorf("failed assistant span status = %v", span.Status())
				}
				if tc.metadata {
					for _, key := range []string{"gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.system_instructions", "gen_ai.tool.definitions", MetaConversationTitle} {
						if _, present := attrs[key]; present {
							t.Errorf("metadata-only span retained %s", key)
						}
					}
				}
			}
		})
	}
}
