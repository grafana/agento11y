package entry

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestClaudeEvalImportDispatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"help", []string{"import", "--help"}, 0},
		{"missing file", []string{"import"}, 2},
		{"empty file", []string{"import", ""}, 2},
		{"invalid verb", []string{"export"}, 2},
		{"invalid flag", []string{"import", "--nonsense"}, 2},
		{"dry run", []string{"import", "../claudeevals/testdata/claude-2.1.269.json", "--dry-run"}, 0},
		{"flags first", []string{"import", "--dry-run", "../claudeevals/testdata/claude-2.1.269.json"}, 0},
		{"extra argument", []string{"import", "file.json", "unexpected"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			gotExit := withExit(t, func() { run(append([]string{"claude", "eval"}, tc.args...), strings.NewReader(""), &stdout, &stderr) })
			code := 0
			if gotExit != nil {
				code = *gotExit
			}
			if code != tc.code {
				t.Fatalf("exit %d, want %d: %s", code, tc.code, stderr.String())
			}
			if tc.name == "dry run" || tc.name == "flags first" {
				var plan map[string]any
				if err := json.Unmarshal(stdout.Bytes(), &plan); err != nil {
					t.Fatal(err)
				}
				if len(plan["runs"].([]any)) != 2 {
					t.Fatal("missing baseline")
				}
				if strings.Contains(stdout.String(), "getUser") {
					t.Fatal("content included without consent")
				}
			}
		})
	}
}
