package history

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
)

// recordingExporter captures every turn handed to it and can fail on demand,
// either when the turn is handed over or when its batch is flushed.
type recordingExporter struct {
	got    []HistoricalGeneration
	failOn func(HistoricalGeneration) error
	// onFlush, when set, decides a flush's outcome from its context and its
	// ordinal, so a test can cut one flush short and let the retry succeed.
	onFlush  func(ctx context.Context, flushes int) error
	flushErr error
	flushes  int
	// pending counts the turns handed over since the last flush. Its high-water
	// mark is how the tests prove the exporter is used in batches.
	pending    int
	maxPending int
}

func (e *recordingExporter) ExportHistoricalGeneration(_ context.Context, gen HistoricalGeneration) error {
	if e.failOn != nil {
		if err := e.failOn(gen); err != nil {
			return err
		}
	}
	e.got = append(e.got, gen)
	e.pending++
	e.maxPending = max(e.maxPending, e.pending)
	return nil
}

func (e *recordingExporter) Flush(ctx context.Context) error {
	e.flushes++
	e.pending = 0
	if e.onFlush != nil {
		return e.onFlush(ctx, e.flushes)
	}
	return e.flushErr
}

func (e *recordingExporter) turnIDs() []string {
	out := make([]string, len(e.got))
	for i, g := range e.got {
		out[i] = g.Source.TurnID
	}
	return out
}

// turnSeq yields the given turns, then the given error when it is non-nil.
func turnSeq(gens []HistoricalGeneration, err error) iter.Seq2[HistoricalGeneration, error] {
	return func(yield func(HistoricalGeneration, error) bool) {
		for _, g := range gens {
			if !yield(g, nil) {
				return
			}
		}
		if err != nil {
			yield(HistoricalGeneration{}, err)
		}
	}
}

func turn(id string, opts ...func(*HistoricalGeneration)) HistoricalGeneration {
	g := HistoricalGeneration{
		Source: SourceRef{TurnID: id},
		Gen: agento11y.Generation{
			ConversationID: "session-1",
			Model:          agento11y.ModelRef{Provider: "anthropic", Name: "claude"},
		},
	}
	for _, opt := range opts {
		opt(&g)
	}
	return g
}

func testLedger(t *testing.T) *Ledger {
	t.Helper()
	pinStateHome(t)
	return openTestLedger(t, filepath.Join(t.TempDir(), "ledger.jsonl"))
}

func sessionAt(path string) SessionPreview {
	return SessionPreview{
		Agent:      AgentClaudeCode,
		SessionID:  "session-1",
		SourcePath: path,
		TurnCount:  3,
	}
}

func TestRunImportExportsAndRecords(t *testing.T) {
	exp := &recordingExporter{}
	imp := &stubImporter{
		turns: func(context.Context, SessionPreview) iter.Seq2[HistoricalGeneration, error] {
			return turnSeq([]HistoricalGeneration{turn("t1"), turn("t2")}, nil)
		},
	}
	ledger := testLedger(t)

	got, err := RunImport(context.Background(), ImportOptions{
		Agent:    AgentClaudeCode,
		Importer: imp,
		Sessions: []SessionPreview{sessionAt("/a.jsonl")},
		Exporter: exp,
		Ledger:   ledger,
	})
	if err != nil {
		t.Fatalf("RunImport: %v", err)
	}
	if got.Imported != 2 || got.Skipped != 0 || got.Failed != 0 {
		t.Fatalf("result = %+v, want 2 imported", got)
	}
	if !equalStrings(exp.turnIDs(), []string{"t1", "t2"}) {
		t.Fatalf("exported turns = %v", exp.turnIDs())
	}
	for _, g := range exp.got {
		if g.Source.Agent != AgentClaudeCode {
			t.Fatalf("turn %q has agent %q", g.Source.TurnID, g.Source.Agent)
		}
		if g.Source.SourcePath != "/a.jsonl" {
			t.Fatalf("turn %q has source path %q", g.Source.TurnID, g.Source.SourcePath)
		}
		if g.Source.SessionID != "session-1" {
			t.Fatalf("turn %q has session %q", g.Source.TurnID, g.Source.SessionID)
		}
		if !ledger.ShouldImport(g.Source.Identity(), true) {
			t.Fatal("force should always re-import")
		}
		if ledger.ShouldImport(g.Source.Identity(), false) {
			t.Fatalf("turn %q was not recorded as exported", g.Source.TurnID)
		}
	}
}

func TestRunImportIsIdempotent(t *testing.T) {
	newRun := func(exp TurnExporter, ledger *Ledger, force bool) (ImportResult, error) {
		return RunImport(context.Background(), ImportOptions{
			Agent: AgentClaudeCode,
			Importer: &stubImporter{
				turns: func(context.Context, SessionPreview) iter.Seq2[HistoricalGeneration, error] {
					return turnSeq([]HistoricalGeneration{turn("t1"), turn("t2")}, nil)
				},
			},
			Sessions: []SessionPreview{sessionAt("/a.jsonl")},
			Exporter: exp,
			Force:    force,
			Ledger:   ledger,
		})
	}

	ledger := testLedger(t)
	first := &recordingExporter{}
	if _, err := newRun(first, ledger, false); err != nil {
		t.Fatalf("first run: %v", err)
	}

	second := &recordingExporter{}
	got, err := newRun(second, ledger, false)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got.Imported != 0 || got.Skipped != 2 {
		t.Fatalf("second run = %+v, want 0 imported and 2 skipped", got)
	}
	if len(second.got) != 0 {
		t.Fatalf("second run exported %d turns", len(second.got))
	}

	forced := &recordingExporter{}
	got, err = newRun(forced, ledger, true)
	if err != nil {
		t.Fatalf("forced run: %v", err)
	}
	if got.Imported != 2 || got.Skipped != 0 {
		t.Fatalf("forced run = %+v, want 2 imported", got)
	}
	for i := range forced.got {
		if forced.got[i].Source.GenerationID() != first.got[i].Source.GenerationID() {
			t.Fatalf("force changed the deterministic generation ID for turn %d", i)
		}
	}
}

func TestRunImportContinuesPastAFailedTurn(t *testing.T) {
	exp := &recordingExporter{failOn: func(g HistoricalGeneration) error {
		if g.Source.TurnID == "t2" {
			return errors.New("export refused")
		}
		return nil
	}}
	ledger := testLedger(t)

	got, err := RunImport(context.Background(), ImportOptions{
		Agent: AgentClaudeCode,
		Importer: &stubImporter{
			turns: func(context.Context, SessionPreview) iter.Seq2[HistoricalGeneration, error] {
				return turnSeq([]HistoricalGeneration{turn("t1"), turn("t2"), turn("t3")}, nil)
			},
		},
		Sessions: []SessionPreview{sessionAt("/a.jsonl")},
		Exporter: exp,
		Ledger:   ledger,
	})
	if err != nil {
		t.Fatalf("RunImport: %v", err)
	}
	if got.Imported != 2 || got.Failed != 1 {
		t.Fatalf("result = %+v, want 2 imported and 1 failed", got)
	}
	if !equalStrings(exp.turnIDs(), []string{"t1", "t3"}) {
		t.Fatalf("exported turns = %v", exp.turnIDs())
	}
	failed := SourceRef{Agent: AgentClaudeCode, SessionID: "session-1", SourcePath: "/a.jsonl", TurnID: "t2", TurnIndex: 0}
	entry, ok := ledger.Status(failed.Identity())
	if !ok || entry.Status != StatusFailed || entry.ErrorClass != "export_failed" {
		t.Fatalf("failed turn ledger entry = %+v (found %v)", entry, ok)
	}
}

// TestRunImportRecordsAnUnreadableSession covers a session whose turn stream
// stops with an error. The run keeps whatever the stream produced first, counts
// the session as failed, and names the error in a warning.
func TestRunImportRecordsAnUnreadableSession(t *testing.T) {
	tests := []struct {
		name         string
		yielded      []HistoricalGeneration
		readErr      error
		wantImported int
		wantFailed   int
	}{
		{
			name:         "the stream fails after a turn",
			yielded:      []HistoricalGeneration{turn("t1")},
			readErr:      errors.New("torn transcript"),
			wantImported: 1,
			wantFailed:   1,
		},
		{
			name:       "the stream fails before any turn",
			readErr:    errors.New("permission denied"),
			wantFailed: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RunImport(context.Background(), ImportOptions{
				Agent: AgentClaudeCode,
				Importer: &stubImporter{
					turns: func(context.Context, SessionPreview) iter.Seq2[HistoricalGeneration, error] {
						return turnSeq(tt.yielded, tt.readErr)
					},
				},
				Sessions: []SessionPreview{sessionAt("/a.jsonl")},
				Exporter: &recordingExporter{},
				Ledger:   testLedger(t),
			})
			if err != nil {
				t.Fatalf("RunImport: %v", err)
			}
			if got.Imported != tt.wantImported || got.Failed != tt.wantFailed {
				t.Fatalf("result = %+v, want %d imported and %d failed", got, tt.wantImported, tt.wantFailed)
			}
			if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], tt.readErr.Error()) {
				t.Fatalf("warnings = %v, want one naming %q", got.Warnings, tt.readErr)
			}
		})
	}
}

// TestRunImportConsumesTurnsLazily pins the streaming requirement: a session's
// turns must not all be produced before the first is exported, so a 700 MB
// rollout costs one turn rather than the whole session.
func TestRunImportConsumesTurnsLazily(t *testing.T) {
	const total = 50
	produced := 0
	maxAhead := 0
	exported := 0

	exp := ExportFunc(func(context.Context, HistoricalGeneration) error {
		exported++
		if ahead := produced - exported; ahead > maxAhead {
			maxAhead = ahead
		}
		return nil
	})
	imp := &stubImporter{
		turns: func(context.Context, SessionPreview) iter.Seq2[HistoricalGeneration, error] {
			return func(yield func(HistoricalGeneration, error) bool) {
				for i := range total {
					produced++
					if !yield(turn(fmt.Sprintf("t%d", i)), nil) {
						return
					}
				}
			}
		},
	}

	got, err := RunImport(context.Background(), ImportOptions{
		Agent:    AgentClaudeCode,
		Importer: imp,
		Sessions: []SessionPreview{sessionAt("/a.jsonl")},
		Exporter: exp,
		Ledger:   testLedger(t),
	})
	if err != nil {
		t.Fatalf("RunImport: %v", err)
	}
	if got.Imported != total {
		t.Fatalf("imported %d turns, want %d", got.Imported, total)
	}
	if maxAhead > 1 {
		t.Fatalf("the importer ran %d turns ahead of export; turns are not consumed lazily", maxAhead)
	}
}

func TestRunImportStopsReadingAtMaxTurns(t *testing.T) {
	produced := 0
	imp := &stubImporter{
		turns: func(context.Context, SessionPreview) iter.Seq2[HistoricalGeneration, error] {
			return func(yield func(HistoricalGeneration, error) bool) {
				for i := range 100 {
					produced++
					if !yield(turn(fmt.Sprintf("t%d", i)), nil) {
						return
					}
				}
			}
		},
	}
	exp := &recordingExporter{}
	got, err := RunImport(context.Background(), ImportOptions{
		Agent:    AgentClaudeCode,
		Importer: imp,
		Filter:   Filter{MaxTurns: 3},
		Sessions: []SessionPreview{sessionAt("/a.jsonl")},
		Exporter: exp,
		Ledger:   testLedger(t),
	})
	if err != nil {
		t.Fatalf("RunImport: %v", err)
	}
	if got.Imported != 3 {
		t.Fatalf("imported %d turns, want 3", got.Imported)
	}
	if produced > 4 {
		t.Fatalf("the importer produced %d turns for a 3-turn cap", produced)
	}
}

func TestRunImportAppliesMaxTurnsPerSession(t *testing.T) {
	imp := &stubImporter{
		turns: func(_ context.Context, sess SessionPreview) iter.Seq2[HistoricalGeneration, error] {
			return turnSeq([]HistoricalGeneration{
				turn(sess.SourcePath + "-t1"),
				turn(sess.SourcePath + "-t2"),
			}, nil)
		},
	}
	exp := &recordingExporter{}
	got, err := RunImport(context.Background(), ImportOptions{
		Agent:    AgentClaudeCode,
		Importer: imp,
		Filter:   Filter{MaxTurns: 1},
		Sessions: []SessionPreview{sessionAt("/a.jsonl"), sessionAt("/b.jsonl")},
		Exporter: exp,
		Ledger:   testLedger(t),
	})
	if err != nil {
		t.Fatalf("RunImport: %v", err)
	}
	if got.Imported != 2 {
		t.Fatalf("imported %d turns, want one per session", got.Imported)
	}
	if !equalStrings(exp.turnIDs(), []string{"/a.jsonl-t1", "/b.jsonl-t1"}) {
		t.Fatalf("exported turns = %v", exp.turnIDs())
	}
}

func TestRunImportCancellationKeepsPartialProgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	exp := ExportFunc(func(_ context.Context, g HistoricalGeneration) error {
		if g.Source.TurnID == "t2" {
			cancel()
		}
		return nil
	})
	ledger := testLedger(t)

	got, err := RunImport(ctx, ImportOptions{
		Agent: AgentClaudeCode,
		Importer: &stubImporter{
			turns: func(context.Context, SessionPreview) iter.Seq2[HistoricalGeneration, error] {
				return turnSeq([]HistoricalGeneration{turn("t1"), turn("t2"), turn("t3")}, nil)
			},
		},
		Sessions: []SessionPreview{sessionAt("/a.jsonl")},
		Exporter: exp,
		Ledger:   ledger,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunImport error = %v, want context.Canceled", err)
	}
	if got.Imported != 2 {
		t.Fatalf("imported %d turns before cancellation, want 2", got.Imported)
	}

	// A rerun resumes: the two exported turns are skipped and only the third
	// is imported.
	rerun := &recordingExporter{}
	got, err = RunImport(context.Background(), ImportOptions{
		Agent: AgentClaudeCode,
		Importer: &stubImporter{
			turns: func(context.Context, SessionPreview) iter.Seq2[HistoricalGeneration, error] {
				return turnSeq([]HistoricalGeneration{turn("t1"), turn("t2"), turn("t3")}, nil)
			},
		},
		Sessions: []SessionPreview{sessionAt("/a.jsonl")},
		Exporter: rerun,
		Ledger:   ledger,
	})
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if got.Imported != 1 || got.Skipped != 2 {
		t.Fatalf("rerun = %+v, want 1 imported and 2 skipped", got)
	}
	if !equalStrings(rerun.turnIDs(), []string{"t3"}) {
		t.Fatalf("rerun exported %v, want only t3", rerun.turnIDs())
	}
}

// TestRunImportCancelledFlushDoesNotFailTheBatch pins that a user abort is not
// a transport failure. The cancellation cuts the flush short, but the turns are
// already with the exporter, so the run confirms them on a context that
// outlives the abort instead of reporting a batch that nothing was wrong with
// as failed.
func TestRunImportCancelledFlushDoesNotFailTheBatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	exp := &recordingExporter{}
	exp.onFlush = func(flushCtx context.Context, flushes int) error {
		if flushes == 1 {
			cancel()
			return context.Canceled
		}
		return flushCtx.Err()
	}
	second := sessionAt("/b.jsonl")
	second.SessionID = "session-2"
	ledger := testLedger(t)

	got, err := RunImport(ctx, ImportOptions{
		Agent: AgentClaudeCode,
		Importer: &stubImporter{
			turns: func(context.Context, SessionPreview) iter.Seq2[HistoricalGeneration, error] {
				return turnSeq([]HistoricalGeneration{turn("t1")}, nil)
			},
		},
		Sessions: []SessionPreview{sessionAt("/a.jsonl"), second},
		Exporter: exp,
		Ledger:   ledger,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunImport error = %v, want context.Canceled", err)
	}
	if got.Imported != 1 || got.Failed != 0 {
		t.Fatalf("result = %+v, want 1 imported and none failed", got)
	}
	if len(got.Warnings) != 0 {
		t.Fatalf("a user abort produced warnings: %v", got.Warnings)
	}
	exported := SourceRef{Agent: AgentClaudeCode, SessionID: "session-1", SourcePath: "/a.jsonl", TurnID: "t1", TurnIndex: 0}
	entry, ok := ledger.Status(exported.Identity())
	if !ok || entry.Status != StatusExported {
		t.Fatalf("ledger entry = %+v (found %v), want the turn recorded as exported", entry, ok)
	}
}

// TestRunImportCancelledExportDoesNotFailTheTurn covers the same abort one step
// earlier, while a turn is being handed over. The exporter checks the context
// before it records anything, so the turn never left and must not be counted or
// recorded as failed.
func TestRunImportCancelledExportDoesNotFailTheTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	exp := &recordingExporter{}
	exp.failOn = func(g HistoricalGeneration) error {
		if g.Source.TurnID != "t2" {
			return nil
		}
		cancel()
		return context.Canceled
	}
	ledger := testLedger(t)

	got, err := RunImport(ctx, ImportOptions{
		Agent: AgentClaudeCode,
		Importer: &stubImporter{
			turns: func(context.Context, SessionPreview) iter.Seq2[HistoricalGeneration, error] {
				return turnSeq([]HistoricalGeneration{turn("t1"), turn("t2"), turn("t3")}, nil)
			},
		},
		Sessions: []SessionPreview{sessionAt("/a.jsonl")},
		Exporter: exp,
		Ledger:   ledger,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunImport error = %v, want context.Canceled", err)
	}
	if got.Imported != 1 || got.Failed != 0 {
		t.Fatalf("result = %+v, want 1 imported and none failed", got)
	}
	if len(got.Warnings) != 0 {
		t.Fatalf("a user abort produced warnings: %v", got.Warnings)
	}
	refused := SourceRef{Agent: AgentClaudeCode, SessionID: "session-1", SourcePath: "/a.jsonl", TurnID: "t2", TurnIndex: 0}
	if entry, ok := ledger.Status(refused.Identity()); ok {
		t.Fatalf("ledger entry = %+v for the turn the abort refused, want none so a rerun retries it", entry)
	}
}

func TestRunImportReportsProgress(t *testing.T) {
	var last Progress
	updates := 0
	_, err := RunImport(context.Background(), ImportOptions{
		Agent: AgentClaudeCode,
		Importer: &stubImporter{
			turns: func(context.Context, SessionPreview) iter.Seq2[HistoricalGeneration, error] {
				return turnSeq([]HistoricalGeneration{turn("t1"), turn("t2")}, nil)
			},
		},
		Sessions:   []SessionPreview{sessionAt("/a.jsonl")},
		Exporter:   &recordingExporter{},
		Ledger:     testLedger(t),
		OnProgress: func(p Progress) { last = p; updates++ },
	})
	if err != nil {
		t.Fatalf("RunImport: %v", err)
	}
	if updates < 1 {
		t.Fatalf("progress reported %d times, want at least one per session", updates)
	}
	want := Progress{Agent: AgentClaudeCode, Sessions: 1, Total: 1, Imported: 2}
	if last != want {
		t.Fatalf("final progress = %+v, want %+v", last, want)
	}
}

// TestRunImportExportsInBatches pins the batching contract from both ends: a
// turn is not confirmed on its own, and a long session still reports progress
// before it ends.
func TestRunImportExportsInBatches(t *testing.T) {
	const total = exportBatchSize*2 + 5
	gens := make([]HistoricalGeneration, total)
	for i := range gens {
		gens[i] = turn(fmt.Sprintf("t%03d", i))
	}
	exp := &recordingExporter{}
	updates := 0
	got, err := RunImport(context.Background(), ImportOptions{
		Agent: AgentClaudeCode,
		Importer: &stubImporter{
			turns: func(context.Context, SessionPreview) iter.Seq2[HistoricalGeneration, error] {
				return turnSeq(gens, nil)
			},
		},
		Sessions:   []SessionPreview{sessionAt("/a.jsonl")},
		Exporter:   exp,
		Ledger:     testLedger(t),
		OnProgress: func(Progress) { updates++ },
	})
	if err != nil {
		t.Fatalf("RunImport: %v", err)
	}
	if got.Imported != total {
		t.Fatalf("imported %d turns, want %d", got.Imported, total)
	}
	if exp.flushes != 3 {
		t.Fatalf("flushes = %d, want 3 (two full batches and the remainder)", exp.flushes)
	}
	if exp.maxPending < exportBatchSize {
		t.Fatalf("largest batch = %d turns, want %d; the export is not batched", exp.maxPending, exportBatchSize)
	}
	if updates < 3 {
		t.Fatalf("progress reported %d times, want one per batch", updates)
	}
}

// TestRunImportFailedFlushFailsTheBatch pins what a transport failure costs:
// the batch the SDK sent as one request, no more, and every turn in it is left
// in the ledger for a rerun to retry.
func TestRunImportFailedFlushFailsTheBatch(t *testing.T) {
	exp := &recordingExporter{flushErr: errors.New("transport down")}
	ledger := testLedger(t)
	got, err := RunImport(context.Background(), ImportOptions{
		Agent: AgentClaudeCode,
		Importer: &stubImporter{
			turns: func(context.Context, SessionPreview) iter.Seq2[HistoricalGeneration, error] {
				return turnSeq([]HistoricalGeneration{turn("t1"), turn("t2")}, nil)
			},
		},
		Sessions: []SessionPreview{sessionAt("/a.jsonl")},
		Exporter: exp,
		Ledger:   ledger,
	})
	if err != nil {
		t.Fatalf("RunImport: %v", err)
	}
	if got.Imported != 0 || got.Failed != 2 {
		t.Fatalf("result = %+v, want 0 imported and 2 failed", got)
	}
	if len(got.Warnings) == 0 {
		t.Fatal("a failed batch produced no warning, so the reason is nowhere")
	}
	failed := SourceRef{Agent: AgentClaudeCode, SessionID: "session-1", SourcePath: "/a.jsonl", TurnID: "t1", TurnIndex: 0}
	entry, ok := ledger.Status(failed.Identity())
	if !ok || entry.Status != StatusFailed {
		t.Fatalf("ledger entry = %+v (found %v), want a failed turn a rerun retries", entry, ok)
	}
}

// TestRunImportWarnsAboutAnUnreadableSession pins that the reason a session
// stopped survives the call: Failed alone cannot tell a permission error from a
// format change.
func TestRunImportDryRunReadsNothing(t *testing.T) {
	imp := &stubImporter{
		turns: func(context.Context, SessionPreview) iter.Seq2[HistoricalGeneration, error] {
			t.Fatal("a dry run must not read session content")
			return nil
		},
	}
	got, err := RunImport(context.Background(), ImportOptions{
		Agent:    AgentClaudeCode,
		Importer: imp,
		Sessions: []SessionPreview{sessionAt("/a.jsonl")},
		DryRun:   true,
	})
	if err != nil {
		t.Fatalf("RunImport: %v", err)
	}
	if !got.DryRun || got.Sessions != 1 || got.Imported != 0 {
		t.Fatalf("result = %+v", got)
	}
}

func TestImportRejectsBadInput(t *testing.T) {
	sessions := []SessionPreview{sessionAt("/a.jsonl")}
	tests := []struct {
		name    string
		call    func(context.Context) error
		wantIs  error
		wantMsg string
	}{
		{
			name: "RunImport without an exporter",
			call: func(ctx context.Context) error {
				_, err := RunImport(ctx, ImportOptions{
					Agent:    AgentClaudeCode,
					Importer: &stubImporter{},
					Sessions: sessions,
				})
				return err
			},
			wantIs: ErrExporterUnavailable,
		},
		{
			name: "RunImport for an unregistered agent",
			call: func(ctx context.Context) error {
				_, err := RunImport(ctx, ImportOptions{
					Agent:    AgentID("nope"),
					Sessions: sessions,
					Exporter: &recordingExporter{},
				})
				return err
			},
			wantMsg: "unknown agent",
		},
		{
			name: "BuildPlan for an unregistered agent",
			call: func(ctx context.Context) error {
				_, err := BuildPlan(ctx, PlanOptions{Agent: AgentID("nope")})
				return err
			},
			wantMsg: "unknown agent",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call(context.Background())
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Fatalf("error = %v, want %v", err, tt.wantIs)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("error = %v, want one mentioning %q", err, tt.wantMsg)
			}
		})
	}
}

// TestRunImportDisambiguatesCollidedConversations pins both halves of the
// split: two files claiming one session ID become two conversations, and the
// transcripts of one session stay in one. A Claude subagent turn is read from
// its own file under the session directory, so scoping by the turn's file
// rather than the session's would break the session apart.
func TestRunImportDisambiguatesCollidedConversations(t *testing.T) {
	exp := &recordingExporter{}
	imp := &stubImporter{
		turns: func(_ context.Context, sess SessionPreview) iter.Seq2[HistoricalGeneration, error] {
			subagent := turn("t2", func(g *HistoricalGeneration) {
				g.Source.SourcePath = strings.TrimSuffix(sess.SourcePath, ".jsonl") +
					"/subagents/agent-1.jsonl"
			})
			return turnSeq([]HistoricalGeneration{turn("t1"), subagent}, nil)
		},
	}
	sessions := []SessionPreview{sessionAt("/a.jsonl"), sessionAt("/b.jsonl")}

	got, err := RunImport(context.Background(), ImportOptions{
		Agent:    AgentClaudeCode,
		Importer: imp,
		Sessions: sessions,
		Exporter: exp,
		Ledger:   testLedger(t),
	})
	if err != nil {
		t.Fatalf("RunImport: %v", err)
	}
	if got.Collisions != 1 {
		t.Fatalf("collisions = %d, want 1", got.Collisions)
	}
	if len(exp.got) != 4 {
		t.Fatalf("exported %d turns, want 4", len(exp.got))
	}
	a, b := exp.got[0].Gen.ConversationID, exp.got[2].Gen.ConversationID
	if a == b {
		t.Fatalf("collided sessions kept one conversation ID %q", a)
	}
	for _, id := range []string{a, b} {
		if !strings.HasPrefix(id, "histconv-") {
			t.Fatalf("conversation ID %q is not source-scoped", id)
		}
	}
	if sub := exp.got[1].Gen.ConversationID; sub != a {
		t.Fatalf("subagent turn joined conversation %q, want the session's %q", sub, a)
	}
	if sub := exp.got[3].Gen.ConversationID; sub != b {
		t.Fatalf("subagent turn joined conversation %q, want the session's %q", sub, b)
	}
}

// TestRunImportDisambiguatesAcrossRuns pins that two files claiming one session
// ID stay apart even when they are imported separately. A run sees only its own
// selection, so the clash has to come from the plan.
func TestRunImportDisambiguatesAcrossRuns(t *testing.T) {
	sessions := []SessionPreview{sessionAt("/a.jsonl"), sessionAt("/b.jsonl")}
	collisions := DetectCollisions(sessions)
	if len(collisions) != 1 {
		t.Fatalf("test fixture does not collide: %+v", collisions)
	}
	imp := &stubImporter{
		turns: func(context.Context, SessionPreview) iter.Seq2[HistoricalGeneration, error] {
			return turnSeq([]HistoricalGeneration{turn("t1")}, nil)
		},
	}
	ledger := testLedger(t)

	ids := make([]string, 0, 2)
	for _, sess := range sessions {
		exp := &recordingExporter{}
		if _, err := RunImport(context.Background(), ImportOptions{
			Agent:      AgentClaudeCode,
			Importer:   imp,
			Sessions:   []SessionPreview{sess},
			Collisions: collisions,
			Exporter:   exp,
			Ledger:     ledger,
		}); err != nil {
			t.Fatalf("RunImport: %v", err)
		}
		if len(exp.got) != 1 {
			t.Fatalf("exported %d turns, want 1", len(exp.got))
		}
		ids = append(ids, exp.got[0].Gen.ConversationID)
	}
	if ids[0] == ids[1] {
		t.Fatalf("two files claiming session %q merged into conversation %q when imported separately",
			sessions[0].SessionID, ids[0])
	}
}

func TestBuildPlanIsMetadataOnly(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	recent := writeFile(t, filepath.Join(root, "recent.jsonl"), "x")
	stale := writeFile(t, filepath.Join(root, "stale.jsonl"), "x")
	setModTime(t, recent, now.Add(-24*time.Hour))
	setModTime(t, stale, now.Add(-200*24*time.Hour))

	imp := &stubImporter{
		roots: []string{root},
		preview: func(_ context.Context, path string) (SessionPreview, bool, error) {
			info := SessionPreview{SessionID: filepath.Base(path)}
			return info, true, nil
		},
		turns: func(context.Context, SessionPreview) iter.Seq2[HistoricalGeneration, error] {
			t.Fatal("BuildPlan must not read session content")
			return nil
		},
	}

	filter := NewFilter()
	filter.Since = now.Add(-DefaultSinceWindow)
	plan, err := BuildPlan(context.Background(), PlanOptions{
		Agent:    AgentClaudeCode,
		Importer: imp,
		Filter:   filter,
		Discover: DiscoverOptions{Now: func() time.Time { return now }},
	})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if ids := sessionIDs(plan.Sessions); !equalStrings(ids, []string{"recent.jsonl"}) {
		t.Fatalf("planned sessions = %v", ids)
	}
	if len(plan.Skipped) != 1 || plan.Skipped[0].Reason != SkipOutOfRange {
		t.Fatalf("skipped = %+v", plan.Skipped)
	}
}
