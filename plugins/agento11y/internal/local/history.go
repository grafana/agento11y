package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/grafana/agento11y/plugins/agento11y/internal/history"
)

// historyRunHistory is how many finished import runs the daemon keeps. Run
// state is presentation only: the durable record of what was imported is the
// per-agent ledger, so a rerun after a restart resumes from it rather than
// from this list.
const historyRunHistory = 10

// Import run states. A run is terminal in every state but pending and running.
const (
	importStatusPending   = "pending"
	importStatusRunning   = "running"
	importStatusCompleted = "completed"
	importStatusFailed    = "failed"
	importStatusCancelled = "cancelled"
)

// ImportRun is one import run as the viewer sees it. Every field is a counter,
// a status, or an agent ID: nothing here carries session content.
type ImportRun struct {
	RunID      string `json:"run_id"`
	Agent      string `json:"agent"`
	Status     string `json:"status"`
	Discovered int    `json:"discovered"`
	Selected   int    `json:"selected"`
	Sessions   int    `json:"sessions"`
	Imported   int    `json:"imported"`
	Skipped    int    `json:"skipped"`
	Failed     int    `json:"failed"`
	Missing    int    `json:"missing"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	Error      string `json:"error,omitempty"`
}

func (r ImportRun) terminal() bool {
	switch r.Status {
	case importStatusCompleted, importStatusFailed, importStatusCancelled:
		return true
	default:
		return false
	}
}

// importRun is the server-side run: the public state plus its cancel function.
type importRun struct {
	state  ImportRun
	cancel context.CancelFunc
}

// SetLocalEndpoint records the address this daemon listens on, which an import
// exports to. Serve calls it once the listener has a port. Without it the
// import endpoints report that the daemon cannot reach itself rather than
// guessing a port.
func (s *Server) SetLocalEndpoint(endpoint string) {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	s.localEndpoint = endpoint
}

func (s *Server) historyEndpoint() string {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	return s.localEndpoint
}

// handleHistoryAgents lists the agents with a registered importer. The CLI, the
// API, and the viewer all read the same registry, so adding an importer needs
// no edit here or in the frontend.
func (s *Server) handleHistoryAgents(w http.ResponseWriter, _ *http.Request) {
	specs := history.Specs()
	agents := make([]map[string]any, 0, len(specs))
	for _, spec := range specs {
		aliases := spec.Aliases
		if aliases == nil {
			aliases = []string{}
		}
		agents = append(agents, map[string]any{
			"id":           string(spec.ID),
			"display_name": spec.DisplayName,
			"aliases":      aliases,
		})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
}

// historyOffer is the per-agent import offer. It is derived from metadata-only
// discovery, so rendering the banner never reads session content.
type historyOffer struct {
	Agent       string `json:"agent"`
	DisplayName string `json:"display_name"`
	Sessions    int    `json:"sessions"`
	Turns       int    `json:"turns"`
	ApproxTurns bool   `json:"approx_turns"`
	Show        bool   `json:"show"`
}

// handleHistoryOffer reports, per agent, whether importable history exists and
// whether the offer has already been answered.
func (s *Server) handleHistoryOffer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	offers := make([]historyOffer, 0, len(history.Specs()))
	for _, spec := range history.Specs() {
		offer := historyOffer{Agent: string(spec.ID), DisplayName: spec.DisplayName}
		show, err := history.ShouldOfferPrompt(spec.ID)
		if err != nil {
			s.logger.Printf("local: history offer state %s: %v", spec.ID, err)
		}
		if show {
			plan, err := history.BuildPlan(ctx, history.PlanOptions{
				Agent:  spec.ID,
				Filter: defaultHistoryFilter(s.now()),
			})
			if err != nil {
				s.logger.Printf("local: history offer plan %s: %v", spec.ID, err)
			} else {
				offer.Sessions = len(plan.Sessions)
				for _, sess := range plan.Sessions {
					offer.Turns += sess.TurnCount
					offer.ApproxTurns = offer.ApproxTurns || sess.ApproxTurns
				}
			}
		}
		offer.Show = show && offer.Sessions > 0
		offers = append(offers, offer)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"offers": offers})
}

// handleHistoryDismiss records that the user does not want the import offer.
// An empty agent dismisses every registered one. The banner's single "not now"
// sends that form.
func (s *Server) handleHistoryDismiss(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Agent string `json:"agent"`
	}
	if err := decodeHistoryBody(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	agents := history.AgentIDs()
	if strings.TrimSpace(body.Agent) != "" {
		agent, ok := history.Resolve(body.Agent)
		if !ok {
			http.Error(w, "unknown agent "+strconv.Quote(body.Agent), http.StatusBadRequest)
			return
		}
		agents = []history.AgentID{agent}
	}
	for _, agent := range agents {
		if err := history.MarkPrompt(agent, history.PromptSkipped); err != nil {
			http.Error(w, "record dismissal: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"dismissed": true})
}

// historySessionJSON is one discovered session in an API response. It mirrors
// history.SessionPreview and, like it, carries no prompt or response text.
type historySessionJSON struct {
	SessionID      string `json:"session_id"`
	Title          string `json:"title"`
	Workspace      string `json:"workspace"`
	SourcePath     string `json:"source_path"`
	TurnCount      int    `json:"turn_count"`
	ApproxTurns    bool   `json:"approx_turns"`
	SizeBytes      int64  `json:"size_bytes"`
	StartedAt      string `json:"started_at,omitempty"`
	LastActivityAt string `json:"last_activity_at,omitempty"`
	Active         bool   `json:"active"`
}

func historySessionsJSON(sessions []history.SessionPreview) []historySessionJSON {
	out := make([]historySessionJSON, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, historySessionJSON{
			SessionID:      s.SessionID,
			Title:          s.Title,
			Workspace:      s.Workspace,
			SourcePath:     s.SourcePath,
			TurnCount:      s.TurnCount,
			ApproxTurns:    s.ApproxTurns,
			SizeBytes:      s.SizeBytes,
			StartedAt:      formatHistoryTime(s.StartedAt),
			LastActivityAt: formatHistoryTime(s.LastActivityAt),
			Active:         s.Active,
		})
	}
	return out
}

func formatHistoryTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// handleHistoryPlan runs metadata-only discovery and returns what an import
// would cover. It reads no prompt, response, thinking, or tool text.
func (s *Server) handleHistoryPlan(w http.ResponseWriter, r *http.Request) {
	agent, ok := s.historyAgentParam(w, r.URL.Query().Get("agent"))
	if !ok {
		return
	}
	filter, ok := s.historyFilterFromQuery(w, r)
	if !ok {
		return
	}
	plan, err := history.BuildPlan(r.Context(), history.PlanOptions{Agent: agent, Filter: filter})
	if err != nil {
		s.logger.Printf("local: history plan %s: %v", agent, err)
		http.Error(w, "history plan: "+err.Error(), http.StatusInternalServerError)
		return
	}
	skipped := make([]map[string]any, 0, len(plan.Skipped))
	for _, sk := range plan.Skipped {
		skipped = append(skipped, map[string]any{
			"session_id": sk.Session.SessionID,
			"reason":     string(sk.Reason),
		})
	}
	warnings := plan.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"agent":    string(agent),
		"since":    formatHistoryTime(filter.Since),
		"until":    formatHistoryTime(filter.Until),
		"sessions": historySessionsJSON(plan.Sessions),
		"skipped":  skipped,
		"warnings": warnings,
	})
}

// historyImportRequest is the body of POST /api/v1/history:import.
//
// SourcePaths names the sessions the user picked. They are matched against a
// fresh discovery result rather than read directly: a plan can be minutes old,
// and a path from it must not become a file the daemon reads on request.
type historyImportRequest struct {
	Agent       string   `json:"agent"`
	SourcePaths []string `json:"source_paths"`
	Since       string   `json:"since"`
	Until       string   `json:"until"`
	Workspace   string   `json:"workspace"`
	MaxSessions int      `json:"max_sessions"`
	MaxTurns    int      `json:"max_turns"`
	Force       bool     `json:"force"`
}

func (s *Server) handleHistoryImport(w http.ResponseWriter, r *http.Request) {
	var body historyImportRequest
	if err := decodeHistoryBody(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	agent, ok := s.historyAgentParam(w, body.Agent)
	if !ok {
		return
	}
	endpoint := s.historyEndpoint()
	if endpoint == "" {
		http.Error(w, "this daemon does not know its own address, so it cannot import", http.StatusServiceUnavailable)
		return
	}

	filter := defaultHistoryFilter(s.now())
	var err error
	if filter.Since, err = parseHistoryTime(body.Since, filter.Since); err != nil {
		http.Error(w, "invalid since: "+err.Error(), http.StatusBadRequest)
		return
	}
	if filter.Until, err = parseHistoryTime(body.Until, time.Time{}); err != nil {
		http.Error(w, "invalid until: "+err.Error(), http.StatusBadRequest)
		return
	}
	filter.Workspace = strings.TrimSpace(body.Workspace)
	filter.MaxSessions = body.MaxSessions
	filter.MaxTurns = body.MaxTurns

	run := ImportRun{
		RunID:     uuid.NewString(),
		Agent:     string(agent),
		Status:    importStatusPending,
		StartedAt: s.now().Format(time.RFC3339Nano),
	}
	ctx, cancel := context.WithCancel(context.Background())
	if !s.startImportRun(run, cancel) {
		cancel()
		http.Error(w, "an import is already running", http.StatusConflict)
		return
	}

	// The daemon imports into itself. history.NewTargetExporter recognises the
	// loopback endpoint and gives the import full content plus the forward
	// marker, so the backfill is stored here and never relayed to Grafana
	// Cloud.
	target := history.Target{
		Endpoint:     endpoint,
		OTLPEndpoint: endpoint + "/otlp",
	}
	go s.runImport(ctx, cancel, run.RunID, agent, filter, body.SourcePaths, body.Force, target)

	s.writeJSON(w, http.StatusAccepted, map[string]any{"run_id": run.RunID, "status": run.Status})
}

// startImportRun registers a run if none is active. It returns false when one
// already is, and the handler turns that into HTTP 409 rather than letting two
// runs write the same ledger.
func (s *Server) startImportRun(run ImportRun, cancel context.CancelFunc) bool {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	if s.activeImport != "" {
		return false
	}
	s.activeImport = run.RunID
	s.importRuns = append(s.importRuns, &importRun{state: run, cancel: cancel})
	if len(s.importRuns) > historyRunHistory {
		s.importRuns = s.importRuns[len(s.importRuns)-historyRunHistory:]
	}
	return true
}

// updateImportRun applies mutate to a run and publishes the new state over the
// existing SSE hub.
func (s *Server) updateImportRun(runID string, mutate func(*ImportRun)) {
	if updated, ok := s.applyImportRun(runID, mutate); ok {
		s.hub.broadcast(changeEvent{Import: &updated})
	}
}

// applyImportRun applies mutate to a run under the lock and returns the new
// state without publishing it. A run that has reached a terminal state frees
// the single import slot.
func (s *Server) applyImportRun(runID string, mutate func(*ImportRun)) (ImportRun, bool) {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	for _, run := range s.importRuns {
		if run.state.RunID != runID {
			continue
		}
		mutate(&run.state)
		if run.state.terminal() && s.activeImport == runID {
			s.activeImport = ""
		}
		return run.state, true
	}
	return ImportRun{}, false
}

// importProgressInterval is how often per-turn progress reaches the SSE hub. A
// large import exports hundreds of thousands of turns, and one frame per turn
// would fan out to every open viewer for a counter that only has to look live.
// Run state itself is updated on every turn, so the status endpoint stays
// exact.
const importProgressInterval = 250 * time.Millisecond

func (s *Server) importRunState(runID string) (ImportRun, bool) {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	for _, run := range s.importRuns {
		if run.state.RunID == runID {
			return run.state, true
		}
	}
	return ImportRun{}, false
}

// runImport executes one import off the request goroutine. The handler has
// already returned a run ID, so every outcome is reported through run state and
// SSE rather than through an HTTP status.
func (s *Server) runImport(
	ctx context.Context,
	cancel context.CancelFunc,
	runID string,
	agent history.AgentID,
	filter history.Filter,
	sourcePaths []string,
	force bool,
	target history.Target,
) {
	defer cancel()

	finish := func(status, message string) {
		s.updateImportRun(runID, func(run *ImportRun) {
			run.Status = status
			run.Error = message
			run.FinishedAt = s.now().Format(time.RFC3339Nano)
		})
	}

	s.updateImportRun(runID, func(run *ImportRun) { run.Status = importStatusRunning })

	// Discovery runs again here rather than trusting the request. The plan the
	// user acted on can be minutes old, and a source path from it must be
	// something discovery still finds, not a path the daemon opens on request.
	plan, err := history.BuildPlan(ctx, history.PlanOptions{Agent: agent, Filter: filter})
	if err != nil {
		s.terminateImport(ctx, runID, finish, err)
		return
	}
	selected, missing := selectHistorySessions(plan.Sessions, sourcePaths)
	s.updateImportRun(runID, func(run *ImportRun) {
		run.Discovered = len(plan.Sessions)
		run.Selected = len(selected)
		run.Missing = missing
	})

	exporter, cleanup, err := history.NewTargetExporter(ctx, target, s.logger)
	if err != nil {
		s.terminateImport(ctx, runID, finish, err)
		return
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()
		if err := cleanup(shutdownCtx); err != nil {
			s.logger.Printf("local: shut down import exporter: %v", err)
		}
	}()

	lastPublished := time.Time{}
	result, err := history.RunImport(ctx, history.ImportOptions{
		Agent:      agent,
		Filter:     filter,
		Sessions:   selected,
		Collisions: plan.Collisions,
		Force:      force,
		Target:     target,
		Exporter:   exporter,
		OnProgress: func(p history.Progress) {
			updated, ok := s.applyImportRun(runID, func(run *ImportRun) {
				run.Sessions = p.Sessions
				run.Imported = p.Imported
				run.Skipped = p.Skipped
				run.Failed = p.Failed
			})
			now := s.now()
			if !ok || now.Sub(lastPublished) < importProgressInterval {
				return
			}
			lastPublished = now
			s.hub.broadcast(changeEvent{Import: &updated})
		},
	})
	// Sessions stays at the count the progress callback reported, which is the
	// number of sessions the run finished. result.Sessions is the number it
	// selected, and a cancelled run must keep its partial counters.
	s.updateImportRun(runID, func(run *ImportRun) {
		run.Imported = result.Imported
		run.Skipped = result.Skipped
		run.Failed = result.Failed
	})
	for _, warning := range result.Warnings {
		s.logger.Printf("local: history import %s: %s", runID, warning)
	}
	if err != nil {
		s.terminateImport(ctx, runID, finish, err)
		return
	}
	if result.Failed > 0 && result.Imported == 0 {
		// Nothing was imported. Reporting that as completed would hide the
		// failure and answer the offer, so the banner would never come back.
		finish(importStatusFailed, fmt.Sprintf("every turn failed to export (%d)", result.Failed))
		return
	}
	// A completed import answers the offer, whether it exported turns or found
	// them all already imported. A run that selected no session answers nothing:
	// the filter or the request's source paths matched none, and recording a
	// decision would hide the banner for good. The CLI returns without recording
	// one in the same case.
	if len(selected) > 0 {
		if err := history.MarkPrompt(agent, history.PromptImported); err != nil {
			s.logger.Printf("local: record import prompt state %s: %v", agent, err)
		}
	}
	finish(importStatusCompleted, "")
}

// terminateImport ends a run as cancelled or failed. A cancelled run keeps its
// partial counters: the ledger recorded every turn already exported, so a rerun
// resumes rather than repeating them.
func (s *Server) terminateImport(ctx context.Context, runID string, finish func(status, message string), err error) {
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		finish(importStatusCancelled, "")
		return
	}
	s.logger.Printf("local: history import %s: %v", runID, err)
	finish(importStatusFailed, err.Error())
}

// selectHistorySessions keeps the freshly discovered sessions the request asked
// for and reports how many of its paths are gone. An empty request means every
// discovered session.
func selectHistorySessions(discovered []history.SessionPreview, sourcePaths []string) ([]history.SessionPreview, int) {
	if len(sourcePaths) == 0 {
		return discovered, 0
	}
	want := make(map[string]bool, len(sourcePaths))
	for _, p := range sourcePaths {
		if p = strings.TrimSpace(p); p != "" {
			want[p] = true
		}
	}
	selected := make([]history.SessionPreview, 0, len(want))
	for _, sess := range discovered {
		if want[sess.SourcePath] {
			selected = append(selected, sess)
			delete(want, sess.SourcePath)
		}
	}
	return selected, len(want)
}

func (s *Server) handleHistoryRunStatus(w http.ResponseWriter, r *http.Request) {
	run, ok := s.importRunState(r.PathValue("runID"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.writeJSON(w, http.StatusOK, run)
}

// handleHistoryRunCancel stops an active run at the next turn boundary.
// Cancelling a finished run is not an error: the caller wanted it stopped, and
// it is.
//
// The route matches the whole last path segment, so the runID and the action
// are split here: only "<runID>:cancel" is an action this endpoint serves.
func (s *Server) handleHistoryRunCancel(w http.ResponseWriter, r *http.Request) {
	runID, action, ok := strings.Cut(r.PathValue("runAction"), ":")
	if !ok || action != "cancel" {
		http.NotFound(w, r)
		return
	}
	s.importMu.Lock()
	var cancel context.CancelFunc
	var state ImportRun
	found := false
	for _, run := range s.importRuns {
		if run.state.RunID != runID {
			continue
		}
		found = true
		state = run.state
		cancel = run.cancel
		break
	}
	s.importMu.Unlock()
	if !found {
		http.NotFound(w, r)
		return
	}
	if !state.terminal() && cancel != nil {
		cancel()
	}
	s.writeJSON(w, http.StatusAccepted, map[string]any{"run_id": runID, "status": state.Status})
}

// defaultHistoryFilter is the viewer's starting selection: the previous 90 days,
// skipping sessions an agent may still be writing.
func defaultHistoryFilter(now time.Time) history.Filter {
	f := history.NewFilter()
	f.Since = now.Add(-history.DefaultSinceWindow)
	return f
}

func (s *Server) historyAgentParam(w http.ResponseWriter, raw string) (history.AgentID, bool) {
	if strings.TrimSpace(raw) == "" {
		http.Error(w, "missing agent", http.StatusBadRequest)
		return "", false
	}
	agent, ok := history.Resolve(raw)
	if !ok {
		http.Error(w, "unknown agent "+strconv.Quote(raw), http.StatusBadRequest)
		return "", false
	}
	return agent, true
}

func (s *Server) historyFilterFromQuery(w http.ResponseWriter, r *http.Request) (history.Filter, bool) {
	q := r.URL.Query()
	filter := defaultHistoryFilter(s.now())
	var err error
	if filter.Since, err = parseHistoryTime(q.Get("since"), filter.Since); err != nil {
		http.Error(w, "invalid since: "+err.Error(), http.StatusBadRequest)
		return filter, false
	}
	if filter.Until, err = parseHistoryTime(q.Get("until"), time.Time{}); err != nil {
		http.Error(w, "invalid until: "+err.Error(), http.StatusBadRequest)
		return filter, false
	}
	filter.Workspace = strings.TrimSpace(q.Get("workspace"))
	if raw := strings.TrimSpace(q.Get("max_sessions")); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n < 0 {
			http.Error(w, "invalid max_sessions: want a non-negative number", http.StatusBadRequest)
			return filter, false
		}
		filter.MaxSessions = n
	}
	return filter, true
}

// parseHistoryTime accepts an RFC3339 timestamp or an empty string, which
// yields fallback.
func parseHistoryTime(raw string, fallback time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, errors.New("want an RFC 3339 timestamp")
	}
	return t, nil
}

// decodeHistoryBody reads a small JSON body. An empty body is valid and leaves
// the target at its zero value, so a dismissal with no fields still works.
func decodeHistoryBody(r *http.Request, target any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxHookBodyBytes))
	if err := dec.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("decode body: %w", err)
	}
	return nil
}
