package local

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"
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
	storage *Storage
	logger  *log.Logger
	now     func() time.Time
	mux     *http.ServeMux
}

// NewServer builds a Server backed by the given storage. logger may be
// nil — the server logs only diagnostic information.
func NewServer(storage *Storage, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	s := &Server{
		storage: storage,
		logger:  logger,
		now:     func() time.Time { return time.Now().UTC() },
	}
	s.mux = s.routes()
	return s
}

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /conversations/{id}", s.handleIndex)
	mux.HandleFunc("GET /conversations/{id}/{$}", s.handleIndex)
	mux.HandleFunc("GET /assets/app.css", s.handleAppCSS)
	mux.HandleFunc("GET /assets/app.jsx", s.handleAppJSX)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /api/v1/conversations", s.handleListConversations)
	mux.HandleFunc("GET /api/v1/metrics/tokens", s.handleTokenMetrics)
	mux.HandleFunc("GET /api/v1/conversations/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.handleConversationDetail(w, r, r.PathValue("id"))
	})
	mux.HandleFunc("POST /api/v1/generations:export", s.handleGenerations)
	mux.HandleFunc("POST /otlp/v1/traces", s.handleOTLP)
	mux.HandleFunc("POST /otlp/v1/metrics", s.handleOTLP)
	// Cloud-style hook endpoint with no run prefix. The Sigil SDK strips
	// the path from API.Endpoint before appending /api/v1/hooks:evaluate,
	// so we must accept the bare path too — otherwise local hook
	// evaluation 404s.
	mux.HandleFunc("POST /api/v1/hooks:evaluate", s.handleHookEvaluate)
	return mux
}

// ServeHTTP routes incoming requests to the appropriate handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(indexHTML)
}

func (s *Server) handleAppCSS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(appCSS)
}

func (s *Server) handleAppJSX(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/babel; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(appJSX)
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
// Matches sigilv1.ExportGenerationsResponse / ExportGenerationResult.
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
	for _, raw := range req.Generations {
		var gen storedGeneration
		if err := json.Unmarshal(raw, &gen); err != nil {
			resp.Results = append(resp.Results, generationResult{Accepted: false, Error: "decode generation: " + err.Error()})
			continue
		}
		rec := generationRecord{
			ReceivedAt:     receivedAt,
			GenerationID:   gen.ID,
			ConversationID: gen.ConversationID,
			Generation:     append(json.RawMessage(nil), raw...),
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
		resp.Results = append(resp.Results, generationResult{
			GenerationID: gen.ID,
			Accepted:     true,
		})
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// handleOTLP accepts local OTLP exporter traffic so local mode does not
// leak spans or metrics to a user-configured global collector. The viewer
// does not read these signals yet, so the endpoint drains and acknowledges
// them without persisting a second local data model.
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
	// OTLP/HTTP collectors return an empty protobuf message on success;
	// 200 + empty body is accepted by the otlphttp exporter.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
}

// hookResponse is the allow-only payload returned to the SDK. It matches
// the sigil.HookEvaluateResponse JSON shape so the SDK decodes it
// without complaint.
type hookResponse struct {
	Action      string `json:"action"`
	Evaluations []any  `json:"evaluations"`
}

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
	s.writeJSON(w, http.StatusOK, hookResponse{Action: "allow", Evaluations: []any{}})
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
