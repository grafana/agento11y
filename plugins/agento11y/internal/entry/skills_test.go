package entry

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/skills"
)

// TestRun_SkillsList pins the listing surface: names and one-line descriptions
// on stdout, nothing on stderr, no exit call.
func TestRun_SkillsList(t *testing.T) {
	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run([]string{"skills", "list"}, strings.NewReader(""), &stdout, &stderr)
	})
	if gotExit != nil {
		t.Fatalf("exit = %v, want no exit", *gotExit)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr non-empty: %q", stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, skills.SetupCodingAgentSkill) {
		t.Errorf("listing does not name %q:\n%s", skills.SetupCodingAgentSkill, out)
	}
	// One line per skill, carrying both the name and a description.
	for _, skill := range skills.All() {
		var found bool
		for line := range strings.SplitSeq(out, "\n") {
			if strings.HasPrefix(line, skill.Name+" ") || line == skill.Name {
				found = true
				if !strings.Contains(line, firstWords(skill.Description)) {
					t.Errorf("line for %q carries no description: %q", skill.Name, line)
				}
			}
		}
		if !found {
			t.Errorf("no line for skill %q:\n%s", skill.Name, out)
		}
	}
	// The listed order is the sorted order, which is what makes the output
	// stable across builds rather than dependent on the embed walk.
	var listed []string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		name, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		listed = append(listed, name)
	}
	if want := skills.Names(); !slices.Equal(listed, want) {
		t.Errorf("listed %v, want %v:\n%s", listed, want, out)
	}
}

// firstWords returns enough of a description to identify it on a line without
// pinning the whole sentence.
func firstWords(desc string) string {
	fields := strings.Fields(desc)
	if len(fields) > 4 {
		fields = fields[:4]
	}
	return strings.Join(fields, " ")
}

// TestRun_SkillsShowPrintsRawFile pins that `show` writes the file itself,
// frontmatter included, so a coding agent reading stdout gets the skill as it
// ships. `get` is the gcx spelling and must print the same bytes.
func TestRun_SkillsShowPrintsRawFile(t *testing.T) {
	skill, err := skills.Get(skills.SetupCodingAgentSkill)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	outputs := map[string]string{}
	for _, verb := range []string{skills.ShowVerb, skills.GetVerb} {
		t.Run(verb, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			gotExit := withExit(t, func() {
				run([]string{"skills", verb, skills.SetupCodingAgentSkill}, strings.NewReader(""), &stdout, &stderr)
			})
			if gotExit != nil {
				t.Fatalf("exit = %v, want no exit", *gotExit)
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr non-empty: %q", stderr.String())
			}
			if stdout.String() != skill.Body {
				t.Errorf("%s printed something other than the raw SKILL.md", verb)
			}
			outputs[verb] = stdout.String()
		})
	}
	if outputs[skills.ShowVerb] != outputs[skills.GetVerb] {
		t.Error("`skills get` output differs from `skills show`")
	}
}

// TestPrintedSetupCommandRuns closes the loop the constant only half pins:
// internal/skills owns the string doctor, login, and help print, but the words
// in it have to be the words this package's dispatch accepts. Running the
// constant is the only check that a renamed verb does not leave three surfaces
// printing a command that exits 2.
func TestPrintedSetupCommandRuns(t *testing.T) {
	fields := strings.Fields(skills.SetupCodingAgentCommand)
	if len(fields) < 2 || fields[0] != "agento11y" {
		t.Fatalf("SetupCodingAgentCommand = %q, want it to start with the binary name", skills.SetupCodingAgentCommand)
	}
	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run(fields[1:], strings.NewReader(""), &stdout, &stderr)
	})
	if gotExit != nil {
		t.Fatalf("running the printed command %q exited %d: %s", skills.SetupCodingAgentCommand, *gotExit, stderr.String())
	}
	if !strings.Contains(stdout.String(), "name: "+skills.SetupCodingAgentSkill) {
		t.Errorf("the printed command did not print the skill:\n%s", stdout.String())
	}
}

// TestRun_SkillsMisuseExits2 pins every way the command can be called wrong,
// including the unknown top-level word, which turning help into a command must
// not have turned into a command too. An unsafe name must be refused by the
// name guard, not by a failed read, so nothing outside the embedded tree is
// ever opened.
func TestRun_SkillsMisuseExits2(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantErrs []string
	}{
		{name: "no verb", args: []string{"skills"}, wantErrs: []string{"usage:"}},
		{name: "unknown verb", args: []string{"skills", "install"}, wantErrs: []string{`unknown skills verb "install"`}},
		{name: "show without name", args: []string{"skills", "show"}, wantErrs: []string{"usage:"}},
		{name: "get without name", args: []string{"skills", "get"}, wantErrs: []string{"usage:"}},
		{name: "unknown skill", args: []string{"skills", "show", "nope"}, wantErrs: []string{"unknown skill", `"nope"`}},
		{name: "traversal", args: []string{"skills", "show", "../etc/passwd"}, wantErrs: []string{"invalid skill name"}},
		{name: "backslash", args: []string{"skills", "show", `..\windows`}, wantErrs: []string{"invalid skill name"}},
		{name: "too many args", args: []string{"skills", "show", "a", "b"}, wantErrs: []string{"usage:"}},
		{name: "unknown top-level word", args: []string{"nonsense"}, wantErrs: []string{"usage:"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			gotExit := withExit(t, func() {
				run(tc.args, strings.NewReader(""), &stdout, &stderr)
			})
			if gotExit == nil || *gotExit != 2 {
				t.Fatalf("exit = %v, want 2", gotExit)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout non-empty on error: %q", stdout.String())
			}
			for _, want := range tc.wantErrs {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr missing %q: %q", want, stderr.String())
				}
			}
		})
	}
}

// TestRun_HelpIsARealCommand pins that help answers on stdout and exits 0.
// Before, `agento11y help` fell through to the arity guard, printed the usage
// line to stderr, and exited 2.
func TestRun_HelpIsARealCommand(t *testing.T) {
	for _, alias := range []string{"help", "--help", "-h"} {
		t.Run(alias, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			gotExit := withExit(t, func() {
				run([]string{alias}, strings.NewReader(""), &stdout, &stderr)
			})
			if gotExit != nil {
				t.Fatalf("exit = %v, want no exit (code 0)", *gotExit)
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr non-empty: %q", stderr.String())
			}
			out := stdout.String()
			// Match the row, not the bare word. Every one of these words also
			// occurs in prose elsewhere in the block ("skills" in the paste
			// hint, "local" inside --local, "pi" inside "pipelines"), so a
			// substring check would pass with the row deleted.
			for _, want := range helpRows() {
				if !strings.Contains(out, want) {
					t.Errorf("help has no row %q:\n%s", want, out)
				}
			}
			if !strings.Contains(out, "agento11y skills list") {
				t.Errorf("help does not name `agento11y skills list`:\n%s", out)
			}
			if !strings.Contains(out, skills.SetupCodingAgentCommand) {
				t.Errorf("help does not carry the setup hint %q:\n%s", skills.SetupCodingAgentCommand, out)
			}
		})
	}
}

// helpRows is the row each top-level word must appear as in the help block.
// The subcommand list is hand-written for the reason given at helpBody, so
// adding a subcommand to run and forgetting this list is not caught.
func helpRows() []string {
	rows := []string{"\n  <agent> hook", "\n  cursor install|uninstall", "\n  claude install [--json]", "\n  agents reconcile --agents claude,cursor --json"}
	for _, name := range []string{"login", "doctor", "skills", "local", "history", "help"} {
		rows = append(rows, "\n  "+name+" ")
	}
	// The launchers share one comma-separated row, built from the launchers
	// map so a new launcher fails this test until the row lists it too.
	names := make([]string, 0, len(launchers))
	for name := range launchers {
		names = append(names, name)
	}
	slices.Sort(names)
	return append(rows, "\n  "+strings.Join(names, ", ")+"\n")
}

// TestUsageLineNamesSkills pins that the one-line stderr form learned the new
// subcommand. It stays one line: it is what misuse prints, not a help page.
func TestUsageLineNamesSkills(t *testing.T) {
	got := usageLine()
	if !strings.Contains(got, "agento11y skills list|show <name>") {
		t.Errorf("usageLine does not offer skills: %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("usageLine spans lines: %q", got)
	}
}
