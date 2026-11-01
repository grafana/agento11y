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
// for Grafana Cloud. Sigil keeps byte-for-byte copies and turns each pack into
// one tool filter rule (its patterns become tool_filter.shell_patterns), so a
// Cloud guard denies the calls the local pack denies, apart from the cases
// marked local_only. Set UPDATE_GOLDENS=1 to regenerate it.
const cloudPacksFixture = "../../../../conformance/guards/coding-agent-packs.json"

// cloudPackMessages is what a Cloud guard tells the agent and the user when it
// denies a call. Local packs have no message of their own yet.
var cloudPackMessages = map[string]string{
	packGit:         "Your organization's coding agent policy blocks git commands that discard uncommitted work or rewrite history (reset --hard, push --force or +refspec, clean -f, checkout -- or ., checkout or switch -f, restore without --staged, stash drop or clear, branch -D).",
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
//
// A search tool's glob can put a * on either side of .env. Cloud has no way to
// name a literal *, so the search forms put ? there: .env* matches ".env?",
// and *.env matches "?.env". That also blocks a one-character name in that
// position, such as a glob of a.env, which the local pack allows.
func cloudEnvFileBlockedNames() []string {
	var out []string
	for _, name := range envFileToolNames {
		for _, arg := range []string{`*/.env"*`, `*/.env.*`, `*".env"*`, `*".env.*`} {
			out = append(out, name+"("+arg+")")
		}
		if slices.Contains(envSearchToolNames, name) {
			for _, arg := range []string{`*/.env?"*`, `*".env?"*`, `*/?.env"*`, `*"?.env"*`, `*/?.env?"*`, `*"?.env?"*`, `*/?.env.*`, `*"?.env.*`} {
				out = append(out, name+"("+arg+")")
			}
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
// checks them against the local engine; sigil runs the same cases against each
// pack's tool filter (TestCodingAgentPacksMatchLocalCases).
var cloudPackCases = []cloudPackCase{
	// A blocked command mentioned in quoted text, a comment or a heredoc body
	// runs nothing, so the packs allow it.
	{Pack: packGit, Command: "git commit -m \"$(cat <<'EOF'\nUndo with git checkout . or git reset --hard, never \"git push --force\".\nEOF\n)\""},
	{Pack: packDestructive, Command: "gh pr create --title x --body \"$(cat <<'EOF'\nThe destructive pack blocks rm -rf /.\nEOF\n)\""},
	{Pack: packGit, Command: "git log -5 # before git reset --hard"},
	{Pack: packGit, Command: `echo "run git reset --hard to start over"`},
	{Pack: packGit, Command: `git commit -m 'git push --force is blocked'`},
	{Pack: packDestructive, Command: `grep -r "rm -rf" .`},
	{Pack: packFiles, Command: `git commit -m "add .env to .gitignore"`},
	// A heredoc body is data unless a shell reads it, whether or not its
	// delimiter is quoted, so a newline ends what can come before a name.
	{Pack: packDestructive, Command: "cat > notes.md <<EOF\nnever run rm -rf /\nEOF"},
	{Pack: packDestructive, Command: "cat > notes.md <<\\EOF\nnever run rm -rf /\nEOF"},
	{Pack: packGit, Command: "git commit -F - <<EOF\nrevert: stop calling git reset --hard\nEOF"},
	// A command after another, behind a wrapper or in code the shell runs is
	// still a command.
	{Pack: packGit, Command: "cd repo && git reset --hard", Deny: true},
	{Pack: packGit, Command: "GIT_DIR=x git reset --hard", Deny: true},
	{Pack: packDestructive, Command: "sudo rm -rf /", Deny: true},
	{Pack: packGit, Command: "env FOO=1 git push --force", Deny: true},
	{Pack: packGit, Command: "/usr/bin/git reset --hard", Deny: true},
	{Pack: packGit, Command: `\git reset --hard`, Deny: true},
	{Pack: packDestructive, Command: "xargs rm -rf", Deny: true},
	{Pack: packDestructive, Command: "find . | xargs -I{} rm -rf {}", Deny: true},
	{Pack: packDestructive, Command: `find . -name build -exec rm -rf {} \;`, Deny: true},
	{Pack: packGit, Command: `bash -lc 'git reset --hard'`, Deny: true},
	{Pack: packDestructive, Command: `sh -c "rm -rf /tmp/x"`, Deny: true},
	{Pack: packGit, Command: `eval "git reset --hard"`, Deny: true},
	{Pack: packGit, Command: "bash <<'EOF'\ngit reset --hard\nEOF", Deny: true},
	{Pack: packGit, Command: "echo $(git reset --hard)", Deny: true},
	{Pack: packGit, Command: `echo "$(git reset --hard)"`, Deny: true},
	{Pack: packGit, Command: "(cd x; git reset --hard)", Deny: true},
	{Pack: packGit, Command: "{ git reset --hard; }", Deny: true},
	{Pack: packGit, Command: "if true; then git reset --hard; fi", Deny: true},
	{Pack: packGit, Command: "time git push --force", Deny: true},
	{Pack: packGit, Command: "timeout 30 git clean -fdx", Deny: true},
	{Pack: packGit, Command: "cat <<EOF > notes\nhi\nEOF\ngit reset --hard", Deny: true},
	{Pack: packFiles, Command: "cat .env", Deny: true},
	{Pack: packFiles, Command: `cat ".env"`, Deny: true},
	{Pack: packFiles, Command: "echo x > .env", Deny: true},
	{Pack: packFiles, Command: "source .env && npm start", Deny: true},
	{Pack: packGit, Command: "git reset --hard HEAD~1", Deny: true},
	{Pack: packGit, Command: "git push --force origin main", Deny: true},
	{Pack: packGit, Command: "git push -f", Deny: true},
	{Pack: packGit, Command: "git clean -fdx", Deny: true},
	{Pack: packGit, Command: "git checkout -- .", Deny: true},
	{Pack: packGit, Command: "git stash drop", Deny: true},
	{Pack: packGit, Command: "git branch -D feature", Deny: true},
	{Pack: packGit, Command: `bash -c "git reset --hard"`, Deny: true},
	{Pack: packGit, Command: "git -C repo reset --hard", Deny: true},
	// The shell tools of Codex, Gemini CLI, and Copilot, which are not named Bash.
	{Pack: packGit, Tool: "exec_command", Input: json.RawMessage(`{"cmd":"git reset --hard"}`), Deny: true},
	{Pack: packGit, Tool: "local_shell", Input: json.RawMessage(`{"command":["bash","-lc","git reset --hard"]}`), Deny: true},
	{Pack: packGit, Tool: "run_shell_command", Input: json.RawMessage(`{"command":"git reset --hard"}`), Deny: true},
	{Pack: packGit, Tool: "run_in_terminal", Input: json.RawMessage(`{"command":"git reset --hard"}`), Deny: true},
	{Pack: packGit, Tool: "exec_command", Input: json.RawMessage(`{"cmd":"git status"}`)},
	// The shell joins a backslash-newline before it runs the command.
	{Pack: packGit, Command: "git reset \\\n--hard", Deny: true},
	{Pack: packDestructive, Command: "rm \\\n-rf /tmp/x", Deny: true},
	// The shell removes quotes and backslashes before it runs a word.
	{Pack: packGit, Command: `git "reset" --hard`, Deny: true},
	// A wrapper the packs do not know still does not hide the command, since a
	// command name matches after any unquoted separator.
	{Pack: packDestructive, Command: "pkexec rm -rf /", Deny: true},
	{Pack: packDestructive, Command: "chroot / rm -rf /", Deny: true},
	{Pack: packDestructive, Command: "strace -f rm -rf /", Deny: true},
	{Pack: packDestructive, Command: "runuser -u x -- rm -rf /", Deny: true},
	{Pack: packDestructive, Command: "taskset -c 0 rm -rf /", Deny: true},
	{Pack: packDestructive, Command: "ssh host rm -rf /", Deny: true},
	{Pack: packDestructive, Command: "docker exec c rm -rf /", Deny: true},
	{Pack: packDestructive, Command: "mise exec -- rm -rf /", Deny: true},
	{Pack: packGit, Command: "pkexec git reset --hard", Deny: true},
	// A command on a later line, a heredoc fed to a shell, and a quoted
	// argument that spans lines in front of the name are still read.
	{Pack: packDestructive, Command: "echo hi\npkexec rm -rf /", Deny: true},
	{Pack: packDestructive, Command: "bash <<EOF\nrm -rf /\nEOF", Deny: true},
	{Pack: packDestructive, Command: "docker exec -e \"A=x\ny\" c rm -rf /", Deny: true},
	// An argv list is also judged joined with plain spaces, as v0.52 judged it,
	// so a message in its own argv word reads as a command there.
	{Pack: packGit, Tool: "exec_command", Input: json.RawMessage(`{"cmd":["git","commit","-m","drop git reset --hard"]}`), Deny: true},
	{Pack: packDestructive, Command: "pkexec a#b rm -rf /", Deny: true},
	// The trade: a command named as an unquoted argument is judged too.
	{Pack: packDestructive, Command: "echo rm -rf /", Deny: true},
	{Pack: packGit, Command: "git log -5 # before git reset --hard"},
	{Pack: packGit, Command: `git 'reset' --hard`, Deny: true},
	{Pack: packGit, Command: `g\it re""set --hard`, Deny: true},
	{Pack: packGit, Command: `git checkout "."`, Deny: true},
	{Pack: packGit, Command: `git checkout \*`, Deny: true},
	{Pack: packDestructive, Command: `rm $'-rf' /tmp/x`, Deny: true},
	// A quoted ~ is not the home directory.
	{Pack: packPermissions, Command: `chmod -R 755 "~/"`},
	{Pack: packGit, Command: `g\it 'reset' --hard`, Deny: true},
	{Pack: packDestructive, Command: `rm "-rf" /tmp/x`, Deny: true},
	{Pack: packDestructive, Command: `rm -r /tmp/"path -f"`},
	// The other ways git throws away uncommitted work.
	{Pack: packGit, Command: "git checkout .", Deny: true},
	{Pack: packGit, Command: "git checkout -f main", Deny: true},
	{Pack: packGit, Command: "git restore src/main.go", Deny: true},
	{Pack: packGit, Command: "git restore --staged --worktree .", Deny: true},
	{Pack: packGit, Command: "git push origin +main", Deny: true},
	{Pack: packGit, Command: "git restore --staged ."},
	{Pack: packGit, Command: "git diff --name-only | xargs git restore", Deny: true},
	{Pack: packGit, Command: "git restore --staged --pathspec-from-file -"},
	{Pack: packGit, Command: "git reset --ha", Deny: true},
	{Pack: packGit, Command: "git -P reset --hard", Deny: true},
	{Pack: packGit, Command: "git switch --di main", Deny: true},
	{Pack: packGit, Command: "cat <<< EOF\ngit \"reset\" --hard", Deny: true},
	{Pack: packGit, Command: "echo $((a<<b))\ngit \"reset\" --hard", Deny: true},
	{Pack: packPermissions, Command: "chmod -R 777 ~/.", Deny: true},
	{Pack: packGit, Command: "git restore --source HEAD --staged src/a.go"},
	{Pack: packGit, Command: "git restore . --staged"},
	{Pack: packGit, Command: `git checkout '*'`, Deny: true},
	{Pack: packGit, Command: `git $'reset' --hard`, Deny: true},
	{Pack: packGit, Command: `git "reset" --hard # don't`, Deny: true},
	{Pack: packGit, Command: "cat <<EOF > notes.md\nit's done\nEOF\ngit \"reset\" --hard", Deny: true},
	{Pack: packGit, Command: "git checkout -b feature"},
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
	// The $(...) and backticks of an unquoted heredoc body run as the shell
	// expands it; a quoted delimiter keeps the body as data, and any word is a
	// delimiter.
	{Pack: packDestructive, Command: "cat > notes.md <<EOF\nNever run `rm -rf ~`\nEOF", Deny: true},
	{Pack: packGit, Command: "cat <<EOF\n$(git reset --hard)\nEOF", Deny: true},
	{Pack: packGit, Command: "cat <<'EOF'\n$(git reset --hard)\nEOF"},
	{Pack: packDestructive, Command: "cat <<'END-OF-SCRIPT' > clean.sh\nrm -rf build\nEND-OF-SCRIPT"},
	// Wrapper options with values, shell options before -c, and redirections
	// before the command do not hide it.
	{Pack: packGit, Command: "sudo -u deploy bash -c 'git reset --hard'", Deny: true},
	{Pack: packDestructive, Command: "env -u X find ~ -exec rm -rf {} +", Deny: true},
	{Pack: packDestructive, Command: "xargs -I % sh -c 'rm -rf %'", Deny: true},
	{Pack: packGit, Command: "timeout -s KILL 60 bash -c 'git reset --hard'", Deny: true},
	{Pack: packGit, Command: "env -S 'git reset --hard'", Deny: true},
	{Pack: packGit, Command: "coproc git reset --hard", Deny: true},
	{Pack: packGit, Command: "flock -n /tmp/lock git reset --hard", Deny: true},
	{Pack: packGit, Command: "watch -n 5 'git reset --hard'", Deny: true},
	{Pack: packGit, Command: "script -q /dev/null git reset --hard", Deny: true},
	{Pack: packGit, Command: "bash -o pipefail -c 'git reset --hard'", Deny: true},
	{Pack: packGit, Command: "bash --norc -c -- 'git reset --hard'", Deny: true},
	{Pack: packGit, Command: "bash --norc <<'EOF'\ngit reset --hard\nEOF", Deny: true},
	{Pack: packGit, Command: "2>&1 git reset --hard", Deny: true},
	{Pack: packGit, Command: "git 2>/dev/null reset --hard", Deny: true},
	{Pack: packDestructive, Command: ">/dev/null rm -rf ~", Deny: true},
	{Pack: packGit, Command: `bash -c "bash -c \"bash -c 'git reset --hard'\""`, Deny: true},
	{Pack: packGit, Command: "git --attr-source=HEAD reset --hard", Deny: true},
	{Pack: packGit, Command: `git $'\x72eset' --hard`, Deny: true},
	{Pack: packGit, Command: `git $'\u0072eset' --hard`, Deny: true},
	{Pack: packGit, Command: `git $'\U00000072eset' --hard`, Deny: true},
	// Long wrapper options take their value from the next word.
	{Pack: packGit, Command: "sudo --user deploy git reset --hard", Deny: true},
	{Pack: packGit, Command: "timeout --signal KILL 5 git reset --hard", Deny: true},
	{Pack: packGit, Command: "xargs --max-args 1 git reset --hard", Deny: true},
	{Pack: packGit, Command: "env --split-string 'git reset --hard'", Deny: true},
	{Pack: packGit, Command: "sudo --preserve-env git status"},
	// A nested command and an argv list are each one command.
	{Pack: packGit, Tool: "local_shell", Input: json.RawMessage(`{"action":{"type":"exec","command":["git","reset","--hard"]}}`), Deny: true},
	{Pack: packGit, Tool: "Bash", Input: json.RawMessage(`{"args":["git","reset","--hard"]}`), Deny: true},
	// Read-only disk and git commands.
	{Pack: packDisk, Command: "dd if=/dev/zero of=/dev/null bs=1M count=1"},
	{Pack: packDisk, Command: "dd if=disk.img of=/dev/stdout"},
	{Pack: packDisk, Command: "dd if=/dev/zero of=/dev/disk2", Deny: true},
	{Pack: packDisk, Command: "sudo fdisk -l"},
	{Pack: packDisk, Command: "fdisk /dev/sdb", Deny: true},
	{Pack: packDisk, Command: "parted -l"},
	{Pack: packDisk, Command: "parted /dev/sda print"},
	{Pack: packDisk, Command: "parted -s /dev/sdb mklabel gpt", Deny: true},
	{Pack: packDisk, Command: "parted /dev/sdb", Deny: true},
	{Pack: packGit, Command: "git restore --help"},
	{Pack: packGit, Command: "git clean -nfd"},
	// A shell call whose command key is missing or unreadable is judged by
	// its string values.
	{Pack: packFiles, Tool: "Bash", Input: json.RawMessage(`{"input":"cat .env"}`), Deny: true},
	{Pack: packFiles, Tool: "Bash", Input: json.RawMessage(`{"command":"ls","script":"cat .env"}`), Deny: true},
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
	{Pack: packFiles, Tool: "Grep", Input: json.RawMessage(`{"pattern":"API_KEY","glob":".env*"}`), Deny: true},
	{Pack: packFiles, Tool: "Grep", Input: json.RawMessage(`{"pattern":"API_KEY","glob":"**/.env*"}`), Deny: true},
	{Pack: packFiles, Tool: "Grep", Input: json.RawMessage(`{"pattern":"API_KEY","glob":"*.env"}`), Deny: true},
	{Pack: packFiles, Tool: "grep_search", Input: json.RawMessage(`{"query":"API_KEY","include_pattern":"**/*.env*"}`), Deny: true},
	{Pack: packFiles, Tool: "Write", Input: json.RawMessage(`{"file_path":"notes.md","content":"the keys live in .env here"}`), Deny: true, LocalOnly: true},
	{Pack: packFiles, Tool: "Grep", Input: json.RawMessage(`{"pattern":"load .env","path":"src"}`), Deny: true, LocalOnly: true},
	{Pack: packFiles, Tool: "Read", Input: json.RawMessage(`{"file_path":".environment"}`)},
	{Pack: packFiles, Tool: "Read", Input: json.RawMessage(`{"file_path":".envrc"}`)},
	{Pack: packFiles, Tool: "read_file", Input: json.RawMessage(`{"file_path":"config.env"}`)},
	{Pack: packFiles, Tool: "Read", Input: json.RawMessage(`{"file_path":".env\nbackup"}`)},
	{Pack: packFiles, Tool: "apply_patch", Input: json.RawMessage(`{"command":"*** Begin Patch\n*** Add File: .envrc\n+NEW=value\n*** End Patch"}`)},
	{Pack: packFiles, Tool: "mcp__fs__read", Input: json.RawMessage(`{"path":".env"}`)},
	{Pack: packFiles, Tool: "Grep", Input: json.RawMessage(`{"pattern":"TODO","path":"src"}`)},
	{Pack: packFiles, Tool: "Grep", Input: json.RawMessage(`{"pattern":"API_KEY","glob":".envrc"}`)},
	{Pack: packFiles, Tool: "Grep", Input: json.RawMessage(`{"pattern":"API_KEY","glob":"config.env"}`)},
	{Pack: packFiles, Tool: "Grep", Input: json.RawMessage(`{"pattern":"API_KEY","glob":"*.environment"}`)},
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
