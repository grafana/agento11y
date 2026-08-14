package history

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex/codexlog"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex/mapper"
)

func init() {
	Register(AgentSpec{
		ID:          AgentCodex,
		DisplayName: "Codex",
		Aliases:     []string{"openai-codex"},
	}, func() Importer { return &codexImporter{} })
}

// codexMaxTitleLen caps the conversation title derived from the first prompt.
const codexMaxTitleLen = 100

// codexImporter reads Codex rollout transcripts under ~/.codex/sessions.
//
// A rollout is a flat event log rather than a turn log, so the importer
// reconstructs one [fragment.Fragment] per turn, exactly what the live hook
// assembles from its stop events, and passes it to the live [mapper.Map].
type codexImporter struct {
	// roots overrides the resolved session directories. Tests set it.
	roots []string
	// now supplies the clock the mapper falls back to for a turn with no
	// timestamps. nil uses time.Now.
	now func() time.Time

	// mu guards the lookup caches below, which one import run shares across
	// every session it reads.
	mu               sync.Mutex
	files            []string
	pathsBySessionID map[string]string
	readAllMeta      bool
	parents          map[string]codexParent
}

func (c *codexImporter) Roots() []string {
	if len(c.roots) > 0 {
		return c.roots
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".codex", "sessions")}
}

func (c *codexImporter) Match(path string) bool {
	base := filepath.Base(path)
	return strings.HasPrefix(base, "rollout-") && strings.HasSuffix(base, ".jsonl")
}

func (c *codexImporter) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Preview reads a bounded head and tail window. The head carries session_meta
// with the session ID and workspace; both windows carry timestamps. The turn
// count is scaled from the head for a file past the budget.
//
// A rollout has no single line that says how many turns it holds, so the head
// is scanned with the same segment rule the import uses. That scan decodes only
// the envelope of each line, and looks at response_item and event_msg payloads
// alone. A large window therefore costs far less than a full decode.
func (c *codexImporter) Preview(ctx context.Context, path string) (SessionPreview, bool, error) {
	if err := ctx.Err(); err != nil {
		return SessionPreview{}, false, err
	}
	win, err := ReadPreviewWindows(path, PreviewByteBudget)
	if err != nil {
		return SessionPreview{}, false, err
	}
	head := win.HeadLines()
	if len(head) == 0 {
		return SessionPreview{}, false, nil
	}

	p := SessionPreview{Agent: AgentCodex, SourcePath: path, SizeBytes: win.Size}
	headTurns := 0
	var segments codexSegmenter
	for i, raw := range head {
		var rec codexlog.Record
		if err := json.Unmarshal(raw, &rec); err != nil {
			continue
		}
		if i < previewMetadataLines {
			codexApplyPreviewRecord(&p, rec)
		}
		if act := segments.observe(rec); act.Emit {
			headTurns++
		}
	}
	if segments.flush() {
		headTurns++
	}

	// The last activity is the last timestamp in the file: in the tail window,
	// or in the head when the head is the whole file.
	last := win.TailLines()
	if win.Whole {
		last = head
	}
	for i := len(last) - 1; i >= 0 && i >= len(last)-previewMetadataLines; i-- {
		var rec codexlog.Record
		if err := json.Unmarshal(last[i], &rec); err != nil {
			continue
		}
		if ts := parseClaudeTime(rec.Timestamp); !ts.IsZero() {
			if ts.After(p.LastActivityAt) {
				p.LastActivityAt = ts
			}
			break
		}
	}

	p.TurnCount, p.ApproxTurns = win.EstimateTotal(headTurns)
	if p.SessionID == "" {
		p.SessionID = codexSessionIDFromPath(path)
	}
	// The title is the session ID: a preview must not surface prompt text.
	p.Title = p.SessionID
	if p.LastActivityAt.IsZero() {
		p.LastActivityAt = win.ModTime
	}
	return p, true, nil
}

// codexSegmenter splits a rollout into the segments an import exports, one
// generation each. The replay, the preview count, and the parent-generation
// index all drive this one type. A preview, an imported turn, and a subagent's
// recorded parent therefore cannot disagree about where a turn begins.
//
// A segment closes at the next turn_context. Within a turn it also closes at
// each token_count that follows model output. Codex reports a long turn in
// steps, and without the second rule those steps collapse into one generation
// with one combined usage figure.
//
// A token_count that arrives while a tool call is still awaiting its output
// defers the close until the output arrives. Codex writes function_call,
// token_count, then function_call_output, so closing on the token_count would
// leave the call in one generation and its output in the next.
type codexSegmenter struct {
	turnID      string // native or synthetic ID of the open turn
	fallback    bool   // the turn ID is synthetic
	segment     int    // step ordinal within the turn
	turnOrdinal int    // turns seen so far, which names a turn with no ID
	active      bool   // the open segment holds model output
	openCalls   int    // tool calls in the open segment awaiting their output
	deferred    bool   // a token_count closed the step, waiting on tool output
}

// codexSegmentAction is what one record does to the segment stream. A close is
// always followed by a new segment, which costs nothing when it stays empty:
// only a segment whose close reports Emit becomes a generation.
type codexSegmentAction struct {
	CloseBefore bool // close the open segment before applying the record
	CloseAfter  bool // close it after applying the record
	NewTurn     bool // the record opened a new turn
	Emit        bool // the closed segment holds model output
	Usage       bool // the record is a token_count reporting the open segment's usage
}

// observe folds one record into the segment stream.
func (s *codexSegmenter) observe(rec codexlog.Record) codexSegmentAction {
	switch rec.Type {
	case "turn_context":
		act := codexSegmentAction{CloseBefore: true, NewTurn: true, Emit: s.endSegment()}
		tc := codexlog.ParseTurnContext(rec.Payload)
		s.turnID = strings.TrimSpace(tc.TurnID)
		s.fallback = s.turnID == ""
		if s.fallback {
			s.turnID = fallbackTurnID(s.turnOrdinal)
		}
		s.turnOrdinal++
		s.segment = 0
		return act
	case "response_item":
		if s.turnID == "" {
			// Nothing before the first turn_context belongs to a turn.
			return codexSegmentAction{}
		}
		item, ok := codexlog.ParseResponseItem(rec.Payload)
		if !ok {
			return codexSegmentAction{}
		}
		if codexIsToolOutput(item) {
			if s.openCalls > 0 {
				s.openCalls--
			}
			if !s.deferred || s.openCalls > 0 {
				return codexSegmentAction{}
			}
			act := codexSegmentAction{CloseAfter: true, Emit: s.endSegment()}
			s.segment++
			return act
		}
		var act codexSegmentAction
		if s.deferred {
			// The step's usage is already recorded and new model output has
			// arrived, so the deferred close happens now and any tool output
			// still missing is missing from the source.
			act.CloseBefore = true
			act.Emit = s.endSegment()
			s.segment++
		}
		if codexEmitsGeneration(item) {
			s.active = true
		}
		if codexIsToolCall(item) {
			s.openCalls++
		}
		return act
	case "event_msg":
		if codexlog.ParseEventMsg(rec.Payload).Type != "token_count" {
			return codexSegmentAction{}
		}
		if !s.active {
			// A token_count before any model output is the session's opening
			// balance, not a turn's usage.
			return codexSegmentAction{}
		}
		if s.openCalls > 0 {
			s.deferred = true
			return codexSegmentAction{Usage: true}
		}
		act := codexSegmentAction{CloseAfter: true, Usage: true, Emit: s.endSegment()}
		s.segment++
		return act
	}
	return codexSegmentAction{}
}

// flush reports whether the segment still open at the end of the rollout holds
// model output.
func (s *codexSegmenter) flush() bool { return s.endSegment() }

// endSegment clears the per-segment state and reports whether the segment that
// just ended becomes a generation.
func (s *codexSegmenter) endSegment() bool {
	emit := s.active
	s.active = false
	s.openCalls = 0
	s.deferred = false
	return emit
}

// current names the open segment: the turn's own ID, the ID this segment
// exports under, and whether the turn ID is synthetic.
func (s *codexSegmenter) current() (turnID, sourceID string, fallback bool) {
	return s.turnID, codexSegmentTurnID(s.turnID, s.segment), s.fallback
}

func codexApplyPreviewRecord(p *SessionPreview, rec codexlog.Record) {
	if ts := parseClaudeTime(rec.Timestamp); !ts.IsZero() {
		if p.StartedAt.IsZero() || ts.Before(p.StartedAt) {
			p.StartedAt = ts
		}
		if ts.After(p.LastActivityAt) {
			p.LastActivityAt = ts
		}
	}
	switch rec.Type {
	case "session_meta":
		meta, _ := codexlog.ParseSessionMeta(rec.Payload)
		if p.SessionID == "" {
			p.SessionID = meta.SessionID
		}
		if p.Workspace == "" {
			p.Workspace = meta.Cwd
		}
	case "turn_context":
		if p.Workspace == "" {
			p.Workspace = codexlog.ParseTurnContext(rec.Payload).Cwd
		}
	}
}

var codexRolloutSessionIDRe = regexp.MustCompile(`([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

// codexSessionIDFromPath recovers the session ID from a rollout filename, which
// ends in the session UUID. It is the fallback for a rollout with no readable
// session_meta.
func codexSessionIDFromPath(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if m := codexRolloutSessionIDRe.FindStringSubmatch(base); len(m) > 1 {
		return m[1]
	}
	return base
}

// Turns replays the rollout and yields one generation per completed turn.
// [codexSegmenter] decides where each turn begins and ends; this method only
// builds the fragment between those boundaries.
//
// Nothing accumulates across turns: the segment is emitted and dropped as soon
// as it closes, so a 700 MB rollout costs one turn plus the scanner buffer.
func (c *codexImporter) Turns(ctx context.Context, sess SessionPreview) iter.Seq2[HistoricalGeneration, error] {
	return func(yield func(HistoricalGeneration, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(HistoricalGeneration{}, err)
			return
		}
		link := c.subagentLink(ctx, sess)
		r := &codexReplay{
			importer: c,
			sess:     sess,
			link:     link,
			sessionID: firstNonEmptyString(
				sess.SessionID,
				codexSessionIDFromPath(sess.SourcePath),
			),
			toolNames: map[string]string{},
			yield:     yield,
		}
		err := codexlog.ScanRecords(sess.SourcePath, codexlog.ImportScanOptions(), func(rec codexlog.Record) (bool, error) {
			if err := ctx.Err(); err != nil {
				return true, err
			}
			return r.observe(rec), nil
		})
		if err != nil {
			if !r.stopped {
				yield(HistoricalGeneration{}, fmt.Errorf("read codex rollout %s: %w", sess.SourcePath, err))
			}
			return
		}
		if r.stopped {
			return
		}
		r.finalizeAndEmit(r.segments.flush())
	}
}

// codexReplay is the streaming rollout replay. It holds the open segment only.
type codexReplay struct {
	importer  *codexImporter
	sess      SessionPreview
	link      *fragment.SubagentLink
	sessionID string
	yield     func(HistoricalGeneration, error) bool

	segments      codexSegmenter
	title         string
	current       *codexTurn
	pendingPrompt string
	activeCwd     string
	activeModel   string
	// toolNames remembers each tool call's name for the whole rollout, so a
	// tool output that arrives after its segment closed is still attributable.
	toolNames     map[string]string
	lastTotal     codexlog.TokenUsage
	haveLastTotal bool

	emitted int
	stopped bool // the consumer stopped early
}

// codexTurn is one open segment: the fragment being rebuilt plus the
// approximations made while rebuilding it.
type codexTurn struct {
	frag          *fragment.Fragment
	toolByCallID  map[string]int
	quality       QualityReport
	fallbackTurn  bool
	unsupported   bool
	reasoning     bool
	orphanOutputs int // tool outputs whose call is not in this segment
	snapshot      *codexlog.TokenSnapshot
}

// observe folds one record into the replay. It returns true when the consumer
// stopped, which ends the scan.
//
// The segmenter decides where a turn begins and ends; this method only builds
// the fragment between those boundaries.
func (r *codexReplay) observe(rec codexlog.Record) bool {
	act := r.segments.observe(rec)
	if act.CloseBefore {
		if r.finalizeAndEmit(act.Emit) {
			return true
		}
		if act.NewTurn {
			tc := codexlog.ParseTurnContext(rec.Payload)
			r.activeCwd = tc.Cwd
			r.activeModel = tc.Model
		}
		prompt := ""
		if act.NewTurn {
			prompt, r.pendingPrompt = r.pendingPrompt, ""
		}
		r.startSegment(rec.Timestamp, prompt)
	}

	r.apply(rec, act)

	if act.CloseAfter {
		if r.finalizeAndEmit(act.Emit) {
			return true
		}
		r.startSegment(rec.Timestamp, "")
	}
	return false
}

// apply folds one record's content into the open segment.
func (r *codexReplay) apply(rec codexlog.Record, act codexSegmentAction) {
	switch rec.Type {
	case "session_meta":
		meta, _ := codexlog.ParseSessionMeta(rec.Payload)
		if meta.SessionID != "" {
			r.sessionID = meta.SessionID
		}
		if r.current != nil && r.current.frag.Cwd == "" {
			r.current.frag.Cwd = meta.Cwd
		}
	case "input_item":
		r.addPrompt(codexInputText(rec.Payload))
	case "event_msg":
		r.applyEvent(rec, act)
	case "response_item":
		r.applyResponseItem(rec)
	default:
		r.touch(rec.Timestamp)
	}
}

func (r *codexReplay) applyEvent(rec codexlog.Record, act codexSegmentAction) {
	msg := codexlog.ParseEventMsg(rec.Payload)
	if msg.Type == "user_message" && msg.Message != "" {
		r.addPrompt(msg.Message)
	}
	if r.current == nil {
		return
	}
	r.touch(rec.Timestamp)
	if msg.Type == "task_complete" {
		r.current.frag.CompletedAt = rec.Timestamp
	}
	if msg.Type != "token_count" {
		return
	}
	info, ok := codexlog.ParseTokenUsageInfo(rec.Payload)
	if !ok {
		return
	}
	if !act.Usage {
		// The opening balance of a session or a resumed turn: it becomes the
		// baseline the next turn's usage is measured from.
		r.lastTotal = info.TotalTokenUsage
		r.haveLastTotal = true
		return
	}
	if snapshot, ok := codexTokenSnapshot(r.current.frag.TurnID, info, r.lastTotal, r.haveLastTotal); ok {
		r.current.snapshot = &snapshot
	}
	r.lastTotal = info.TotalTokenUsage
	r.haveLastTotal = true
	if r.current.frag.CompletedAt == "" {
		r.current.frag.CompletedAt = rec.Timestamp
	}
}

func (r *codexReplay) applyResponseItem(rec codexlog.Record) {
	if r.current == nil {
		return
	}
	r.touch(rec.Timestamp)
	item, ok := codexlog.ParseResponseItem(rec.Payload)
	if !ok {
		return
	}
	switch {
	case item.Type == "message":
		text := codexlog.MessageText(item.Content)
		switch item.Role {
		case "user":
			r.noteTitle(text)
			r.current.frag.Prompt = appendText(r.current.frag.Prompt, text)
		case "assistant":
			r.current.frag.LastAssistantMessage = appendText(r.current.frag.LastAssistantMessage, text)
		}
	case codexIsToolCall(item):
		r.current.addToolCall(item)
		if callID := strings.TrimSpace(item.CallID); callID != "" {
			r.toolNames[callID] = r.current.frag.Tools[len(r.current.frag.Tools)-1].ToolName
		}
	case codexIsToolOutput(item):
		r.current.addToolOutput(item, r.toolNames[strings.TrimSpace(item.CallID)])
	case item.Type == "reasoning":
		// The live Codex mapper has no thinking field, so reasoning is recorded
		// as a flag rather than dropped silently.
		r.current.reasoning = true
		r.current.unsupported = true
	default:
		if item.Type != "" {
			r.current.unsupported = true
		}
	}
}

func (r *codexReplay) addPrompt(text string) {
	if text == "" {
		return
	}
	r.noteTitle(text)
	if r.current == nil {
		r.pendingPrompt = appendText(r.pendingPrompt, text)
		return
	}
	r.current.frag.Prompt = appendText(r.current.frag.Prompt, text)
}

func (r *codexReplay) noteTitle(text string) {
	if r.title == "" {
		r.title = codexTitleFromText(text)
	}
}

func (r *codexReplay) touch(ts string) {
	if r.current == nil || ts == "" {
		return
	}
	if r.current.frag.StartedAt == "" {
		r.current.frag.StartedAt = ts
	}
	r.current.frag.LastEventAt = ts
}

// codexSegmentTurnID names a segment within a turn. The first segment keeps the
// native turn ID; a later one gets an ordinal suffix, which keeps the
// deterministic generation IDs distinct.
func codexSegmentTurnID(base string, segment int) string {
	if segment == 0 {
		return base
	}
	return fmt.Sprintf("%s:step-%06d", base, segment)
}

// startSegment opens the segment the segmenter has just moved to.
func (r *codexReplay) startSegment(ts, prompt string) {
	_, sourceID, fallback := r.segments.current()
	if sourceID == "" {
		// No turn is open, so there is nothing to build into. The next
		// turn_context opens one.
		r.current = nil
		return
	}
	r.current = &codexTurn{
		frag: &fragment.Fragment{
			SessionID:      r.sessionID,
			TurnID:         sourceID,
			Cwd:            firstNonEmptyString(r.activeCwd, r.sess.Workspace),
			Source:         "history",
			Model:          r.activeModel,
			Prompt:         prompt,
			TranscriptPath: r.sess.SourcePath,
			StartedAt:      ts,
			LastEventAt:    ts,
		},
		toolByCallID: map[string]int{},
		fallbackTurn: fallback,
	}
	if ts == "" {
		r.current.quality.ApproxStartedAt = true
	}
	if r.activeModel == "" {
		r.current.quality.MissingModel = true
	}
}

// finalizeAndEmit closes the open segment and yields it when the segmenter
// reports it holds model output. It returns true when the consumer stopped.
func (r *codexReplay) finalizeAndEmit(emit bool) bool {
	turn := r.current
	r.current = nil
	if turn == nil || !emit {
		return false
	}
	if turn.frag.CompletedAt == "" {
		turn.quality.ApproxCompletedAt = true
		turn.frag.CompletedAt = turn.frag.LastEventAt
	}
	if turn.fallbackTurn {
		turn.quality.Notes = append(turn.quality.Notes, "missing_turn_id")
	}
	if turn.unsupported {
		turn.quality.Notes = append(turn.quality.Notes, "unsupported_codex_items_ignored")
	}
	if turn.orphanOutputs > 0 {
		turn.quality.Notes = append(turn.quality.Notes, "tool_output_without_call")
	}
	turn.frag.SessionID = r.sessionID

	src := SourceRef{
		Agent:      AgentCodex,
		SessionID:  r.sessionID,
		SourcePath: r.sess.SourcePath,
		TurnIndex:  r.emitted,
		TurnID:     turn.frag.TurnID,
	}
	if turn.snapshot == nil {
		turn.quality.ApproxUsage = true
	}
	mapped := mapper.Map(mapper.Inputs{
		Fragment:       turn.frag,
		SubagentLink:   r.link,
		TokenSnapshot:  turn.snapshot,
		ContentCapture: agento11y.ContentCaptureModeFull,
		// The framework Sanitizer is the single redaction point for import.
		RawContent: true,
		Now:        r.importer.clock(),
	})
	gen := mapped.Generation
	gen.ID = src.GenerationID()
	gen.ConversationTitle = r.title
	if turn.reasoning {
		gen.ThinkingEnabled = codexBoolPtr(true)
	}
	if gen.ResponseID == "" {
		gen.ResponseID = turn.frag.TurnID
	}
	r.emitted++

	if !r.yield(HistoricalGeneration{Source: src, Gen: gen, Quality: turn.quality}, nil) {
		r.stopped = true
		return true
	}
	return false
}

func (t *codexTurn) addToolCall(item codexlog.ResponseItem) {
	name := strings.TrimSpace(item.Name)
	if name == "" {
		name = item.Type
	}
	callID := strings.TrimSpace(item.CallID)
	if callID == "" {
		callID = fmt.Sprintf("call-%06d", len(t.frag.Tools))
	}
	rawInput := item.Arguments
	if len(rawInput) == 0 {
		rawInput = item.Input
	}
	if len(rawInput) == 0 && item.Type == "local_shell_call" {
		rawInput = item.Raw
	}
	t.toolByCallID[callID] = len(t.frag.Tools)
	t.frag.Tools = append(t.frag.Tools, fragment.ToolRecord{
		ToolName:  name,
		ToolUseID: callID,
		ToolInput: codexNormalizeJSON(rawInput),
		StartedAt: t.frag.LastEventAt,
	})
}

// addToolOutput attaches a tool result to the call it answers. name is the tool
// name remembered from the call, which is only needed when the call was made in
// an earlier segment.
func (t *codexTurn) addToolOutput(item codexlog.ResponseItem, name string) {
	callID := strings.TrimSpace(item.CallID)
	idx, ok := t.toolByCallID[callID]
	if !ok {
		// The call was made in an earlier segment, which is already exported.
		// Record the result under the call's real name so the output is not
		// lost, and flag the turn: the pairing is one-sided.
		t.orphanOutputs++
		if strings.TrimSpace(name) == "" {
			return // no call and no name: an unattributable result
		}
		idx = len(t.frag.Tools)
		t.toolByCallID[callID] = idx
		t.frag.Tools = append(t.frag.Tools, fragment.ToolRecord{ToolName: name, ToolUseID: callID})
	}
	resp := codexNormalizeJSON(item.Output)
	t.frag.Tools[idx].ToolResponse = resp
	t.frag.Tools[idx].CompletedAt = t.frag.LastEventAt
	if status := codexToolStatus(resp); status != "" {
		t.frag.Tools[idx].Status = status
	}
}

// codexTokenSnapshot turns two cumulative token totals into one turn's usage.
func codexTokenSnapshot(turnID string, info codexlog.TokenUsageInfo, baseline codexlog.TokenUsage, haveBaseline bool) (codexlog.TokenSnapshot, bool) {
	if !haveBaseline {
		baseline = codexlog.TokenUsage{}
	}
	turnUsage, ok := codexlog.SubtractUsage(info.TotalTokenUsage, baseline)
	if !ok || !codexlog.HasPositiveUsage(turnUsage) {
		return codexlog.TokenSnapshot{}, false
	}
	return codexlog.TokenSnapshot{
		TurnID:             turnID,
		TurnUsage:          turnUsage,
		BaselineUsage:      baseline,
		LastUsage:          info.LastTokenUsage,
		TotalUsage:         info.TotalTokenUsage,
		ModelContextWindow: info.ModelContextWindow,
		Source:             "token_count_delta",
	}, true
}

// subagentLink resolves the parent of a subagent session, so an imported
// subagent turn hangs off the turn that spawned it rather than floating.
func (c *codexImporter) subagentLink(ctx context.Context, sess SessionPreview) *fragment.SubagentLink {
	meta, ok, err := codexlog.ReadSessionMeta(sess.SourcePath, codexlog.ImportScanOptions())
	if err != nil || !ok || meta.ThreadSource != "subagent" || meta.ParentSessionID == "" {
		return nil
	}
	childID := firstNonEmptyString(meta.SessionID, sess.SessionID, codexSessionIDFromPath(sess.SourcePath))
	link := &fragment.SubagentLink{
		ChildSessionID:  childID,
		ParentSessionID: meta.ParentSessionID,
		AgentRole:       meta.AgentRole,
		AgentNickname:   meta.AgentNickname,
		AgentDepth:      meta.AgentDepth,
		Source:          "transcript.session_meta",
	}
	parentPath, ok := c.findSessionPath(ctx, meta.ParentSessionID)
	if !ok {
		return link
	}
	spawn, ok := c.parentIndex(parentPath).spawns[childID]
	if !ok {
		return link
	}
	link.ParentSessionID = firstNonEmptyString(spawn.parentSessionID, meta.ParentSessionID)
	link.ParentTurnID = spawn.parentTurnID
	link.SpawnCallID = spawn.spawnCallID
	if spawn.nickname != "" {
		link.AgentNickname = spawn.nickname
	}
	if spawn.segmentTurnID == "" {
		return link
	}
	// The parent's generation ID is the import ID, not the live one, because
	// the parent turn is imported by this same code.
	link.ParentGenerationID = SourceRef{
		Agent:      AgentCodex,
		SessionID:  link.ParentSessionID,
		SourcePath: parentPath,
		TurnIndex:  spawn.generationIndex,
		TurnID:     spawn.segmentTurnID,
	}.GenerationID()
	return link
}

// findSessionPath locates a rollout by session ID. The filename carries the ID,
// so that index is built first and session_meta is read only when a lookup
// misses. Both are cached: a parent that spawned ten subagents would otherwise
// have the whole sessions tree walked ten times.
func (c *codexImporter) findSessionPath(ctx context.Context, sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pathsBySessionID == nil {
		c.pathsBySessionID = map[string]string{}
		for _, root := range c.Roots() {
			files, _ := walkFiles(ctx, root, c.Match)
			c.files = append(c.files, files...)
		}
		for _, path := range c.files {
			if id := codexSessionIDFromPath(path); id != "" {
				if _, seen := c.pathsBySessionID[id]; !seen {
					c.pathsBySessionID[id] = path
				}
			}
		}
	}
	if path, ok := c.pathsBySessionID[sessionID]; ok {
		return path, true
	}
	if c.readAllMeta {
		return "", false
	}
	c.readAllMeta = true
	for _, path := range c.files {
		if ctx.Err() != nil {
			return "", false
		}
		meta, ok, err := codexlog.ReadSessionMeta(path, codexlog.ImportScanOptions())
		if err != nil || !ok || meta.SessionID == "" {
			continue
		}
		if _, seen := c.pathsBySessionID[meta.SessionID]; !seen {
			c.pathsBySessionID[meta.SessionID] = path
		}
	}
	path, ok := c.pathsBySessionID[sessionID]
	return path, ok
}

// codexSpawn is one subagent a parent rollout started, and the parent
// generation that started it.
type codexSpawn struct {
	parentSessionID string
	parentTurnID    string
	spawnCallID     string
	nickname        string
	generationIndex int    // position of the spawning turn among the parent's exported turns
	segmentTurnID   string // the turn ID that turn exports under
}

// codexParent is one scan of a parent rollout: every subagent it spawned, and
// where. It is cached per path, because a parent with ten subagents is
// otherwise scanned ten times.
type codexParent struct {
	spawns map[string]codexSpawn // by child session ID
}

func (c *codexImporter) parentIndex(path string) codexParent {
	c.mu.Lock()
	defer c.mu.Unlock()
	if parent, ok := c.parents[path]; ok {
		return parent
	}
	parent := codexScanParent(path)
	if c.parents == nil {
		c.parents = map[string]codexParent{}
	}
	c.parents[path] = parent
	return parent
}

// codexScanParent reads a parent rollout once. For every subagent the rollout
// spawned it records two things: the turn that spawned the subagent, and that
// turn's position among the turns an import of the parent produces.
//
// The position comes from the same [codexSegmenter] the import itself runs on,
// so a child's recorded parent generation is one the import really writes.
func codexScanParent(path string) codexParent {
	var (
		segments      codexSegmenter
		emitted       int
		sessionID     string
		open          = map[string]codexSpawn{} // spawn calls in the open segment
		calls         = map[string]codexSpawn{} // spawn calls in closed segments
		childByCallID = map[string]string{}
		nicknames     = map[string]string{}
	)
	closeSegment := func(emit bool, sourceID string) {
		for id, spawn := range open {
			if emit {
				spawn.generationIndex = emitted
				spawn.segmentTurnID = sourceID
			}
			calls[id] = spawn
			delete(open, id)
		}
		if emit {
			emitted++
		}
	}

	_ = codexlog.ScanRecords(path, codexlog.ImportScanOptions(), func(rec codexlog.Record) (bool, error) {
		_, sourceID, _ := segments.current()
		act := segments.observe(rec)
		if act.CloseBefore {
			closeSegment(act.Emit, sourceID)
		}
		switch rec.Type {
		case "session_meta":
			if meta, ok := codexlog.ParseSessionMeta(rec.Payload); ok && meta.SessionID != "" {
				sessionID = meta.SessionID
			}
		case "response_item":
			item, ok := codexlog.ParseResponseItem(rec.Payload)
			if !ok {
				break
			}
			callID := strings.TrimSpace(item.CallID)
			switch {
			case item.Type == "function_call" && item.Name == "spawn_agent" && callID != "":
				turnID, _, _ := segments.current()
				open[callID] = codexSpawn{parentTurnID: turnID, spawnCallID: callID}
			case codexIsToolOutput(item) && callID != "":
				if _, isSpawn := open[callID]; !isSpawn {
					if _, isSpawn = calls[callID]; !isSpawn {
						break
					}
				}
				childID, nickname := codexlog.ParseSpawnOutput(item.Output)
				if childID == "" {
					break
				}
				childByCallID[callID] = childID
				nicknames[callID] = nickname
			}
		}
		if act.CloseAfter {
			closeSegment(act.Emit, sourceID)
		}
		return false, nil
	})
	_, sourceID, _ := segments.current()
	closeSegment(segments.flush(), sourceID)

	parent := codexParent{spawns: map[string]codexSpawn{}}
	for callID, childID := range childByCallID {
		spawn, ok := calls[callID]
		if !ok {
			continue
		}
		spawn.parentSessionID = sessionID
		spawn.nickname = nicknames[callID]
		parent.spawns[childID] = spawn
	}
	return parent
}

// codexEmitsGeneration reports whether a response item is the kind of output
// that makes a segment exportable: assistant text or a tool call. Reasoning
// alone is not, because the live Codex mapper has no thinking field, so a
// reasoning-only segment would export an empty generation.
func codexEmitsGeneration(item codexlog.ResponseItem) bool {
	if codexIsToolCall(item) {
		return true
	}
	return item.Type == "message" && item.Role == "assistant" &&
		strings.TrimSpace(codexlog.MessageText(item.Content)) != ""
}

// codexIsToolCall and codexIsToolOutput name the two halves of a tool call.
// Codex spells the output after the call it answers ("function_call" ->
// "function_call_output"), and has added a spelling per tool kind.
func codexIsToolCall(item codexlog.ResponseItem) bool {
	switch item.Type {
	case "function_call", "custom_tool_call", "local_shell_call":
		return true
	default:
		return false
	}
}

func codexIsToolOutput(item codexlog.ResponseItem) bool {
	switch item.Type {
	case "function_call_output", "custom_tool_call_output", "local_shell_call_output":
		return true
	default:
		return false
	}
}

func codexInputText(raw json.RawMessage) string {
	var p struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	if p.Text != "" {
		return p.Text
	}
	return codexlog.MessageText(p.Content)
}

// codexNormalizeJSON returns a value that is valid JSON. A tool payload arrives
// as an object, as a JSON string holding an object, or as a bare string, and
// the exported field must stay decodable in all three cases.
func codexNormalizeJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		trimmed := strings.TrimSpace(s)
		if json.Valid([]byte(trimmed)) {
			return json.RawMessage(trimmed)
		}
		encoded, _ := json.Marshal(s)
		return encoded
	}
	if json.Valid(raw) {
		return raw
	}
	encoded, _ := json.Marshal(string(raw))
	return encoded
}

// codexToolStatus reads a success or failure signal out of a tool result.
func codexToolStatus(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	for _, key := range []string{"status", "state"} {
		if s, ok := obj[key].(string); ok {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "error", "failed", "failure":
				return "error"
			case "completed", "complete", "success", "succeeded", "ok":
				return "completed"
			}
		}
	}
	for _, key := range []string{"exit_code", "exitCode"} {
		if code, ok := obj[key].(float64); ok {
			if code == 0 {
				return "completed"
			}
			return "error"
		}
	}
	return ""
}

// appendText joins two blocks of text with a blank line, ignoring an empty one.
// The codex and cursor importers both rebuild a prompt or a reply from several
// source records.
func appendText(existing, next string) string {
	existing = strings.TrimSpace(existing)
	next = strings.TrimSpace(next)
	switch {
	case existing == "":
		return next
	case next == "":
		return existing
	default:
		return existing + "\n\n" + next
	}
}

func codexTitleFromText(text string) string {
	title := strings.Join(strings.Fields(text), " ")
	if title == "" {
		return ""
	}
	if len(title) > codexMaxTitleLen {
		title = title[:codexMaxTitleLen]
		for !utf8.ValidString(title) {
			title = title[:len(title)-1]
		}
	}
	return title
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func codexBoolPtr(v bool) *bool { return &v }
