package history

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/mapper"
)

// Cursor agent transcripts live under
// ~/.cursor/projects/<project-slug>/agent-transcripts/<uuid>/<uuid>.jsonl.
// Cursor publishes no schema for them. The shapes below were recovered from
// current builds: a user/assistant role line, optional turn_ended, and no
// tool results, model, or usage.

const (
	cursorAgentTranscriptsDir = "agent-transcripts"
	cursorSubagentsDir        = "subagents"
	cursorTranscriptTSOpen    = "<timestamp>"
	cursorTranscriptTSClose   = "</timestamp>"
)

// cursorUserRoleMarker is what a preview counts to estimate turns. Cursor
// writes compact JSON with no spaces, so a byte scan finds user lines without
// decoding them.
var cursorUserRoleMarker = []byte(`"role":"user"`)

// isCursorParentTranscript reports whether path is a parent session JSONL
// under agent-transcripts. Subagent files live under .../subagents/ and are
// not sessions of their own. The parent layout is always
// <uuid>/<uuid>.jsonl: the directory name equals the file stem.
func isCursorParentTranscript(path string) bool {
	if !strings.HasSuffix(path, ".jsonl") {
		return false
	}
	slash := filepath.ToSlash(path)
	if !strings.Contains(slash, "/"+cursorAgentTranscriptsDir+"/") {
		return false
	}
	if strings.Contains(slash, "/"+cursorSubagentsDir+"/") {
		return false
	}
	dir := filepath.Base(filepath.Dir(path))
	stem := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	return dir != "" && dir == stem
}

// previewTranscript builds a metadata-only preview of a Cursor agent
// transcript. It reads a bounded head and tail window and never returns
// prompt or response text.
func (c *cursorImporter) previewTranscript(ctx context.Context, path string) (SessionPreview, bool, error) {
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

	sessionID := cursorSessionIDFromPath(path)
	turns, approx := win.EstimateTotal(countCursorTranscriptTurns(head))
	if turns == 0 {
		// No user lines means nothing to import.
		return SessionPreview{}, false, nil
	}

	started, last := cursorTranscriptPreviewTimes(win)
	if started.IsZero() {
		started = win.ModTime
	}
	if last.IsZero() {
		last = win.ModTime
	}
	if started.IsZero() {
		started = last
	}

	p := SessionPreview{
		Agent:          AgentCursor,
		SessionID:      sessionID,
		Title:          sessionID,
		Workspace:      cursorWorkspaceFromTranscriptPath(path),
		SourcePath:     path,
		TurnCount:      turns,
		ApproxTurns:    approx || turns > 0,
		SizeBytes:      win.Size,
		StartedAt:      started,
		LastActivityAt: last,
	}
	return p, true, nil
}

// countCursorTranscriptTurns counts user-role lines in a preview window.
// Each user line opens a turn the import may export.
func countCursorTranscriptTurns(lines [][]byte) int {
	n := 0
	for _, raw := range lines {
		if bytes.Contains(raw, cursorUserRoleMarker) {
			n++
		}
	}
	return n
}

// cursorTranscriptPreviewTimes reads <timestamp> wrappers from the head and
// tail windows. Only the timestamp string is decoded; the surrounding prompt
// text never leaves this function as a return value.
func cursorTranscriptPreviewTimes(win PreviewWindows) (started, last time.Time) {
	head := win.HeadLines()
	for _, raw := range head[:min(len(head), previewMetadataLines)] {
		if ts := cursorTimestampFromLine(raw); !ts.IsZero() {
			started = ts
			break
		}
	}
	tail := win.TailLines()
	if win.Whole {
		tail = head
	}
	for i := len(tail) - 1; i >= 0 && i >= len(tail)-previewMetadataLines; i-- {
		if ts := cursorTimestampFromLine(tail[i]); !ts.IsZero() {
			last = ts
			break
		}
	}
	if last.IsZero() {
		last = started
	}
	if !started.IsZero() && !last.IsZero() && last.Before(started) {
		started, last = last, started
	}
	return started, last
}

func cursorTimestampFromLine(raw []byte) time.Time {
	if !bytes.Contains(raw, []byte(cursorTranscriptTSOpen)) {
		return time.Time{}
	}
	var line cursorTranscriptLine
	if err := json.Unmarshal(raw, &line); err != nil {
		return time.Time{}
	}
	if line.Role != "user" {
		return time.Time{}
	}
	return parseCursorTranscriptTimestamp(line.Text())
}

// cursorWorkspaceFromTranscriptPath recovers a workspace path from the project
// slug Cursor encodes as the directory under ~/.cursor/projects. The slug is
// the absolute workspace path with every '/' replaced by '-'. When a folder
// name itself contains a hyphen the mapping is lossy; this returns empty
// rather than inventing a path.
func cursorWorkspaceFromTranscriptPath(path string) string {
	slash := filepath.ToSlash(path)
	const marker = "/.cursor/projects/"
	i := strings.Index(slash, marker)
	if i < 0 {
		return ""
	}
	rest := slash[i+len(marker):]
	slug, _, ok := strings.Cut(rest, "/")
	if !ok || slug == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	homeSlug := strings.ReplaceAll(filepath.ToSlash(home), "/", "-")
	homeSlug = strings.TrimPrefix(homeSlug, "-")
	if slug == homeSlug {
		return home
	}
	if !strings.HasPrefix(slug, homeSlug+"-") {
		return ""
	}
	remainder := slug[len(homeSlug)+1:]
	if remainder == "" {
		return home
	}
	return cursorMatchSlugPath(home, remainder)
}

// cursorMatchSlugPath greedily matches hyphen-joined path components under
// root against real directories on disk. At each step it tries the longest
// remaining prefix that exists as a child, so a folder named "foo-bar" wins
// over "foo" then a missing "bar".
func cursorMatchSlugPath(root, slug string) string {
	cur := root
	rest := slug
	for rest != "" {
		entries, err := os.ReadDir(cur)
		if err != nil {
			return ""
		}
		names := make(map[string]bool, len(entries))
		for _, e := range entries {
			if e.IsDir() {
				names[e.Name()] = true
			}
		}
		matched := ""
		// Prefer the longest hyphen-joined prefix that exists.
		parts := strings.Split(rest, "-")
		for n := len(parts); n >= 1; n-- {
			cand := strings.Join(parts[:n], "-")
			if names[cand] {
				matched = cand
				rest = strings.Join(parts[n:], "-")
				break
			}
		}
		if matched == "" {
			return ""
		}
		cur = filepath.Join(cur, matched)
	}
	return cur
}

// parseCursorTranscriptTimestamp parses Cursor's human-readable prompt
// wrapper, e.g. "Wednesday, May 13, 2026, 2:30 PM (UTC+2)" or with an
// abbreviated month such as "Tuesday, Jul 21, 2026, 1:04 PM (UTC+2)".
func parseCursorTranscriptTimestamp(text string) time.Time {
	opensAt := strings.Index(text, cursorTranscriptTSOpen)
	closesAt := strings.Index(text, cursorTranscriptTSClose)
	if opensAt < 0 || closesAt < opensAt {
		return time.Time{}
	}
	raw := strings.TrimSpace(text[opensAt+len(cursorTranscriptTSOpen) : closesAt])
	if raw == "" {
		return time.Time{}
	}
	body, zone, hasZone := strings.Cut(raw, " (")
	if hasZone && strings.HasSuffix(zone, ")") {
		zone = strings.TrimSuffix(zone, ")")
	} else {
		body = raw
		zone = ""
	}
	t, ok := parseCursorTranscriptBody(body)
	if !ok {
		return time.Time{}
	}
	if zone == "" || zone == "UTC" {
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
	}
	offset, ok := parseCursorUTCOffset(zone)
	if !ok {
		return time.Time{}
	}
	loc := time.FixedZone(zone, offset)
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, loc)
}

// parseCursorTranscriptBody accepts both full and abbreviated English month
// names. Current Cursor builds write "Jul"; older or hand-edited wrappers may
// still spell "July".
func parseCursorTranscriptBody(body string) (time.Time, bool) {
	for _, layout := range []string{
		"Monday, January 2, 2006, 3:04 PM",
		"Monday, Jan 2, 2006, 3:04 PM",
	} {
		t, err := time.Parse(layout, body)
		if err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// parseCursorUTCOffset parses zone labels Cursor puts in timestamps: UTC,
// UTC+2, UTC-5, UTC+05:30.
func parseCursorUTCOffset(zone string) (int, bool) {
	if !strings.HasPrefix(zone, "UTC") {
		return 0, false
	}
	rest := zone[len("UTC"):]
	if rest == "" {
		return 0, true
	}
	sign := 1
	switch rest[0] {
	case '+':
		rest = rest[1:]
	case '-':
		sign = -1
		rest = rest[1:]
	default:
		return 0, false
	}
	hours := 0
	mins := 0
	var err error
	if h, m, ok := strings.Cut(rest, ":"); ok {
		hours, err = strconv.Atoi(h)
		if err != nil {
			return 0, false
		}
		mins, err = strconv.Atoi(m)
		if err != nil {
			return 0, false
		}
	} else {
		hours, err = strconv.Atoi(rest)
		if err != nil {
			return 0, false
		}
	}
	return sign * (hours*3600 + mins*60), true
}

// cursorTranscriptLine is one JSONL record. Only the fields the importer
// needs are declared; unknown fields are ignored.
type cursorTranscriptLine struct {
	Type    string          `json:"type"`
	Status  string          `json:"status"`
	Error   json.RawMessage `json:"error"`
	Role    string          `json:"role"`
	Message *struct {
		Content []cursorTranscriptPart `json:"content"`
	} `json:"message"`
}

type cursorTranscriptPart struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

func (l cursorTranscriptLine) Text() string {
	if l.Message == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range l.Message.Content {
		if p.Type == "text" || p.Type == "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// turnsTranscript yields one generation per user prompt in a Cursor agent
// transcript. One turn is held at a time so a large file costs one turn's
// memory.
func (c *cursorImporter) turnsTranscript(ctx context.Context, sess SessionPreview) iter.Seq2[HistoricalGeneration, error] {
	return func(yield func(HistoricalGeneration, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(HistoricalGeneration{}, err)
			return
		}
		f, err := os.Open(sess.SourcePath)
		if err != nil {
			yield(HistoricalGeneration{}, err)
			return
		}
		defer func() { _ = f.Close() }()

		sessionID := firstNonEmptyString(sess.SessionID, cursorSessionIDFromPath(sess.SourcePath))
		r := &cursorTranscriptReplay{
			importer:  c,
			sess:      sess,
			sessionID: sessionID,
			workspace: sess.Workspace,
			window:    cursorTurnWindows(sess.StartedAt, sess.LastActivityAt, sess.TurnCount),
			yield:     yield,
		}
		r.previousEnd = r.window.start

		sc := bufio.NewScanner(f)
		// Agent transcripts can hold large tool inputs on one line.
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			if err := ctx.Err(); err != nil {
				yield(HistoricalGeneration{}, err)
				return
			}
			raw := bytes.TrimSpace(sc.Bytes())
			if len(raw) == 0 {
				continue
			}
			var line cursorTranscriptLine
			if err := json.Unmarshal(raw, &line); err != nil {
				r.unreadable++
				continue
			}
			if r.observe(line) {
				return
			}
		}
		if err := sc.Err(); err != nil && err != io.EOF {
			yield(HistoricalGeneration{}, fmt.Errorf("read cursor transcript %s: %w", sess.SourcePath, err))
			return
		}
		if r.emit() {
			return
		}
		if err := r.walkReport(); err != nil {
			yield(HistoricalGeneration{}, err)
		}
	}
}

// cursorTranscriptReplay walks a JSONL transcript and holds the open turn only.
type cursorTranscriptReplay struct {
	importer  *cursorImporter
	sess      SessionPreview
	sessionID string
	workspace string
	window    cursorWindows
	yield     func(HistoricalGeneration, error) bool

	previousEnd time.Time
	current     *cursorTranscriptTurn
	unreadable  int
	emitted     int
}

type cursorTranscriptTurn struct {
	frag      *fragment.Fragment
	assistant string
	stop      *mapper.StopInput
	notes     []string
	// dated is set when the user prompt carried a parseable <timestamp>.
	dated time.Time
}

func (t *cursorTranscriptTurn) note(n string) {
	for _, existing := range t.notes {
		if existing == n {
			return
		}
	}
	t.notes = append(t.notes, n)
}

func (r *cursorTranscriptReplay) observe(line cursorTranscriptLine) bool {
	switch {
	case line.Type == "turn_ended":
		if r.current == nil {
			return false
		}
		status := strings.ToLower(strings.TrimSpace(line.Status))
		switch status {
		case "error", "failed":
			r.current.stop = &mapper.StopInput{Status: string(mapper.StopStatusError), Error: line.Error}
		case "aborted", "cancelled", "canceled":
			r.current.stop = &mapper.StopInput{Status: string(mapper.StopStatusAborted)}
		default:
			r.current.stop = &mapper.StopInput{Status: string(mapper.StopStatusCompleted)}
		}
		return false
	case line.Role == "user":
		if r.emit() {
			return true
		}
		r.open(line)
	case line.Role == "assistant":
		r.addAssistant(line)
	}
	return false
}

func (r *cursorTranscriptReplay) open(line cursorTranscriptLine) {
	text := line.Text()
	dated := parseCursorTranscriptTimestamp(text)
	// The timestamp wrapper is Cursor metadata, not what the user typed. Live
	// capture reads the beforeSubmitPrompt payload, which has neither tag.
	prompt := cursorUnwrapPrompt(cursorStripTranscriptTimestamp(text))
	r.current = &cursorTranscriptTurn{
		frag: &fragment.Fragment{
			ConversationID: r.sessionID,
			GenerationID:   fallbackTurnID(r.emitted),
			UserPrompt:     prompt,
		},
		dated: dated,
	}
}

// cursorStripTranscriptTimestamp removes Cursor's <timestamp> wrapper so an
// imported prompt matches the text live capture records.
func cursorStripTranscriptTimestamp(text string) string {
	opensAt := strings.Index(text, cursorTranscriptTSOpen)
	closesAt := strings.Index(text, cursorTranscriptTSClose)
	if opensAt < 0 || closesAt < opensAt {
		return text
	}
	before := text[:opensAt]
	after := text[closesAt+len(cursorTranscriptTSClose):]
	return strings.TrimSpace(before + after)
}

func (r *cursorTranscriptReplay) addAssistant(line cursorTranscriptLine) {
	if r.current == nil {
		return
	}
	if line.Message == nil {
		return
	}
	turn := r.current
	for _, p := range line.Message.Content {
		switch p.Type {
		case "text", "":
			turn.assistant = appendText(turn.assistant, p.Text)
		case "tool_use":
			name := strings.TrimSpace(p.Name)
			if name == "" {
				continue
			}
			idx := len(turn.frag.Tools)
			turn.frag.Tools = append(turn.frag.Tools, fragment.ToolRecord{
				ToolName:  name,
				ToolUseID: fmt.Sprintf("call-%06d", idx),
				ToolInput: cursorToolPayload(p.Input),
				Status:    "completed",
			})
			turn.note(cursorNoteMissingToolResults)
		}
	}
}

func (r *cursorTranscriptReplay) emit() bool {
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
	approxTimes := turn.dated.IsZero()
	quality := QualityReport{
		ApproxStartedAt:   approxTimes,
		ApproxCompletedAt: approxTimes,
		ApproxUsage:       true,
		MissingModel:      true,
		Notes:             append([]string{cursorNoteMissingTurnID}, turn.notes...),
	}
	stop := turn.stop
	if stop == nil {
		stop = &mapper.StopInput{Status: string(mapper.StopStatusCompleted)}
	}
	mapped := mapper.MapFragment(mapper.Inputs{
		Fragment: turn.frag,
		Session: &fragment.Session{
			ConversationID: r.sessionID,
			WorkspaceRoots: cursorWorkspaceRoots(r.workspace),
			StartedAt:      turn.frag.StartedAt,
		},
		Stop:           stop,
		ContentCapture: agento11y.ContentCaptureModeFull,
		Now:            r.importer.clock(),
	})
	gen := mapped.Generation
	gen.ID = src.GenerationID()
	r.emitted++
	return !r.yield(HistoricalGeneration{Source: src, Gen: gen, Quality: quality}, nil)
}

func (r *cursorTranscriptReplay) times(turn *cursorTranscriptTurn) (start, end time.Time) {
	if turn.dated.IsZero() {
		turn.note(cursorNoteInterpolatedTimes)
		start, end = r.window.at(r.emitted)
	} else {
		start = turn.dated.UTC()
		// A transcript turn has no end time of its own. Give it a nominal
		// duration so it does not share an instant with the next turn.
		end = start.Add(cursorNominalStep)
	}
	if start.Before(r.previousEnd) {
		start = r.previousEnd
	}
	if end.Before(start) {
		end = start
	}
	return start, end
}

func (r *cursorTranscriptReplay) walkReport() error {
	if r.unreadable == 0 {
		return nil
	}
	return fmt.Errorf("cursor session %s: %d transcript lines could not be read", r.sessionID, r.unreadable)
}
