package local

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/go/proto/agento11y/wire"
	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/guardeval"
)

// Maximum body sizes accepted by the receiver. These guard against
// runaway agents filling the local disk; they are generous enough for
// realistic LLM transcripts.
const (
	maxGenerationBodyBytes = 64 * 1024 * 1024 // 64 MiB
	maxOTLPBodyBytes       = 16 * 1024 * 1024 // 16 MiB
	maxHookBodyBytes       = 4 * 1024 * 1024  // 4 MiB

	otlpTracesPath   = "/otlp/v1/traces"
	otlpMetricsPath  = "/otlp/v1/metrics"
	noncePlaceholder = "{{AGENTO11Y_CSP_NONCE}}"
)

// Server is the in-process HTTP handler that records generations from
// local agent sessions and serves the local viewer API.
type Server struct {
	storage      *Storage
	logger       *log.Logger
	now          func() time.Time
	configPath   string
	guards       guardeval.Config
	allowedHosts []string
	mux          *http.ServeMux
	forward      *forwardLoader
	hub          *eventHub
	// eventPingInterval overrides defaultEventPingInterval for tests; zero
	// uses the default.
	eventPingInterval time.Duration

	// warmStore fills the summary cache in the background. Serve sets it;
	// a nil hook means no warming. warmOnce keeps the first viewer read the
	// only trigger.
	warmStore func()
	warmOnce  sync.Once

	// importMu guards the history-import fields below. One import runs at a
	// time: two would write the same per-agent ledger and race on it.
	importMu sync.Mutex
	// localEndpoint is this daemon's own address, which an import exports to.
	// Serve sets it once the listener has a port.
	localEndpoint string
	// activeImport is the run ID of the import in flight, empty when none is.
	activeImport string
	// importRuns holds the most recent runs, newest last. It is in-memory
	// only: the ledger is the durable record of what was imported, so a run
	// list that does not survive a restart costs nothing.
	importRuns []*importRun
}

// NewServer builds a Server backed by the given storage. logger may be
// nil — the server logs only diagnostic information. configPath is the
// dotenv file the Settings API reads and writes, and the same file the
// Cloud forwarder resolves its configuration from; an empty path disables
// persistence (reads return defaults, writes fail) but keeps the rest of
// the server usable for tests.
//
// guards.toml is resolved in configPath's directory. An empty configPath
// disables local rules.
func NewServer(storage *Storage, logger *log.Logger, configPath string) *Server {
	return newServer(storage, logger, configPath, guardeval.FilePath(configPath))
}

// newServer is the test seam: it takes the guards ruleset path explicitly so a
// test can point it at a temp file without also relocating config.env.
func newServer(storage *Storage, logger *log.Logger, configPath, guardsPath string) *Server {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	if storage != nil {
		storage.SetLogger(logger)
	}
	guardConfig := guardeval.Config{RulesPath: guardsPath, Logger: logger}
	s := &Server{
		storage:      storage,
		logger:       logger,
		configPath:   configPath,
		guards:       guardConfig,
		allowedHosts: allowedHostsFromEnv(),
		now:          func() time.Time { return time.Now().UTC() },
		forward:      newForwardLoader(configPath, logger),
		hub:          newEventHub(),
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

// WarmSummariesOnFirstRead arms the background summary-cache warm. The warm
// starts once the first viewer read has answered, and stops when ctx is
// done. Nothing warms before that read. Several commands start a daemon
// without opening the viewer: an agent session in local mode, `local start`,
// `local restart`, `history import`. Warming at startup would make each of
// them decode the whole store and hold it resident until the daemon exits.
func (s *Server) WarmSummariesOnFirstRead(ctx context.Context) {
	s.warmStore = func() { s.storage.warmSummaries(ctx) }
}

// warmSummariesInBackground starts the warm the first time a viewer read
// finishes. It runs after the response so the request the user is waiting
// on does not queue behind the rest of the store.
func (s *Server) warmSummariesInBackground() {
	if s.warmStore == nil {
		return
	}
	s.warmOnce.Do(func() { go s.warmStore() })
}

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /conversations/{id}", s.handleIndex)
	mux.HandleFunc("GET /conversations/{id}/{$}", s.handleIndex)
	mux.HandleFunc("GET /settings", s.handleIndex)
	mux.HandleFunc("GET /settings/{$}", s.handleIndex)
	mux.HandleFunc("GET /analytics", s.handleIndex)
	mux.HandleFunc("GET /analytics/{$}", s.handleIndex)
	mux.HandleFunc("GET /assets/app.css", s.handleAppCSS)
	mux.HandleFunc("GET /assets/app.js", s.handleAppJS)
	mux.HandleFunc("GET /assets/vendor/{file}", s.handleVendorAsset)
	mux.HandleFunc("GET /assets/fonts/{file}", s.handleFontAsset)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /api/v1/conversations", s.handleListConversations)
	mux.HandleFunc("GET /api/v1/search", s.handleSearch)
	mux.HandleFunc("GET /api/v1/search/capabilities", s.handleSearchCapabilities)
	mux.HandleFunc("GET /api/v1/events", s.handleEvents)
	mux.HandleFunc("GET /api/v1/metrics/conversations", s.handleConversationMetrics)
	mux.HandleFunc("GET /api/v1/metrics/tokens", s.handleTokenMetrics)
	mux.HandleFunc("GET /api/v1/metrics/tools", s.handleToolMetrics)
	mux.HandleFunc("GET /api/v1/metrics/skills-tools", s.handleSkillsToolsMetrics)
	mux.HandleFunc("GET /api/v1/config", s.handleGetConfig)
	mux.HandleFunc("POST /api/v1/config:preview", s.handlePreviewConfig)
	mux.HandleFunc("PUT /api/v1/config", s.handleSaveConfig)
	mux.HandleFunc("GET /api/v1/conversations/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.handleConversationDetail(w, r, r.PathValue("id"))
	})
	mux.HandleFunc("GET /api/v1/history/agents", s.handleHistoryAgents)
	mux.HandleFunc("GET /api/v1/history/offer", s.handleHistoryOffer)
	mux.HandleFunc("POST /api/v1/history/offer:dismiss", s.handleHistoryDismiss)
	mux.HandleFunc("GET /api/v1/history/plan", s.handleHistoryPlan)
	mux.HandleFunc("POST /api/v1/history:import", s.handleHistoryImport)
	mux.HandleFunc("GET /api/v1/history/runs/{runID}", s.handleHistoryRunStatus)
	// The path is /api/v1/history/runs/{runID}:cancel. A ServeMux wildcard has
	// to be a whole segment, so the segment is matched as one value and the
	// handler splits the action off it.
	mux.HandleFunc("POST /api/v1/history/runs/{runAction}", s.handleHistoryRunCancel)
	mux.HandleFunc("POST /api/v1/generations:export", s.handleGenerations)
	mux.HandleFunc("POST "+otlpTracesPath, s.handleOTLP)
	mux.HandleFunc("POST "+otlpMetricsPath, s.handleOTLP)
	// Cloud-style hook endpoint with no run prefix. The agento11y SDK strips
	// the path from API.Endpoint before appending /api/v1/hooks:evaluate,
	// so we must accept the bare path too — otherwise local hook
	// evaluation 404s.
	mux.HandleFunc("POST "+hookEvaluatePath, s.handleHookEvaluate)
	return mux
}

// mediaTypeAccepted permits only types that a browser cannot send cross-site
// without a preflight. The OTLP routes also accept binary protobuf.
func mediaTypeAccepted(path, header string) bool {
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return false
	}

	switch mediaType {
	case wire.ContentTypeJSON:
		return true
	case wire.ContentTypeProto:
		return path == otlpTracesPath || path == otlpMetricsPath
	default:
		return false
	}
}

// ServeHTTP routes incoming requests to the appropriate handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w.Header())

	if err := checkRequestOrigin(r, s.allowedHosts); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	if postOrPutWithUnsupportedMediaType(r) {
		s.logger.Printf("local: refused %s %q: Content-Type %q", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}

	s.mux.ServeHTTP(w, r)
}

func postOrPutWithUnsupportedMediaType(r *http.Request) bool {
	return (r.Method == http.MethodPost || r.Method == http.MethodPut) &&
		!mediaTypeAccepted(r.URL.Path, r.Header.Get("Content-Type"))
}

func securityHeaders(header http.Header) {
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Cross-Origin-Resource-Policy", "same-origin")
	header.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'")
}

func documentCSP(nonce string) string {
	return "default-src 'self'; " +
		"script-src 'self' 'nonce-" + nonce + "'; " +
		"style-src 'self'; " +
		"img-src 'self' data:; " +
		"font-src 'self'; " +
		"connect-src 'self' https://models.dev; " +
		"object-src 'none'; " +
		"frame-ancestors 'none'; " +
		"base-uri 'none'; " +
		"form-action 'none'"
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
	nonce := rand.Text()
	body := devAsset("index.html", indexHTML)
	if !bytes.Contains(body, []byte(noncePlaceholder)) {
		s.logger.Printf("local: index.html missing CSP nonce placeholder %q", noncePlaceholder)
	}
	body = bytes.ReplaceAll(body, []byte(noncePlaceholder), []byte(nonce))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Security-Policy", documentCSP(nonce))
	_, _ = w.Write(body)
}

func (s *Server) handleAppCSS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(devAsset("app.css", appCSS))
}

// handleAppJS serves the compiled viewer. A compile error is reported as a 500
// carrying esbuild's message: a blank page with a working status code would
// send whoever edited the source looking at the browser console for a runtime
// fault that is not there.
func (s *Server) handleAppJS(w http.ResponseWriter, _ *http.Request) {
	bundle, err := viewerBundle()
	if err != nil {
		s.logger.Printf("local: viewer bundle failed: %v", err)
		http.Error(w, "viewer bundle failed:\n"+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(bundle)
}

func (s *Server) handleVendorAsset(w http.ResponseWriter, r *http.Request) {
	s.serveStatic(w, r, "vendor", ".js", "application/javascript; charset=utf-8")
}

func (s *Server) handleFontAsset(w http.ResponseWriter, r *http.Request) {
	s.serveStatic(w, r, "fonts", ".woff2", "font/woff2")
}

// serveStatic returns one vendored asset from [webStatic]. The file name comes
// from the URL, so it is checked against the expected extension and rejected if
// it carries any path at all: only a bare name inside the named directory is
// servable.
//
// These assets are versioned by their content and never change for a given
// binary, so they are cached for a year. app.css and app.js stay no-cache,
// because LOCAL_WEB_DIR reloads them from disk during development.
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request, dir, ext, contentType string) {
	name := r.PathValue("file")
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, `\`) ||
		strings.Contains(name, "..") || !strings.HasSuffix(name, ext) {
		http.NotFound(w, r)
		return
	}
	body, err := webStatic.ReadFile("web/" + dir + "/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(body)
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

// pendingGeneration is one decoded generation waiting to be appended,
// carrying its position in the request so the per-record result keeps its
// request position.
type pendingGeneration struct {
	index  int
	record generationRecord
	// activity is the moment this record represents. The newest activity
	// among the records a write accepts becomes the conversation file's
	// modification time, which is the key the list orders and bounds by.
	activity time.Time
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
	resp := generationsResponse{Results: make([]generationResult, len(req.Generations))}

	// Group the request by conversation so each conversation file is opened
	// once, however many generations the batch carries for it. Results stay
	// indexed by request position: grouping does not reorder them.
	var order []string
	groups := map[string][]pendingGeneration{}
	stored := make([]json.RawMessage, len(req.Generations))
	for i, raw := range req.Generations {
		var gen storedGeneration
		if err := json.Unmarshal(raw, &gen); err != nil {
			resp.Results[i] = generationResult{Accepted: false, Error: "decode generation: " + err.Error()}
			continue
		}
		// json.Unmarshal allocates a fresh slice per RawMessage element, so
		// raw is private to this request and needs no copy.
		stored[i] = raw
		if _, ok := groups[gen.ConversationID]; !ok {
			order = append(order, gen.ConversationID)
		}
		groups[gen.ConversationID] = append(groups[gen.ConversationID], pendingGeneration{
			index: i,
			record: generationRecord{
				ReceivedAt:     receivedAt,
				GenerationID:   gen.ID,
				ConversationID: gen.ConversationID,
				Generation:     stored[i],
			},
			// gen already carries the timestamps recordActivity reads, so
			// the stamp costs no extra decode.
			activity: recordActivity(gen.summaryGeneration, receivedAt),
		})
	}

	// One change event per conversation the request wrote to. The viewer
	// coalesces a burst into a single refresh anyway, so a per-generation
	// event would multiply full-store refetches during a large import.
	var events []changeEvent
	for _, convID := range order {
		pending := groups[convID]
		recs := make([]generationRecord, len(pending))
		activities := make([]time.Time, len(pending))
		for i, p := range pending {
			recs[i] = p.record
			activities[i] = p.activity
		}
		written, err := s.storage.AppendGenerations(convID, recs, activities)
		if err != nil {
			s.logger.Printf("local: append generations: %v", err)
		}
		// A short write always carries an error today. Copy the message into
		// a local first, so a future path that returns a short count with a
		// nil error cannot dereference err here.
		reason := "append rejected"
		if err != nil {
			reason = err.Error()
		}
		for i, p := range pending {
			result := generationResult{GenerationID: p.record.GenerationID, Accepted: i < written}
			if !result.Accepted {
				result.Error = reason
				stored[p.index] = nil
			}
			resp.Results[p.index] = result
		}
		if written > 0 {
			events = append(events, changeEvent{
				ConversationID: convID,
				GenerationID:   recs[written-1].GenerationID,
			})
		}
	}

	// Notify connected viewers so the list (and any open matching
	// conversation) refreshes without waiting for the backstop poll.
	// Broadcast is non-blocking; a stalled subscriber cannot stall ingest.
	for _, ev := range events {
		s.hub.broadcast(ev)
	}

	accepted := make([]json.RawMessage, 0, len(req.Generations))
	for _, raw := range stored {
		if raw != nil {
			accepted = append(accepted, raw)
		}
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
// leak spans or metrics to a user-configured global collector. It retains a
// metadata-only projection of execute_tool spans for local analytics and
// otherwise drains the signal.
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

	contentType := r.Header.Get("Content-Type")
	contentEncoding := r.Header.Get("Content-Encoding")
	if r.URL.Path == otlpTracesPath {
		if spans, projectErr := projectToolSpans(body, contentType, contentEncoding); projectErr != nil {
			s.logger.Printf("local: project tool spans: %v", projectErr)
		} else if changed, appendErr := s.storage.appendToolSpans(spans); appendErr != nil {
			s.logger.Printf("local: append tool spans: %v", appendErr)
			for _, conversationID := range changed {
				s.hub.broadcast(changeEvent{ConversationID: conversationID})
			}
		} else {
			for _, conversationID := range changed {
				s.hub.broadcast(changeEvent{ConversationID: conversationID})
			}
		}
	}

	// Best-effort Cloud forwarding: metrics relay unchanged, trace content is
	// stripped under a reduced content mode inside forwardOTLP.
	if signal := otlpSignalFromPath(r.URL.Path); signal != "" && !isForwardedRequest(r) {
		if cfg := s.forward.load(); cfg.enabled {
			s.forward.enqueue(otlpForwardLabel(signal), func() { s.forward.forwardOTLP(cfg, signal, contentType, contentEncoding, body) })
		}
	}

	// OTLP/HTTP collectors return an empty protobuf message on success;
	// 200 + empty body is accepted by the otlphttp exporter.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
}

// handleHookEvaluate answers the guard check before or after a tool call. The
// daemon reads and compiles guards.toml for every request, then runs local tool
// filters, regex evaluators, and redactions. Invalid rules are skipped
// individually. A local deny returns before relay, so rejected content stays on
// the machine. If Cloud guard forwarding is configured, surviving requests are
// evaluated there too.
//
// Except for local redactions, Cloud receives the tool call for postflight or
// the outgoing conversation for preflight regardless of CONTENT_CAPTURE_MODE.
// Guards must see the content they evaluate, so the viewer, launcher banner,
// and `agento11y doctor` report this posture.
//
// The decoder accepts three hook request spellings (see guardwire.go). Local
// redactions apply to the decoded input before the request is re-encoded for
// Cloud. Unchanged requests are relayed byte-for-byte.
func (s *Server) handleHookEvaluate(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readHookSizedBody(w, r)
	if !ok {
		return
	}
	req, err := decodeHookEvaluateRequest(body)
	if err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}

	resp, localTransform := guardeval.NewEngine(s.guards).EvaluateWithTransform(req)
	if resp.Action == agento11y.HookActionDeny {
		s.writeJSON(w, http.StatusOK, encodeHookEvaluateResponse(resp))
		return
	}

	// Never chain a payload another daemon relayed here, which would loop.
	if !isForwardedRequest(r) {
		if cfg := s.forward.load(); cfg.hookURL != "" {
			relayBody, redacted, prepareErr := prepareHookRelayBody(body, req, localTransform, s.logger)
			if prepareErr != nil {
				prepareErr = s.forward.recordHookFailure("prepare Cloud hook relay: %v", prepareErr)
				if cfg.failOpen {
					s.forward.recordFailOpen()
				} else {
					resp = guardeval.FromHookResponse(denyFromCloudError(body, prepareErr))
				}
			} else {
				if redacted {
					// AGENTO11Y_DEBUG only: confirms relay redaction without logging values.
					s.logger.Printf("local guards: relaying a redacted body to Cloud")
				}
				s.chainHookEvaluate(r, cfg, body, relayBody, localTransform, &resp)
			}
		}
	}
	s.writeJSON(w, http.StatusOK, encodeHookEvaluateResponse(resp))
}

func prepareHookRelayBody(body []byte, req agento11y.HookEvaluateRequest, localTransform *guardeval.Transform, logger *log.Logger) ([]byte, bool, error) {
	if localTransform == nil {
		return body, false, nil
	}
	relayInput, changed, dropped := guardeval.ApplyRelayTransform(req.Input, localTransform, logger)
	if len(dropped) > 0 {
		return nil, false, fmt.Errorf("local redaction of %s would produce invalid JSON", strings.Join(dropped, ", "))
	}
	if !changed {
		return body, false, nil
	}
	encoded, err := encodeRelayBody(req.Phase, req.Context, relayInput)
	if err != nil {
		return nil, false, fmt.Errorf("encode redacted request: %w", err)
	}
	return encoded, true, nil
}

// chainHookEvaluate merges the Cloud verdict after the local verdict. Cloud
// supplies the action, rule ID, and reason. A local transform survives when
// Cloud supplies none, and local evaluations remain first. On failure,
// fail-open keeps the local verdict; fail-closed returns an evaluation-failure
// deny.
func (s *Server) chainHookEvaluate(r *http.Request, cfg forwardConfig, originalBody, body []byte, localTransform *guardeval.Transform, resp *guardeval.Response) {
	timeout := hookTimeoutFromHeader(r, time.Duration(cfg.timeoutMs)*time.Millisecond)
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	cloud, err := s.forward.evaluateCloudHook(ctx, cfg, timeout, body)
	switch {
	case err == nil:
		mergeCloudVerdict(resp, cloud, localTransform, s.logger)
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
		*resp = guardeval.FromHookResponse(denyFromCloudError(originalBody, err))
	}
}

// mergeCloudVerdict folds a Cloud response into the local one. Cloud decides
// the action, so its rule id and reason travel with it even when empty:
// carrying a local rule id under a Cloud action would name the wrong rule.
//
// The transform needs more care:
//
//   - Cloud sends no transform: its silence means "no opinion", so the local
//     rewrite stands.
//   - Cloud sends one: the local patterns run over it again because Cloud does
//     not state whether its transformed input came from the redacted relay. If
//     that would produce invalid JSON, the local rewrite stands instead.
//   - Cloud denies: the call does not run, so the local rewrite is not
//     re-attached. Only what Cloud sent survives.
func mergeCloudVerdict(resp *guardeval.Response, cloud agento11y.HookEvaluateResponse, localTransform *guardeval.Transform, logger *log.Logger) {
	localTransformed := resp.TransformedInput
	localEvaluations := resp.Evaluations
	localRuleID := resp.RuleID
	cloudResponse := guardeval.FromHookResponse(cloud)

	*resp = cloudResponse
	var dropped []string
	switch {
	case cloud.Action == agento11y.HookActionDeny:
	case cloud.TransformedInput == nil:
		resp.TransformedInput = localTransformed
	case localTransform != nil:
		redacted, _, drops := guardeval.ApplyTransform(*cloud.TransformedInput, localTransform, logger)
		dropped = drops
		if len(drops) > 0 {
			resp.TransformedInput = localTransformed
		} else {
			resp.TransformedInput = &redacted
		}
	}
	merged := append([]guardeval.Evaluation{}, localEvaluations...)
	merged = append(merged, droppedRedactionEvaluations(localRuleID, dropped)...)
	resp.Evaluations = append(merged, cloudResponse.Evaluations...)
}

// droppedRedactionEvaluations reports local transforms that could not be
// applied to Cloud's transformed input. The Cloud rewrite is discarded, but
// the evaluation explains why its changes are absent.
func droppedRedactionEvaluations(ruleID string, dropped []string) []guardeval.Evaluation {
	out := make([]guardeval.Evaluation, 0, len(dropped))
	for _, what := range dropped {
		out = append(out, guardeval.Evaluation{
			RuleID:        ruleID,
			EvaluatorKind: "transform",
			Reason:        fmt.Sprintf("the local redaction of %s would make Cloud's rewrite invalid JSON, so Cloud's rewrite was discarded", what),
		})
	}
	return out
}

func (s *Server) readHookSizedBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxHookBodyBytes+1))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return nil, false
	}
	if len(body) > maxHookBodyBytes {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return nil, false
	}
	return body, true
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
	Settings Settings `json:"settings"`
	Preview  string   `json:"preview"`
	Path     string   `json:"path"`
	// StackURL is the stack `agento11y login` was pointed at, read back so the
	// connect flow can prefill its setup-page link. It is read-only: it is not
	// part of Settings, so a save never writes or deletes it.
	StackURL      string        `json:"stackUrl"`
	ForwardStatus forwardStatus `json:"forwardStatus"`
	// LocalGuards reports local-rule posture separately from Cloud forwarding;
	// either can be active without the other.
	LocalGuards localGuardsStatus `json:"localGuards"`
}

type localGuardsStatus struct {
	guardeval.Posture
	// Enabled reflects GUARDS_ENABLED from config.env and defaults to false. A
	// host using that setting sends no hook requests. Launchers resolve their own
	// environment because the daemon cannot observe process-level overrides.
	Enabled bool `json:"enabled"`
}

// configRequest is the POST :preview / PUT body: the form state the viewer
// edits.
type configRequest struct {
	Settings Settings `json:"settings"`
}

// handleGetConfig hydrates Settings from the current config.env and returns
// them with a rendered preview. Only the page-managed keys are exposed.
func (s *Server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	s.writeConfigResponse(w, dotenv.LoadDotenv(s.configPath, s.logger))
}

// stackURLFrom reads the saved stack URL. STACK_URL has no alias family:
// AliasSuffixes does not list it, and `agento11y login` writes only the
// AGENTO11Y_ spelling, so the raw key is the whole lookup.
func stackURLFrom(env map[string]string) string {
	return strings.TrimSpace(env["AGENTO11Y_STACK_URL"])
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
	s.writeConfigResponse(w, dotenv.LoadDotenv(s.configPath, s.logger))
}

// writeConfigResponse renders the whole response from one config.env snapshot,
// so the settings, the preview and the stack URL cannot come from separate
// reads of a file another writer is changing.
func (s *Server) writeConfigResponse(w http.ResponseWriter, env map[string]string) {
	settings := ParseSettings(env)
	preview, err := dotenv.RenderManaged(settings.previewUpdates())
	if err != nil {
		http.Error(w, "render config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, http.StatusOK, configResponse{
		Settings:      settings,
		Preview:       string(preview),
		Path:          displayConfigPath(s.configPath),
		StackURL:      stackURLFrom(env),
		ForwardStatus: s.forward.status(),
		LocalGuards:   s.localGuardsStatus(),
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

// conversationListLimit caps how many matching conversations the list
// endpoint returns when the client does not pass a limit. Exact filters are
// checked before a candidate counts toward the cap, so those requests may
// scan more files. The viewer's list is not virtualised, so an unbounded
// response would hurt both ends. Same idiom as searchResultLimit: the default
// lives in the handler, and Storage.ListConversations keeps ≤ 0 as unbounded
// for its own callers.
const conversationListLimit = 200

// handleListConversations returns the aggregated conversation list as JSON.
// The response is newest-first. ?limit= caps matching rows. Exact time,
// workspace, and tool filters are applied to each decoded candidate before
// it counts toward that cap; see ConversationListOptions.
//
// total_conversations counts the conversation files in the store, before
// either bound. The viewer needs it to tell an empty store from an empty
// range: with a range-scoped page it cannot otherwise distinguish first
// launch from a quiet week, and the two want different notices.
func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	limit, ok := limitParam(w, r, conversationListLimit)
	if !ok {
		return
	}
	since, before, workspace, ok := metricsPeriodParams(w, r)
	if !ok {
		return
	}
	var tool *string
	if values, present := r.URL.Query()["tool"]; present {
		value := ""
		if len(values) > 0 {
			value = values[0]
		}
		tool = &value
	}
	_, hasBefore := r.URL.Query()["before"]
	// A since-only request is the normal ranged list and uses last activity.
	exact := hasBefore || workspace != nil || tool != nil
	convs, total, err := s.storage.ListConversations(ConversationListOptions{
		Limit: limit, Since: since, Before: before, Workspace: workspace, Tool: tool, Exact: exact,
	})
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
	s.writeJSON(w, http.StatusOK, map[string]any{
		"conversations":       convs,
		"total_conversations": total,
	})
	s.warmSummariesInBackground()
}

// limitParam reads a ?limit= page size, falling back to def when the
// parameter is absent. A non-positive or unparseable value is a client
// error. ?since= and ?interval= reject a bad value the same way: a client
// that means "everything" passes a large number, not a value the server has
// to guess at.
func limitParam(w http.ResponseWriter, r *http.Request, def int) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		http.Error(w, "invalid limit: want a positive number", http.StatusBadRequest)
		return 0, false
	}
	return n, true
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

// handleTokenMetrics returns token usage aggregated per bucket and model
// as JSON, for the viewer's token chart. ?since= (RFC 3339) bounds the
// range and ?interval= sets the bucket width in seconds; when the client
// omits the interval the server derives one from the span it found and
// reports it as interval_seconds, which the viewer widens its bars to so a
// server bucket never straddles two of them. An empty store returns an
// empty array, never null.
func (s *Server) handleTokenMetrics(w http.ResponseWriter, r *http.Request) {
	since, before, workspace, ok := metricsPeriodParams(w, r)
	if !ok {
		return
	}
	var interval time.Duration
	if raw := r.URL.Query().Get("interval"); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds <= 0 {
			http.Error(w, "invalid interval: want a positive number of seconds", http.StatusBadRequest)
			return
		}
		interval = time.Duration(seconds) * time.Second
	}
	points, used, err := s.storage.TokenUsagePoints(TokenUsageOptions{
		Since: since, Before: before, Workspace: workspace, Interval: interval,
	})
	if err != nil {
		s.logger.Printf("local: token metrics: %v", err)
		http.Error(w, "token metrics: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if points == nil {
		points = []TokenUsagePoint{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"points":           points,
		"interval_seconds": int64(used.Seconds()),
	})
	s.warmSummariesInBackground()
}

// handleConversationMetrics returns lifetime conversation identity metadata
// with all analytic fields clipped to the requested generation-time period.
func (s *Server) handleConversationMetrics(w http.ResponseWriter, r *http.Request) {
	limit, ok := limitParam(w, r, conversationListLimit)
	if !ok {
		return
	}
	since, before, workspace, ok := metricsPeriodParams(w, r)
	if !ok {
		return
	}
	rows, matched, aggregate, err := s.storage.ConversationMetrics(ConversationListOptions{
		Limit: limit, Since: since, Before: before, Workspace: workspace,
	})
	if err != nil {
		s.logger.Printf("local: conversation metrics: %v", err)
		http.Error(w, "conversation metrics: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []ConversationSummary{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"aggregate":             aggregate,
		"conversations":         rows,
		"matched_conversations": matched,
	})
	s.warmSummariesInBackground()
}

// handleToolMetrics returns period-clipped tool counts and failures per
// matching conversation.
func (s *Server) handleToolMetrics(w http.ResponseWriter, r *http.Request) {
	limit, ok := limitParam(w, r, conversationListLimit)
	if !ok {
		return
	}
	since, before, workspace, ok := metricsPeriodParams(w, r)
	if !ok {
		return
	}
	rows, err := s.storage.ToolUsage(ConversationListOptions{
		Limit: limit, Since: since, Before: before, Workspace: workspace,
	})
	if err != nil {
		s.logger.Printf("local: tool metrics: %v", err)
		http.Error(w, "tool metrics: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []ConversationToolUsage{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"conversations": rows})
	s.warmSummariesInBackground()
}

func (s *Server) handleSkillsToolsMetrics(w http.ResponseWriter, r *http.Request) {
	since, before, workspace, ok := metricsPeriodParams(w, r)
	if !ok {
		return
	}
	var interval time.Duration
	if raw := r.URL.Query().Get("interval"); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds <= 0 {
			http.Error(w, "invalid interval: want a positive number of seconds", http.StatusBadRequest)
			return
		}
		interval = time.Duration(seconds) * time.Second
	}
	tools, err := s.storage.ToolAnalytics(ToolAnalyticsOptions{
		Since: since, Before: before, Workspace: workspace, Interval: interval,
	})
	if err != nil {
		s.logger.Printf("local: skills-tools metrics: %v", err)
		http.Error(w, "skills-tools metrics: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, http.StatusOK, SkillsToolsMetricsResponse{Tools: tools})
	s.warmSummariesInBackground()
}

// metricsPeriodParams reads the half-open generation-time bounds and the
// presence-sensitive workspace filter shared by all metrics endpoints.
func metricsPeriodParams(w http.ResponseWriter, r *http.Request) (time.Time, time.Time, *string, bool) {
	parse := func(name string) (time.Time, bool) {
		values, present := r.URL.Query()[name]
		if !present {
			return time.Time{}, true
		}
		raw := ""
		if len(values) > 0 {
			raw = values[0]
		}
		value, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			http.Error(w, "invalid "+name+": want an RFC 3339 timestamp", http.StatusBadRequest)
			return time.Time{}, false
		}
		return value, true
	}
	since, ok := parse("since")
	if !ok {
		return time.Time{}, time.Time{}, nil, false
	}
	before, ok := parse("before")
	if !ok {
		return time.Time{}, time.Time{}, nil, false
	}
	var workspace *string
	if values, present := r.URL.Query()["workspace"]; present {
		value := ""
		if len(values) > 0 {
			value = values[0]
		}
		workspace = &value
	}
	return since, before, workspace, true
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
