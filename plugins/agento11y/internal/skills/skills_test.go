package skills

import (
	"errors"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"gopkg.in/yaml.v3"
)

// TestNamesAreSortedAndComplete pins the listing order and the one skill the
// doctor, login and help hints promise. An unsorted list makes `skills list`
// output depend on the embed walk order rather than on the names.
func TestNamesAreSortedAndComplete(t *testing.T) {
	names := Names()
	if len(names) == 0 {
		t.Fatal("Names() returned nothing; the embedded content tree is empty")
	}
	if !slices.IsSorted(names) {
		t.Errorf("Names() = %v, want lexicographically sorted", names)
	}
	if !slices.Contains(names, SetupCodingAgentSkill) {
		t.Errorf("Names() = %v, want it to contain %q", names, SetupCodingAgentSkill)
	}
}

// TestNamesFrom drives the listing off a fake tree, because the bundled tree
// has one skill and so exercises neither the sort nor the filter. A directory
// without a SKILL.md is not a skill: listing one would print a name that Get
// then fails on.
func TestNamesFrom(t *testing.T) {
	const front = "---\nname: x\ndescription: x\n---\n"
	fsys := fstest.MapFS{
		contentRoot + "/b-second/" + skillFile:  {Data: []byte(front)},
		contentRoot + "/a-first/" + skillFile:   {Data: []byte(front)},
		contentRoot + "/no-skill-file/notes.md": {Data: []byte("not a skill")},
		contentRoot + "/README.md":              {Data: []byte("not a directory")},
	}
	got := namesFrom(reverseDirFS{fsys})
	want := []string{"a-first", "b-second"}
	if !slices.Equal(got, want) {
		t.Errorf("namesFrom() = %v, want %v", got, want)
	}
}

// reverseDirFS hands back directory entries in reverse order. Both embed.FS
// and fstest.MapFS return theirs already sorted, so without a filesystem that
// does not, a test cannot tell a namesFrom that sorts from one that inherits
// the order it was given.
type reverseDirFS struct{ fs.FS }

func (r reverseDirFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(r.FS, name)
	if err != nil {
		return nil, err
	}
	slices.Reverse(entries)
	return entries, nil
}

// TestNamesFromMissingRoot pins that a binary built without the content tree
// lists nothing rather than failing.
func TestNamesFromMissingRoot(t *testing.T) {
	if got := namesFrom(fstest.MapFS{}); got != nil {
		t.Errorf("namesFrom(empty) = %v, want nil", got)
	}
}

// TestAllMatchesNames pins that the listing helper and the name list agree.
// `skills list` reads All, so a disagreement is a skill missing from the
// output.
func TestAllMatchesNames(t *testing.T) {
	var got []string
	for _, skill := range All() {
		got = append(got, skill.Name)
		if strings.TrimSpace(skill.Body) == "" {
			t.Errorf("All() returned %q with an empty body", skill.Name)
		}
	}
	if want := Names(); !slices.Equal(got, want) {
		t.Errorf("All() names = %v, want %v", got, want)
	}
}

// maxSkillLines bounds a bundled skill. A skill is read into a coding agent's
// context in full, so an unbounded file is a cost every invocation pays.
const maxSkillLines = 500

// TestBundledSkillsAreValid is the guard that skills/agento11y-eval-starter
// never had: every bundled SKILL.md must open and close its frontmatter with
// `---`, parse as YAML, name itself after its directory, carry a description,
// and stay under the line bound.
func TestBundledSkillsAreValid(t *testing.T) {
	for _, name := range Names() {
		t.Run(name, func(t *testing.T) {
			skill, err := Get(name)
			if err != nil {
				t.Fatalf("Get(%q): %v", name, err)
			}

			body := skill.Body
			raw, _, ok := splitFrontmatter(body)
			if !ok {
				t.Fatalf("skill %q: frontmatter must open and close with a --- line", name)
			}

			var front struct {
				Name        string `yaml:"name"`
				Description string `yaml:"description"`
			}
			if err := yaml.Unmarshal([]byte(raw), &front); err != nil {
				t.Fatalf("skill %q: frontmatter is not valid YAML: %v", name, err)
			}
			if front.Name != name {
				t.Errorf("skill %q: frontmatter name = %q, want the directory name %q", name, front.Name, name)
			}
			if strings.TrimSpace(front.Description) == "" {
				t.Errorf("skill %q: frontmatter description is empty", name)
			}
			if skill.Name != name {
				t.Errorf("skill %q: Skill.Name = %q", name, skill.Name)
			}
			if strings.TrimSpace(skill.Description) == "" {
				t.Errorf("skill %q: Skill.Description is empty", name)
			}
			if strings.ContainsAny(skill.Description, "\n\r") {
				t.Errorf("skill %q: Skill.Description spans lines: %q", name, skill.Description)
			}

			if lines := countLines(body); lines >= maxSkillLines {
				t.Errorf("skill %q: %d lines, want fewer than %d", name, lines, maxSkillLines)
			}
		})
	}
}

// countLines counts the lines in a file. A trailing newline ends the last
// line rather than starting a new one, so counting separators alone would
// report one line too many and make the bound off by one.
func countLines(body string) int {
	if body == "" {
		return 0
	}
	return strings.Count(strings.TrimSuffix(body, "\n"), "\n") + 1
}

func TestCountLines(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{name: "empty", body: "", want: 0},
		{name: "one line, no newline", body: "a", want: 1},
		{name: "one line, trailing newline", body: "a\n", want: 1},
		{name: "two lines", body: "a\nb\n", want: 2},
		{name: "blank last line", body: "a\n\n", want: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := countLines(tc.body); got != tc.want {
				t.Errorf("countLines(%q) = %d, want %d", tc.body, got, tc.want)
			}
		})
	}
}

// TestGetRejectsUnsafeAndUnknownNames pins the traversal guard copied from
// local.serveStatic: a name is a bare directory name inside the embedded tree,
// never a path.
func TestGetRejectsUnsafeAndUnknownNames(t *testing.T) {
	cases := []struct {
		name    string
		arg     string
		wantErr error
	}{
		{name: "empty", arg: "", wantErr: ErrUnsafeName},
		{name: "slash", arg: "../etc/passwd", wantErr: ErrUnsafeName},
		{name: "backslash", arg: `..\windows`, wantErr: ErrUnsafeName},
		{name: "dotdot", arg: "..", wantErr: ErrUnsafeName},
		{name: "nested", arg: "setup-coding-agent/SKILL.md", wantErr: ErrUnsafeName},
		{name: "absolute", arg: "/etc/passwd", wantErr: ErrUnsafeName},
		{name: "unknown", arg: "nope", wantErr: ErrNotFound},
		{name: "content root", arg: "content", wantErr: ErrNotFound},
		{name: "dot", arg: ".", wantErr: ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			skill, err := Get(tc.arg)
			if err == nil {
				t.Fatalf("Get(%q) succeeded, want %v", tc.arg, tc.wantErr)
			}
			if skill.Body != "" {
				t.Errorf("Get(%q) returned a body on error: %q", tc.arg, skill.Body)
			}
			// errors.Is, not a string match: the sentinels exist so a caller
			// can tell misuse from a missing skill, and a string match would
			// still pass if the %w wrap became a %v.
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Get(%q) error = %v, want it to wrap %v", tc.arg, err, tc.wantErr)
			}
		})
	}
}

// TestParseDescription covers the malformed frontmatter the bundled tree
// cannot show, because the one skill in it is valid.
func TestParseDescription(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "folded scalar collapses to one line",
			body: "---\nname: x\ndescription: >-\n  first line\n  second line\n---\n\n# x\n",
			want: "first line second line",
		},
		{
			name: "plain scalar",
			body: "---\nname: x\ndescription: one line\n---\n",
			want: "one line",
		},
		{name: "no opening delimiter", body: "name: x\ndescription: y\n---\n"},
		{name: "no closing delimiter", body: "---\nname: x\ndescription: y\n"},
		{name: "invalid yaml", body: "---\ndescription: unquoted: colon\n---\n"},
		{name: "no description key", body: "---\nname: x\n---\n"},
		{name: "empty file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseDescription(tc.body); got != tc.want {
				t.Errorf("parseDescription() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSetupCodingAgentCommand pins the command string the doctor footer, the
// login next step, and the help block all print. A drift here is a user
// pasting a command that does not run. internal/entry proves the other half:
// that the words in this string are the words its dispatch accepts.
func TestSetupCodingAgentCommand(t *testing.T) {
	const want = "agento11y skills show setup-coding-agent"
	if SetupCodingAgentCommand != want {
		t.Errorf("SetupCodingAgentCommand = %q, want %q", SetupCodingAgentCommand, want)
	}
	if _, err := Get(SetupCodingAgentSkill); err != nil {
		t.Errorf("the command names a skill that is not bundled: %v", err)
	}
}

// TestSetupHintLines pins the shape the doctor footer depends on: every line
// starts at column 0, because doctor's row grammar claims any indented line,
// and the paste line names the command a user is told to run.
func TestSetupHintLines(t *testing.T) {
	lines := map[string]string{
		"intro":     SetupCodingAgentHintIntro,
		"paste":     SetupCodingAgentPasteLine,
		"one-liner": SetupCodingAgentOneLiner,
	}
	for name, line := range lines {
		if strings.TrimSpace(line) == "" {
			t.Errorf("%s hint line is empty", name)
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			t.Errorf("%s hint line %q is indented; every line must start at column 0", name, line)
		}
		if strings.Contains(line, "\n") {
			t.Errorf("%s hint line %q contains a newline; each is one line", name, line)
		}
	}
	if !strings.Contains(SetupCodingAgentPasteLine, SetupCodingAgentCommand) {
		t.Errorf("the paste line does not name %q: %q", SetupCodingAgentCommand, SetupCodingAgentPasteLine)
	}
	if !strings.Contains(SetupCodingAgentOneLiner, SetupCodingAgentCommand) {
		t.Errorf("the one-liner does not name %q: %q", SetupCodingAgentCommand, SetupCodingAgentOneLiner)
	}
}
