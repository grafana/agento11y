package entry

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"testing"
)

func TestClaudeEvalImportDispatch(t *testing.T) {
	const fixture = "../claudeevals/testdata/claude-2.1.269.json"
	for _, tc := range []struct {
		name string
		args []string
		code int
		help bool
	}{
		{"group", nil, 0, true},
		{"group help", []string{"--help"}, 0, true},
		{"group short help", []string{"-h"}, 0, true},
		{"help", []string{"import", "--help"}, 0, true},
		{"short help", []string{"import", "-h"}, 0, true},
		{"file help", []string{"import", "missing-results.json", "--help"}, 0, true},
		{"file short help", []string{"import", "missing-results.json", "-h"}, 0, true},
		{"flags first help", []string{"import", "--name", "test", "--help", "missing-results.json"}, 0, true},
		{"missing file", []string{"import"}, 2, false},
		{"empty file", []string{"import", ""}, 2, false},
		{"invalid verb", []string{"export"}, 2, false},
		{"invalid verb then help", []string{"export", "--help"}, 2, false},
		{"invalid group flag", []string{"--nonsense"}, 2, false},
		{"invalid flag", []string{"import", "--nonsense"}, 2, false},
		{"invalid flag then help", []string{"import", "--nonsense", "--help"}, 2, false},
		{"dry run", []string{"import", fixture, "--dry-run"}, 0, false},
		{"flags first", []string{"import", "--dry-run", fixture}, 0, false},
		{"help as value", []string{"import", fixture, "--name", "--help", "--dry-run"}, 0, false},
		{"short help as value", []string{"import", "--name", "-h", "--dry-run", fixture}, 0, false},
		{"extra argument", []string{"import", "file.json", "unexpected"}, 2, false},
		{"extra argument then help", []string{"import", "file.json", "unexpected", "--help"}, 2, false},
		{"help after terminator", []string{"import", "file.json", "--", "--help"}, 2, false},
		{"help after flags first file", []string{"import", "--dry-run", fixture, "--help"}, 2, false},
	} {
		for _, direct := range []bool{false, true} {
			name := tc.name
			if direct {
				name += "/direct"
			}
			t.Run(name, func(t *testing.T) {
				isolateDotenvHome(t)
				if tc.help {
					writeHistoryConfig(t, "AGENTO11Y_AUTH_TOKEN=must-not-load\n")
				}
				var stdout, stderr bytes.Buffer
				gotExit := withExit(t, func() {
					if direct {
						runClaudeEvalCommand(tc.args, &stdout, &stderr)
					} else {
						run(append([]string{"claude", "eval"}, tc.args...), strings.NewReader(""), &stdout, &stderr)
					}
				})
				code := 0
				if gotExit != nil {
					code = *gotExit
				}
				if code != tc.code {
					t.Fatalf("exit %d, want %d: %s", code, tc.code, stderr.String())
				}
				if tc.code != 0 {
					if stdout.Len() != 0 || !strings.Contains(stderr.String(), "usage: agento11y claude eval") ||
						!strings.Contains(stderr.String(), "--help` for help.") || strings.Count(stderr.String(), "\n") != 3 {
						t.Fatalf("want compact usage error only: stdout=%q stderr=%q", stdout.String(), stderr.String())
					}
					return
				}
				if stderr.Len() != 0 {
					t.Fatalf("stderr = %q", stderr.String())
				}
				if tc.help {
					if !strings.Contains(stdout.String(), "Usage:") {
						t.Fatalf("stdout = %q, want help", stdout.String())
					}
					if len(tc.args) > 0 && tc.args[0] == "import" && !strings.Contains(stdout.String(), "--trace-root") {
						t.Fatal("import help is missing owning flags")
					}
					if os.Getenv("AGENTO11Y_AUTH_TOKEN") != "" {
						t.Fatal("help loaded configuration")
					}
					return
				}
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
			})
		}
	}
}

func TestClaudeEvalHelpFlagsAreFreshAndSilent(t *testing.T) {
	isolateDotenvHome(t)
	fs, opts := newClaudeEvalFlags()
	if err := fs.Parse([]string{"--name", "test", "--include-content", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	if opts.name != "test" || !opts.content || !opts.dryRun {
		t.Fatalf("owning options = %+v", opts)
	}
	fresh := claudeEvalHelpFlags()
	if fresh == fs || fresh.Output() != io.Discard || fresh.Lookup("name").Value.String() != "" || fresh.Lookup("include-content").Value.String() != "false" {
		t.Fatal("help flags reused state or output")
	}
	fs.VisitAll(func(f *flag.Flag) {
		other := fresh.Lookup(f.Name)
		if other == nil || other.DefValue != f.DefValue || other.Usage != f.Usage {
			t.Errorf("help flag differs from execution: %s", f.Name)
		}
	})
	var out bytes.Buffer
	fresh.SetOutput(&out)
	fresh.Usage()
	if err := fresh.Parse([]string{"--help"}); !errors.Is(err, flag.ErrHelp) || out.Len() != 0 {
		t.Fatalf("help parse: err=%v output=%q", err, out.String())
	}
}
