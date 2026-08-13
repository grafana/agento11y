package history

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/chatstore"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/mapper"
)

func init() {
	Register(AgentSpec{
		ID:          AgentCursor,
		DisplayName: "Cursor",
		Aliases:     []string{"cursor-agent"},
	}, func() Importer { return &cursorImporter{} })
}

// cursorStoreName is the name of the file every Cursor session lives in.
const cursorStoreName = "store.db"

// cursorPreviewIDTail is how many of a session's last messages Preview reads
// provider IDs from to date the session's end. The last ID a session issued is
// in its last few messages, and reading the tail keeps the preview off a scan of
// the whole session, which for a long one is hundreds of megabytes of JSON.
const cursorPreviewIDTail = 32

// cursorMaxFileSpan bounds how far a store's modification time may sit from its
// createdAt before the file is treated as no evidence of when the session ran.
//
// A modification time is a record of the last process to write the file, not of
// the conversation. Cursor checkpoints a session's write-ahead log when it next
// opens the database, so one Cursor start re-dates every session on the machine:
// on 127 real stores, 42 store.db-wal files shared a modification time to the
// second, months after the sessions in them ran, and 45 of the 127 produced a
// span of over a week that way. A Cursor session is one sitting at a keyboard,
// so a span past this is a fact about the file rather than about the session.
//
// Six hours rather than a day because the same batch shows up on store.db: three
// of the 127 were created in the same millisecond and written in the same second
// sixteen hours later, which gave each of them a single sixteen-hour turn. The
// longest turn any provider actually dated in that corpus ran 3h53m, and the
// longest span a file suggested for an undated session, outside that batch, was
// 3h34m.
const cursorMaxFileSpan = 6 * time.Hour

// cursorWALSuffix names a store's write-ahead log, and cursorWALHeaderSize is
// the size of an empty one: a header and no frames. A log that small holds none
// of the session, so its modification time records a checkpoint by some other
// process rather than a write of the conversation.
const (
	cursorWALSuffix     = "-wal"
	cursorWALHeaderSize = 32
)

// cursorImporter reads Cursor sessions under ~/.cursor/chats.
//
// Cursor stores a session as a content-addressed blob table rather than a
// transcript, so the decoding lives in [chatstore] and this file only groups the
// decoded messages into turns and hands each turn to the live [mapper].
//
// The store records no per-turn timestamp field, no per-turn token usage, and
// only a session-wide model name. Every turn is therefore marked
// ApproxStartedAt, ApproxCompletedAt and ApproxUsage, and a session with no
// recorded model is marked MissingModel. Usage is left at zero rather than
// charged from the session total.
//
// A turn is dated from the provider IDs its messages carry, which is a
// measurement at second resolution taken on the provider's clock: see
// cursor_clock.go. A turn whose provider issues IDs with no time in them, and a
// session on a model that never does, fall back to a window interpolated across
// the session's span, which is a guess and is flagged as one.
//
// A prompt is what the user typed, with Cursor's <user_query> wrapper removed,
// and the environment block Cursor prepends as a message of its own kept in
// front of it. See [cursorUnwrapPrompt].
//
// Content reaches the framework [Sanitizer] already redacted, unlike the other
// three importers. The Cursor mapper redacts unconditionally and takes no raw
// mode, so reusing it means the redaction runs twice. Redacting redacted text
// changes nothing, but it does mean the framework is not the single redaction
// point here that its documentation describes.
type cursorImporter struct {
	// roots overrides the resolved chats directory. Tests set it.
	roots []string
	// now supplies the clock the mapper falls back to. nil uses time.Now.
	now func() time.Time
}

func (c *cursorImporter) Roots() []string {
	if len(c.roots) > 0 {
		return c.roots
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".cursor", "chats")}
}

// Match accepts the session database and nothing beside it. SQLite's
// store.db-wal and store.db-shm sit in the same directory, and matching either
// would preview and import the same session more than once.
func (c *cursorImporter) Match(path string) bool {
	return filepath.Base(path) == cursorStoreName
}

func (c *cursorImporter) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Preview reads the meta row and the root record: the session ID, its title, its
// start time, its workspace, and its turn count. It decodes no message blob in
// Go, so no prompt or response text is read, let alone returned.
//
// [PreviewByteBudget] is not applied, because there is no window to apply it to:
// the metadata is two indexed row reads, and the turn count is one query that
// asks SQLite which of the listed messages are prompts. That query does make
// SQLite read and JSON-parse every listed message, so the read is linear in the
// session's message bytes even though only a count comes back. The alternative,
// counting messages instead of prompts, overstates the turns about sixfold, and
// the figure is what the user is asked to approve.
func (c *cursorImporter) Preview(ctx context.Context, path string) (SessionPreview, bool, error) {
	if err := ctx.Err(); err != nil {
		return SessionPreview{}, false, err
	}
	store, err := chatstore.Open(path)
	if err != nil {
		return SessionPreview{}, false, err
	}
	defer func() { _ = store.Close() }()

	meta, err := store.Meta(ctx)
	if err != nil {
		return SessionPreview{}, false, err
	}
	root, ok, err := store.Root(ctx, meta.LatestRootBlobID)
	if err != nil {
		return SessionPreview{}, false, err
	}
	if !ok {
		// Cursor writes a store as soon as a session is created, so a session
		// the user opened and never sent a message in has a meta row and no
		// root record. There is nothing to import.
		return SessionPreview{}, false, nil
	}

	turns, err := store.PromptCount(ctx, root.MessageIDs)
	if err != nil {
		return SessionPreview{}, false, err
	}

	sessionID := firstNonEmptyString(meta.AgentID, cursorSessionIDFromPath(path))
	p := SessionPreview{
		Agent:     AgentCursor,
		SessionID: sessionID,
		// The title is the session ID: a preview must not surface prompt text,
		// and Cursor's own session name is user-facing text Cursor derives from the
		// first prompt.
		Title:      sessionID,
		Workspace:  root.Workspace(),
		SourcePath: path,
		TurnCount:  turns,
		// The count is the session's prompts, and an import can produce fewer
		// generations than that: an interrupted session's last prompt was never
		// answered, so there is no turn to export. Marking it approximate keeps
		// the plan from promising a number the run then misses.
		ApproxTurns: turns > 0,
		StartedAt:   meta.Created(),
		SizeBytes:   cursorStoreSize(path),
	}
	p.LastActivityAt, err = cursorLastActivity(ctx, store, path, meta.Created(), root.MessageIDs)
	if err != nil {
		return SessionPreview{}, false, err
	}
	if p.StartedAt.IsZero() {
		p.StartedAt = p.LastActivityAt
	}
	return p, true, nil
}

// cursorLastActivity is when the session stopped.
//
// The provider IDs in the last messages answer it where the provider puts a time
// in an ID, and that is the only source here that is about the conversation. The
// store's modification time is the fallback, and it is one only within
// [cursorMaxFileSpan] of the session's start: past that it is a record of some
// later process opening the database. A session with neither ends where it
// started, which sorts it by when it ran and leaves its turns to be spaced by
// [cursorWindows].
func cursorLastActivity(
	ctx context.Context, store *chatstore.Store, path string, created time.Time, messageIDs []string,
) (time.Time, error) {
	tail := messageIDs
	if len(tail) > cursorPreviewIDTail {
		tail = tail[len(tail)-cursorPreviewIDTail:]
	}
	ids, err := store.ProviderIDs(ctx, tail)
	if err != nil {
		return time.Time{}, err
	}
	if ts, ok := newCursorClock(created, ids).max(ids); ok && ts.After(created) {
		return ts, nil
	}

	mod := cursorLastWrite(path)
	if created.IsZero() {
		// Nothing to measure the file against. The write time is the only time
		// the session has, so it is both the start and the end.
		return mod, nil
	}
	if mod.After(created) && mod.Sub(created) <= cursorMaxFileSpan {
		return mod, nil
	}
	return created, nil
}

// cursorSessionIDFromPath recovers the session ID from the directory the store
// lives in, which Cursor names after the session. It is the fallback for a meta
// row that carries no ID.
func cursorSessionIDFromPath(path string) string {
	return filepath.Base(filepath.Dir(path))
}

// cursorStoreSize is the session's size on disk: the database plus its
// write-ahead log. A live Cursor leaves store.db one page long with the whole
// session in the log, so the database alone understates a session by orders of
// magnitude. The shared-memory file is an index, not data, and is left out.
func cursorStoreSize(path string) int64 {
	var total int64
	for _, p := range cursorStoreFiles(path) {
		if info, err := os.Stat(p); err == nil {
			total += info.Size()
		}
	}
	return total
}

// cursorLastWrite is when the session's data was last written: the later of the
// database's modification time and its write-ahead log's, and the log's only
// when the log still holds frames.
//
// The log is where a live Cursor writes, and Cursor checkpoints it rarely. Of
// 127 real stores, 72 had a one-page store.db whose modification time was
// seconds after the session was created, with hours of conversation in a log
// written later. Reading store.db alone would interpolate every turn of a long
// session into those first seconds, sort the session to the wrong end of the
// list, and let --since drop it.
//
// An emptied log is the opposite case: checkpointing it writes the frames into
// store.db and truncates it, so a log with nothing in it holds none of the
// session and its modification time is the checkpoint's, not Cursor's. 45 of the
// 127 stores were in that state, all re-dated to the same second.
func cursorLastWrite(path string) time.Time {
	var latest time.Time
	if info, err := os.Stat(path); err == nil {
		latest = info.ModTime()
	}
	info, err := os.Stat(path + cursorWALSuffix)
	if err == nil && info.Size() > cursorWALHeaderSize && info.ModTime().After(latest) {
		latest = info.ModTime()
	}
	return latest
}

// cursorStoreFiles are the files that hold a session's data. store.db-shm is an
// index SQLite rebuilds, so it is neither data nor a record of a write.
func cursorStoreFiles(path string) []string {
	return []string{path, path + cursorWALSuffix}
}

// Turns walks the session's message list in the order the root record gives and
// yields one generation per user prompt.
//
// A turn is a prompt and everything that follows it up to the next prompt: the
// assistant's text, its reasoning, and every tool call and result in between.
// That is the same span the live Cursor hook accumulates into one fragment
// between beforeSubmitPrompt and stop, which is why the live mapper can be
// reused unchanged.
//
// One turn is held at a time. The messages are read one blob at a time, so a
// session of several hundred megabytes costs one turn's memory.
func (c *cursorImporter) Turns(ctx context.Context, sess SessionPreview) iter.Seq2[HistoricalGeneration, error] {
	return func(yield func(HistoricalGeneration, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(HistoricalGeneration{}, err)
			return
		}
		store, err := chatstore.Open(sess.SourcePath)
		if err != nil {
			yield(HistoricalGeneration{}, err)
			return
		}
		defer func() { _ = store.Close() }()

		meta, err := store.Meta(ctx)
		if err != nil {
			yield(HistoricalGeneration{}, err)
			return
		}
		root, ok, err := store.Root(ctx, meta.LatestRootBlobID)
		if err != nil {
			yield(HistoricalGeneration{}, err)
			return
		}
		if !ok {
			return
		}

		// The IDs of the whole session are read before the walk, in one query
		// that returns IDs and no text, because the layout they carry a time in
		// is decided by which one dates the most of them. Deciding that from the
		// first turn alone would let one stray match date the rest wrongly.
		ids, err := store.ProviderIDs(ctx, root.MessageIDs)
		if err != nil {
			yield(HistoricalGeneration{}, err)
			return
		}

		r := &cursorReplay{
			importer:  c,
			sess:      sess,
			sessionID: firstNonEmptyString(meta.AgentID, sess.SessionID, cursorSessionIDFromPath(sess.SourcePath)),
			model:     strings.TrimSpace(meta.LastUsedModel),
			workspace: firstNonEmptyString(root.Workspace(), sess.Workspace),
			title:     strings.TrimSpace(meta.Name),
			clock:     newCursorClock(meta.Created(), ids),
			yield:     yield,
		}
		r.window = cursorTurnWindows(meta.Created(), sess.LastActivityAt, sess.TurnCount)
		r.previousEnd = r.window.start

		for msg, err := range store.Messages(ctx, root.MessageIDs) {
			if err != nil {
				yield(HistoricalGeneration{}, err)
				return
			}
			if r.observe(msg) {
				return // the consumer stopped
			}
		}
		if r.emit() {
			return
		}
		if err := r.walkReport(); err != nil {
			yield(HistoricalGeneration{}, err)
		}
	}
}

// Notes a Cursor turn can carry. They stay local: the export ships the quality
// booleans and not the notes.
const (
	// cursorNoteMissingTurnID is on every turn. No message carries a turn ID,
	// and the per-turn records that do are absent from most stores, so the
	// ordinal ID is used for every session rather than for some of them.
	cursorNoteMissingTurnID = "missing_turn_id"
	// cursorNoteOrphanToolResult says a tool result arrived with no call to
	// attach it to, so the pairing in this turn is one-sided.
	cursorNoteOrphanToolResult = "tool_result_without_call"
	// cursorNoteUnreadableMessage says a message next to this turn could not be
	// read, so the turn may be missing part of what it holds.
	cursorNoteUnreadableMessage = "unreadable_message"
	// cursorNoteInterpolatedTimes says no message in this turn carried a provider
	// ID with a time in it, so the turn's times are a share of the session's span
	// rather than anything measured.
	cursorNoteInterpolatedTimes = "interpolated_times"
)

// cursorReplay walks a session's messages and holds the open turn only.
type cursorReplay struct {
	importer  *cursorImporter
	sess      SessionPreview
	sessionID string
	model     string
	workspace string
	title     string
	window    cursorWindows
	clock     cursorClock
	yield     func(HistoricalGeneration, error) bool

	// previousEnd is where the last emitted turn ended. A turn starts no earlier
	// than that, so a provider clock that runs backwards between two turns, and
	// an interpolated window that overlaps a measured one, still leave the turns
	// in the order the store lists them.
	previousEnd time.Time

	current *cursorTurn
	// pendingContext holds the block Cursor prepends to a prompt: the workspace
	// and git-status text, which arrives as a user message of its own just before
	// the prompt it belongs to. The model saw it as part of that prompt.
	pendingContext string
	// lostMessage says the walk passed a message it could not read since the last
	// turn opened, so the next turn is flagged too.
	lostMessage bool
	// unreadable counts the messages the store could not produce.
	unreadable int
	// unattributed counts the messages the walk read and could not put in a
	// turn: a role it does not know, and model output before any prompt. Both
	// are what a change to Cursor's format looks like from here.
	unattributed int
	emitted      int
}

// cursorTurn is one open turn: the fragment being rebuilt, the text the
// assistant has written into it, and which tool calls are still waiting for
// their output.
type cursorTurn struct {
	frag *fragment.Fragment
	// assistant is the turn's assistant text. It is accumulated here and written
	// to the fragment as one segment, because the live mapper concatenates
	// segments with no separator: they are streaming deltas in the live path, and
	// whole messages here, so appending one per message would run "...the
	// Makefile." and "One test fails..." together.
	assistant string
	// awaiting maps a tool call ID to the calls under it that have no output yet,
	// in the order they were made. A turn spans a whole agentic run, so one ID
	// can be used twice, and a result must reach the call it answers.
	awaiting map[string][]int
	// unkeyed holds the same for calls that carried no ID at all, which are
	// paired with ID-less results in arrival order.
	unkeyed []int
	notes   []string
	// first and last are the earliest and latest times read out of the provider
	// IDs in this turn. Both are zero when none of them carried one.
	first, last time.Time
}

// observe folds one message into the open turn. It returns true when the
// consumer stopped.
func (r *cursorReplay) observe(msg chatstore.Message) bool {
	if msg.Unreadable {
		return r.lose()
	}
	switch msg.Role {
	case chatstore.RoleUser:
		if msg.StringContent {
			// Context Cursor prepended rather than something the user typed. It
			// belongs to the prompt that follows it, wherever in the session it
			// appears.
			r.deferContext(msg.Text())
			return false
		}
		if r.emit() {
			return true
		}
		r.open(msg.Text())
	case chatstore.RoleAssistant:
		r.addAssistant(msg)
	case chatstore.RoleTool:
		r.addToolResults(msg)
	case chatstore.RoleSystem:
		// Cursor's own system prompt, identical for every turn in the session.
		// The live mapper exports no system prompt, so there is nowhere to put
		// it that would not be a second, different mapping.
	default:
		r.unattributed++
	}
	return false
}

// lose records a message the store could not produce and closes the open turn.
//
// What follows an unreadable message cannot be attributed. If the lost blob was
// a prompt, folding the answer to it into the turn before would export that
// answer under a different question, and nothing in the generation would say so.
// The turn therefore ends here, the messages up to the next prompt are dropped,
// and the turns on both sides of the gap carry
// [cursorNoteUnreadableMessage].
func (r *cursorReplay) lose() bool {
	r.unreadable++
	if r.current != nil {
		r.current.note(cursorNoteUnreadableMessage)
	}
	stopped := r.emit()
	r.lostMessage = true
	// Any deferred context belonged to the message that is gone.
	r.pendingContext = ""
	return stopped
}

func (r *cursorReplay) deferContext(text string) {
	r.pendingContext = appendText(r.pendingContext, text)
}

// Cursor wraps the text you type in these before it sends the prompt, and puts
// whatever else it attached to the message outside them: 366 of 386 prompts in
// 127 real stores were wrapped this way, and one carried an attachment block
// before the opening tag.
const (
	cursorPromptOpen  = "<user_query>"
	cursorPromptClose = "</user_query>"
)

// cursorUnwrapPrompt takes Cursor's wrapper off a prompt and keeps everything
// else in it.
//
// The live hook reads the prompt from Cursor's beforeSubmitPrompt payload, which
// is the text the user typed. Reading the store gives what Cursor sent the model
// instead, so without this an imported turn and a live one show different text
// for the same prompt. Only the two markers go: what sits outside them is
// context Cursor added, and it is kept for the same reason the environment
// preamble is.
func cursorUnwrapPrompt(text string) string {
	opensAt := strings.Index(text, cursorPromptOpen)
	closesAt := strings.LastIndex(text, cursorPromptClose)
	if opensAt < 0 || closesAt < opensAt {
		return text
	}
	before, typed := text[:opensAt], text[opensAt+len(cursorPromptOpen):closesAt]
	after := text[closesAt+len(cursorPromptClose):]
	return appendText(appendText(before, typed), after)
}

// open starts a turn. Its times are set when it closes, because they come from
// the provider IDs its messages carry and none of them has been read yet.
func (r *cursorReplay) open(prompt string) {
	prompt = appendText(r.pendingContext, cursorUnwrapPrompt(prompt))
	r.pendingContext = ""
	r.current = &cursorTurn{
		frag: &fragment.Fragment{
			ConversationID: r.sessionID,
			GenerationID:   fallbackTurnID(r.emitted),
			UserPrompt:     prompt,
			Model:          r.model,
		},
		awaiting: map[string][]int{},
	}
	if r.lostMessage {
		r.current.note(cursorNoteUnreadableMessage)
		r.lostMessage = false
	}
}

func (r *cursorReplay) addAssistant(msg chatstore.Message) {
	if r.current == nil {
		r.unattributed++
		return // output before any prompt: not part of a turn
	}
	turn := r.current
	for _, p := range msg.Parts {
		r.date(turn, p)
		switch p.Type {
		case chatstore.PartText:
			turn.assistant = appendText(turn.assistant, p.Text)
		case chatstore.PartReasoning:
			turn.frag.ThinkingPresent = true
		case chatstore.PartToolCall:
			turn.addToolCall(p)
		}
	}
}

// date folds the times in a part's provider IDs into the open turn. A part
// carries at most two of them, a tool call ID and a reasoning ID, and on most
// models neither holds a time.
func (r *cursorReplay) date(turn *cursorTurn, p chatstore.Part) {
	for _, field := range []string{p.ToolCallID, p.ReasoningID()} {
		if ts, ok := r.clock.lookup(field); ok {
			turn.noteTime(ts)
		}
	}
}

func (r *cursorReplay) addToolResults(msg chatstore.Message) {
	if r.current == nil {
		r.unattributed++
		return
	}
	for _, p := range msg.Parts {
		if p.Type != chatstore.PartToolResult {
			continue
		}
		r.date(r.current, p)
		r.current.addToolResult(p)
	}
}

func (t *cursorTurn) addToolCall(p chatstore.Part) {
	name := strings.TrimSpace(p.ToolName)
	if name == "" {
		return // a call with no name is not attributable to a tool
	}
	idx := len(t.frag.Tools)
	callID := strings.TrimSpace(p.ToolCallID)
	useID := callID
	if callID == "" {
		// Cursor's own IDs are opaque, so an ordinal stands in for a missing one
		// and the result is paired by position instead.
		useID = fmt.Sprintf("call-%06d", idx)
		t.unkeyed = append(t.unkeyed, idx)
	} else {
		t.awaiting[callID] = append(t.awaiting[callID], idx)
	}
	t.frag.Tools = append(t.frag.Tools, fragment.ToolRecord{
		ToolName:  name,
		ToolUseID: useID,
		ToolInput: cursorToolPayload(p.Args),
		Status:    "completed",
	})
}

// addToolResult attaches a result to the call it answers: the earliest call with
// that ID that has no output yet, or the earliest ID-less call when the result
// carries no ID either.
//
// A result with no call in this turn is recorded on its own so the output is not
// lost, and the turn is flagged: the pairing is one-sided.
func (t *cursorTurn) addToolResult(p chatstore.Part) {
	callID := strings.TrimSpace(p.ToolCallID)
	if idx, ok := t.takeCall(callID); ok {
		t.frag.Tools[idx].ToolOutput = cursorToolPayload(p.Result)
		return
	}
	name := strings.TrimSpace(p.ToolName)
	if name == "" {
		return // an output with neither a call nor a tool name names nothing
	}
	t.note(cursorNoteOrphanToolResult)
	if callID == "" {
		callID = fmt.Sprintf("result-%06d", len(t.frag.Tools))
	}
	t.frag.Tools = append(t.frag.Tools, fragment.ToolRecord{
		ToolName:   name,
		ToolUseID:  callID,
		ToolOutput: cursorToolPayload(p.Result),
		Status:     "completed",
	})
}

// takeCall claims the call a result answers, so a second result under the same
// ID reaches the second call rather than overwriting the first.
func (t *cursorTurn) takeCall(callID string) (int, bool) {
	if callID == "" {
		if len(t.unkeyed) == 0 {
			return 0, false
		}
		idx := t.unkeyed[0]
		t.unkeyed = t.unkeyed[1:]
		return idx, true
	}
	waiting := t.awaiting[callID]
	if len(waiting) == 0 {
		return 0, false
	}
	idx := waiting[0]
	if len(waiting) == 1 {
		delete(t.awaiting, callID)
	} else {
		t.awaiting[callID] = waiting[1:]
	}
	return idx, true
}

// note records a local observation about the turn, once.
func (t *cursorTurn) note(note string) {
	if !slices.Contains(t.notes, note) {
		t.notes = append(t.notes, note)
	}
}

// noteTime widens the turn to hold one more time read out of a provider ID.
// They arrive in message order, which is not always time order: one turn's tool
// calls can be issued together and answered out of order.
func (t *cursorTurn) noteTime(ts time.Time) {
	if t.first.IsZero() || ts.Before(t.first) {
		t.first = ts
	}
	if ts.After(t.last) {
		t.last = ts
	}
}

// emit closes the open turn and yields it when it holds model output. It
// returns true when the consumer stopped.
//
// A turn with no assistant text and no tool call is dropped. Cursor writes the
// message list as it streams, so the last prompt of an interrupted session can
// sit there with nothing after it, and exporting that would be a generation the
// model never answered.
func (r *cursorReplay) emit() bool {
	turn := r.current
	r.current = nil
	if turn == nil {
		return false
	}
	if turn.assistant != "" {
		turn.frag.Assistant = []fragment.AssistantSegment{{Text: turn.assistant}}
	}
	if len(turn.frag.Assistant) == 0 && len(turn.frag.Tools) == 0 {
		return false
	}
	start, end := r.times(turn)
	turn.frag.StartedAt = start.Format(time.RFC3339Nano)
	turn.frag.LastEventAt = end.Format(time.RFC3339Nano)
	r.previousEnd = end

	src := SourceRef{
		Agent:      AgentCursor,
		SessionID:  r.sessionID,
		SourcePath: r.sess.SourcePath,
		TurnIndex:  r.emitted,
		TurnID:     turn.frag.GenerationID,
	}
	quality := QualityReport{
		// The store records no timestamp field and no token count per turn. A
		// turn's times are read out of provider IDs where they are in them and
		// interpolated where they are not, and usage is left at zero, so both are
		// flagged. Even a measured turn starts at its first provider ID rather
		// than when the prompt was sent, on the provider's clock and to the
		// second.
		ApproxStartedAt:   true,
		ApproxCompletedAt: true,
		ApproxUsage:       true,
		MissingModel:      r.model == "",
		Notes:             append([]string{cursorNoteMissingTurnID}, turn.notes...),
	}

	mapped := mapper.MapFragment(mapper.Inputs{
		Fragment: turn.frag,
		Session: &fragment.Session{
			ConversationID:    r.sessionID,
			ConversationTitle: r.title,
			WorkspaceRoots:    cursorWorkspaceRoots(r.workspace),
			StartedAt:         turn.frag.StartedAt,
		},
		Stop:           &mapper.StopInput{Status: string(mapper.StopStatusCompleted)},
		ContentCapture: agento11y.ContentCaptureModeFull,
		Now:            r.importer.clock(),
	})
	gen := mapped.Generation
	gen.ID = src.GenerationID()
	r.emitted++

	return !r.yield(HistoricalGeneration{Source: src, Gen: gen, Quality: quality}, nil)
}

// times are the turn's start and end.
//
// A turn its provider dated runs from the first ID in it to the last. A turn
// with no dated ID takes a share of the session's span instead, which is a
// guess, and says so in a note.
//
// Neither may start before the previous turn ended: the provider's clock is not
// the one the session was created on, and an interpolated window knows nothing
// of the measured turns around it.
func (r *cursorReplay) times(turn *cursorTurn) (start, end time.Time) {
	if turn.first.IsZero() {
		turn.note(cursorNoteInterpolatedTimes)
		start, end = r.window.at(r.emitted)
	} else {
		start, end = turn.first, turn.last
	}
	if start.Before(r.previousEnd) {
		start = r.previousEnd
	}
	if end.Before(start) {
		end = start
	}
	return start, end
}

// walkReport is what the walk has to say about the session as a whole, or nil
// when it read everything the root record named. The framework has one channel
// for that, an error beside a turn, which it records as a warning naming the
// session.
func (r *cursorReplay) walkReport() error {
	var problems []string
	if r.emitted == 0 && r.sess.TurnCount > 0 && r.unattributed > 0 {
		// The preview counted the prompts in SQLite and the walk decoded the same
		// messages in Go. No turn from a session that holds prompts, with
		// messages the walk could not place, means the two disagree, which is
		// what a change to Cursor's format looks like from here. Without this the
		// run reports importing nothing and gives no reason, so it cannot be told
		// apart from a genuinely empty history.
		//
		// A session that answered nothing and holds nothing unplaceable is not
		// that. Cursor keeps the reply it is still streaming in root field 4 and
		// appends it to the message list later, so a session interrupted before
		// that lists its prompt and no answer. Warning there would fail every
		// later import of the same store, which no rerun can fix.
		problems = append(problems, fmt.Sprintf(
			"no turn was rebuilt from %d prompts, and %d messages could not be placed in one: "+
				"the store's format may have changed", r.sess.TurnCount, r.unattributed))
	}
	if r.unreadable > 0 {
		problems = append(problems, fmt.Sprintf(
			"%d messages could not be read, and what followed each of them was dropped", r.unreadable))
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("cursor session %s: %s", r.sessionID, strings.Join(problems, "; "))
}

func cursorWorkspaceRoots(workspace string) []string {
	if workspace == "" {
		return nil
	}
	return []string{workspace}
}

// cursorToolPayload returns a tool argument or result as JSON. The value came
// out of a decoded message, so it is JSON already; this rejects the empty and
// null cases the mapper would otherwise export as the literal "null".
func cursorToolPayload(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return raw
}

// cursorWindows assigns a turn a start and end time when nothing dated it.
//
// A turn whose provider issues IDs with no time in them has nothing of its own
// to go on, so the session's span is divided evenly over its turns and turn i
// takes the i-th slice. The result is deterministic for a given store, which is
// what a re-import needs, and it keeps the turns in order on a timeline. It is
// not a measurement, which is why the turn carries
// [cursorNoteInterpolatedTimes] and every turn is flagged ApproxStartedAt and
// ApproxCompletedAt.
type cursorWindows struct {
	start time.Time
	step  time.Duration
	turns int
}

// cursorNominalStep spaces the turns of a session with no span to divide: one
// whose provider dates no ID and whose file times were rejected. It is not a
// duration. It keeps two turns from sharing an instant, because a consumer that
// receives two turns at the same instant picks their order itself.
const cursorNominalStep = time.Second

func cursorTurnWindows(start, last time.Time, turns int) cursorWindows {
	w := cursorWindows{start: start.UTC(), turns: max(turns, 1), step: cursorNominalStep}
	if start.IsZero() {
		w.start = last.UTC()
	}
	if span := last.Sub(w.start); span > 0 {
		w.step = span / time.Duration(w.turns)
	}
	return w
}

// at returns the window for turn index. An index past the counted turns keeps
// the last window rather than running past the session's end: the count comes
// from a preview, and a store written to between the preview and the import
// would otherwise place a turn in the future.
func (w cursorWindows) at(index int) (start, end time.Time) {
	start = w.start.Add(w.step * time.Duration(min(index, w.turns-1)))
	return start, start.Add(w.step)
}
