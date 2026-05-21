package mapper

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/claudecode/state"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/claudecode/transcript"
)

// TestLiveTranscripts is an opt-in regression harness. It runs the
// production transcript.Read -> Coalesce -> Process pipeline against real
// .claude/projects/*.jsonl files supplied via the CC_LIVE_FILES env var
// (comma-separated paths) and reports per-file stats plus field-level
// validation errors.
//
// Skipped unless CC_LIVE_FILES is set, so it adds zero CI cost. Use it
// when touching Coalesce, Process, or transcript.Read to confirm
// generation counts and token totals are unchanged from baseline.
//
//	CC_LIVE_FILES=$(find ~/.claude/projects -name '*.jsonl' | head -20 | paste -sd, -) \
//	    go test ./plugins/sigil/internal/agents/claudecode/mapper \
//	    -run TestLiveTranscripts -v -count=1
func TestLiveTranscripts(t *testing.T) {
	raw := strings.TrimSpace(os.Getenv("CC_LIVE_FILES"))
	if raw == "" {
		t.Skip("set CC_LIVE_FILES=path1,path2,... to run the live transcript harness")
	}

	var (
		totalRaw, totalCoalesced int
		totalGens                int
		totalInputTokens         int64
		totalOutputTokens        int64
		totalErrors              int
	)

	for _, path := range strings.Split(raw, ",") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		t.Run(path, func(t *testing.T) {
			lines, _, err := transcript.Read(path, 0)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			coalesced, safeOffset := Coalesce(lines)
			st := &state.Session{}
			gens := Process(coalesced, st, Options{SessionID: "live-test"}, nil)

			var inTokens, outTokens int64
			var fieldErrors []string
			for i, gen := range gens {
				inTokens += gen.Usage.InputTokens
				outTokens += gen.Usage.OutputTokens
				if gen.ID == "" {
					fieldErrors = append(fieldErrors, fmt.Sprintf("gen[%d] missing ID", i))
				}
				if gen.ConversationID == "" {
					fieldErrors = append(fieldErrors, fmt.Sprintf("gen[%d] missing ConversationID", i))
				}
				if gen.Model.Provider == "" || gen.Model.Name == "" {
					fieldErrors = append(fieldErrors, fmt.Sprintf("gen[%d] incomplete Model %+v", i, gen.Model))
				}
				if gen.CompletedAt.IsZero() {
					fieldErrors = append(fieldErrors, fmt.Sprintf("gen[%d] zero CompletedAt", i))
				}
				if gen.Mode == "" {
					fieldErrors = append(fieldErrors, fmt.Sprintf("gen[%d] missing Mode", i))
				}
				if gen.AgentName == "" {
					fieldErrors = append(fieldErrors, fmt.Sprintf("gen[%d] missing AgentName", i))
				}
			}

			t.Logf("raw=%d coalesced=%d gens=%d safeOffset=%d input_tokens=%d output_tokens=%d",
				len(lines), len(coalesced), len(gens), safeOffset, inTokens, outTokens)

			for _, msg := range fieldErrors {
				t.Errorf("%s", msg)
			}

			totalRaw += len(lines)
			totalCoalesced += len(coalesced)
			totalGens += len(gens)
			totalInputTokens += inTokens
			totalOutputTokens += outTokens
			totalErrors += len(fieldErrors)
		})
	}

	t.Logf("TOTAL: raw=%d coalesced=%d gens=%d input_tokens=%d output_tokens=%d field_errors=%d",
		totalRaw, totalCoalesced, totalGens, totalInputTokens, totalOutputTokens, totalErrors)
}
