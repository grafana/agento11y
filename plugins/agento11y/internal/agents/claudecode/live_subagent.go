package claudecode

import (
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/mapper"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/subagent"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/transcript"
	"github.com/grafana/agento11y/plugins/agento11y/internal/redact"
)

type liveSubagentRead struct {
	path       string
	raw        []transcript.Line
	readPos    int64
	unreadTail bool
}

// liveSubagentFiles lists sibling subagent transcripts of the session file the
// hook received. A hook that already points at a subagent file still walks the
// parent session's subagents/ tree so nested children are found.
func liveSubagentFiles(sessionPath string, logger *log.Logger) ([]string, error) {
	files, err := subagent.Files(subagentSessionPath(sessionPath))
	if err != nil {
		return nil, err
	}
	if len(files) > 0 && logger != nil {
		logger.Printf("found %d subagent transcripts", len(files))
	}
	return files, nil
}

// subagentSessionPath maps a per-subagent transcript back to the session
// JSONL that sits beside the subagents/ directory.
func subagentSessionPath(path string) string {
	if !subagent.IsTranscript(path) {
		return path
	}
	slash := filepath.ToSlash(path)
	sessionDir, _, ok := strings.Cut(slash, "/subagents/")
	if !ok {
		return path
	}
	sessionDir = filepath.FromSlash(sessionDir)
	return filepath.Join(filepath.Dir(sessionDir), filepath.Base(sessionDir)+".jsonl")
}

// All reads finish before mapping or export. On Stop, an unreadable child must not
// cost its parent checkpoint and the only recoverable generation span context.
func readLiveSubagentFiles(files []string, parent string, st *state.Session, logger *log.Logger) ([]liveSubagentRead, error) {
	queue := slices.Clone(files)
	slices.SortFunc(queue, func(a, b string) int {
		return strings.Count(a, string(filepath.Separator)) - strings.Count(b, string(filepath.Separator))
	})
	seen := map[string]bool{toolPath(parent): true}
	var reads []liveSubagentRead
	var readErr error
	for _, path := range queue {
		canonical := toolPath(path)
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		offset := toolReadOffset(st, path, st.SubagentOffsets[subagent.FileKey(path)])
		raw, pos, _, err := readLiveComplete(path, offset)
		var size int64
		if err == nil {
			var info os.FileInfo
			info, err = os.Stat(path)
			if err == nil {
				size = info.Size()
			}
		}
		if err != nil {
			if logger != nil {
				logger.Printf("read subagent transcript %s: %v", path, err)
			}
			readErr = err
			continue
		}
		reads = append(reads, liveSubagentRead{path: path, raw: raw, readPos: pos, unreadTail: size > pos})
	}
	return reads, readErr
}

// liveSubagentGens incrementally maps sibling and nested subagent transcripts
// and attaches new turns to the parent session. Each file resumes from the
// byte offset stored in st so a background agent that keeps writing after its
// first tool_result is not snapshotted once and then ignored.
func liveSubagentGens(input *hookInput, parentRaw, parentLines []transcript.Line, parentGens []agento11y.Generation, files []string, reads []liveSubagentRead, st *state.Session, opts mapper.Options, r *redact.Redactor, logger *log.Logger) ([]agento11y.Generation, [][]*state.PendingTool) {
	if len(reads) == 0 {
		return nil, nil
	}
	if st.SubagentOffsets == nil {
		st.SubagentOffsets = map[string]int64{}
	}
	sessionEnd := strings.TrimSpace(input.HookEventName) == "SessionEnd"
	blockingDone := subagent.CompletedKeys(blockingToolLines(st, input.TranscriptPath, uncoveredToolLines(parentRaw, files, input.TranscriptPath)), files)
	maps.Copy(blockingDone, st.Tools.CompletedChildren)
	for _, item := range reads {
		for key, done := range subagent.CompletedKeys(blockingToolLines(st, item.path, uncoveredToolLines(item.raw, files, item.path)), files) {
			if done {
				blockingDone[key] = true
			}
		}
	}
	st.Tools.CompletedChildren = blockingDone
	ids := subagent.NewIDs()
	ids.Observe(parentGens)
	mapped := []subagent.Transcript{{Path: input.TranscriptPath, Lines: parentLines, Gens: parentGens}}
	sources := []map[int]transcript.Line{nil}
	for _, item := range reads {
		key := subagent.FileKey(item.path)
		raw := afterOffset(item.raw, st.SubagentOffsets[key])
		var lines []transcript.Line
		var next int64
		// Completion cannot finalize a prefix while a later fragment is still
		// unread: that fragment may reuse the prefix's generation ID.
		if (sessionEnd || blockingDone[key]) && !item.unreadTail {
			lines = mapper.CoalesceSession(raw)
			next = item.readPos
			delete(st.Tools.CompletedChildren, key)
		} else {
			lines, next = mapper.CoalesceGrowing(raw)
			if next == 0 {
				continue
			}
		}
		lines = uncoveredToolLines(lines, files, item.path)
		subSt := *st
		subOpts := opts
		subOpts.Titles = transcript.Titles{}
		subOpts.GenerationSources = map[int]transcript.Line{}
		subOpts.SuppressSyntheticSubagentToolCallIDs = subagent.SyntheticToolCallIDs(lines, subagent.RefsForFiles(files))
		gens, _ := mapper.Process(lines, &subSt, subOpts, r)
		ids.Unique(gens)
		mapped = append(mapped, subagent.Transcript{Path: item.path, Lines: lines, Gens: gens})
		sources = append(sources, subOpts.GenerationSources)
		st.SubagentOffsets[key] = next
		if logger != nil {
			logger.Printf("mapped %d generations from subagent transcript %s", len(gens), item.path)
		}
	}
	base := strings.TrimSpace(opts.AgentName)
	if base == "" {
		base = AgentName
	}
	for i, p := range mapped[1:] {
		rememberMappedToolChildren(st, p.Path, sources[i+1], p.Gens, base)
	}
	parents := subagent.ParentMap(nil, subagent.RefsForFiles(files)).WithFallback(st.SubagentParents, st.SubagentNames)
	var out []agento11y.Generation
	var associated [][]*state.PendingTool
	for i, p := range mapped[1:] {
		// Directory-only results for non-agent filenames are remembered by
		// FileKey. Keep that scoped link; ParentMap's agent-ID lookup misses it.
		if len(p.Gens) > 0 && subagent.AgentIDFromPath(p.Path) == "" {
			if parent := st.SubagentParents[subagent.FileKey(p.Path)]; parent != "" && len(p.Gens[0].ParentGenerationIDs) == 0 {
				p.Gens[0].ParentGenerationIDs = []string{parent}
			}
		}
		subagent.Finalize(p.Path, p.Gens, parents, input.SessionID, base)
		associated = append(associated, associateTools(st, p.Path, sources[i+1], p.Gens)...)
		out = append(out, p.Gens...)
	}
	rememberToolChildren(st, base)
	return out, associated
}
