package hook

import (
	"bytes"
	"log"
	"os"
	"testing"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
)

func TestAfterAgentThought(t *testing.T) {
	cases := []struct {
		name    string
		seed    func(t *testing.T, logger *log.Logger)
		payload Payload
		verify  func(t *testing.T, logBuf *bytes.Buffer)
	}{
		{
			name: "first call sets flag",
			payload: Payload{
				HookEventName:  "afterAgentThought",
				ConversationID: "conv",
				GenerationID:   "gen",
				Timestamp:      "2026-04-28T12:00:00Z",
			},
			verify: func(t *testing.T, _ *bytes.Buffer) {
				got := fragment.LoadTolerant("conv", "gen", log.New(&bytes.Buffer{}, "", 0))
				if got == nil || !got.ThinkingPresent {
					t.Fatalf("ThinkingPresent should be set; got %+v", got)
				}
			},
		},
		{
			name: "already true skips rewrite",
			seed: func(t *testing.T, logger *log.Logger) {
				if err := fragment.Update("conv", "gen", logger, func(f *fragment.Fragment) bool {
					f.ThinkingPresent = true
					return true
				}); err != nil {
					t.Fatalf("seed: %v", err)
				}
				path := fragment.FragmentFilePath("conv", "gen")
				old := time.Now().Add(-time.Hour)
				if err := os.Chtimes(path, old, old); err != nil {
					t.Fatalf("chtimes: %v", err)
				}
			},
			payload: Payload{
				HookEventName:  "afterAgentThought",
				ConversationID: "conv",
				GenerationID:   "gen",
				Timestamp:      "2026-04-28T12:00:01Z",
			},
			verify: func(t *testing.T, _ *bytes.Buffer) {
				path := fragment.FragmentFilePath("conv", "gen")
				stat, err := os.Stat(path)
				if err != nil {
					t.Fatalf("stat after: %v", err)
				}
				if time.Since(stat.ModTime()) < 30*time.Minute {
					t.Errorf("file was rewritten: mtime=%v (expected ~1h old)", stat.ModTime())
				}
			},
		},
		{
			name:    "missing ids logs",
			payload: Payload{HookEventName: "afterAgentThought"},
			verify: func(t *testing.T, logBuf *bytes.Buffer) {
				if !bytes.Contains(logBuf.Bytes(), []byte("missing")) {
					t.Errorf("expected 'missing' log; got %q", logBuf.String())
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			var logBuf bytes.Buffer
			logger := log.New(&logBuf, "", 0)

			if tc.seed != nil {
				tc.seed(t, logger)
			}
			AfterAgentThought(tc.payload, logger)
			tc.verify(t, &logBuf)
		})
	}
}
