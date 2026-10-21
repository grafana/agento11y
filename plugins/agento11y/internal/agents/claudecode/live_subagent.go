package claudecode

import (
	"log"
	"slices"
	"strings"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/mapper"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/subagent"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/transcript"
	"github.com/grafana/agento11y/plugins/agento11y/internal/redact"
)

// liveSubagentFiles lists sibling subagent transcripts of the session file the
// hook received. A hook that already points at a subagent file returns none,
// so a nested discovery does not recurse into itself.
func liveSubagentFiles(sessionPath string, logger *log.Logger) ([]string, error) {
	if subagent.IsTranscript(sessionPath) {
		return nil, nil
	}
	files, err := subagent.Files(sessionPath)
	if err != nil {
		return nil, err
	}
	if len(files) > 0 && logger != nil {
		logger.Printf("found %d subagent transcripts", len(files))
	}
	return files, nil
}

// liveSubagentGens maps sibling subagent transcripts that finished this turn
// (or every remaining file on SessionEnd) and attaches them to the parent
// session. Already-exported files are skipped so a later Stop does not send
// the same turns again.
func liveSubagentGens(input *hookInput, parentLines []transcript.Line, parentGens []agento11y.Generation, files []string, st *state.Session, opts mapper.Options, r *redact.Redactor, logger *log.Logger) []agento11y.Generation {
	if len(files) == 0 {
		return nil
	}

	exported := map[string]bool{}
	for _, key := range st.ExportedSubagents {
		exported[key] = true
	}

	sessionEnd := strings.TrimSpace(input.HookEventName) == "SessionEnd"
	completed := subagent.CompletedKeys(parentLines, files)

	pending := make([]string, 0, len(files))
	for _, path := range files {
		key := subagent.FileKey(path)
		if exported[key] {
			continue
		}
		if sessionEnd || completed[key] {
			pending = append(pending, path)
		}
	}
	if len(pending) == 0 {
		return nil
	}

	ids := subagent.NewIDs()
	ids.Observe(parentGens)
	mapped := []subagent.Transcript{{
		Path:  input.TranscriptPath,
		Lines: parentLines,
		Gens:  parentGens,
	}}

	for _, path := range pending {
		raw, _, err := transcript.Read(path, 0)
		if err != nil {
			if logger != nil {
				logger.Printf("read subagent transcript %s: %v", path, err)
			}
			continue
		}
		// The Agent tool_result (or SessionEnd) means this file is finished, so
		// CoalesceSession keeps the trailing turn that live Coalesce would hold.
		coalesced := mapper.CoalesceSession(raw)
		subSt := *st
		subOpts := opts
		subOpts.Titles = transcript.Titles{}
		subOpts.SuppressSyntheticSubagentToolCallIDs = subagent.SyntheticToolCallIDs(raw, subagent.RefsForFiles(files))
		subGens, _ := mapper.Process(coalesced, &subSt, subOpts, r)
		ids.Unique(subGens)
		mapped = append(mapped, subagent.Transcript{Path: path, Lines: raw, Gens: subGens})
		st.ExportedSubagents = appendUnique(st.ExportedSubagents, subagent.FileKey(path))
		if logger != nil {
			logger.Printf("mapped %d generations from subagent transcript %s", len(subGens), path)
		}
	}
	if len(mapped) == 1 {
		return nil
	}

	parents := subagent.ParentMap(mapped, subagent.RefsForFiles(files))
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

func appendUnique(keys []string, key string) []string {
	if key == "" || slices.Contains(keys, key) {
		return keys
	}
	return append(keys, key)
}
