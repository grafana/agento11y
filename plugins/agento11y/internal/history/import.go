package history

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Target is the destination for one import run. An empty Endpoint falls back to
// the configured AGENTO11Y_ENDPOINT.
//
// The loopback rules are not set here: [NewTargetExporter] decides them from
// the resolved endpoint, so an import that reaches the local daemon carries the
// forward marker and full content whether or not the caller expected a local
// destination.
type Target struct {
	Endpoint     string
	OTLPEndpoint string
	// ContentMode overrides the resolved CONTENT_CAPTURE_MODE for this import
	// only. Empty falls back to the resolved environment value; a loopback
	// endpoint always captures full content regardless.
	ContentMode string
	// Headers are sent with every generation-export and OTLP request this
	// import makes. A non-nil map also replaces the ambient OTLP header set, so
	// an import cannot leak a Cloud Authorization header to another endpoint.
	Headers map[string]string
}

// TurnExporter is the boundary between orchestration and the export pipeline.
// Handing over a turn and confirming it arrived are separate calls so an import
// can export in batches; [RunImport] marks a turn in the ledger only once a
// Flush confirms it.
type TurnExporter interface {
	ExportHistoricalGeneration(context.Context, HistoricalGeneration) error
	Flush(context.Context) error
}

// ExportFunc adapts a function to TurnExporter. Its Flush is a no-op: a
// function that has already taken the turn has nothing left to confirm.
type ExportFunc func(context.Context, HistoricalGeneration) error

func (f ExportFunc) ExportHistoricalGeneration(ctx context.Context, gen HistoricalGeneration) error {
	return f(ctx, gen)
}

func (f ExportFunc) Flush(context.Context) error { return nil }

var ErrExporterUnavailable = errors.New("history: export pipeline unavailable")

// PlanOptions controls metadata-only discovery and selection.
type PlanOptions struct {
	Agent    AgentID
	Importer Importer
	Filter   Filter
	Discover DiscoverOptions
}

// ImportPlan is the content-free result of discovery. It is safe to render
// because it holds only [SessionPreview] metadata.
type ImportPlan struct {
	Agent    AgentID
	Sessions []SessionPreview
	Skipped  []SkippedSession
	Warnings []string
	// Collisions covers every discovered session, not only the selected ones.
	// Two files claiming one session ID must be told apart the same way in
	// every run, and a run that selects one of the two cannot see the clash on
	// its own. Pass this to [ImportOptions.Collisions].
	Collisions []Collision
}

// BuildPlan discovers sessions and applies the filter without reading prompt,
// response, or tool content from any source.
func BuildPlan(ctx context.Context, opts PlanOptions) (ImportPlan, error) {
	imp, err := resolveImporter(opts.Agent, opts.Importer)
	if err != nil {
		return ImportPlan{}, err
	}
	discovery, err := Discover(ctx, opts.Agent, imp, opts.Discover)
	if err != nil {
		return ImportPlan{}, err
	}
	selected, skipped := opts.Filter.SelectSessions(discovery.Sessions)
	return ImportPlan{
		Agent:      opts.Agent,
		Sessions:   selected,
		Skipped:    skipped,
		Warnings:   append([]string(nil), discovery.Warnings...),
		Collisions: DetectCollisions(discovery.Sessions),
	}, nil
}

func resolveImporter(agent AgentID, imp Importer) (Importer, error) {
	if imp != nil {
		return imp, nil
	}
	if _, ok := Spec(agent); !ok {
		return nil, fmt.Errorf("history: unknown agent %q", agent)
	}
	imp, ok := NewImporter(agent)
	if !ok {
		return nil, fmt.Errorf("history: no importer registered for %q", agent)
	}
	return imp, nil
}

// Progress is the running tally of an import, reported after every turn so a
// caller can stream it. Sessions counts the sessions the run finished.
type Progress struct {
	Agent    AgentID
	Sessions int
	Total    int // sessions selected for this run
	Imported int
	Skipped  int
	Failed   int
}

// ImportOptions controls one import run after the caller has selected
// sessions. DryRun reads no session content and touches no ledger.
type ImportOptions struct {
	Agent    AgentID
	Importer Importer
	Filter   Filter
	Sessions []SessionPreview
	DryRun   bool
	Force    bool
	Target   Target
	Exporter TurnExporter
	Now      func() time.Time
	// OnProgress is called after each batch and at the end of each session. It
	// must not block: the viewer publishes it to the SSE hub.
	OnProgress func(Progress)
	// Ledger overrides the per-agent ledger. Tests set it; production leaves
	// it nil so RunImport opens and closes the real one.
	Ledger *Ledger
	// Collisions are the session-ID clashes the plan found across every
	// discovered session. Sessions holds one run's selection, which cannot see
	// a clash with a session it left out, so a caller that selects a subset
	// must pass [ImportPlan.Collisions] here or the two files merge into one
	// conversation.
	Collisions []Collision
}

// exportBatchSize is how many turns are handed to the exporter before delivery
// is confirmed. It matches the SDK's generation batch, so a full batch costs
// one request.
//
// Confirming every turn on its own made a large import one blocking round trip
// per turn. A full history holds hundreds of thousands of turns; at that size
// the per-turn confirmation added hours of latency.
const exportBatchSize = 100

// RunImport reads the selected sessions, sanitizes each turn, exports it, and
// records the outcome in the private ledger. It continues past a per-turn
// failure so a rerun resumes from the ledger.
//
// Turns are consumed lazily, one at a time, and are not retained after export.
// A Codex rollout of several hundred megabytes therefore costs one turn plus
// fixed parser buffers rather than the whole session.
//
// Export is batched: a turn counts as imported once a flush confirms its batch
// reached the destination, and only then is it marked in the ledger. A crash
// between the two re-exports the batch on the next run under the same
// deterministic IDs.
//
// Cancelling ctx stops the run at the next turn boundary and returns the
// partial result with ctx.Err(). Everything already exported stays in the
// ledger, so a rerun skips it.
func RunImport(ctx context.Context, opts ImportOptions) (ImportResult, error) {
	result := ImportResult{Agent: opts.Agent, Sessions: len(opts.Sessions), DryRun: opts.DryRun}
	collisions := opts.Collisions
	if collisions == nil {
		collisions = DetectCollisions(opts.Sessions)
	}
	result.Collisions = len(collisions)
	if opts.DryRun {
		return result, nil
	}
	imp, err := resolveImporter(opts.Agent, opts.Importer)
	if err != nil {
		return result, err
	}
	if opts.Exporter == nil {
		return result, ErrExporterUnavailable
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	ledger := opts.Ledger
	if ledger == nil {
		opened, err := OpenLedger(opts.Agent)
		if err != nil {
			return result, err
		}
		defer func() { _ = opened.Close() }()
		ledger = opened
	}

	collided := collisionSessionKeys(collisions)
	sanitizer := Sanitizer{}
	total := len(opts.Sessions)
	report := func(done int) {
		if opts.OnProgress == nil {
			return
		}
		opts.OnProgress(Progress{
			Agent:    opts.Agent,
			Sessions: done,
			Total:    total,
			Imported: result.Imported,
			Skipped:  result.Skipped,
			Failed:   result.Failed,
		})
	}

	// pending holds the turns handed to the exporter but not yet confirmed.
	type pendingTurn struct {
		id    SourceIdentity
		genID string
	}
	var pending []pendingTurn

	// flush confirms delivery of the turns handed over since the last flush.
	//
	// stopping says the run is already ending, so the flush skips the run's
	// context; a flush the cancellation cut short takes the same detached
	// retry. The turns are with the exporter either way, and Flush confirms
	// what it holds rather than sending it again, so the retry duplicates
	// nothing. Without this a cancelled run would report every turn of its
	// last batch as failed and export them again on the next run.
	flush := func(stopping bool) error {
		if !stopping {
			err := opts.Exporter.Flush(ctx)
			if err == nil || ctx.Err() == nil {
				return err
			}
		}
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalFlushTimeout)
		defer cancel()
		return opts.Exporter.Flush(flushCtx)
	}

	// confirm flushes the batch and records each turn's outcome. A flush
	// failure fails the whole batch: the SDK sends it as one request, so a
	// turn's fate is the batch's. The ledger keeps those failures for a rerun to
	// retry.
	confirm := func(stopping bool) error {
		if len(pending) == 0 {
			return nil
		}
		flushErr := flush(stopping)
		status, errorClass := StatusExported, ""
		if flushErr != nil {
			status, errorClass = StatusFailed, "export_failed"
		}
		stamp := now().Unix()
		for _, turn := range pending {
			// If even the mark cannot persist, the ledger is broken: stop rather
			// than keep exporting turns no rerun can account for.
			if mErr := ledger.Mark(turn.id, status, turn.genID, errorClass, stamp); mErr != nil {
				pending = nil
				return fmt.Errorf("history: record %s turn: %w", status, mErr)
			}
		}
		if flushErr != nil {
			result.Failed += len(pending)
			result.Warnings = appendWarning(result.Warnings,
				fmt.Sprintf("export of %d turns failed: %v", len(pending), flushErr))
		} else {
			result.Imported += len(pending)
		}
		pending = nil
		return nil
	}

	// confirmFinal flushes the last batch of a run that is stopping early.
	confirmFinal := func() error { return confirm(true) }

	for i, sess := range opts.Sessions {
		if err := ctx.Err(); err != nil {
			if flushErr := confirmFinal(); flushErr != nil {
				return result, flushErr
			}
			return result, err
		}
		turns := 0
		for gen, err := range imp.Turns(ctx, sess) {
			if ctxErr := ctx.Err(); ctxErr != nil {
				if flushErr := confirmFinal(); flushErr != nil {
					return result, flushErr
				}
				return result, ctxErr
			}
			if err != nil {
				result.Failed++
				// The reason a session stopped is the only record of it: the
				// ledger holds turns, and this session may have produced none.
				result.Warnings = appendWarning(result.Warnings,
					fmt.Sprintf("session %s: %v", sessionLabel(sess), err))
				break // the session's remaining turns are unreadable
			}
			if opts.Filter.MaxTurns > 0 && turns >= opts.Filter.MaxTurns {
				break
			}
			turns++

			fillSource(&gen, opts.Agent, sess)
			disambiguateCollidedConversation(&gen, collided, sess.SourcePath)
			id := gen.Source.Identity()
			if !ledger.ShouldImport(id, opts.Force) {
				result.Skipped++
				continue
			}
			sanitizer.Sanitize(&gen)
			if exportErr := opts.Exporter.ExportHistoricalGeneration(ctx, gen); exportErr != nil {
				if ctx.Err() != nil {
					// The cancellation refused the turn, not the destination:
					// the exporter checks the context before it records
					// anything. Stop the way the checks above do, so a user
					// abort does not report a turn as failed and count an
					// attempt against it.
					if flushErr := confirmFinal(); flushErr != nil {
						return result, flushErr
					}
					return result, ctx.Err()
				}
				result.Failed++
				result.Warnings = appendWarning(result.Warnings,
					fmt.Sprintf("session %s: export turn: %v", sessionLabel(sess), exportErr))
				if mErr := ledger.Mark(id, StatusFailed, gen.Source.GenerationID(), "export_failed", now().Unix()); mErr != nil {
					return result, fmt.Errorf("history: record failed turn: %w", mErr)
				}
				continue
			}
			pending = append(pending, pendingTurn{id: id, genID: gen.Source.GenerationID()})
			if len(pending) >= exportBatchSize {
				if err := confirm(false); err != nil {
					return result, err
				}
				report(i)
			}
		}
		if err := confirm(false); err != nil {
			return result, err
		}
		report(i + 1)
	}
	return result, nil
}

// finalFlushTimeout bounds the last flush of a cancelled run.
const finalFlushTimeout = 15 * time.Second

// appendWarning keeps the warning list bounded: an import over a broken
// directory can fail every session, and the list is rendered in the CLI and
// carried in an HTTP response.
func appendWarning(warnings []string, warning string) []string {
	if len(warnings) >= maxImportWarnings {
		return warnings
	}
	if len(warnings) == maxImportWarnings-1 {
		return append(warnings, "further warnings suppressed")
	}
	return append(warnings, warning)
}

const maxImportWarnings = 50

// sessionLabel names a session in a warning. The session ID is metadata, and
// the source path is what a user needs to find the file, so both appear; no
// session content does.
func sessionLabel(sess SessionPreview) string {
	if sess.SessionID != "" {
		return sess.SessionID
	}
	return sess.SourcePath
}

// fillSource completes the parts of a turn's SourceRef an importer may leave
// to the framework, so every importer does not repeat the same defaulting.
func fillSource(gen *HistoricalGeneration, agent AgentID, sess SessionPreview) {
	if gen.Source.Agent == "" {
		gen.Source.Agent = agent
	}
	if gen.Source.SessionID == "" {
		gen.Source.SessionID = sess.SessionID
	}
	if gen.Source.SourcePath == "" {
		gen.Source.SourcePath = sess.SourcePath
	}
}

type collisionSessionKey struct {
	agent     AgentID
	sessionID string
}

func collisionSessionKeys(collisions []Collision) map[collisionSessionKey]bool {
	out := map[collisionSessionKey]bool{}
	for _, c := range collisions {
		out[collisionSessionKey{agent: c.Agent, sessionID: c.SessionID}] = true
	}
	return out
}

// disambiguateCollidedConversation gives a turn a source-scoped conversation ID
// when its native session ID is claimed by more than one file, so two unrelated
// sessions do not merge into one conversation in the viewer.
//
// The scope is sessionPath, the file the session was discovered at, not the
// file the turn was read from. A Claude subagent turn comes from its own
// transcript under the session directory, and scoping by that path would break
// one conversation into one per transcript. [DetectCollisions] compares the
// same discovered paths, so the two agree on what collided.
func disambiguateCollidedConversation(gen *HistoricalGeneration, collided map[collisionSessionKey]bool, sessionPath string) {
	if gen == nil || gen.Source.SessionID == "" || sessionPath == "" {
		return
	}
	k := collisionSessionKey{agent: gen.Source.Agent, sessionID: gen.Source.SessionID}
	if !collided[k] {
		return
	}
	gen.Gen.ConversationID = "histconv-" + hashFields(
		string(gen.Source.Agent), gen.Source.SessionID, sessionPath,
	)[:24]
}
