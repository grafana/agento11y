package history

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/mapper"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/subagent"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/transcript"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/userid"
)

func init() {
	Register(AgentSpec{
		ID:          AgentClaudeCode,
		DisplayName: "Claude Code",
		Aliases:     []string{"claude", "claudecode"},
	}, func() Importer { return &claudeImporter{} })
}

// claudeImporter reads Claude Code's JSONL transcripts under
// $CLAUDE_CONFIG_DIR/projects (or ~/.claude/projects).
//
// It reuses the live transcript reader, [mapper.Coalesce], and [mapper.Process]
// so an imported turn matches what live capture would have produced, including
// the synthesized generations for Agent tool calls.
type claudeImporter struct {
	// roots overrides the resolved project directories. Tests set it.
	roots []string
}

func (c *claudeImporter) Roots() []string {
	if len(c.roots) > 0 {
		return c.roots
	}
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return []string{filepath.Join(dir, "projects")}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".claude", "projects")}
}

// Match accepts a session transcript. A subagent transcript is excluded here
// because it is not a session of its own: [claudeImporter.Turns] pulls it in
// alongside its parent so the subagent turns keep their parent links.
func (c *claudeImporter) Match(path string) bool {
	return strings.HasSuffix(path, ".jsonl") && !subagent.IsTranscript(path)
}

// Preview reads a bounded head and tail window instead of the whole file. The
// development machine holds 14,507 transcripts totalling 5.3 GB, so a
// full decode per file makes discovery time out before it renders.
//
// Even inside the window, only the few lines that carry metadata are decoded:
// the head until the session ID, workspace, and start time are known, and the
// tail backwards until a timestamp appears. Turns are counted by scanning
// bytes, because decoding a megabyte of JSON per file over 14,507 files costs
// more than every other part of discovery together.
func (c *claudeImporter) Preview(ctx context.Context, path string) (SessionPreview, bool, error) {
	if err := ctx.Err(); err != nil {
		return SessionPreview{}, false, err
	}
	win, err := ReadPreviewWindows(path, PreviewByteBudget)
	if err != nil {
		return SessionPreview{}, false, err
	}

	p := SessionPreview{
		Agent:      AgentClaudeCode,
		SourcePath: path,
		SizeBytes:  win.Size,
	}
	head := win.HeadLines()
	if len(head) == 0 {
		return SessionPreview{}, false, nil // not a usable session
	}

	for _, raw := range head[:min(len(head), previewMetadataLines)] {
		var line claudePreviewLine
		if err := json.Unmarshal(raw, &line); err != nil {
			continue
		}
		claudeApplyPreviewLine(&p, line)
		if p.SessionID != "" && p.Workspace != "" && !p.StartedAt.IsZero() {
			break
		}
	}
	// The last activity is the last timestamp in the file, which is in the tail
	// window, or in the head when the head is the whole file.
	last := win.TailLines()
	if win.Whole {
		last = head
	}
	for i := len(last) - 1; i >= 0 && i >= len(last)-previewMetadataLines; i-- {
		var line claudePreviewLine
		if err := json.Unmarshal(last[i], &line); err != nil {
			continue
		}
		if ts := parseClaudeTime(line.Timestamp); !ts.IsZero() {
			if ts.After(p.LastActivityAt) {
				p.LastActivityAt = ts
			}
			break
		}
	}

	p.TurnCount, p.ApproxTurns = win.EstimateTotal(countClaudeTurns(head))
	// Turns imports the session's subagent transcripts too, so a count that
	// leaves them out understates a subagent-heavy session several times over.
	subTurns, subApprox, subBytes := c.previewSubagentTurns(ctx, path)
	p.TurnCount += subTurns
	p.ApproxTurns = p.ApproxTurns || subApprox
	p.SizeBytes += subBytes
	if p.SessionID == "" {
		p.SessionID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	// The title is the session ID on purpose: a preview must not surface prompt
	// text, and the ai-title Claude Code writes is generated from it.
	p.Title = p.SessionID
	if p.LastActivityAt.IsZero() {
		p.LastActivityAt = win.ModTime
	}
	return p, true, nil
}

// claudeAssistantTypeMarker and claudeRequestIDMarker are what a preview counts
// to estimate turns. Claude Code writes compact JSON with no spaces, so a byte
// scan finds both fields without decoding the line. That is what holds
// discovery over 14,507 transcripts to a few seconds.
var (
	claudeAssistantTypeMarker = []byte(`"type":"assistant"`)
	claudeRequestIDMarker     = []byte(`"requestId":"`)
)

// countClaudeTurns counts the turns in a window of transcript lines.
//
// The unit is the request, not the line: Claude Code writes one line per
// content block of a streaming response, and [mapper.Coalesce] merges the lines
// sharing a requestId back into the single turn the import exports. Counting
// lines instead reported five to twenty times the number of turns an import
// then produced.
func countClaudeTurns(lines [][]byte) int {
	n := 0
	seen := map[string]bool{}
	for _, raw := range lines {
		if !bytes.Contains(raw, claudeAssistantTypeMarker) {
			continue
		}
		id := claudeRequestIDValue(raw)
		if id == "" {
			n++ // no request ID: the line is a turn of its own
			continue
		}
		if !seen[id] {
			seen[id] = true
			n++
		}
	}
	return n
}

func claudeRequestIDValue(raw []byte) string {
	_, rest, found := bytes.Cut(raw, claudeRequestIDMarker)
	if !found {
		return ""
	}
	value, _, closed := bytes.Cut(rest, []byte(`"`))
	if !closed {
		return ""
	}
	return string(value)
}

// claudeSubagentPreviewBudget is the window used to count a subagent
// transcript's turns. It is smaller than the session budget because a session
// can own dozens of subagent files and only their turn count is wanted; the
// count is scaled from the window and reported as approximate.
const claudeSubagentPreviewBudget = 128 << 10

// previewSubagentTurns counts the turns in a session's subagent transcripts.
// A session with no subagents costs one failed os.Stat.
func (c *claudeImporter) previewSubagentTurns(ctx context.Context, sessionPath string) (turns int, approx bool, sizeBytes int64) {
	files, err := claudeSubagentFiles(sessionPath)
	if err != nil || len(files) == 0 {
		return 0, false, 0
	}
	for _, file := range files {
		if ctx.Err() != nil {
			return turns, true, sizeBytes
		}
		win, err := ReadPreviewWindows(file, claudeSubagentPreviewBudget)
		if err != nil {
			approx = true
			continue
		}
		sizeBytes += win.Size
		count, scaled := win.EstimateTotal(countClaudeTurns(win.HeadLines()))
		turns += count
		approx = approx || scaled
	}
	return turns, approx, sizeBytes
}

// claudePreviewLine is the metadata-only subset of a transcript line. Decoding
// into it instead of transcript.Line keeps message content out of preview
// memory even for a line inside the window.
type claudePreviewLine struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	Timestamp string `json:"timestamp"`
}

func claudeApplyPreviewLine(p *SessionPreview, line claudePreviewLine) {
	if p.SessionID == "" && line.SessionID != "" {
		p.SessionID = line.SessionID
	}
	if p.Workspace == "" && line.CWD != "" {
		p.Workspace = line.CWD
	}
	ts := parseClaudeTime(line.Timestamp)
	if ts.IsZero() {
		return
	}
	if p.StartedAt.IsZero() || ts.Before(p.StartedAt) {
		p.StartedAt = ts
	}
	if ts.After(p.LastActivityAt) {
		p.LastActivityAt = ts
	}
}

// Turns maps the session transcript and every subagent transcript that belongs
// to it, then yields the resulting turns in order.
//
// A transcript is read whole because Coalesce and Process both need the full
// line sequence: a turn's input comes from the lines before it, and a subagent
// link from the lines after. Turns are yielded one at a time all the same, so
// [RunImport] holds one generation rather than the mapped session.
func (c *claudeImporter) Turns(ctx context.Context, sess SessionPreview) iter.Seq2[HistoricalGeneration, error] {
	return func(yield func(HistoricalGeneration, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(HistoricalGeneration{}, err)
			return
		}
		lines, _, titles, err := transcript.ReadWithTitles(sess.SourcePath, 0)
		if err != nil {
			yield(HistoricalGeneration{}, fmt.Errorf("read claude transcript %s: %w", sess.SourcePath, err))
			return
		}
		subagentFiles, err := claudeSubagentFiles(sess.SourcePath)
		if err != nil {
			yield(HistoricalGeneration{}, err)
			return
		}
		refs := subagent.RefsForFiles(subagentFiles)

		processed := make([]claudeProcessedTranscript, 0, 1+len(subagentFiles))
		ids := subagent.NewIDs()
		st := &state.Session{}
		processed = append(processed, claudeProcess(sess, sess.SourcePath, lines, refs, subagentFiles, ids, st, titles))
		for _, subPath := range subagentFiles {
			if err := ctx.Err(); err != nil {
				yield(HistoricalGeneration{}, err)
				return
			}
			subLines, _, err := transcript.Read(subPath, 0)
			if err != nil {
				yield(HistoricalGeneration{}, fmt.Errorf("read claude subagent transcript %s: %w", subPath, err))
				return
			}
			// A subagent's generations belong to the session's conversation, so
			// they carry its title. The mapper never takes one from a subagent's
			// own (sidechain) prompts.
			sub := *st
			processed = append(processed, claudeProcess(sess, subPath, subLines, refs, subagentFiles, ids, &sub, transcript.Titles{}))
		}

		parents := subagent.ParentMap(claudeTranscripts(processed), refs)
		for _, p := range processed {
			if p.sourcePath != sess.SourcePath {
				subagent.Finalize(p.sourcePath, p.gens, parents, sess.SessionID, string(AgentClaudeCode))
			}
		}
		// Match live capture: the hook attaches the user id from the selected account file
		// to every generation it emits, so a backfilled turn resolves it the
		// same way.
		user := userid.Resolve()
		historical := claudeHistoricalGenerations(sess, processed, user)

		for _, gen := range historical {
			if err := ctx.Err(); err != nil {
				yield(HistoricalGeneration{}, err)
				return
			}
			if !yield(gen, nil) {
				return
			}
		}
	}
}

// claudeHistoricalGenerations gives every mapped turn its import identity.
//
// The mapper's own generation ID is uuid5(sessionID + ":" + requestID), which
// says nothing about where the turn came from. Each turn therefore gets the
// deterministic ID derived from its [SourceRef], and every parent link is
// rewritten to the parent's new ID in the same pass. [subagent.IDs] has already
// made the mapper IDs unique across the session's transcripts, so one lookup
// key means one turn.
//
// The turn index counts within one transcript file rather than across the
// session. Adding a subagent transcript later then leaves the turns of the
// files beside it numbered as they were, and their generation IDs unchanged.
// Merged same-message fragments keep the first part's index so a rerun after
// the merge does not remint later turns.
func claudeHistoricalGenerations(sess SessionPreview, processed []claudeProcessedTranscript, user string) []HistoricalGeneration {
	total := 0
	for _, p := range processed {
		total += len(p.gens)
	}
	out := make([]HistoricalGeneration, 0, total)
	renamed := make(map[string]string, total)
	for _, p := range processed {
		for i, gen := range p.gens {
			index := i
			if i < len(p.indexes) {
				index = p.indexes[i]
			}
			hist := claudeHistorical(sess, gen, p.sourcePath, index, user)
			if gen.ID != "" {
				renamed[gen.ID] = hist.Gen.ID
			}
			out = append(out, hist)
		}
	}
	for i := range out {
		parents := out[i].Gen.ParentGenerationIDs
		for j, parent := range parents {
			if newID, ok := renamed[parent]; ok {
				parents[j] = newID
			}
		}
	}
	return out
}

func claudeHistorical(sess SessionPreview, gen agento11y.Generation, sourcePath string, index int, user string) HistoricalGeneration {
	if gen.UserID == "" {
		gen.UserID = user
	}
	turnID := gen.ResponseID
	if turnID == "" {
		turnID = gen.ID
	}
	src := SourceRef{
		Agent:      AgentClaudeCode,
		SessionID:  sess.SessionID,
		SourcePath: sourcePath,
		TurnIndex:  index,
		TurnID:     turnID,
	}
	gen.ID = src.GenerationID()
	return HistoricalGeneration{
		Source:  src,
		Gen:     gen,
		Quality: claudeQuality(gen),
	}
}

type claudeProcessedTranscript struct {
	sourcePath string
	lines      []transcript.Line
	gens       []agento11y.Generation
	indexes    []int
}

// claudeProcess maps one transcript of the session. st carries the session's
// title state: the parent transcript fills it from its title lines and first
// prompt, and each subagent transcript gets a copy.
func claudeProcess(sess SessionPreview, sourcePath string, lines []transcript.Line, refs subagent.Refs, files []string, ids *subagent.IDs, st *state.Session, titles transcript.Titles) claudeProcessedTranscript {
	// CoalesceSession, not Coalesce: an imported transcript is complete, so the
	// trailing assistant turn is kept instead of being held back for a next
	// read that never comes.
	coalesced := subagent.DropCoveredSidechain(mapper.CoalesceSession(lines), files, sourcePath)
	// The discarded map holds each tool result's arrival time, which the live
	// hook uses to time separate tool spans. An import emits none: tool activity
	// travels in the generation's message parts.
	gens, _ := mapper.Process(coalesced, st, mapper.Options{
		SessionID: sess.SessionID,
		Titles:    titles,
		// A subagent whose own transcript is imported must not also appear as
		// the parent's one-line Agent summary.
		SuppressSyntheticSubagentToolCallIDs: subagent.SyntheticToolCallIDs(lines, refs),
		// nil redactor: Turns returns raw content and the framework Sanitizer
		// redacts once before export. The title is the exception: the mapper
		// cuts it, so it redacts it first, as the Sanitizer would.
	}, nil)
	ids.Unique(gens)
	return claudeProcessedTranscript{
		sourcePath: sourcePath,
		lines:      lines,
		gens:       gens,
		indexes:    claudeTurnIndexes(coalesced, gens),
	}
}

// claudeTurnIndexes numbers generations on the unmerged segmentation. A
// merged same-message turn keeps its first part's index; later turns keep
// the indexes they had before the merge, including gaps for absorbed parts.
func claudeTurnIndexes(coalesced []transcript.Line, gens []agento11y.Generation) []int {
	type slot struct {
		requestID string
		groups    int
	}
	var slots []slot
	for _, line := range coalesced {
		if line.Type != "assistant" {
			continue
		}
		var msg transcript.AssistantMessage
		if err := json.Unmarshal(line.Message, &msg); err != nil || msg.Usage.OutputTokens <= 0 {
			continue
		}
		groups := max(line.LegacyGroups, 1)
		slots = append(slots, slot{requestID: line.RequestID, groups: groups})
	}

	indexes := make([]int, len(gens))
	next := 0
	si := 0
	for i, gen := range gens {
		indexes[i] = next
		inc := 1
		if si < len(slots) && gen.ResponseID != "" && gen.ResponseID == slots[si].requestID {
			inc = slots[si].groups
			si++
		}
		next += inc
	}
	return indexes
}

func claudeTranscripts(processed []claudeProcessedTranscript) []subagent.Transcript {
	out := make([]subagent.Transcript, len(processed))
	for i, p := range processed {
		out[i] = subagent.Transcript{Path: p.sourcePath, Lines: p.lines, Gens: p.gens}
	}
	return out
}

// claudeSubagentFiles returns the subagent transcripts belonging to one
// session. Claude Code writes them to <dir>/<sessionID>/subagents/.
func claudeSubagentFiles(sessionPath string) ([]string, error) {
	return subagent.Files(sessionPath)
}

// claudeQuality reads the approximations off the mapped generation. The Claude
// Code mapper leaves a timestamp at the zero value when the source line carried
// none rather than substituting now, so a zero value here means the source
// genuinely lacked it.
func claudeQuality(gen agento11y.Generation) QualityReport {
	var q QualityReport
	if strings.TrimSpace(gen.Model.Name) == "" {
		q.MissingModel = true
	}
	if gen.StartedAt.IsZero() {
		q.ApproxStartedAt = true
	}
	if gen.CompletedAt.IsZero() {
		q.ApproxCompletedAt = true
	}
	if !hasTokenUsage(gen.Usage) {
		q.ApproxUsage = true
	}
	return q
}

func parseClaudeTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	return time.Time{}
}
