package local

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// Maximum body sizes accepted by the receiver. These guard against
// runaway agents filling the local disk; they are generous enough for
// realistic LLM transcripts.
const (
	maxGenerationBodyBytes = 64 * 1024 * 1024 // 64 MiB
	maxOTLPBodyBytes       = 16 * 1024 * 1024 // 16 MiB
	maxHookBodyBytes       = 4 * 1024 * 1024  // 4 MiB
)

// Server is the in-process HTTP handler that records generations from
// local agent sessions and serves the local viewer API.
type Server struct {
	storage    *Storage
	logger     *log.Logger
	now        func() time.Time
	configPath string
	mux        *http.ServeMux
	forward    *forwardLoader
	hub        *eventHub
	// eventPingInterval overrides defaultEventPingInterval for tests; zero
	// uses the default.
	eventPingInterval time.Duration
}

// NewServer builds a Server backed by the given storage. logger may be
// nil — the server logs only diagnostic information. configPath is the
// dotenv file the Settings API reads and writes, and the same file the
// Cloud forwarder resolves its configuration from; an empty path disables
// persistence (reads return defaults, writes fail) but keeps the rest of
// the server usable for tests.
func NewServer(storage *Storage, logger *log.Logger, configPath string) *Server {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	s := &Server{
		storage:    storage,
		logger:     logger,
		configPath: configPath,
		now:        func() time.Time { return time.Now().UTC() },
		forward:    newForwardLoader(configPath, logger),
		hub:        newEventHub(),
	}
	s.mux = s.routes()
	return s
}

// Close releases server resources. It closes the event hub so open SSE
// connections return promptly instead of holding the HTTP shutdown
// deadline. Safe to call more than once.
func (s *Server) Close() {
	s.hub.closeAll()
}

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /conversations/{id}", s.handleIndex)
	mux.HandleFunc("GET /conversations/{id}/{$}", s.handleIndex)
	mux.HandleFunc("GET /settings", s.handleIndex)
	mux.HandleFunc("GET /settings/{$}", s.handleIndex)
	mux.HandleFunc("GET /assets/app.css", s.handleAppCSS)
	mux.HandleFunc("GET /assets/app.jsx", s.handleAppJSX)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /api/v1/conversations", s.handleListConversations)
	mux.HandleFunc("GET /api/v1/search", s.handleSearch)
	mux.HandleFunc("GET /api/v1/search/capabilities", s.handleSearchCapabilities)
	mux.HandleFunc("GET /api/v1/events", s.handleEvents)
	mux.HandleFunc("GET /api/v1/metrics/tokens", s.handleTokenMetrics)
	mux.HandleFunc("GET /api/v1/config", s.handleGetConfig)
	mux.HandleFunc("POST /api/v1/config:preview", s.handlePreviewConfig)
	mux.HandleFunc("PUT /api/v1/config", s.handleSaveConfig)
	mux.HandleFunc("GET /api/v1/conversations/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.handleConversationDetail(w, r, r.PathValue("id"))
	})
	mux.HandleFunc("POST /api/v1/generations:export", s.handleGenerations)
	mux.HandleFunc("POST /otlp/v1/traces", s.handleOTLP)
	mux.HandleFunc("POST /otlp/v1/metrics", s.handleOTLP)
	// Cloud-style hook endpoint with no run prefix. The agento11y SDK strips
	// the path from API.Endpoint before appending /api/v1/hooks:evaluate,
	// so we must accept the bare path too — otherwise local hook
	// evaluation 404s.
	mux.HandleFunc("POST "+hookEvaluatePath, s.handleHookEvaluate)
	return mux
}

// ServeHTTP routes incoming requests to the appropriate handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// devAsset returns the on-disk copy of a web asset when the
// AGENTO11Y_LOCAL_WEB_DIR (or legacy SIGIL_LOCAL_WEB_DIR) variable points
// at the web/ source dir, so the frontend can be iterated on with a
// browser reload instead of a Go rebuild. Unset returns the embedded copy.
// Dev-only convenience — no caching, no file watching.
func devAsset(name string, embedded []byte) []byte {
	dir, _, ok := envconfig.LookupEnv("LOCAL_WEB_DIR")
	if !ok {
		return embedded
	}
	if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
		return b
	}
	return embedded
}

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(devAsset("index.html", indexHTML))
}

func (s *Server) handleAppCSS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(devAsset("app.css", appCSS))
}

func (s *Server) handleAppJSX(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/babel; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(devAsset("app.jsx", appJSX))
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// generationsRequest mirrors the proto-JSON ExportGenerationsRequest
// envelope used by the HTTP exporter. The local receiver stores each
// generation exactly as it arrived; the query layer decodes only the
// fields needed by the viewer.
type generationsRequest struct {
	Generations []json.RawMessage `json:"generations"`
}

// generationsResponse is the JSON shape the SDK's HTTP exporter parses.
// Matches agento11y.v1 ExportGenerationsResponse / ExportGenerationResult.
type generationsResponse struct {
	Results []generationResult `json:"results"`
}

type generationResult struct {
	GenerationID string `json:"generation_id"`
	Accepted     bool   `json:"accepted"`
	Error        string `json:"error,omitempty"`
}

// generationRecord is one JSONL line in conversations/<conversation_id>.jsonl.
type generationRecord struct {
	ReceivedAt     string          `json:"received_at"`
	GenerationID   string          `json:"generation_id"`
	ConversationID string          `json:"conversation_id"`
	Generation     json.RawMessage `json:"generation"`
}

func (s *Server) handleGenerations(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxGenerationBodyBytes+1))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > maxGenerationBodyBytes {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	var req generationsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}

	receivedAt := s.now().Format(time.RFC3339Nano)
	resp := generationsResponse{Results: make([]generationResult, 0, len(req.Generations))}
	accepted := make([]json.RawMessage, 0, len(req.Generations))
	for _, raw := range req.Generations {
		var gen storedGeneration
		if err := json.Unmarshal(raw, &gen); err != nil {
			resp.Results = append(resp.Results, generationResult{Accepted: false, Error: "decode generation: " + err.Error()})
			continue
		}
		stored := append(json.RawMessage(nil), raw...)
		rec := generationRecord{
			ReceivedAt:     receivedAt,
			GenerationID:   gen.ID,
			ConversationID: gen.ConversationID,
			Generation:     stored,
		}
		if err := s.storage.AppendGeneration(rec); err != nil {
			s.logger.Printf("local: append generations: %v", err)
			resp.Results = append(resp.Results, generationResult{
				GenerationID: gen.ID,
				Accepted:     false,
				Error:        err.Error(),
			})
			continue
		}
		accepted = append(accepted, stored)
		resp.Results = append(resp.Results, generationResult{
			GenerationID: gen.ID,
			Accepted:     true,
		})
		// Notify connected viewers so the list (and any open matching
		// conversation) refreshes without waiting for the backstop poll.
		// Broadcast is non-blocking; a stalled subscriber cannot stall
		// ingest.
		s.hub.broadcast(changeEvent{
			ConversationID: gen.ConversationID,
			GenerationID:   gen.ID,
		})
	}

	// Best-effort Cloud forwarding runs after the local store so a Cloud
	// failure can never affect the JSONL write or the ack below. Off by
	// default: when forwarding is disabled load() returns enabled=false and we
	// spawn nothing.
	if len(accepted) > 0 && !isForwardedRequest(r) {
		if cfg := s.forward.load(); cfg.enabled {
			gens := accepted
			s.forward.enqueue(forwardLabelGenerations, func() { s.forward.forwardGenerations(cfg, gens) })
		}
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// handleOTLP accepts local OTLP exporter traffic so local mode does not
// leak spans or metrics to a user-configured global collector. The viewer
// does not read these signals yet, so the endpoint drains and acknowledges
// them without persisting a second local data model.
//
// When Cloud forwarding is enabled the payload is also relayed to the
// configured Cloud OTLP endpoint.
func (s *Server) handleOTLP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxOTLPBodyBytes+1))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > maxOTLPBodyBytes {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}

	// Best-effort Cloud forwarding: metrics relay unchanged, trace content is
	// stripped under a reduced content mode inside forwardOTLP.
	if signal := otlpSignalFromPath(r.URL.Path); signal != "" && !isForwardedRequest(r) {
		if cfg := s.forward.load(); cfg.enabled {
			contentType := r.Header.Get("Content-Type")
			contentEncoding := r.Header.Get("Content-Encoding")
			s.forward.enqueue(otlpForwardLabel(signal), func() { s.forward.forwardOTLP(cfg, signal, contentType, contentEncoding, body) })
		}
	}

	// OTLP/HTTP collectors return an empty protobuf message on success;
	// 200 + empty body is accepted by the otlphttp exporter.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
}

// handleHookEvaluate answers the synchronous guard check every host agent runs
// before (or right after) a tool call. The local verdict is always allow —
// there is no local guards engine yet — but when the daemon is configured to
// forward and guards are on, the request is relayed to Cloud so `--local` still
// enforces the rules the user configured there.
//
// Note what that means for content: a chained evaluation sends whatever the
// agent asked to have checked — the tool call for a postflight check, the whole
// outgoing conversation for a preflight one — to Cloud in full, whatever
// CONTENT_CAPTURE_MODE says, because a guard cannot evaluate what it cannot
// see. The viewer, the launcher banner, and `agento11y doctor` all report the
// resolved posture for that reason.
func (s *Server) handleHookEvaluate(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxHookBodyBytes+1))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > maxHookBodyBytes {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !json.Valid(body) {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	resp := agento11y.HookEvaluateResponse{
		Action:      agento11y.HookActionAllow,
		Evaluations: []agento11y.HookEvaluation{},
	}

	// Never chain a payload another daemon relayed here, which would loop.
	if !isForwardedRequest(r) {
		if cfg := s.forward.load(); cfg.hookURL != "" {
			s.chainHookEvaluate(r, cfg, body, &resp)
		}
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// chainHookEvaluate replaces the local verdict with Cloud's. A failed call
// follows the resolved fail mode: fail-open keeps the local allow, fail-closed
// denies with a response labelled as an evaluation failure so no consumer
// reports it as a policy decision.
func (s *Server) chainHookEvaluate(r *http.Request, cfg forwardConfig, body []byte, resp *agento11y.HookEvaluateResponse) {
	timeout := hookTimeoutFromHeader(r, time.Duration(cfg.timeoutMs)*time.Millisecond)
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	cloud, err := s.forward.evaluateCloudHook(ctx, cfg, timeout, body)
	switch {
	case err == nil:
		*resp = cloud
	case isCallerAbort(err):
		// The agent stopped waiting, so whatever this handler writes is not a
		// verdict anything acted on. Leaving the counters alone here is the
		// same call evaluateCloudHook makes for the failure ring: a count of
		// abandoned waits would report unchecked allows that never happened.
	case cfg.failOpen:
		// The agent cannot tell this allow from a Cloud allow, so count it:
		// the failure ring is cleared by the next success, and a guard that
		// silently stopped enforcing would otherwise leave no trace.
		s.forward.recordFailOpen()
	default:
		*resp = denyFromCloudError(body, err)
	}
}

// configResponse is the GET /api/v1/config and PUT /api/v1/config payload:
// the page-managed settings, the rendered config.env preview, and a display
// path for the file. It never includes the endpoint, tenant id, or auth
// token — those keys are not part of Settings and are never read back into
// the response.
//
// It also carries the daemon's current Cloud forwarding posture so the viewer
// can show what would leave this machine right now, including a reason when
// the user opted in but the target is unusable.
type configResponse struct {
	Settings      Settings      `json:"settings"`
	Preview       string        `json:"preview"`
	Path          string        `json:"path"`
	ForwardStatus forwardStatus `json:"forwardStatus"`
}

// configRequest is the POST :preview / PUT body: the form state the viewer
// edits.
type configRequest struct {
	Settings Settings `json:"settings"`
}

// handleGetConfig hydrates Settings from the current config.env and returns
// them with a rendered preview. Only the page-managed keys are exposed.
func (s *Server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	settings := ParseSettings(dotenv.LoadDotenv(s.configPath, s.logger))
	s.writeConfigResponse(w, settings)
}

// handlePreviewConfig renders the config.env the given form state would
// produce, without writing. It backs the viewer's live preview panel.
func (s *Server) handlePreviewConfig(w http.ResponseWriter, r *http.Request) {
	settings, ok := s.decodeConfigRequest(w, r)
	if !ok {
		return
	}
	preview, err := dotenv.RenderManaged(settings.previewUpdates())
	if err != nil {
		http.Error(w, "render config: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"preview": string(preview)})
}

// handleSaveConfig persists the given form state to config.env (merging with
// and preserving any keys the page does not manage) and returns the re-read
// settings plus preview so the client gets a clean saved snapshot.
func (s *Server) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	if s.configPath == "" {
		http.Error(w, "config persistence disabled", http.StatusServiceUnavailable)
		return
	}
	settings, ok := s.decodeConfigRequest(w, r)
	if !ok {
		return
	}
	if err := dotenv.WriteDotenv(s.configPath, settings.Updates(), s.logger); err != nil {
		s.logger.Printf("local: write config: %v", err)
		http.Error(w, "write config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Re-read so the response reflects the normalised on-disk state (dropped
	// defaults, deleted keys), which the client adopts as its saved snapshot.
	settings = ParseSettings(dotenv.LoadDotenv(s.configPath, s.logger))
	s.writeConfigResponse(w, settings)
}

func (s *Server) writeConfigResponse(w http.ResponseWriter, settings Settings) {
	preview, err := dotenv.RenderManaged(settings.previewUpdates())
	if err != nil {
		http.Error(w, "render config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, http.StatusOK, configResponse{
		Settings:      settings,
		Preview:       string(preview),
		Path:          displayConfigPath(s.configPath),
		ForwardStatus: s.forward.status(),
	})
}

// decodeConfigRequest reads and decodes a configRequest body, writing the
// appropriate HTTP error and returning ok=false on failure.
func (s *Server) decodeConfigRequest(w http.ResponseWriter, r *http.Request) (Settings, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxHookBodyBytes+1))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return Settings{}, false
	}
	if len(body) > maxHookBodyBytes {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return Settings{}, false
	}
	var req configRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return Settings{}, false
	}
	return req.Settings, true
}

// handleListConversations returns the aggregated conversation list as
// JSON. The response is newest-first.
func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	convs, err := s.storage.ListConversations(limit)
	if err != nil {
		s.logger.Printf("local: list conversations: %v", err)
		http.Error(w, "list conversations: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if convs == nil {
		// Distinguish "no data yet" from "daemon misconfigured": always
		// surface an array, never null, so the client can iterate without
		// guarding.
		convs = []ConversationSummary{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"conversations": convs})
}

// searchResultLimit caps the number of hits the search endpoint returns
// when the client does not pass a limit. The cap exists to bound the
// response body on stores with thousands of conversations; the viewer
// renders the full list it receives.
const searchResultLimit = 100

// handleSearch runs a full-text search across every recorded conversation
// and returns the hits as JSON. Empty/whitespace queries are not an
// error; they yield {"hits":[],"mode":"fts"} so the client can treat "no
// query" the same as "no results" without a special case.
//
// The response carries mode ("fts") so the viewer can show a faint
// backend hint. Semantic search is not wired up in this build, so the
// mode is always "fts".
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	limit := searchResultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	hits, err := s.storage.SearchConversations(q, limit)
	if err != nil {
		s.logger.Printf("local: search: %v", err)
		http.Error(w, "search: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if hits == nil {
		hits = []SearchHit{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"hits": hits, "mode": "fts"})
}

// SearchCapabilities is the JSON shape /api/v1/search/capabilities
// returns: whether full-text search is available (always true here),
// whether semantic search is available (not in this build), and a short
// status string the UI can show. The search endpoint always uses FTS.
type SearchCapabilities struct {
	FullText   bool   `json:"fts"`
	Semantic   bool   `json:"semantic"`
	IndexState string `json:"indexState"`
	Status     string `json:"status"`
}

// handleSearchCapabilities reports the search backends available to the
// viewer. Full-text search is always available; semantic search is not
// wired up in this build, so it is reported unavailable.
func (s *Server) handleSearchCapabilities(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, SearchCapabilities{
		FullText:   true,
		Semantic:   false,
		IndexState: "unavailable",
		Status:     "Full-text search only",
	})
}

// handleTokenMetrics returns one token-usage point per recorded
// generation as JSON. The viewer buckets these by time to draw the
// token-usage chart; an empty store returns an empty array, never null.
func (s *Server) handleTokenMetrics(w http.ResponseWriter, _ *http.Request) {
	points, err := s.storage.TokenUsagePoints()
	if err != nil {
		s.logger.Printf("local: token metrics: %v", err)
		http.Error(w, "token metrics: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if points == nil {
		points = []TokenUsagePoint{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"points": points})
}

// handleConversationDetail returns the per-conversation generation
// list. 404s when no generations have been recorded for the given id.
func (s *Server) handleConversationDetail(w http.ResponseWriter, r *http.Request, id string) {
	if !validConversationID(id) {
		http.NotFound(w, r)
		return
	}
	detail, err := s.storage.ConversationDetail(id)
	if err != nil {
		s.logger.Printf("local: conversation detail %q: %v", id, err)
		http.Error(w, "conversation detail: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if detail == nil {
		http.NotFound(w, r)
		return
	}
	s.writeJSON(w, http.StatusOK, detail)
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "marshal response: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
