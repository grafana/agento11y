package history

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/grafana/agento11y/go/agento11y"
)

func init() {
	Register(AgentSpec{
		ID:          AgentPi,
		DisplayName: "pi",
		Aliases:     []string{"pi-coding-agent"},
	}, func() Importer { return &piImporter{} })
}

// piMaxTitleLen caps a conversation title derived from the first prompt. It
// matches MAX_TITLE_LEN in plugins/pi/src/mappers.ts and counts runes, so a
// title never splits a multi-byte character.
const piMaxTitleLen = 100

// piImporter reads pi's session logs under $PI_CODING_AGENT_DIR/sessions (or
// ~/.pi/agent/sessions).
//
// pi is the first agent with no Go live mapper: live capture is the TypeScript
// plugin @grafana/agento11y-pi, so the mapping below is a port of the
// export-path subset of plugins/pi/src/mappers.ts and plugins/pi/src/lineage.ts
// rather than a call into shared code. conformance/pi-sessions holds the
// fixtures that keep the two in agreement; see pi_conformance_test.go.
type piImporter struct {
	// roots overrides the resolved session directories. Tests set it.
	roots []string
}

func (p *piImporter) Roots() []string {
	if len(p.roots) > 0 {
		return p.roots
	}
	// The same resolution as internal/agents/pi, duplicated on purpose: that
	// package imports internal/local, which imports this one, so importing it
	// here would be a cycle.
	if dir := strings.TrimSpace(os.Getenv("PI_CODING_AGENT_DIR")); dir != "" {
		return []string{filepath.Join(dir, "sessions")}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".pi", "agent", "sessions")}
}

// piSubagentRunDir matches the run directory the third-party pi-subagents
// package writes its child sessions into: <session-dir>/<runID>/run-<N>/.
var piSubagentRunDir = regexp.MustCompile(`^run-[0-9]+$`)

// Match accepts a pi session log and rejects a subagent run log.
//
// pi writes one session per file directly inside the encoded-cwd directory
// (SessionManager.getDefaultSessionDir), and lists sessions from that directory
// alone. Anything deeper is another tool's: pi-subagents nests a child run
// under <session-dir>/<runID>/run-<N>/, as session.jsonl on current versions
// and as <timestamp>_<uuid>.jsonl on older ones, and writes task input and
// output dumps into a sibling subagent-artifacts directory. Live capture
// records none of those runs (the pi plugin has no subagent code at all), so
// importing them would exceed live fidelity rather than match it.
func (p *piImporter) Match(path string) bool {
	if !strings.HasSuffix(path, ".jsonl") {
		return false
	}
	dir, base := filepath.Split(filepath.ToSlash(path))
	if base == "session.jsonl" {
		return false
	}
	for segment := range strings.SplitSeq(strings.Trim(dir, "/"), "/") {
		if segment == "subagent-artifacts" || piSubagentRunDir.MatchString(segment) {
			return false
		}
	}
	return true
}

// piPreviewLine is the metadata-only view of a session line. It deliberately
// has no message or details field: a preview must not decode content, and one
// subagent tool result's details inlines a full task prompt, a full subagent
// transcript, and every child tool call.
type piPreviewLine struct {
	Type string `json:"type"`
	ID   string `json:"id"`   // session header: the conversation ID
	CWD  string `json:"cwd"`  // session header: the workspace
	Name string `json:"name"` // session_info: the user-set display name
	// ParentSession is the header's trunk path. Only its presence is read: it
	// makes the header timestamp a fork instant, and the turns before it belong
	// to the trunk.
	ParentSession string `json:"parentSession"`
	Timestamp     string `json:"timestamp"`
}

// piRoleMarker locates a message's role by byte scan. pi writes compact JSON
// with no spaces, so this finds the role without decoding the line, which is
// what holds discovery over thousands of sessions to seconds.
//
// Only the first occurrence in a line is the entry's own role. A later one is
// nested content: a tool result's details hold arbitrary JSON, and a subagent
// result nests whole assistant messages in there. Counting those made a preview
// promise more turns than its own import delivered.
//
// Checked against decoding every line of all 2,536 top-level sessions on the
// development machine: the rule agrees with the decoded role on all 121,718
// assistant entries and reports no others.
var piRoleMarker = []byte(`"role":"`)

// piIsAssistantLine reports whether a raw session line is an assistant message
// entry.
func piIsAssistantLine(raw []byte) bool {
	_, rest, found := bytes.Cut(raw, piRoleMarker)
	return found && bytes.HasPrefix(rest, []byte(`assistant"`))
}

// piEntryTimestampMarker finds an entry's own ISO timestamp by byte scan. It is
// the first `"timestamp":"` in the line, because pi writes the entry's fields
// before its message, and the message's timestamp is a number rather than a
// string. A later occurrence inside message content cannot be mistaken for it.
var piEntryTimestampMarker = []byte(`"timestamp":"`)

// piCountAssistantTurns counts the turns a session produced in a window of its
// lines.
//
// cutoff, when non-zero, is the fork instant: an entry at or before it was
// copied from the trunk, and [piImporter.Turns] does not export it, so counting
// it would promise turns the import will not deliver. A line with no readable
// timestamp counts, matching the same choice in [piSession.isCopied].
func piCountAssistantTurns(lines [][]byte, cutoff time.Time) int {
	turns := 0
	for _, raw := range lines {
		if !piIsAssistantLine(raw) {
			continue
		}
		if !cutoff.IsZero() {
			if ts := piEntryTimestamp(raw); !ts.IsZero() && !ts.After(cutoff) {
				continue
			}
		}
		turns++
	}
	return turns
}

// piEstimateTurns scales a sampled assistant-turn count to the whole file.
//
// The head window is the sample, the way every other importer takes it. When
// the head holds no turn the import would export, the tail window becomes the
// sample instead, because scaling zero by any factor stays zero. A fork hits
// that case whenever it is larger than the byte budget: its copied entries sit
// at the front of the file by construction, so the head window is nothing but
// entries the import skips, and the session previewed as "about 0 turns" while
// its import produced dozens. On the development machine 77 of the 305 sessions
// over the budget are such forks.
//
// The tail estimate spreads the tail's turn density over the bytes the head did
// not cover, since the head is known to hold none. Both branches report approx,
// so the number is labelled either way.
func piEstimateTurns(win PreviewWindows, cutoff time.Time) (total int, approx bool) {
	head := piCountAssistantTurns(win.HeadLines(), cutoff)
	if win.Whole || head > 0 {
		return win.EstimateTotal(head)
	}
	tail := piCountAssistantTurns(win.TailLines(), cutoff)
	if tail == 0 || len(win.Tail) == 0 {
		return 0, true
	}
	rest := win.Size - int64(len(win.Head))
	scaled := float64(tail) * float64(rest) / float64(len(win.Tail))
	return int(scaled), true
}

func piEntryTimestamp(raw []byte) time.Time {
	_, rest, found := bytes.Cut(raw, piEntryTimestampMarker)
	if !found {
		return time.Time{}
	}
	value, _, closed := bytes.Cut(rest, []byte(`"`))
	if !closed {
		return time.Time{}
	}
	return parseClaudeTime(string(value))
}

// Preview reads a bounded head and tail window. pi puts the session ID, the
// workspace, and the start time on line 1, so only that line is decoded rather
// than a run of lines carrying scattered metadata.
//
// The turn count is the number of assistant messages in the window, scaled to
// the file. Unlike Claude Code there is nothing to deduplicate: one assistant
// entry is one turn, because pi's turn_end fires once per LLM request. In a fork
// the copied region is left out, because the import leaves it out too.
func (p *piImporter) Preview(ctx context.Context, path string) (SessionPreview, bool, error) {
	if err := ctx.Err(); err != nil {
		return SessionPreview{}, false, err
	}
	win, err := ReadPreviewWindows(path, PreviewByteBudget)
	if err != nil {
		return SessionPreview{}, false, err
	}
	head := win.HeadLines()
	if len(head) == 0 {
		return SessionPreview{}, false, nil // not a usable session
	}

	pv := SessionPreview{Agent: AgentPi, SourcePath: path, SizeBytes: win.Size}
	var header piPreviewLine
	if err := json.Unmarshal(head[0], &header); err != nil || header.Type != "session" {
		return SessionPreview{}, false, nil // no session header: not a pi session
	}
	pv.SessionID = header.ID
	pv.Workspace = header.CWD
	pv.StartedAt = parseClaudeTime(header.Timestamp)

	var cutoff time.Time
	if strings.TrimSpace(header.ParentSession) != "" {
		cutoff = pv.StartedAt
	}
	pv.TurnCount, pv.ApproxTurns = piEstimateTurns(win, cutoff)

	// The title and the last activity both come from the end of the file: the
	// title from the newest session_info, which is where pi keeps the user-set
	// display name, and the activity from the last entry that carries a
	// timestamp.
	last := win.TailLines()
	if win.Whole {
		last = head
	}
	for i := len(last) - 1; i >= 0 && i >= len(last)-previewMetadataLines; i-- {
		var line piPreviewLine
		if err := json.Unmarshal(last[i], &line); err != nil {
			continue
		}
		if pv.LastActivityAt.IsZero() {
			if ts := parseClaudeTime(line.Timestamp); !ts.IsZero() {
				pv.LastActivityAt = ts
			}
		}
		if pv.Title == "" && line.Type == "session_info" {
			pv.Title = strings.TrimSpace(line.Name)
		}
	}

	if pv.SessionID == "" {
		pv.SessionID = piSessionIDFromPath(path)
	}
	if pv.Title == "" {
		// The session ID, never prompt text: a preview runs over every session
		// on the machine and is rendered before the user picks anything.
		pv.Title = pv.SessionID
	}
	if pv.LastActivityAt.IsZero() {
		pv.LastActivityAt = win.ModTime
	}
	return pv, true, nil
}

// piSessionIDFromPath recovers the session ID from a session filename, which pi
// builds as <timestamp>_<sessionID>.jsonl. It is the fallback for a file whose
// header carries no ID.
func piSessionIDFromPath(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if _, id, ok := strings.Cut(base, "_"); ok && id != "" {
		return id
	}
	return base
}

// piEntry is one line of a session log. Content is decoded here and nowhere
// else: Preview uses piPreviewLine instead.
type piEntry struct {
	Type      string     `json:"type"`
	ID        string     `json:"id"`
	ParentID  *string    `json:"parentId"`
	Timestamp string     `json:"timestamp"`
	Message   *piMessage `json:"message"`

	// Session header fields, on line 1 only.
	CWD           string `json:"cwd"`
	ParentSession string `json:"parentSession"`

	// session_info fields.
	Name string `json:"name"`

	// copied marks an entry a fork inherited from its trunk rather than
	// produced itself. Set while reading, never decoded from the line.
	copied bool
}

// piMessage is a pi AgentMessage: a user message, an assistant message, or a
// tool result. The union is flat on disk, so one struct covers all three.
type piMessage struct {
	Role string `json:"role"`
	// Content is a string or an array of blocks for a user message, and an
	// array of blocks for an assistant message and a tool result.
	Content json.RawMessage `json:"content"`

	// Assistant fields.
	Provider     string   `json:"provider"`
	Model        string   `json:"model"`
	ResponseID   string   `json:"responseId"`
	StopReason   string   `json:"stopReason"`
	ErrorMessage string   `json:"errorMessage"`
	Usage        *piUsage `json:"usage"`
	// Timestamp is unix milliseconds, stamped when the provider builds the
	// message object, which is before the HTTP request goes out.
	Timestamp int64 `json:"timestamp"`

	// Tool result fields.
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	IsError    bool   `json:"isError"`
}

// piUsage is pi's Usage. A count a session written by an older pi does not carry
// decodes to zero, and the turn is then flagged with QualityReport.ApproxUsage,
// so "the source said nothing about tokens" is never read as "no tokens".
type piUsage struct {
	Input       int64   `json:"input"`
	Output      int64   `json:"output"`
	CacheRead   int64   `json:"cacheRead"`
	CacheWrite  int64   `json:"cacheWrite"`
	TotalTokens int64   `json:"totalTokens"`
	Cost        *piCost `json:"cost"`
}

type piCost struct {
	Total *float64 `json:"total"`
}

// piContentBlock is one block of message content. One struct covers the union
// the way it is written on disk. An image block has no field here: agento11y's
// message parts cannot represent one, so both sides drop it.
type piContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Redacted  bool            `json:"redacted"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// piSession is a decoded session log: its header, its entries in file order,
// and the indexes the mapping walks.
type piSession struct {
	header  piEntry
	entries []piEntry
	byID    map[string]int // entry ID -> index in entries
	// turnIndex numbers the assistant entries this session produced, in file
	// order, keyed by their position in entries. It is the TurnIndex of the
	// generation each one exports, which makes a parent's generation ID
	// resolvable before the parent is reached. An entry a fork copied from its
	// trunk is not in it, because this session did not produce it. The key is
	// the position rather than the entry ID so two entries that somehow carry
	// no ID cannot land on one generation ID.
	turnIndex map[int]int
	// forkedAt is the fork instant read from the header, and the cutoff that
	// separates copied entries from the session's own. Zero when the session is
	// not a fork, or when its header carries no usable timestamp.
	forkedAt time.Time
}

// readPiSession decodes a whole session file.
//
// pi session files are small: the sessions tree on the development machine holds
// 5,977 .jsonl files totalling 2.0 GB, of which 2,536 are top-level sessions and
// the rest nested subagent runs, and the largest single file is 10.6 MB. So the
// streaming Codex needs for its 700 MB rollouts buys nothing here. Turns are
// still yielded one at a time.
//
// A fork's copied entries are marked, not dropped: lineage has to walk into
// them to recognise a parent that belongs to the trunk, while [piImporter.Turns]
// must not export them. pi copies the trunk's entries with their own
// timestamps and stamps the fork's header at fork time, so an entry at or
// before the header instant came from the trunk. The trunk's own import exports
// those turns; exporting them again here would report one model call twice,
// under two conversation IDs.
func readPiSession(path string) (*piSession, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sess := &piSession{byID: map[string]int{}, turnIndex: map[int]int{}}
	turns := 0
	for raw := range bytes.SplitSeq(data, []byte{'\n'}) {
		raw = bytes.TrimRight(raw, "\r")
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var entry piEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			continue // a partially written last line, or schema drift
		}
		if entry.Type == "session" {
			if sess.header.Type == "" {
				sess.header = entry
				if strings.TrimSpace(entry.ParentSession) != "" {
					sess.forkedAt = parseClaudeTime(entry.Timestamp)
				}
			}
			continue
		}
		entry.copied = sess.isCopied(entry)
		if entry.ID != "" {
			sess.byID[entry.ID] = len(sess.entries)
		}
		if piIsAssistantEntry(entry) && !entry.copied {
			sess.turnIndex[len(sess.entries)] = turns
			turns++
		}
		sess.entries = append(sess.entries, entry)
	}
	return sess, nil
}

// isCopied reports whether an entry predates the fork that created this
// session. A session that is not a fork copied nothing. An entry with no
// readable timestamp counts as the session's own: dropping a turn on a guess
// loses data the file does have.
func (s *piSession) isCopied(entry piEntry) bool {
	if s.forkedAt.IsZero() {
		return false
	}
	ts := parseClaudeTime(entry.Timestamp)
	return !ts.IsZero() && !ts.After(s.forkedAt)
}

func piIsAssistantEntry(entry piEntry) bool {
	return entry.Type == "message" && entry.Message != nil && entry.Message.Role == "assistant"
}

// sourceRef locates the turn the assistant entry at position pos exports.
func (s *piSession) sourceRef(pos int, conversationID, sourcePath string) SourceRef {
	turn := s.turnIndex[pos]
	entryID := ""
	if pos >= 0 && pos < len(s.entries) {
		entryID = s.entries[pos].ID
	}
	return SourceRef{
		Agent:      AgentPi,
		SessionID:  conversationID,
		SourcePath: sourcePath,
		TurnIndex:  turn,
		TurnID:     firstNonEmptyString(entryID, fallbackTurnID(turn)),
	}
}

// Turns yields one generation per assistant entry, carrying the tool results
// that answer that entry's tool calls.
//
// One assistant entry is one generation because pi's turn_end fires once per
// LLM request with one message and its tool results, so an imported turn covers
// the same call live capture would have exported.
//
// Every assistant entry the session produced is imported, including entries on
// branches the user abandoned. Each regeneration was a separate model call that
// live capture exported, and the parentId chain keeps an abandoned branch
// parented to the turn it diverged from. A fork's copied entries are the one
// exception: they belong to the trunk, which exports them itself.
func (p *piImporter) Turns(ctx context.Context, sess SessionPreview) iter.Seq2[HistoricalGeneration, error] {
	return func(yield func(HistoricalGeneration, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(HistoricalGeneration{}, err)
			return
		}
		log, err := readPiSession(sess.SourcePath)
		if err != nil {
			yield(HistoricalGeneration{}, fmt.Errorf("read pi session %s: %w", sess.SourcePath, err))
			return
		}
		conversationID := firstNonEmptyString(
			log.header.ID,
			sess.SessionID,
			piSessionIDFromPath(sess.SourcePath),
		)
		fork := resolvePiFork(log.header, sess.SourcePath)
		title := piTitle(log, conversationID)

		var pendingInput []agento11y.Message
		for i, entry := range log.entries {
			if err := ctx.Err(); err != nil {
				yield(HistoricalGeneration{}, err)
				return
			}
			// A fork's copied entries are skipped whole: the trunk exported both
			// its turns and the prompts that drove them.
			if entry.Type != "message" || entry.Message == nil || entry.copied {
				continue
			}
			switch entry.Message.Role {
			case "user":
				// Buffered until the next assistant entry, which is what live
				// does with the user messages seen since the last turn.
				if msg, ok := piUserMessage(*entry.Message); ok {
					pendingInput = append(pendingInput, msg)
				}
				continue
			case "assistant":
			default:
				continue // a tool result, collected by the turn it answers
			}

			gen := piGeneration(log, i, entry, pendingInput, conversationID, title, sess.SourcePath, fork)
			pendingInput = nil
			if !yield(gen, nil) {
				return
			}
		}
	}
}

// piGeneration maps one assistant entry into a historical generation.
func piGeneration(
	log *piSession,
	index int,
	entry piEntry,
	input []agento11y.Message,
	conversationID, title, sourcePath string,
	fork *piFork,
) HistoricalGeneration {
	msg := entry.Message
	src := log.sourceRef(index, conversationID, sourcePath)
	blocks := piBlocks(msg.Content)

	gen := agento11y.Generation{
		ID:                src.GenerationID(),
		ConversationID:    conversationID,
		ConversationTitle: title,
		Mode:              agento11y.GenerationModeStream,
		// pi streams every provider response, and the SDK records the
		// time-to-first-token histogram only for streamText, so live exports
		// this operation name too.
		OperationName: "streamText",
		Model:         agento11y.ModelRef{Provider: msg.Provider, Name: msg.Model},
		ResponseID:    msg.ResponseID,
		ResponseModel: msg.Model,
		StopReason:    piStopReason(msg.StopReason),
		Usage:         piMapUsage(msg.Usage),
		Input:         input,
		Output:        append(piAssistantOutput(blocks), piToolResultsOutput(log, index, blocks)...),
		Tools:         piToolDefinitions(blocks),
		CallError:     msg.ErrorMessage,
		Tags:          piTags(log.header.CWD),
	}
	if piHasThinking(blocks) {
		gen.ThinkingEnabled = new(true)
	}
	if msg.Usage != nil && msg.Usage.Cost != nil && msg.Usage.Cost.Total != nil {
		gen.Metadata = map[string]any{"cost_usd": *msg.Usage.Cost.Total}
	}

	// Both timestamps are real. The assistant message's unix-ms timestamp is
	// stamped when the provider builds the message, before the request goes
	// out; the entry's ISO timestamp is when pi appended the entry, after the
	// stream ended. The clamp mirrors live's Math.min: a clock adjustment must
	// not invert the pair.
	gen.CompletedAt = parseClaudeTime(entry.Timestamp)
	if msg.Timestamp > 0 {
		gen.StartedAt = time.UnixMilli(msg.Timestamp).UTC()
	}
	if !gen.StartedAt.IsZero() && !gen.CompletedAt.IsZero() && gen.StartedAt.After(gen.CompletedAt) {
		gen.StartedAt = gen.CompletedAt
	}

	quality := piQuality(gen)
	piApplyLineage(&gen, &quality, log, entry, conversationID, sourcePath, fork)
	return HistoricalGeneration{Source: src, Gen: gen, Quality: quality}
}

// piTags returns the built-in tags for an imported pi turn. There is no branch
// tag: the log records no branch, and resolving one at import time would record
// the branch checked out now rather than the one the session ran on.
func piTags(cwd string) map[string]string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return nil
	}
	return map[string]string{"cwd": cwd}
}

// piQuality reports what the session log did not carry. Neither timestamp flag
// is ever set from a well-formed entry: pi persists both, so a zero value means
// the line was malformed rather than incomplete.
func piQuality(gen agento11y.Generation) QualityReport {
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

// piMapUsage ports mapPiUsage (plugins/pi/src/mappers.ts).
//
// The field set is narrower than pi's Usage: reasoning and cacheWrite1h are not
// forwarded, and totalTokens is passed through as pi computes it
// (input + output + cacheRead + cacheWrite) rather than recomputed to the Go
// launchers' input + output. Both choices are live behavior and moving either
// here would put the import out of step with it.
func piMapUsage(usage *piUsage) agento11y.TokenUsage {
	if usage == nil {
		return agento11y.TokenUsage{}
	}
	return agento11y.TokenUsage{
		InputTokens:           usage.Input,
		OutputTokens:          usage.Output,
		TotalTokens:           usage.TotalTokens,
		CacheReadInputTokens:  usage.CacheRead,
		CacheWriteInputTokens: usage.CacheWrite,
	}
}

// piStopReason ports mapStopReason (plugins/pi/src/mappers.ts). An unknown
// reason passes through, which is how live records a stop reason a newer pi
// added.
func piStopReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "toolUse":
		return "tool_use"
	default:
		return reason
	}
}

// piBlocks decodes a message's content blocks. A string content, which pi
// allows for a user message, becomes one text block.
//
// A decode error keeps whatever decoded. Go's decoder fills every element it
// can and reports the first type error at the end, so one block with an
// unexpected field type costs that block and not the turn: returning nil would
// export a turn that said the model produced nothing.
func piBlocks(raw json.RawMessage) []piContentBlock {
	if len(raw) == 0 {
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []piContentBlock{{Type: "text", Text: text}}
	}
	var blocks []piContentBlock
	_ = json.Unmarshal(raw, &blocks)
	return blocks
}

// piAssistantOutput ports mapAssistantOutput (plugins/pi/src/mappers.ts): one
// output message per content block, in the order the model produced them.
//
// Unlike Claude Code, pi persists thinking text, so an imported turn carries
// the same thinking a live one did. A redacted thinking block is skipped, the
// way live skips it.
func piAssistantOutput(blocks []piContentBlock) []agento11y.Message {
	var out []agento11y.Message
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if strings.TrimSpace(block.Text) == "" {
				continue
			}
			out = append(out, agento11y.Message{
				Role:  agento11y.RoleAssistant,
				Parts: []agento11y.Part{{Kind: agento11y.PartKindText, Text: block.Text}},
			})
		case "thinking":
			if block.Redacted || strings.TrimSpace(block.Thinking) == "" {
				continue
			}
			out = append(out, agento11y.Message{
				Role:  agento11y.RoleAssistant,
				Parts: []agento11y.Part{{Kind: agento11y.PartKindThinking, Thinking: block.Thinking}},
			})
		case "toolCall":
			out = append(out, agento11y.Message{
				Role: agento11y.RoleAssistant,
				Parts: []agento11y.Part{{
					Kind: agento11y.PartKindToolCall,
					ToolCall: &agento11y.ToolCall{
						ID:        block.ID,
						Name:      block.Name,
						InputJSON: piToolArguments(block.Arguments),
					},
				}},
			})
		}
	}
	return out
}

// piToolArguments returns a tool call's arguments as compact JSON.
//
// pi's type makes arguments required, and all 143,105 tool calls on the
// development machine carry them, 9 of those as "{}", where live sends "{}"
// too. The absent case is where the two diverge and has never been observed:
// live stringifies the object, JSON.stringify(undefined) is undefined, and the
// SDK exports that as an empty field, while an absent object becomes "{}" here.
func piToolArguments(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("{}")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return json.RawMessage("{}")
	}
	return json.RawMessage(buf.Bytes())
}

// piToolResultsOutput ports mapToolResultsOutput (plugins/pi/src/mappers.ts)
// for the assistant entry at position index: the tool results that answer that
// entry's tool calls, whose content blocks the caller already decoded.
//
// Live receives them on the turn_end event. On disk each one is its own entry
// appended after the assistant entry, so they are matched by tool call ID and
// yielded in the order the calls were made, which is the order live saw them.
func piToolResultsOutput(log *piSession, index int, blocks []piContentBlock) []agento11y.Message {
	calls := piToolCallIDs(blocks)
	if len(calls) == 0 {
		return nil
	}
	results := piToolResultsByCallID(log, index)
	out := make([]agento11y.Message, 0, len(calls))
	for _, callID := range calls {
		result, ok := results[callID]
		if !ok {
			continue // the run ended before the tool answered
		}
		out = append(out, agento11y.Message{
			Role: agento11y.RoleTool,
			Parts: []agento11y.Part{{
				Kind: agento11y.PartKindToolResult,
				ToolResult: &agento11y.ToolResult{
					ToolCallID: result.Message.ToolCallID,
					Name:       result.Message.ToolName,
					IsError:    result.Message.IsError,
					Content:    piToolResultText(result.Message.Content),
				},
			}},
		})
	}
	return out
}

// piToolResultsByCallID collects the tool result entries that answer the
// assistant entry at position index. It walks forward from that position and
// stops at the next assistant entry, because a tool result appended after that
// one answers that turn's calls instead.
//
// The walk starts from the position rather than from a lookup of the entry's ID,
// for the same reason the generation ID is built from the position: an entry
// that somehow carries no ID, or shares one with another entry, would otherwise
// collect another turn's results or none at all, with nothing to flag it.
func piToolResultsByCallID(log *piSession, index int) map[string]piEntry {
	out := map[string]piEntry{}
	if index < 0 || index >= len(log.entries) {
		return out
	}
	for _, next := range log.entries[index+1:] {
		if piIsAssistantEntry(next) {
			break
		}
		if next.Type != "message" || next.Message == nil || next.Message.Role != "toolResult" {
			continue
		}
		if _, seen := out[next.Message.ToolCallID]; !seen {
			out[next.Message.ToolCallID] = next
		}
	}
	return out
}

func piToolCallIDs(blocks []piContentBlock) []string {
	var out []string
	for _, block := range blocks {
		if block.Type == "toolCall" {
			out = append(out, block.ID)
		}
	}
	return out
}

// piToolResultText flattens a tool result's content blocks to text, dropping
// the image blocks agento11y's message parts cannot represent.
//
// An empty text block is dropped as well, the way toolResultText
// (plugins/pi/src/mappers.ts) drops it, so two blocks where the first is empty
// join to one line rather than to a leading newline. piUserMessage keeps empty
// blocks, because userMessageText keeps them.
func piToolResultText(raw json.RawMessage) string {
	var parts []string
	for _, block := range piBlocks(raw) {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// piToolDefinitions names the tools the turn called.
//
// Live seeds the full catalog with descriptions and schemas from pi's runtime
// registry (mapTools). None of that is in the session log, so an imported turn
// carries name-only definitions for the tools it actually called. Which tools
// were offered but unused is unrecoverable.
func piToolDefinitions(blocks []piContentBlock) []agento11y.ToolDefinition {
	var out []agento11y.ToolDefinition
	seen := map[string]bool{}
	for _, block := range blocks {
		if block.Type != "toolCall" || block.Name == "" || seen[block.Name] {
			continue
		}
		seen[block.Name] = true
		out = append(out, agento11y.ToolDefinition{Name: block.Name})
	}
	return out
}

func piHasThinking(blocks []piContentBlock) bool {
	for _, block := range blocks {
		if block.Type == "thinking" {
			return true
		}
	}
	return false
}

// piUserMessage ports mapUserMessage (plugins/pi/src/mappers.ts): one input
// message per user entry, text parts joined with a newline, image parts
// dropped, and an empty message skipped.
func piUserMessage(msg piMessage) (agento11y.Message, bool) {
	var parts []string
	for _, block := range piBlocks(msg.Content) {
		if block.Type == "text" {
			parts = append(parts, block.Text)
		}
	}
	text := strings.Join(parts, "\n")
	if strings.TrimSpace(text) == "" {
		return agento11y.Message{}, false
	}
	return agento11y.Message{
		Role:  agento11y.RoleUser,
		Parts: []agento11y.Part{{Kind: agento11y.PartKindText, Text: text}},
	}, true
}

// piTitle ports resolveConversationTitle (plugins/pi/src/mappers.ts): pi's
// user-set session name wins, then the first user prompt, then the session ID.
//
// The name is read from the last session_info entry, which is the name in force
// when the session was last written. Live resolves it per turn, so a session
// renamed halfway through gets the new name only on later turns; an import
// applies one name to the whole session, which is the one difference the
// fixture README records.
func piTitle(log *piSession, conversationID string) string {
	name := ""
	firstUserText := ""
	for _, entry := range log.entries {
		if entry.Type == "session_info" && strings.TrimSpace(entry.Name) != "" {
			name = strings.TrimSpace(entry.Name)
		}
		// A copied prompt is not this session's first prompt: live sees only the
		// prompts the fork itself sent.
		if firstUserText != "" || entry.copied ||
			entry.Type != "message" || entry.Message == nil || entry.Message.Role != "user" {
			continue
		}
		if msg, ok := piUserMessage(*entry.Message); ok {
			firstUserText = strings.TrimSpace(msg.Parts[0].Text)
		}
	}
	if name != "" {
		return piClipTitle(name)
	}
	if firstUserText != "" {
		return piClipTitle(firstUserText)
	}
	return conversationID
}

func piClipTitle(text string) string {
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) <= piMaxTitleLen {
		return text
	}
	return string([]rune(text)[:piMaxTitleLen])
}

// Fork metadata keys, matching what the live plugin writes
// (plugins/pi/src/index.ts). A fork copies the trunk's entries, so the parent
// turn of the fork's first generation belongs to the trunk conversation and
// gets a metadata pointer instead of a parent edge.
const (
	MetaPiForkParentSession    = "pi.fork.parent_session_id"
	MetaPiForkParentGeneration = "pi.fork.parent_generation_id"
)

// piFork is what a forked session knows about its trunk. nil means the session
// is not a fork.
type piFork struct {
	// forkedAt is the fork instant: the fork's own header timestamp. An entry
	// stamped at or before it was copied from the trunk.
	forkedAt time.Time
	// trunkPath is where the trunk was read from, and also the SourcePath the
	// trunk generation ID hashes, so the fork's pointer is right only when it
	// spells the trunk the way discovery does. It is cleaned for that reason. A
	// trunk reachable under a genuinely different path, through a symlinked home
	// or a relative PI_CODING_AGENT_DIR, still reads fine and still yields a
	// pointer to an ID nothing ingested; normalizing SourcePath across the
	// framework is what would close that, and the fixture README records it.
	trunkPath string
	// trunkConversationID and trunkStartedAt come from the trunk file's own
	// line 1, so no value is parsed out of a file name.
	trunkConversationID string
	trunkStartedAt      time.Time
	// trunk is the trunk's decoded session. Its own turn numbering is what the
	// trunk's import used as TurnIndex, so a trunk generation ID can be
	// reproduced here.
	trunk *piSession
	// unreadable is set when the header named a trunk this process could not
	// read. The parent link is then dropped rather than guessed.
	unreadable bool
}

// resolvePiFork reads a session header's parentSession and, when there is one,
// the trunk file it names. Ported from sessionOrigin.ts and lineage.ts.
func resolvePiFork(header piEntry, sourcePath string) *piFork {
	parent := strings.TrimSpace(header.ParentSession)
	if parent == "" {
		return nil
	}
	fork := &piFork{forkedAt: parseClaudeTime(header.Timestamp)}
	// pi stores parentSession as typed, so a relative path is relative to the
	// cwd the header recorded, not to whichever process reads it later. Both
	// branches end cleaned, because the same string is hashed into the trunk's
	// generation ID: "/s/a/../a/x.jsonl" and "/s/a/x.jsonl" read the same file
	// and would otherwise name two different generations.
	fork.trunkPath = filepath.Clean(parent)
	if !filepath.IsAbs(parent) {
		if cwd := strings.TrimSpace(header.CWD); cwd != "" {
			fork.trunkPath = filepath.Join(cwd, parent)
		} else {
			fork.trunkPath = filepath.Join(filepath.Dir(sourcePath), parent)
		}
	}
	trunk, err := readPiSession(fork.trunkPath)
	if err != nil || trunk.header.ID == "" {
		fork.unreadable = true
		return fork
	}
	fork.trunkConversationID = trunk.header.ID
	fork.trunkStartedAt = parseClaudeTime(trunk.header.Timestamp)
	fork.trunk = trunk
	return fork
}

// piApplyLineage sets the turn's parent, porting findParentAssistantEntry and
// classifyParentEntry (plugins/pi/src/lineage.ts).
//
// The parent is the nearest ancestor assistant entry on the parentId chain, not
// the previous entry: pi appends a turn as a child of the current leaf, which
// is usually a tool result. On a fork, a parent entry stamped at or before the
// fork instant was copied from the trunk, so its generation belongs to the
// trunk conversation: it ships as metadata instead of an edge, because an edge
// would name a generation that does not exist under this conversation ID.
func piApplyLineage(
	gen *agento11y.Generation,
	quality *QualityReport,
	log *piSession,
	entry piEntry,
	conversationID, sourcePath string,
	fork *piFork,
) {
	parent, parentPos, ok := piParentAssistantEntry(log, entry)
	if !ok {
		return
	}
	switch piClassifyParent(parent, fork) {
	case piParentTrunk:
		// Handled below: a copied parent needs the trunk resolved first.
	case piParentOwn:
		gen.ParentGenerationIDs = []string{
			log.sourceRef(parentPos, conversationID, sourcePath).GenerationID(),
		}
		return
	case piParentUnknown:
		// The fork instant or the parent's own timestamp is missing, so the
		// parent cannot be placed on either side of the fork. Nothing is
		// linked: an ID built on a guess names a generation that may not exist.
		quality.Notes = append(quality.Notes, "pi_fork_parent_unplaceable")
		return
	}
	if fork.unreadable {
		// A named but unreadable trunk: the parent turn cannot be placed in
		// either conversation, so nothing is linked. Live degrades the same way.
		quality.Notes = append(quality.Notes, "pi_fork_trunk_unreadable")
		return
	}
	trunkGen, ok := piTrunkGenerationID(parent, fork)
	if !ok {
		// The trunk holds no generation for the copied entry, so there is
		// nothing to point at. Both keys go together or neither does, the way
		// live's forkMetadata writes them: a session ID with no generation ID
		// names a conversation without saying which turn.
		quality.Notes = append(quality.Notes, "pi_fork_parent_not_in_trunk")
		return
	}
	piSetMetadata(gen, MetaPiForkParentSession, fork.trunkConversationID)
	piSetMetadata(gen, MetaPiForkParentGeneration, trunkGen)
}

// piParentKind says which conversation a parent turn's generation belongs to.
type piParentKind int

const (
	// piParentOwn: this session produced the parent turn, so an edge is safe.
	piParentOwn piParentKind = iota
	// piParentTrunk: a fork copied the parent entry in, so its generation
	// belongs to the trunk conversation.
	piParentTrunk
	// piParentUnknown: the parent cannot be placed on either side of the fork.
	piParentUnknown
)

// piClassifyParent ports classifyParentEntry (plugins/pi/src/lineage.ts). The
// boundary is the fork's header timestamp: a fork copies the trunk's entries
// with their own timestamps and stamps its header at fork time, so every entry
// at or before that instant came from the trunk.
func piClassifyParent(parent piEntry, fork *piFork) piParentKind {
	if fork == nil {
		return piParentOwn
	}
	parentAt := parseClaudeTime(parent.Timestamp)
	if fork.forkedAt.IsZero() || parentAt.IsZero() {
		return piParentUnknown
	}
	if parentAt.After(fork.forkedAt) {
		return piParentOwn
	}
	return piParentTrunk
}

// piTrunkGenerationID returns the generation ID the trunk's own import gave the
// copied parent turn.
//
// ok is false when the trunk cannot hold that generation: a fork of a fork
// inherits entries an intermediate session copied in and never ran itself, and
// those are stamped before the trunk's start.
func piTrunkGenerationID(parent piEntry, fork *piFork) (string, bool) {
	if fork.trunkConversationID == "" {
		return "", false
	}
	parentAt := parseClaudeTime(parent.Timestamp)
	if fork.trunkStartedAt.IsZero() || parentAt.IsZero() || !parentAt.After(fork.trunkStartedAt) {
		return "", false
	}
	pos, ok := fork.trunk.byID[parent.ID]
	if !ok {
		return "", false
	}
	if _, produced := fork.trunk.turnIndex[pos]; !produced {
		// The trunk copied this entry in as well, so it exported no generation
		// for it either.
		return "", false
	}
	return fork.trunk.sourceRef(pos, fork.trunkConversationID, fork.trunkPath).GenerationID(), true
}

// piParentAssistantEntry walks the parentId chain from entry toward the root and
// returns the nearest ancestor assistant entry with its position in the session.
func piParentAssistantEntry(log *piSession, entry piEntry) (piEntry, int, bool) {
	seen := map[string]bool{}
	cursor := entry.ParentID
	for cursor != nil && *cursor != "" && !seen[*cursor] {
		seen[*cursor] = true
		pos, ok := log.byID[*cursor]
		if !ok {
			return piEntry{}, 0, false
		}
		parent := log.entries[pos]
		if piIsAssistantEntry(parent) {
			return parent, pos, true
		}
		cursor = parent.ParentID
	}
	return piEntry{}, 0, false
}

func piSetMetadata(gen *agento11y.Generation, key, value string) {
	if value == "" {
		return
	}
	if gen.Metadata == nil {
		gen.Metadata = map[string]any{}
	}
	gen.Metadata[key] = value
}
