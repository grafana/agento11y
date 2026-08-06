// Package skills serves the agent skills bundled into the agento11y binary.
//
// A skill is a markdown file a coding agent reads to learn a workflow. These
// ship inside the binary rather than being fetched, so `agento11y skills show
// <name>` works with no registry, no network, and no second CLI to install.
// The cost is that a skill fix reaches a user only when the binary is
// upgraded.
//
// The content tree must live under this package. A //go:embed pattern cannot
// contain "." or "..", so it cannot reach the repository-root skills/
// directory, which holds the content gcx distributes instead.
//
// This package also owns the words the command is spelled with (Command,
// ListVerb, ShowVerb, GetVerb) and the setup hint text. internal/entry
// dispatches on those constants, internal/doctor prints the hint, and
// internal/login prints the command inside a sentence of its own. A renamed
// verb therefore cannot leave a printed hint naming a command that no longer
// runs.
package skills

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// The content tree is SKILL.md files only. Plain `content` (rather than
// `all:content`) keeps a stray dot-file such as content/.DS_Store out of the
// binary and out of Names().
//
//go:embed content
var content embed.FS

// contentRoot is the embedded directory each skill lives in as
// content/<name>/SKILL.md.
const contentRoot = "content"

// skillFile is the one file a skill directory must hold.
const skillFile = "SKILL.md"

// SetupCodingAgentSkill is the skill that walks a coding agent through
// configuring agento11y for a coding agent (llms.txt "Path A").
const SetupCodingAgentSkill = "setup-coding-agent"

// The words `agento11y skills` is spelled with. internal/entry dispatches on
// these rather than on its own literals, so renaming one here renames both the
// command and every hint that prints it.
const (
	// Command is the top-level word, as in `agento11y skills`.
	Command = "skills"
	// ListVerb prints one line per bundled skill.
	ListVerb = "list"
	// ShowVerb prints one skill. It is the spelling every printed hint uses.
	ShowVerb = "show"
	// GetVerb is what the sibling gcx CLI calls the same operation, accepted
	// so an agent that pattern-matches that CLI also works.
	GetVerb = "get"
)

// SetupCodingAgentCommand is the exact command doctor, login, and help print.
// It is built from the dispatch words above so the printed command and the
// accepted command cannot differ.
const SetupCodingAgentCommand = "agento11y " + Command + " " + ShowVerb + " " + SetupCodingAgentSkill

// The setup hint, in the two shapes the callers print.
//
// Both start at column 0. The doctor report reads an indented line as a
// key/value row, and its grammar check fails on any indented line that is not
// one.
const (
	// SetupCodingAgentHintIntro tells the reader what the line below it is
	// for. Callers render it faint.
	SetupCodingAgentHintIntro = "Need help? Paste this to your coding agent:"
	// SetupCodingAgentPasteLine is the line a user copies into a coding agent.
	// Callers render it at full contrast, because it is meant to be read and
	// copied.
	SetupCodingAgentPasteLine = "Run `" + SetupCodingAgentCommand + "` and follow it to set up Grafana Agent observability for my coding agent."
	// SetupCodingAgentOneLiner is the quiet form, printed when there is
	// nothing to fix. A paste block on every healthy run is noise.
	SetupCodingAgentOneLiner = "Setting up another coding agent? Run " + SetupCodingAgentCommand + "."
)

var (
	// ErrNotFound reports a name that no bundled skill uses.
	ErrNotFound = errors.New("unknown skill")
	// ErrUnsafeName reports a name that is a path rather than a bare skill
	// name. Rejecting it keeps a lookup inside the embedded tree.
	ErrUnsafeName = errors.New("invalid skill name")
)

// Skill is one bundled skill.
type Skill struct {
	// Name is the directory name under content/, which the frontmatter must
	// repeat.
	Name string
	// Description is the frontmatter description with runs of whitespace
	// collapsed to single spaces, so a folded scalar prints on one line.
	Description string
	// Body is the complete SKILL.md, frontmatter included, exactly as it ships.
	Body string
}

// Names returns every bundled skill name, sorted.
func Names() []string {
	return namesFrom(content)
}

// namesFrom lists the skill directories in fsys. A directory counts only when
// it holds a SKILL.md, so every name Names returns is one Get resolves.
func namesFrom(fsys fs.FS) []string {
	entries, err := fs.ReadDir(fsys, contentRoot)
	if err != nil {
		// The tree is compiled in, so a read failure means the binary was built
		// without it. There is nothing to recover from and nothing to list.
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := fs.Stat(fsys, contentRoot+"/"+e.Name()+"/"+skillFile); err != nil {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// All returns every bundled skill, sorted by name. A caller that lists the
// skills wants the descriptions too, and going through All means it never has
// to handle a lookup failure for a name the package itself produced.
func All() []Skill {
	names := Names()
	all := make([]Skill, 0, len(names))
	for _, name := range names {
		skill, err := Get(name)
		if err != nil {
			// Names only returns a directory that holds a SKILL.md, so a
			// failure here means the tree changed underneath us. Skipping is
			// better than a partial listing that ends in an error.
			continue
		}
		all = append(all, skill)
	}
	return all
}

// Get returns one bundled skill. A name containing a path separator, a "..",
// or nothing at all is rejected before any read, so a lookup can never leave
// the embedded tree.
func Get(name string) (Skill, error) {
	if !safeName(name) {
		return Skill{}, fmt.Errorf("%w: %q", ErrUnsafeName, name)
	}
	body, err := content.ReadFile(contentRoot + "/" + name + "/" + skillFile)
	if err != nil {
		return Skill{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return Skill{
		Name:        name,
		Description: parseDescription(string(body)),
		Body:        string(body),
	}, nil
}

// safeName reports whether name is a bare directory name. It mirrors the
// guard in internal/local.serveStatic.
func safeName(name string) bool {
	return name != "" &&
		!strings.Contains(name, "/") &&
		!strings.Contains(name, `\`) &&
		!strings.Contains(name, "..")
}

// splitFrontmatter separates the YAML frontmatter from the rest of the file.
// Frontmatter opens with a --- line and closes with the next one.
func splitFrontmatter(body string) (front, rest string, ok bool) {
	const delim = "---\n"
	if !strings.HasPrefix(body, delim) {
		return "", "", false
	}
	front, rest, ok = strings.Cut(body[len(delim):], "\n"+delim)
	if !ok {
		return "", "", false
	}
	return front, rest, true
}

// parseDescription reads the frontmatter description and collapses its
// whitespace, so a folded scalar renders as one line in `skills list`. A file
// whose frontmatter is missing or malformed yields an empty description; the
// package test fails on that rather than the command doing so at runtime.
func parseDescription(body string) string {
	front, _, ok := splitFrontmatter(body)
	if !ok {
		return ""
	}
	var meta struct {
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(front), &meta); err != nil {
		return ""
	}
	return strings.Join(strings.Fields(meta.Description), " ")
}
