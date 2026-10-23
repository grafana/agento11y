package claudecode

import (
	"log"
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
	path    string
	raw     []transcript.Line
	readPos int64
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

// liveSubagentGens incrementally maps sibling and nested subagent transcripts
// and attaches new turns to the parent session. Each file resumes from the
// byte offset stored in st so a background agent that keeps writing after its
// first tool_result is not snapshotted once and then ignored.
func liveSubagentGens(input *hookInput, parentLines []transcript.Line, parentGens []agento11y.Generation, files []string, st *state.Session, opts mapper.Options, r *redact.Redactor, logger *log.Logger) []agento11y.Generation {
	if len(files) == 0 {
		return nil
	}

	sessionEnd := strings.TrimSpace(input.HookEventName) == "SessionEnd"
	if st.SubagentOffsets == nil {
		st.SubagentOffsets = map[string]int64{}
	}

	reads, files, blockingDone := readLiveSubagentFiles(files, parentLines, st, logger)
	if len(reads) == 0 {
		return nil
	}

	ids := subagent.NewIDs()
	ids.Observe(parentGens)
	mapped := []subagent.Transcript{{
		Path:  input.TranscriptPath,
		Lines: parentLines,
		Gens:  parentGens,
	}}

	for _, item := range reads {
		key := subagent.FileKey(item.path)
		var (
			coalesced []transcript.Line
			newOffset int64
		)
		if sessionEnd || blockingDone[key] {
			coalesced = mapper.CoalesceSession(item.raw)
			newOffset = item.readPos
		} else {
			var safe int64
			coalesced, safe = mapper.CoalesceGrowing(item.raw)
			if safe == 0 {
				continue
			}
			newOffset = safe
		}
		coalesced = subagent.DropCoveredSidechain(coalesced, files, item.path)
		if len(coalesced) == 0 {
			st.SubagentOffsets[key] = newOffset
			continue
		}

		subSt := *st
		subOpts := opts
		subOpts.Titles = transcript.Titles{}
		subOpts.SuppressSyntheticSubagentToolCallIDs = subagent.SyntheticToolCallIDs(coalesced, subagent.RefsForFiles(files))
		subGens, _ := mapper.Process(coalesced, &subSt, subOpts, r)
		ids.Unique(subGens)
		mapped = append(mapped, subagent.Transcript{Path: item.path, Lines: coalesced, Gens: subGens})
		st.SubagentOffsets[key] = newOffset
		if logger != nil {
			logger.Printf("mapped %d generations from subagent transcript %s", len(subGens), item.path)
		}
	}
	if len(mapped) == 1 {
		return nil
	}

	parents := subagent.ParentMap(mapped, subagent.RefsForFiles(files)).
		WithFallback(st.SubagentParents, st.SubagentNames)
	st.SubagentParents, st.SubagentNames = parents.RememberedParents()

	base := strings.TrimSpace(opts.AgentName)
	if base == "" {
		base = AgentName
	}
	var out []agento11y.Generation
	for _, p := range mapped[1:] {
		subagent.Finalize(p.Path, p.Gens, parents, input.SessionID, base)
		out = append(out, p.Gens...)
	}
	return out
}

// readLiveSubagentFiles reads every file under the session's own subagents/
// tree that has unread bytes. Completion is collected from every file before
// any file is coalesced, so a nested child's last turn is kept when its
// parent's tool_result is in a sibling transcript. Discovery does not follow
// "Transcript dir:" paths: those can name folders from another conversation.
func readLiveSubagentFiles(files []string, parentLines []transcript.Line, st *state.Session, logger *log.Logger) ([]liveSubagentRead, []string, map[string]bool) {
	blockingDone := subagent.BlockingCompletedKeys(parentLines, files)
	queue := append([]string{}, files...)
	slices.SortFunc(queue, func(a, b string) int {
		return strings.Count(a, string(filepath.Separator)) - strings.Count(b, string(filepath.Separator))
	})

	var reads []liveSubagentRead
	seen := map[string]bool{}
	for _, path := range queue {
		if seen[path] {
			continue
		}
		seen[path] = true

		raw, readPos, err := transcript.Read(path, st.SubagentOffsets[subagent.FileKey(path)])
		if err != nil {
			if logger != nil {
				logger.Printf("read subagent transcript %s: %v", path, err)
			}
			continue
		}
		for k, done := range subagent.BlockingCompletedKeys(raw, files) {
			if done {
				blockingDone[k] = true
			}
		}
		if len(raw) == 0 {
			continue
		}
		reads = append(reads, liveSubagentRead{path: path, raw: raw, readPos: readPos})
	}
	return reads, files, blockingDone
}
