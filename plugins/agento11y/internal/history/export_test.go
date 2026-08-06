package history

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
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
func TestLoopbackEndpointAlwaysCarriesTheForwardMarker(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)

	var mu sync.Mutex
	var marker string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		marker = r.Header.Get(ForwardMarkerHeader)
		mu.Unlock()
		_, _ = io.WriteString(w, acceptAllGenerations(r))
	}))
	defer srv.Close()

	// The Cloud path: no endpoint on the target, no headers, no credentials.
	// AGENTO11Y_ENDPOINT happens to be the local daemon.
	t.Setenv("AGENTO11Y_ENDPOINT", srv.URL)
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
}
