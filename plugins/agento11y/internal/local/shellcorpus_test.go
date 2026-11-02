package local

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/guardeval"
	"github.com/stretchr/testify/require"
)

// shellCorpus holds everyday shell commands, blocked commands behind
// wrappers, and quoting tricks, each with the shell packs that deny it (want)
// and the packs v0.52 denied it with (release). A change to what the packs
// deny shows up in review as a change to this file. Set UPDATE_GOLDENS=1 to
// rewrite want; release stays as v0.52 judged it. A command v0.52 denied that
// the packs now allow fails the test unless its intended note says why, which
// UPDATE_GOLDENS never writes.
const shellCorpus = "testdata/shell-corpus.jsonl"

type shellCorpusCase struct {
	Command  string   `json:"command"`
	Source   string   `json:"source"`
	Release  []string `json:"release,omitempty"`
	Want     []string `json:"want,omitempty"`
	Intended string   `json:"intended,omitempty"`
}

func TestShellPackCorpus(t *testing.T) {
	ids := []string{packGit, packDestructive, packPermissions, packDisk, packFiles}
	engines := make([]*guardeval.Engine, len(ids))
	stockEngines := make([]*guardeval.Engine, len(ids))
	for i, id := range ids {
		rule, err := packRule(id)
		require.NoError(t, err)
		data, err := guardeval.EncodeRules([]guardeval.Rule{rule})
		require.NoError(t, err)
		engines[i] = guardeval.NewEngineFromContents("guards.toml", data, nil)
		stockEngines[i] = NewGuardsEngineFromContents("guards.toml", data, nil)
		require.Empty(t, engines[i].Status().Errors)
		require.Empty(t, stockEngines[i].Status().Errors)
	}
	data, err := os.ReadFile(shellCorpus)
	require.NoError(t, err)
	update := os.Getenv("UPDATE_GOLDENS") == "1"
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	changed := 0
	for line := range bytes.SplitSeq(bytes.TrimSpace(data), []byte("\n")) {
		var c shellCorpusCase
		require.NoError(t, json.Unmarshal(line, &c))
		input, err := json.Marshal(map[string]string{"command": c.Command})
		require.NoError(t, err)
		var got []string
		for i, id := range ids {
			req := packRequest("Bash", string(input))
			legacy := engines[i].Evaluate(req)
			stock := stockEngines[i].Evaluate(req)
			require.Equal(t, legacy.Action, stock.Action, "%s without shell metadata: %q", id, c.Command)
			if legacy.Action == agento11y.HookActionDeny {
				got = append(got, id)
			}
		}
		for _, id := range c.Release {
			if !slices.Contains(got, id) && c.Intended == "" {
				t.Errorf("%q: v0.52's %s pack denied it and it is now allowed; add an intended note", c.Command, id)
			}
		}
		if !slices.Equal(got, c.Want) {
			if changed++; !update && changed <= 20 {
				t.Errorf("%q: denied by %v, want %v", c.Command, got, c.Want)
			}
		}
		c.Want = got
		require.NoError(t, enc.Encode(c))
	}
	if update {
		require.NoError(t, os.WriteFile(shellCorpus, out.Bytes(), 0o644))
		return
	}
	require.Zero(t, changed, "verdicts changed; run with UPDATE_GOLDENS=1 and review the diff")
}
