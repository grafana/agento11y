package local

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/guardeval"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cloudPacksFixture is the canonical export of the coding agent guard packs
// for Grafana Cloud. Sigil keeps a byte-for-byte snapshot and builds its coding
// agent guard templates from it, so a Cloud guard denies the calls the local
// pack denies, apart from the cases marked local_only. Set UPDATE_GOLDENS=1 to
// regenerate it.
const cloudPacksFixture = "../../../../conformance/guards/coding-agent-packs.json"

// cloudPackMessages is what a Cloud guard tells the agent and the user when it
// denies a call. Local packs have no message of their own yet.
var cloudPackMessages = map[string]string{
	packGit:         "Your organization's coding agent policy blocks git commands that discard uncommitted work or rewrite history (reset --hard, push --force, clean -f, checkout --, stash drop or clear, branch -D).",
	packDestructive: "Your organization's coding agent policy blocks recursive deletes such as rm -rf.",
	packPermissions: "Your organization's coding agent policy blocks recursive chmod or chown, and chmod 777, on / or the home directory.",
	packDisk:        "Your organization's coding agent policy blocks irreversible disk operations: dd to a device, mkfs, wipefs, fdisk, and parted.",
	packFiles:       "Your organization's coding agent policy blocks reading or writing .env files.",
}

// cloudEnvFileBlockedNames is the File safety tool filter for Cloud. Cloud's
// argument matcher has only * and ?, so it cannot take the character classes
// in envFileBlockedNames. It blocks a .env path the way file tools send one,
// after a / or a quote and before a quote or a dot, and the apply_patch header,
// whose path follows "File: " and ends at an escaped newline. Cloud reads a
// backslash literally, so `\n` here is the two characters JSON escapes a
// newline to. A .env after a space or a shell character elsewhere in the
// arguments is denied only locally, and the cases mark it local_only.
func cloudEnvFileBlockedNames() []string {
	var out []string
	for _, name := range envFileToolNames {
		for _, arg := range []string{`*/.env"*`, `*/.env.*`, `*".env"*`, `*".env.*`} {
			out = append(out, name+"("+arg+")")
		}
		if name == "apply_patch" {
			for _, arg := range []string{`*: .env\n*`, `*: .env\r*`, `*/.env\n*`, `*/.env\r*`, `*: .env.*`} {
				out = append(out, name+"("+arg+")")
			}
		}
	}
	return out
}

// cloudPackCase is one tool call and the verdict on it. A case with no tool is
// a Bash call running command; any other tool takes input as its arguments.
// LocalOnly marks a call the local pack denies and Cloud's narrower File
// safety filter allows.
type cloudPackCase struct {
	Pack      string          `json:"pack"`
	Tool      string          `json:"tool,omitempty"`
	Command   string          `json:"command,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Deny      bool            `json:"deny"`
	LocalOnly bool            `json:"local_only,omitempty"`
}

// cloudPackCases are the calls every runtime must agree on. The test below
// checks them against the local engine; sigil runs the same cases against its
// templates.
var cloudPackCases = []cloudPackCase{
	{Pack: packGit, Command: "git reset --hard HEAD~1", Deny: true},
	{Pack: packGit, Command: "git push --force origin main", Deny: true},
	{Pack: packGit, Command: "git push -f", Deny: true},
	{Pack: packGit, Command: "git clean -fdx", Deny: true},
	{Pack: packGit, Command: "git checkout -- .", Deny: true},
	{Pack: packGit, Command: "git stash drop", Deny: true},
	{Pack: packGit, Command: "git branch -D feature", Deny: true},
	{Pack: packGit, Command: `bash -c "git reset --hard"`, Deny: true},
	{Pack: packGit, Command: "git -C repo reset --hard", Deny: true},
	{Pack: packGit, Command: "git status"},
	{Pack: packGit, Command: "git push origin main"},
	{Pack: packGit, Command: "git branch -d merged"},
	{Pack: packGit, Command: "git reset --soft HEAD~1"},
	{Pack: packDestructive, Command: "rm -rf /tmp/cache", Deny: true},
	{Pack: packDestructive, Command: "rm -fr build", Deny: true},
	{Pack: packDestructive, Command: "rm -r -f dist", Deny: true},
	{Pack: packDestructive, Command: "rm file.txt"},
	{Pack: packDestructive, Command: "rm -r empty-dir"},
	{Pack: packPermissions, Command: "chmod -R 755 /", Deny: true},
	{Pack: packPermissions, Command: "chown -R root $HOME", Deny: true},
	{Pack: packPermissions, Command: "chmod 777 ~", Deny: true},
	{Pack: packPermissions, Command: "chmod -R 755 ./build"},
	{Pack: packPermissions, Command: "chmod 644 README.md"},
	{Pack: packDisk, Command: "dd if=/dev/zero of=/dev/sda", Deny: true},
	{Pack: packDisk, Command: "mkfs.ext4 /dev/sdb1", Deny: true},
	{Pack: packDisk, Command: "wipefs -a /dev/sdb", Deny: true},
	{Pack: packDisk, Command: "dd if=in.img of=out.img"},
	{Pack: packFiles, Command: "cat .env", Deny: true},
	{Pack: packFiles, Command: "cp .env.example .env", Deny: true},
	{Pack: packFiles, Command: "cat environment.md"},
}

// cloudFileToolCases are File safety calls through file tools rather than a
// shell.
var cloudFileToolCases = []cloudPackCase{
	{Pack: packFiles, Tool: "Read", Input: json.RawMessage(`{"file_path":".env"}`), Deny: true},
	{Pack: packFiles, Tool: "Read", Input: json.RawMessage(`{"file_path":"/repo/.env"}`), Deny: true},
	{Pack: packFiles, Tool: "Write", Input: json.RawMessage(`{"file_path":".env.local","content":"A=1"}`), Deny: true},
	{Pack: packFiles, Tool: "Edit", Input: json.RawMessage(`{"file_path":"/repo/.env.production"}`), Deny: true},
	{Pack: packFiles, Tool: "apply_patch", Input: json.RawMessage(`{"command":"*** Begin Patch\n*** Update File: .env\n@@\n-OLD=value\n+NEW=value\n*** End Patch"}`), Deny: true},
	{Pack: packFiles, Tool: "apply_patch", Input: json.RawMessage(`{"command":"*** Begin Patch\r\n*** Add File: /repo/.env\r\n+NEW=value\r\n*** End Patch"}`), Deny: true},
	{Pack: packFiles, Tool: "apply_patch", Input: json.RawMessage(`{"command":"*** Begin Patch\n*** Delete File: .env.local\n*** End Patch"}`), Deny: true},
	{Pack: packFiles, Tool: "Grep", Input: json.RawMessage(`{"pattern":"API_KEY","path":"/repo/.env"}`), Deny: true},
	{Pack: packFiles, Tool: "Grep", Input: json.RawMessage(`{"pattern":"API_KEY","glob":".env.production"}`), Deny: true},
	{Pack: packFiles, Tool: "grep_search", Input: json.RawMessage(`{"query":"API_KEY","include_pattern":".env"}`), Deny: true},
	{Pack: packFiles, Tool: "Write", Input: json.RawMessage(`{"file_path":"notes.md","content":"the keys live in .env here"}`), Deny: true, LocalOnly: true},
	{Pack: packFiles, Tool: "Grep", Input: json.RawMessage(`{"pattern":"load .env","path":"src"}`), Deny: true, LocalOnly: true},
	{Pack: packFiles, Tool: "Read", Input: json.RawMessage(`{"file_path":".environment"}`)},
	{Pack: packFiles, Tool: "Read", Input: json.RawMessage(`{"file_path":".envrc"}`)},
	{Pack: packFiles, Tool: "read_file", Input: json.RawMessage(`{"file_path":"config.env"}`)},
	{Pack: packFiles, Tool: "Read", Input: json.RawMessage(`{"file_path":".env\nbackup"}`)},
	{Pack: packFiles, Tool: "apply_patch", Input: json.RawMessage(`{"command":"*** Begin Patch\n*** Add File: .envrc\n+NEW=value\n*** End Patch"}`)},
	{Pack: packFiles, Tool: "mcp__fs__read", Input: json.RawMessage(`{"path":".env"}`)},
	{Pack: packFiles, Tool: "Grep", Input: json.RawMessage(`{"pattern":"TODO","path":"src"}`)},
	{Pack: packFiles, Tool: "Glob", Input: json.RawMessage(`{"pattern":"**/.env*"}`)},
}

type cloudPack struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Preview      []string `json:"preview"`
	Priority     int      `json:"priority"`
	Target       string   `json:"target"`
	Patterns     []string `json:"patterns"`
	BlockedNames []string `json:"blocked_names,omitempty"`
	FailMessage  string   `json:"fail_message"`
}

func TestCloudPacksFixture(t *testing.T) {
	catalog := packByID()
	cases := slices.Concat(cloudPackCases, cloudFileToolCases)
	var packs []cloudPack
	for _, id := range []string{packGit, packDestructive, packPermissions, packDisk, packFiles} {
		rule, err := packRule(id)
		require.NoError(t, err)
		// The shell_command regex is exported as it is. The files pack's
		// tool filter is exported as cloudEnvFileBlockedNames instead.
		require.Len(t, rule.Evaluators, 1)
		config := rule.Evaluators[0].Config
		var patterns []string
		for _, p := range config["patterns"].([]any) {
			patterns = append(patterns, p.(string))
		}
		pack := cloudPack{
			ID:          id,
			Title:       catalog[id].Title,
			Description: catalog[id].Description,
			Preview:     catalog[id].Preview,
			Priority:    rule.Priority,
			Target:      config["target"].(string),
			Patterns:    patterns,
			FailMessage: cloudPackMessages[id],
		}
		require.Equal(t, id == packFiles, rule.ToolFilter != nil, "only the files pack has a tool filter")
		if rule.ToolFilter != nil {
			pack.BlockedNames = cloudEnvFileBlockedNames()
		}
		packs = append(packs, pack)

		data, err := guardeval.EncodeRules([]guardeval.Rule{rule})
		require.NoError(t, err)
		engine := guardeval.NewEngineFromContents("guards.toml", data, nil)
		require.Empty(t, engine.Status().Errors)
		for _, tc := range cases {
			if tc.Pack != id {
				continue
			}
			tool, input := tc.Tool, tc.Input
			if tool == "" {
				tool = "Bash"
				input, err = json.Marshal(map[string]string{"command": tc.Command})
				require.NoError(t, err)
			}
			want := agento11y.HookActionAllow
			if tc.Deny {
				want = agento11y.HookActionDeny
			}
			assert.Equal(t, want, engine.Evaluate(packRequest(tool, string(input))).Action, "%s: %s %s", id, tool, input)
		}
	}

	got, err := json.MarshalIndent(map[string]any{"packs": packs, "cases": cases}, "", "  ")
	require.NoError(t, err)
	got = append(got, '\n')
	if os.Getenv("UPDATE_GOLDENS") == "1" {
		require.NoError(t, os.MkdirAll(filepath.Dir(cloudPacksFixture), 0o755))
		require.NoError(t, os.WriteFile(cloudPacksFixture, got, 0o644))
	}
	want, err := os.ReadFile(cloudPacksFixture)
	require.NoError(t, err, "run with UPDATE_GOLDENS=1 to create the fixture")
	assert.Equal(t, string(want), string(got), "run with UPDATE_GOLDENS=1 and copy the file to sigil")
}
