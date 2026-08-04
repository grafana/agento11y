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
	return strings.HasSuffix(path, ".jsonl") && !isClaudeSubagentTranscript(path)
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
	// The title is the session ID on purpose. Claude Code records no title, and
	// a preview must not surface prompt text.
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
		lines, _, err := transcript.Read(sess.SourcePath, 0)
		if err != nil {
			yield(HistoricalGeneration{}, fmt.Errorf("read claude transcript %s: %w", sess.SourcePath, err))
			return
		}
		subagentFiles, err := claudeSubagentFiles(sess.SourcePath)
		if err != nil {
			yield(HistoricalGeneration{}, err)
			return
		}
		refs := claudeSubagentRefsForFiles(subagentFiles)

		processed := make([]claudeProcessedTranscript, 0, 1+len(subagentFiles))
		ids := newClaudeGenIDs()
		processed = append(processed, claudeProcess(sess, sess.SourcePath, lines, refs, ids))
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
			processed = append(processed, claudeProcess(sess, subPath, subLines, refs, ids))
		}

		parents := claudeSubagentParentMap(processed, refs)
		for _, p := range processed {
			if p.sourcePath != sess.SourcePath {
				claudeFinalizeSubagentGens(p.sourcePath, p.gens, parents, sess.SessionID)
			}
		}
		// Match live capture: the hook attaches the user id from ~/.claude.json
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
// rewritten to the parent's new ID in the same pass. [claudeGenIDs] has already
// made the mapper IDs unique across the session's transcripts, so one lookup
// key means one turn.
//
// The turn index counts within one transcript file rather than across the
// session. Adding a subagent transcript later then leaves the turns of the
// files beside it numbered as they were, and their generation IDs unchanged.
func claudeHistoricalGenerations(sess SessionPreview, processed []claudeProcessedTranscript, user string) []HistoricalGeneration {
	total := 0
	for _, p := range processed {
		total += len(p.gens)
	}
	out := make([]HistoricalGeneration, 0, total)
	renamed := make(map[string]string, total)
	for _, p := range processed {
		for i, gen := range p.gens {
			hist := claudeHistorical(sess, gen, p.sourcePath, i, user)
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
}

func claudeProcess(sess SessionPreview, sourcePath string, lines []transcript.Line, refs claudeSubagentRefs, ids *claudeGenIDs) claudeProcessedTranscript {
	// CoalesceSession, not Coalesce: an imported transcript is complete, so the
	// trailing assistant turn is kept instead of being held back for a next
	// read that never comes.
	coalesced := mapper.CoalesceSession(lines)
	// The discarded map holds each tool result's arrival time, which the live
	// hook uses to time separate tool spans. An import emits none: tool activity
	// travels in the generation's message parts.
	gens, _ := mapper.Process(coalesced, &state.Session{}, mapper.Options{
		SessionID: sess.SessionID,
		// A subagent whose own transcript is imported must not also appear as
		// the parent's one-line Agent summary.
		SuppressSyntheticSubagentToolCallIDs: claudeSubagentToolCallIDs(lines, refs),
		// nil redactor: Turns returns raw content and the framework Sanitizer
		// redacts once before export.
	}, nil)
	ids.unique(gens)
	return claudeProcessedTranscript{sourcePath: sourcePath, lines: lines, gens: gens}
}

// claudeGenIDs makes the mapper's generation IDs unique across every transcript
// of one session. The mapper derives that ID from the request ID, and two turns
// arrive under one ID in two ways: one request becomes two coalesced turns when
// a tool result interleaves them, and Claude Code reuses a request ID between
// the session transcript and a subagent transcript. Everything downstream — the
// subagent parent map, the chaining in [claudeFinalizeSubagentGens], the
// rewrite in [claudeHistoricalGenerations] — looks a parent up by that ID, and
// with a duplicate the lookup lands on the wrong turn or on the turn itself.
//
// Hold one claudeGenIDs for the whole session and pass it to every
// [claudeProcess] call; one per transcript would leave the cross-file duplicate
// in place.
type claudeGenIDs struct {
	seen map[string]int
	// current maps a mapper ID to the ID of the most recent turn that carried
	// it, which is the turn a parent link written at this point refers to.
	current map[string]string
}

func newClaudeGenIDs() *claudeGenIDs {
	return &claudeGenIDs{seen: map[string]int{}, current: map[string]string{}}
}

// unique renames the duplicate IDs in one transcript's turns, continuing from
// the transcripts already seen.
//
// A parent link always names an earlier turn, so the first turn under an ID
// keeps it and every repeat takes a suffixed one. Links made before the repeat
// still mean the turn they named; links made after are repointed to the repeat.
func (u *claudeGenIDs) unique(gens []agento11y.Generation) {
	for i := range gens {
		for j, parent := range gens[i].ParentGenerationIDs {
			if id, ok := u.current[parent]; ok {
				gens[i].ParentGenerationIDs[j] = id
			}
		}
		id := gens[i].ID
		if id == "" {
			continue
		}
		if n := u.seen[id]; n > 0 {
			gens[i].ID = fmt.Sprintf("%s#%d", id, n)
		}
		u.seen[id]++
		u.current[id] = gens[i].ID
	}
}

func isClaudeSubagentTranscript(path string) bool {
	return strings.Contains(filepath.ToSlash(path), "/subagents/")
}

// claudeSubagentFiles returns the subagent transcripts belonging to one
// session. Claude Code writes them to <dir>/<sessionID>/subagents/.
func claudeSubagentFiles(sessionPath string) ([]string, error) {
	sessionID := strings.TrimSuffix(filepath.Base(sessionPath), filepath.Ext(sessionPath))
	root := filepath.Join(filepath.Dir(sessionPath), sessionID, "subagents")
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return walkFiles(context.Background(), root, func(name string) bool {
		return strings.HasSuffix(name, ".jsonl")
	})
}

// claudeSubagentRefs indexes the subagent transcripts of one session by their
// directory and by the agent ID in their filename, which are the two ways a
// parent's Agent tool result points at a subagent run.
type claudeSubagentRefs struct {
	dirs     map[string]bool
	agentIDs map[string]bool
}

func claudeSubagentRefsForFiles(files []string) claudeSubagentRefs {
	refs := claudeSubagentRefs{dirs: map[string]bool{}, agentIDs: map[string]bool{}}
	for _, file := range files {
		refs.dirs[filepath.Clean(filepath.Dir(file))] = true
		if id := claudeAgentIDFromSubagentPath(file); id != "" {
			refs.agentIDs[id] = true
		}
	}
	return refs
}

func (r claudeSubagentRefs) hasDir(raw string) bool {
	dir := claudeCleanPath(raw)
	if dir == "" {
		return false
	}
	if r.dirs[dir] {
		return true
	}
	for known := range r.dirs {
		if strings.HasPrefix(known, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func (r claudeSubagentRefs) hasAgentID(id string) bool {
	return id != "" && r.agentIDs[id]
}

// claudeSubagentToolCallIDs returns the Agent tool call IDs whose subagent
// transcript is imported separately, so the mapper skips the summary-only
// generation for them.
func claudeSubagentToolCallIDs(lines []transcript.Line, refs claudeSubagentRefs) map[string]bool {
	ids := map[string]bool{}
	claudeEachToolResult(lines, func(block transcript.UserContentBlock, content string) {
		if refs.hasDir(claudeTranscriptDirFromToolResult(content)) {
			ids[block.ToolUseID] = true
		}
		for _, agentID := range claudeAgentIDsFromToolResult(content) {
			if refs.hasAgentID(agentID) {
				ids[block.ToolUseID] = true
			}
		}
	})
	return ids
}

// claudeEachToolResult calls fn for every tool_result block in the user lines.
func claudeEachToolResult(lines []transcript.Line, fn func(transcript.UserContentBlock, string)) {
	for _, line := range lines {
		if line.Type != "user" {
			continue
		}
		var msg transcript.UserMessage
		if err := json.Unmarshal(line.Message, &msg); err != nil {
			continue
		}
		_, blocks, err := transcript.ParseUserContent(msg.Content)
		if err != nil {
			continue
		}
		for _, block := range blocks {
			fn(block, block.Content())
		}
	}
}

// claudeSubagentParents maps a subagent run to the generation that spawned it,
// keyed both by transcript directory and by agent ID.
type claudeSubagentParents struct {
	byDir              map[string]string
	byAgentID          map[string]string
	agentNameByAgentID map[string]string
}

func claudeSubagentParentMap(processed []claudeProcessedTranscript, refs claudeSubagentRefs) claudeSubagentParents {
	parents := claudeSubagentParents{
		byDir:              map[string]string{},
		byAgentID:          map[string]string{},
		agentNameByAgentID: map[string]string{},
	}
	for _, p := range processed {
		parentByToolUseID, agentNameByToolUseID := claudeToolCallParents(p.gens)
		claudeEachToolResult(p.lines, func(block transcript.UserContentBlock, content string) {
			parentID := parentByToolUseID[block.ToolUseID]
			if parentID == "" {
				return
			}
			if dir := claudeTranscriptDirFromToolResult(content); refs.hasDir(dir) {
				parents.byDir[claudeCleanPath(dir)] = parentID
			}
			for _, agentID := range claudeAgentIDsFromToolResult(content) {
				if !refs.hasAgentID(agentID) {
					continue
				}
				parents.byAgentID[agentID] = parentID
				if agentName := agentNameByToolUseID[block.ToolUseID]; agentName != "" {
					parents.agentNameByAgentID[agentID] = agentName
				}
			}
		})
	}
	return parents
}

// claudeToolCallParents indexes each tool call ID to the generation that made
// it, plus the subagent name for an Agent call.
func claudeToolCallParents(gens []agento11y.Generation) (map[string]string, map[string]string) {
	parentByToolUseID := map[string]string{}
	agentNameByToolUseID := map[string]string{}
	for _, gen := range gens {
		for _, msg := range gen.Output {
			for _, part := range msg.Parts {
				if part.ToolCall == nil || part.ToolCall.ID == "" {
					continue
				}
				parentByToolUseID[part.ToolCall.ID] = gen.ID
				if agentName := claudeAgentNameFromToolCall(*part.ToolCall); agentName != "" {
					agentNameByToolUseID[part.ToolCall.ID] = agentName
				}
			}
		}
	}
	return parentByToolUseID, agentNameByToolUseID
}

func claudeAgentNameFromToolCall(call agento11y.ToolCall) string {
	if call.Name != "Agent" {
		return ""
	}
	var parsed struct {
		SubagentType string `json:"subagent_type"`
	}
	_ = json.Unmarshal(call.InputJSON, &parsed)
	suffix := strings.ToLower(strings.TrimSpace(parsed.SubagentType))
	if suffix == "" {
		suffix = "subagent"
	}
	return string(AgentClaudeCode) + "/" + suffix
}

// claudeTranscriptDirFromToolResult reads the "Transcript dir:" line Claude
// Code writes into an Agent tool result.
func claudeTranscriptDirFromToolResult(content string) string {
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if dir, ok := strings.CutPrefix(line, "Transcript dir:"); ok {
			return claudeCleanPath(dir)
		}
	}
	return ""
}

// claudeAgentIDsFromToolResult reads the agent IDs an Agent tool result
// mentions. Claude Code has used three spellings for the same field.
func claudeAgentIDsFromToolResult(content string) []string {
	var ids []string
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		for _, prefix := range []string{"agentId:", "Agent ID:", "agent_id:"} {
			raw, ok := strings.CutPrefix(line, prefix)
			if !ok {
				continue
			}
			fields := strings.Fields(strings.TrimSpace(raw))
			if len(fields) == 0 {
				continue
			}
			if id := strings.Trim(fields[0], "`'\".,;:()[]{}"); id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func claudeCleanPath(raw string) string {
	raw = strings.Trim(strings.TrimSpace(raw), "`'\"")
	if raw == "" {
		return ""
	}
	return filepath.Clean(raw)
}

func claudeAgentIDFromSubagentPath(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if id, ok := strings.CutPrefix(base, "agent-"); ok {
		return id
	}
	return ""
}

// claudeParentForSubagent picks the spawning generation for a subagent
// transcript: by agent ID when the parent named one, otherwise by the longest
// matching transcript directory.
func claudeParentForSubagent(path string, parents claudeSubagentParents) string {
	if agentID := claudeAgentIDFromSubagentPath(path); agentID != "" {
		if parentID := parents.byAgentID[agentID]; parentID != "" {
			return parentID
		}
	}
	path = filepath.Clean(path)
	var bestDir, bestParent string
	for dir, parentID := range parents.byDir {
		dir = filepath.Clean(dir)
		if (path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))) && len(dir) > len(bestDir) {
			bestDir = dir
			bestParent = parentID
		}
	}
	return bestParent
}

// claudeFinalizeSubagentGens attaches a subagent transcript's turns to the
// session and chains them: the first turn hangs off the spawning generation and
// each later turn off the one before, so the viewer shows the subagent run as a
// branch rather than a flat list.
func claudeFinalizeSubagentGens(path string, gens []agento11y.Generation, parents claudeSubagentParents, sessionID string) {
	agentName := string(AgentClaudeCode) + "/subagent"
	if agentID := claudeAgentIDFromSubagentPath(path); agentID != "" {
		if name := parents.agentNameByAgentID[agentID]; name != "" {
			agentName = name
		}
	}
	parentID := claudeParentForSubagent(path, parents)
	for i := range gens {
		if gens[i].AgentName == "" || gens[i].AgentName == string(AgentClaudeCode) {
			gens[i].AgentName = agentName
		}
		gens[i].ConversationID = sessionID
		if parentID != "" && len(gens[i].ParentGenerationIDs) == 0 {
			gens[i].ParentGenerationIDs = []string{parentID}
		}
		parentID = gens[i].ID
	}
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
