// Package subagent locates Claude Code's per-subagent transcripts and links
// their turns to the parent session. Live capture and history import share it
// so a subagent run is attributed the same way on both paths.
package subagent

import (
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/transcript"
)

const defaultSuffix = "subagent"

// IsTranscript reports whether path is a Claude Code per-subagent JSONL file
// rather than a session transcript.
func IsTranscript(path string) bool {
	return strings.Contains(filepath.ToSlash(path), "/subagents/")
}

// Files returns the subagent transcripts belonging to one session. Claude Code
// writes them to <dir>/<sessionID>/subagents/ beside <dir>/<sessionID>.jsonl,
// including nested children under that folder. Discovery stays inside that
// tree: a "Transcript dir:" line that names another conversation is ignored.
func Files(sessionPath string) ([]string, error) {
	sessionID := strings.TrimSuffix(filepath.Base(sessionPath), filepath.Ext(sessionPath))
	root := filepath.Join(filepath.Dir(sessionPath), sessionID, "subagents")
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return out, err
	}
	return out, nil
}

// AgentIDFromPath reads the agent ID Claude Code puts in a subagent filename
// (agent-<id>.jsonl).
func AgentIDFromPath(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if id, ok := strings.CutPrefix(base, "agent-"); ok {
		return id
	}
	return ""
}

// Refs indexes the subagent transcripts of one session by their directory and
// by the agent ID in their filename, which are the two ways a parent's Agent
// tool result points at a subagent run.
type Refs struct {
	dirs     map[string]bool
	agentIDs map[string]bool
}

// RefsForFiles builds a [Refs] index from the given subagent transcript paths.
func RefsForFiles(files []string) Refs {
	refs := Refs{dirs: map[string]bool{}, agentIDs: map[string]bool{}}
	for _, file := range files {
		refs.dirs[filepath.Clean(filepath.Dir(file))] = true
		if id := AgentIDFromPath(file); id != "" {
			refs.agentIDs[id] = true
		}
	}
	return refs
}

func (r Refs) hasDir(raw string) bool {
	dir := cleanPath(raw)
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

func (r Refs) hasAgentID(id string) bool {
	return id != "" && r.agentIDs[id]
}

// FileKey identifies a subagent transcript for session-state bookkeeping: the
// agent ID when the filename has one, otherwise the path.
func FileKey(path string) string {
	if id := AgentIDFromPath(path); id != "" {
		return id
	}
	return path
}

// SyntheticToolCallIDs returns the Agent tool call IDs whose subagent
// transcript is mapped separately, so the mapper skips the summary-only
// generation for them.
func SyntheticToolCallIDs(lines []transcript.Line, refs Refs) map[string]bool {
	ids := map[string]bool{}
	eachToolResult(lines, func(block transcript.UserContentBlock, content string) {
		if refs.hasDir(TranscriptDirFromToolResult(content)) {
			ids[block.ToolUseID] = true
		}
		for _, agentID := range AgentIDsFromToolResult(content) {
			if refs.hasAgentID(agentID) {
				ids[block.ToolUseID] = true
			}
		}
	})
	return ids
}

// DropCoveredSidechain removes sidechain lines whose agent ID has its own
// sibling transcript. Those turns are mapped from that file; keeping the copy
// in the parent (or an ancestor) would export the same request twice.
// selfPath is the file these lines came from and is never treated as covering
// itself.
func DropCoveredSidechain(lines []transcript.Line, files []string, selfPath string) []transcript.Line {
	covered := map[string]bool{}
	self := filepath.Clean(selfPath)
	for _, file := range files {
		if self != "" && filepath.Clean(file) == self {
			continue
		}
		if id := AgentIDFromPath(file); id != "" {
			covered[id] = true
		}
	}
	if len(covered) == 0 {
		return lines
	}
	out := make([]transcript.Line, 0, len(lines))
	for _, line := range lines {
		if line.IsSidechain && covered[line.AgentID] {
			continue
		}
		out = append(out, line)
	}
	return out
}

// BlockingCompletedKeys is [CompletedKeys] minus Agent calls that ran in the
// background. A background tool_result arrives when the child is spawned, not
// when its transcript is finished.
func BlockingCompletedKeys(lines []transcript.Line, files []string) map[string]bool {
	background := map[string]bool{}
	for _, line := range lines {
		if line.Type != "assistant" {
			continue
		}
		var msg transcript.AssistantMessage
		if err := json.Unmarshal(line.Message, &msg); err != nil {
			continue
		}
		for _, block := range msg.Content {
			if block.Type == "tool_use" && block.Name == "Agent" && agentRunInBackground(block.Input) {
				background[block.ID] = true
			}
		}
	}
	refs := RefsForFiles(files)
	keys := map[string]bool{}
	eachToolResult(lines, func(block transcript.UserContentBlock, content string) {
		if background[block.ToolUseID] {
			return
		}
		markCompletedKeys(keys, content, refs, files)
	})
	return keys
}

func agentRunInBackground(raw json.RawMessage) bool {
	var parsed struct {
		RunInBackground bool `json:"run_in_background"`
	}
	_ = json.Unmarshal(raw, &parsed)
	return parsed.RunInBackground
}

// CompletedKeys returns the [FileKey] of every subagent file whose Agent tool
// result is present in lines. Those runs are finished: Claude Code writes the
// result only after the subagent transcript is complete.
func CompletedKeys(lines []transcript.Line, files []string) map[string]bool {
	refs := RefsForFiles(files)
	keys := map[string]bool{}
	eachToolResult(lines, func(_ transcript.UserContentBlock, content string) {
		markCompletedKeys(keys, content, refs, files)
	})
	return keys
}

// markCompletedKeys records the subagent files a tool result finishes.
// Agent IDs name one file. A Transcript dir only does when it uniquely
// identifies one file; the shared <session>/subagents folder would otherwise
// mark every running sibling done.
func markCompletedKeys(keys map[string]bool, content string, refs Refs, files []string) {
	for _, agentID := range AgentIDsFromToolResult(content) {
		if refs.hasAgentID(agentID) {
			keys[agentID] = true
		}
	}
	dir := TranscriptDirFromToolResult(content)
	if dir == "" || !refs.hasDir(dir) {
		return
	}
	dir = cleanPath(dir)
	var matches []string
	for _, file := range files {
		fileDir := filepath.Clean(filepath.Dir(file))
		if fileDir == dir || strings.HasPrefix(fileDir, dir+string(filepath.Separator)) {
			matches = append(matches, file)
		}
	}
	if len(matches) == 1 {
		keys[FileKey(matches[0])] = true
	}
}

func eachToolResult(lines []transcript.Line, fn func(transcript.UserContentBlock, string)) {
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

// Transcript is one mapped file of a session: the parent or a subagent.
type Transcript struct {
	Path  string
	Lines []transcript.Line
	Gens  []agento11y.Generation
}

// Parents maps a subagent run to the generation that spawned it, keyed both
// by transcript directory and by agent ID.
type Parents struct {
	byDir              map[string]string
	byAgentID          map[string]string
	agentNameByAgentID map[string]string
}

// ParentMap builds [Parents] from every mapped transcript of the session.
func ParentMap(processed []Transcript, refs Refs) Parents {
	parents := Parents{
		byDir:              map[string]string{},
		byAgentID:          map[string]string{},
		agentNameByAgentID: map[string]string{},
	}
	for _, p := range processed {
		parentByToolUseID, agentNameByToolUseID := toolCallParents(p.Gens)
		eachToolResult(p.Lines, func(block transcript.UserContentBlock, content string) {
			parentID := parentByToolUseID[block.ToolUseID]
			if parentID == "" {
				return
			}
			if dir := TranscriptDirFromToolResult(content); refs.hasDir(dir) {
				parents.byDir[cleanPath(dir)] = parentID
			}
			for _, agentID := range AgentIDsFromToolResult(content) {
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

// WithFallback fills missing parent links and names from an earlier hook
// invocation's remembered maps.
func (p Parents) WithFallback(parentByID, nameByID map[string]string) Parents {
	if p.byDir == nil {
		p.byDir = map[string]string{}
	}
	if p.byAgentID == nil {
		p.byAgentID = map[string]string{}
	}
	if p.agentNameByAgentID == nil {
		p.agentNameByAgentID = map[string]string{}
	}
	for id, parentID := range parentByID {
		if id == "" || parentID == "" || p.byAgentID[id] != "" {
			continue
		}
		p.byAgentID[id] = parentID
	}
	for id, name := range nameByID {
		if id == "" || name == "" || p.agentNameByAgentID[id] != "" {
			continue
		}
		p.agentNameByAgentID[id] = name
	}
	return p
}

// RememberedParents returns the agent-ID maps Finalize needs on a later hook.
func (p Parents) RememberedParents() (parentByID, nameByID map[string]string) {
	if len(p.byAgentID) == 0 && len(p.agentNameByAgentID) == 0 {
		return nil, nil
	}
	parentByID = maps.Clone(p.byAgentID)
	nameByID = maps.Clone(p.agentNameByAgentID)
	return parentByID, nameByID
}

func toolCallParents(gens []agento11y.Generation) (map[string]string, map[string]string) {
	parentByToolUseID := map[string]string{}
	agentNameByToolUseID := map[string]string{}
	for _, gen := range gens {
		for _, msg := range gen.Output {
			for _, part := range msg.Parts {
				if part.ToolCall == nil || part.ToolCall.ID == "" {
					continue
				}
				parentByToolUseID[part.ToolCall.ID] = gen.ID
				if agentName := agentNameFromToolCall(part.ToolCall, gen.AgentName); agentName != "" {
					agentNameByToolUseID[part.ToolCall.ID] = agentName
				}
			}
		}
	}
	return parentByToolUseID, agentNameByToolUseID
}

func agentNameFromToolCall(call *agento11y.ToolCall, parentAgent string) string {
	if call.Name != "Agent" {
		return ""
	}
	var parsed struct {
		SubagentType string `json:"subagent_type"`
	}
	_ = json.Unmarshal(call.InputJSON, &parsed)
	suffix := strings.ToLower(strings.TrimSpace(parsed.SubagentType))
	if suffix == "" {
		suffix = defaultSuffix
	}
	base := strings.TrimSpace(parentAgent)
	if i := strings.IndexByte(base, '/'); i >= 0 {
		base = base[:i]
	}
	if base == "" {
		base = "claude-code"
	}
	return base + "/" + suffix
}

// TranscriptDirFromToolResult reads the "Transcript dir:" line Claude Code
// writes into an Agent tool result.
func TranscriptDirFromToolResult(content string) string {
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if dir, ok := strings.CutPrefix(line, "Transcript dir:"); ok {
			return cleanPath(dir)
		}
	}
	return ""
}

// AgentIDsFromToolResult reads the agent IDs an Agent tool result mentions.
// Claude Code has used three spellings for the same field.
func AgentIDsFromToolResult(content string) []string {
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

func cleanPath(raw string) string {
	raw = strings.Trim(strings.TrimSpace(raw), "`'\"")
	if raw == "" {
		return ""
	}
	return filepath.Clean(raw)
}

func parentFor(path string, parents Parents) string {
	if agentID := AgentIDFromPath(path); agentID != "" {
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

// Finalize attaches a subagent transcript's turns to the session and chains
// them: the first turn hangs off the spawning generation and each later turn
// off the one before. baseAgent is the product or override name a main-thread
// turn uses; turns still carrying that name are renamed to the subagent.
func Finalize(path string, gens []agento11y.Generation, parents Parents, sessionID, baseAgent string) {
	baseAgent = strings.TrimSpace(baseAgent)
	if baseAgent == "" {
		baseAgent = "claude-code"
	}
	agentName := baseAgent + "/" + defaultSuffix
	if agentID := AgentIDFromPath(path); agentID != "" {
		if name := parents.agentNameByAgentID[agentID]; name != "" {
			agentName = name
		}
	}
	parentID := parentFor(path, parents)
	for i := range gens {
		if gens[i].AgentName == "" || gens[i].AgentName == baseAgent {
			gens[i].AgentName = agentName
		}
		gens[i].ConversationID = sessionID
		if parentID != "" && len(gens[i].ParentGenerationIDs) == 0 {
			gens[i].ParentGenerationIDs = []string{parentID}
		}
		parentID = gens[i].ID
	}
}

// IDs makes the mapper's generation IDs unique across every transcript of one
// session. The mapper derives that ID from the request ID, and Claude Code
// reuses a request ID between the session transcript and a subagent transcript.
type IDs struct {
	seen    map[string]int
	current map[string]string
}

// NewIDs returns an empty [IDs] book for one session.
func NewIDs() *IDs {
	return &IDs{seen: map[string]int{}, current: map[string]string{}}
}

// Observe records gens without renaming them. Live capture uses it for the
// parent transcript, whose IDs have already been exported or are about to be.
func (u *IDs) Observe(gens []agento11y.Generation) {
	if u == nil {
		return
	}
	for _, gen := range gens {
		id := gen.ID
		if id == "" {
			continue
		}
		if u.seen[id] == 0 {
			u.seen[id] = 1
		}
		u.current[id] = id
	}
}

// Unique renames the duplicate IDs in one transcript's turns, continuing from
// the transcripts already seen. A parent link always names an earlier turn, so
// the first turn under an ID keeps it and every repeat takes a suffixed one.
func (u *IDs) Unique(gens []agento11y.Generation) {
	if u == nil {
		return
	}
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
			gens[i].ID = id + "#" + strconv.Itoa(n)
		}
		u.seen[id]++
		u.current[id] = gens[i].ID
	}
}
