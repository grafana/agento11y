package guardeval

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/grafana/agento11y/go/agento11y"
)

// `shell_command` projects command strings from known shell tools' JSON
// arguments and reads them the way the shell does: it splits each into the
// commands the shell runs, keeps quoted text and quoted heredoc bodies as data,
// and decodes the code in sh -c, eval, $(...) and the like (shellSubjects). It
// expands no variables and runs nothing.

// defaultShellToolNames are the tool names treated as "runs a shell command"
// with no configuration. They cover the spellings the supported hosts emit
// (Claude Code's Bash, Cursor's run_terminal_cmd, Codex's exec_command and
// local_shell, Gemini CLI's run_shell_command, Copilot's run_in_terminal,
// execute_command / run_command, and generic terminal / shell). Grafana Cloud's
// shell_patterns use the same list (sigil/internal/eval/hooks/shell.go).
// Matching is case-insensitive, so "Bash" and "bash" are the same tool.
var defaultShellToolNames = []string{
	"bash", "shell", "run_terminal_cmd", "execute_command", "run_command", "terminal", "powershell", "pwsh",
	"exec_command", "local_shell", "run_shell_command", "run_in_terminal",
}

// defaultShellCommandKeys are the argument keys that hold the command line,
// in the order they are tried. The first key present with a non-empty string
// value wins.
var defaultShellCommandKeys = []string{"command", "cmd", "script"}

type shellConfig struct {
	// toolNames is lowercased for case-insensitive comparison.
	toolNames []string
	// commandKeys is lowercased and ordered by precedence.
	commandKeys []string
}

func parseShellConfig(config map[string]any) (shellConfig, error) {
	out := shellConfig{
		toolNames:   append([]string(nil), defaultShellToolNames...),
		commandKeys: append([]string(nil), defaultShellCommandKeys...),
	}
	if config == nil {
		return out, nil
	}
	if raw, ok := config["tool_names"]; ok {
		names, err := cfgStringList(raw, "tool_names")
		if err != nil {
			return shellConfig{}, err
		}
		out.toolNames = lowerCopy(names)
	}
	if raw, ok := config["command_keys"]; ok {
		keys, err := cfgStringList(raw, "command_keys")
		if err != nil {
			return shellConfig{}, err
		}
		out.commandKeys = lowerCopy(keys)
	}
	return out, nil
}

func cfgStringList(raw any, field string) ([]string, error) {
	switch typed := raw.(type) {
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return nil, fmt.Errorf("%s has no value", field)
		}
		return []string{trimmed}, nil
	case []any:
		out := make([]string, 0, len(typed))
		for _, v := range typed {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("%s entries must be strings", field)
			}
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("%s has no value", field)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s must be a string or a list of strings", field)
	}
}

func lowerCopy(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	return out
}

// matchesTool reports whether a tool name is one of the shell tools, comparing
// case-insensitively: hosts disagree on capitalization ("Bash" vs "bash") and a
// rule must not depend on which host produced the call.
func (c shellConfig) matchesTool(name string) bool {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return false
	}
	for _, candidate := range c.toolNames {
		if strings.EqualFold(trimmed, candidate) {
			return true
		}
	}
	return false
}

// shellCommands projects the shell commands of the tool call under evaluation,
// in the order they appear: per call the command, its unquoted form, and each
// simple command in it (shellSubjects). It reads in.Output only: a
// verdict must not depend on a command from earlier in the session, which the
// prompt history in in.Messages carries.
//
// An input with no shell tool call in it projects nothing, and the caller skips
// the evaluator rather than judging it against no text.
func shellCommands(in agento11y.HookInput, cfg shellConfig) []string {
	return appendShellCommands(nil, in.Output, cfg)
}

func appendShellCommands(out []string, messages []agento11y.Message, cfg shellConfig) []string {
	for _, m := range messages {
		for i := range m.Parts {
			tc := m.Parts[i].ToolCall
			if tc == nil || !cfg.matchesTool(tc.Name) {
				continue
			}
			commands, plain := shellCommandsOf(tc, cfg)
			for _, command := range commands {
				// The command as written, unquoted, and split into the
				// commands the shell runs. Grafana Cloud's shell_patterns
				// judge the same subjects.
				out = append(out, shellSubjects(command)...)
			}
			out = append(out, plain...)
		}
	}
	return out
}

// shellCommandsOf projects one shell tool call: the value of every command key
// it carries, as a string or an argv list. plain holds each argv list joined
// with spaces as sent, which earlier versions judged, so a pattern written
// against that form still matches; it is judged as written only.
//
// A call with no readable command is not cleared by having none: the string
// values in its arguments are read as its commands instead, and arguments
// that are not JSON as text, so cat .env in an unknown key is still judged.
func shellCommandsOf(tc *agento11y.ToolCall, cfg shellConfig) (commands, plain []string) {
	var args map[string]any
	if err := json.Unmarshal(tc.InputJSON, &args); err != nil {
		text := strings.Map(func(r rune) rune {
			if strings.ContainsRune(`{}[]":,`, r) {
				return ' '
			}
			return r
		}, string(tc.InputJSON))
		return []string{text}, nil
	}
	for _, key := range cfg.commandKeys {
		raw, ok := lookupArg(args, key)
		if !ok {
			continue
		}
		if command, ok := commandString(raw); ok {
			commands = append(commands, command)
			if argv, isList := raw.([]any); isList {
				if joined := joinArgv(argv); joined != command {
					plain = append(plain, joined)
				}
			}
		}
	}
	// Codex's local_shell can carry the command one level down, as
	// {"action":{"type":"exec","command":[...]}}. A top-level command does not
	// hide a nested one, since a runtime may run either.
	for _, k := range slices.Sorted(maps.Keys(args)) {
		commands = nestedCommands(commands, args[k], cfg)
	}
	if len(commands) == 0 {
		commands = stringValues(nil, args)
	}
	return commands, plain
}

// nestedCommands appends the commands under command keys of the objects in
// value, at any depth, read as the top level reads them.
func nestedCommands(out []string, value any, cfg shellConfig) []string {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			out = nestedCommands(out, item, cfg)
		}
	case map[string]any:
		for _, key := range cfg.commandKeys {
			if raw, ok := lookupArg(v, key); ok {
				if command, ok := commandString(raw); ok {
					out = append(out, command)
				}
			}
		}
		for _, k := range slices.Sorted(maps.Keys(v)) {
			out = nestedCommands(out, v[k], cfg)
		}
	}
	return out
}

// stringValues appends every non-empty string in value, at any depth, in
// sorted key order so a verdict does not depend on map order.
func stringValues(out []string, value any) []string {
	switch v := value.(type) {
	case string:
		if s := strings.TrimSpace(v); s != "" {
			out = append(out, s)
		}
	case []any:
		// A list of two or more strings is an argv list, one command, so its
		// words are not judged one by one.
		if command, ok := commandString(v); ok && len(v) > 1 {
			if command != "" {
				out = append(out, command)
			}
			return out
		}
		for _, item := range v {
			out = stringValues(out, item)
		}
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			out = stringValues(out, v[k])
		}
	}
	return out
}

func joinArgv(list []any) string {
	words := make([]string, 0, len(list))
	for _, item := range list {
		s, _ := item.(string)
		words = append(words, s)
	}
	return strings.Join(words, " ")
}

// shellCommandTooLong reports whether a shell call in in.Output carries a
// command longer than maxShellCommandBytes, which a rule fails closed.
func shellCommandTooLong(in agento11y.HookInput, cfg shellConfig) bool {
	for _, m := range in.Output {
		for i := range m.Parts {
			if tc := m.Parts[i].ToolCall; tc != nil && cfg.matchesTool(tc.Name) {
				commands, _ := shellCommandsOf(tc, cfg)
				if slices.ContainsFunc(commands, func(c string) bool { return len(c) > maxShellCommandBytes }) {
					return true
				}
			}
		}
	}
	return false
}

// lookupArg finds an argument by key, exact match first and then
// case-insensitively. The fallback scans in sorted order so a payload carrying
// two keys that differ only in case resolves the same way on every run.
func lookupArg(args map[string]any, key string) (any, bool) {
	if v, ok := args[key]; ok {
		return v, true
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.EqualFold(k, key) {
			return args[k], true
		}
	}
	return nil, false
}

// commandString reads a command value: a string as sent, or an argv list
// joined with each argument quoted the way the shell would read it, so
// ["bash", "-lc", "git reset --hard"] keeps bash's script as one word.
func commandString(raw any) (string, bool) {
	switch typed := raw.(type) {
	case string:
		if trimmed := strings.TrimSpace(joinLineContinuations(typed)); trimmed != "" {
			return trimmed, true
		}
	case []any:
		if len(typed) == 0 {
			return "", false
		}
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			s, ok := item.(string)
			if !ok {
				return "", false
			}
			parts = append(parts, s)
		}
		if joined := strings.TrimSpace(joinLineContinuations(shellJoin(parts))); joined != "" {
			return joined, true
		}
	}
	return "", false
}

// joinLineContinuations removes the backslash-newlines the shell removes
// before it runs a command, so `git reset \<newline>--hard` is judged as the
// `git reset --hard` it runs. Grafana Cloud's shell_patterns do the same.
func joinLineContinuations(command string) string {
	return strings.NewReplacer("\\\r\n", "", "\\\n", "").Replace(command)
}

// shellSubjects returns what a pattern judges for one shell command: the
// command as written, its unquoted form, and each simple command in it, split
// where the shell splits commands, both as written and from its name on. A
// simple command keeps quoted text and quoted heredoc bodies as escaped data,
// so a commit message that mentions git reset --hard is not a command, while
// the code in sh -c, eval, trap, watch, env -S, a heredoc fed to a shell,
// find -exec, $(...) and backticks is, including those in an unquoted heredoc.
//
// A command longer than maxShellCommandBytes is not read at all: it is judged
// as written, and both engines fail it closed.
func shellSubjects(command string) []string {
	if len(command) > maxShellCommandBytes {
		return []string{command}
	}
	full, commands := lexShell(command, 0)
	return uniqueStrings(append([]string{command, full}, commands...))
}

// maxShellCommandBytes bounds the shell command lexShell reads, far above what
// an agent writes, so a crafted command cannot make the guard slow.
const maxShellCommandBytes = 64 << 10

// maxShellDepth bounds how deep lexShell decodes code inside code, such as
// bash -c 'eval "..."'. Code found at the last level is judged as written.
const maxShellDepth = 3

// shellSpecial are the characters the shell gives a meaning to unquoted; a
// quoted one is written escaped so it stays part of its word.
const shellSpecial = " \t\r\n;&|()<>#'\"\\$`~*?[]{}"

// lexShell returns the unquoted form of command and its simple commands, with
// those of the code it carries, decoded up to maxShellDepth levels deep. The
// unquoted form writes quoted text as the backslash-escaped words the shell
// reads: "reset" and $'\x72eset' become reset, and "a -f" becomes a\ -f, still
// one word. It expands nothing, drops comments, writes heredoc bodies escaped,
// and keeps a quote that never closes as a literal character.
func lexShell(command string, depth int) (string, []string) {
	l := &shellLexer{cmd: command}
	l.run()
	l.endCommand()
	commands := slices.Clone(l.commands)
	for _, c := range l.commands {
		commands = append(commands, l.decode(c))
	}
	for _, code := range l.code {
		if depth+1 < maxShellDepth {
			_, inner := lexShell(code, depth+1)
			commands = append(commands, inner...)
		} else {
			commands = append(commands, code)
		}
	}
	return l.full.String(), uniqueStrings(commands)
}

func uniqueStrings(list []string) []string {
	seen := make(map[string]bool, len(list))
	out := list[:0]
	for _, s := range list {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

type shellLexer struct {
	cmd       string
	full, cur strings.Builder
	commands  []string
	// code holds text the shell runs as commands: the bodies of $(...) and
	// backticks in double quotes and unquoted heredocs, of sh -c, eval and
	// trap, and of a heredoc fed to a shell.
	code     []string
	heredocs []heredoc
	// marked counts the heredocs whose simple command has ended, so whether a
	// shell reads them is known.
	marked int
	// arith counts the (( and $(( arithmetic open here, where << is a shift.
	arith int
}

func (l *shellLexer) put(c byte) {
	l.full.WriteByte(c)
	l.cur.WriteByte(c)
}

func (l *shellLexer) putString(s string) {
	l.full.WriteString(s)
	l.cur.WriteString(s)
}

// putQuoted writes a quoted character, escaped when it is special, so a quoted
// space does not split the word and a quoted ~ or * stays literal, unless the
// quote leaves it live.
func (l *shellLexer) putQuoted(c byte, live string) {
	if strings.IndexByte(shellSpecial, c) >= 0 && strings.IndexByte(live, c) < 0 {
		l.put('\\')
	}
	l.put(c)
}

// split writes a separator and ends the simple command before it.
func (l *shellLexer) split(c byte) {
	l.full.WriteByte(c)
	l.endCommand()
}

// endCommand ends the simple command being read. The heredocs opened in it
// learn whether a shell reads them, as in bash <<EOF, once per command.
func (l *shellLexer) endCommand() {
	s := strings.TrimSpace(l.cur.String())
	if len(l.heredocs) > l.marked {
		code := runsShell(dropRedirections(shellWords(s)))
		for k := l.marked; k < len(l.heredocs); k++ {
			l.heredocs[k].code = code
		}
		l.marked = len(l.heredocs)
	}
	if s != "" {
		l.commands = append(l.commands, s)
	}
	l.cur.Reset()
}

func (l *shellLexer) run() {
	cmd := l.cmd
	for i := 0; i < len(cmd); i++ {
		if last, ok := l.quote(i); ok {
			i = last
			continue
		}
		c := cmd[i]
		switch {
		case c == '#' && (i == 0 || strings.IndexByte(" \t\r\n;&|(", cmd[i-1]) >= 0):
			// A comment runs to the end of the line, and the shell runs
			// none of it. After a ), as in $(pwd)#, a # is part of the word.
			end := strings.IndexByte(cmd[i:], '\n')
			if end < 0 {
				return
			}
			i += end - 1
		case c == '(' || c == ')' || c == '<':
			i = l.operator(i)
		case c == '\n' && len(l.heredocs) > 0:
			// The bodies of the heredocs opened on this line follow it.
			l.split(c)
			i = l.heredocBodies(i+1) - 1
		case c == '`' || c == '$' && strings.HasPrefix(cmd[i:], "$(") && !strings.HasPrefix(cmd[i:], "$(("):
			i = l.substitution(i)
		case c == ';' || c == '|' || c == '\n' || c == '&' && !redirectAmpersand(cmd, i):
			l.split(c)
		case (c == '{' || c == '}') && wholeWord(cmd, i):
			l.split(c)
		default:
			l.put(c)
		}
	}
}

// quote writes the escape or quote that starts at i, if one does, and returns
// the index of its last character.
func (l *shellLexer) quote(i int) (int, bool) {
	cmd := l.cmd
	switch c := cmd[i]; {
	case c == '\\':
		if i+1 == len(cmd) {
			l.put(c)
			return i, true
		}
		l.putQuoted(cmd[i+1], "")
		return i + 1, true
	case c == '$' && i+1 < len(cmd) && cmd[i+1] == '"':
		// $"..." quotes like "...".
		return l.doubleQuoted(i + 1), true
	case c == '$' && i+1 < len(cmd) && cmd[i+1] == '\'':
		end := quoteEnd(cmd, i+2, '\'')
		if end < 0 {
			l.put(c)
			return i, true
		}
		l.ansiC(i+2, end)
		return end, true
	case c == '\'':
		return l.singleQuoted(i, strings.IndexByte(cmd[i+1:], '\'')+i+1), true
	case c == '"':
		return l.doubleQuoted(i), true
	}
	return i, false
}

// ansiC writes the text of a $'...' quote, from i to the closing quote at end,
// with its backslash escapes decoded as the shell decodes them, so
// $'\x72eset' is reset.
func (l *shellLexer) ansiC(i, end int) {
	for ; i < end; i++ {
		c := l.cmd[i]
		if c != '\\' || i+1 == end {
			l.putQuoted(c, "")
			continue
		}
		i++
		switch e := l.cmd[i]; {
		case e == 'x' || e >= '0' && e <= '7':
			start, base, width := i, 8, 3
			if e == 'x' {
				start, base, width = i+1, 16, 2
			}
			stop := start
			for stop < end && stop-start < width && digitValue(l.cmd[stop]) < base {
				stop++
			}
			if stop == start {
				// \x with no digits stays as written.
				l.putQuoted('\\', "")
				c = e
				break
			}
			v, _ := strconv.ParseUint(l.cmd[start:stop], base, 16)
			c, i = byte(v), stop-1
		case e == 'u' || e == 'U':
			width := 4
			if e == 'U' {
				width = 8
			}
			stop := i + 1
			for stop < end && stop-i-1 < width && digitValue(l.cmd[stop]) < 16 {
				stop++
			}
			if stop == i+1 {
				l.putQuoted('\\', "")
				c = e
				break
			}
			v, _ := strconv.ParseUint(l.cmd[i+1:stop], 16, 32)
			r := []byte(string(rune(v)))
			for _, b := range r[:len(r)-1] {
				l.putQuoted(b, "")
			}
			c, i = r[len(r)-1], stop-1
		case e == 'c' && i+1 < end:
			i++
			c = l.cmd[i] & 0x1f
		case strings.IndexByte("nrtvabfeE", e) >= 0:
			c = "\n\r\t\v\a\b\f\x1b\x1b"[strings.IndexByte("nrtvabfeE", e)]
		case strings.IndexByte(`\'"?`, e) >= 0:
			c = e
		default:
			l.putQuoted('\\', "")
			c = e
		}
		l.putQuoted(c, "")
	}
}

// digitValue is the value of a hex digit, or 16 for any other character.
func digitValue(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return 16
}

// substitution reads the $(...) or backtick command at i, which runs on its
// own and leaves its output in the word around it: it is kept as code for
// lexShell to split, and $ stands for it in the word. The unquoted form keeps
// it as written. It returns the index of its last character.
func (l *shellLexer) substitution(i int) int {
	end, inner := len(l.cmd), ""
	if l.cmd[i] == '`' {
		if closing := quoteEnd(l.cmd, i+1, '`'); closing >= 0 {
			end = closing
		}
		inner = l.cmd[i+1 : end]
	} else {
		end = substitutionEnd(l.cmd, i+2)
		inner = l.cmd[i+2 : end]
	}
	if end == len(l.cmd) {
		end = len(l.cmd) - 1
	}
	l.code = append(l.code, inner)
	l.full.WriteString(l.cmd[i : end+1])
	l.cur.WriteByte('$')
	return end
}

// singleQuoted writes the '...' that opens at i and closes at end, and returns
// end. A quote that never closes (end before i) is a literal character.
func (l *shellLexer) singleQuoted(i, end int) int {
	if end <= i {
		l.putQuoted(l.cmd[i], "")
		return i
	}
	for j := i + 1; j < end; j++ {
		l.putQuoted(l.cmd[j], "")
	}
	return end
}

// doubleQuoted writes the "..." that opens at i and returns the index of its
// closing quote. A backslash escapes only ", \, $ and a backtick, $ stays live,
// and the $(...) and backticks in it are code, kept for lexShell to split.
func (l *shellLexer) doubleQuoted(i int) int {
	cmd := l.cmd
	end := doubleQuoteEnd(cmd, i+1)
	if end < 0 {
		l.putQuoted(cmd[i], "")
		return i
	}
	for j := i + 1; j < end; j++ {
		switch c := cmd[j]; {
		case c == '\\' && strings.IndexByte("\"\\$`", cmd[j+1]) >= 0:
			j++
			l.putQuoted(cmd[j], "")
		case c == '$' && cmd[j+1] == '(':
			closing := substitutionEnd(cmd, j+2)
			l.code = append(l.code, cmd[j+2:closing])
			l.put('$')
			j = closing
		case c == '`':
			closing := quoteEnd(cmd, j+1, '`')
			l.code = append(l.code, cmd[j+1:closing])
			l.put('$')
			j = closing
		default:
			l.putQuoted(c, "$")
		}
	}
	return end
}

// operator writes the (, ) or < at i with the operator it starts: the (( and ))
// of arithmetic, where << is a shift, a <<< here-string, a heredoc's << and
// delimiter, or a lone ( or ), which ends a simple command. It returns the
// index of the last character it wrote.
func (l *shellLexer) operator(i int) int {
	rest := l.cmd[i:]
	switch {
	case strings.HasPrefix(rest, "(("):
		l.arith++
	case l.arith > 0 && strings.HasPrefix(rest, "))"):
		l.arith--
	case strings.HasPrefix(rest, "<<<"):
		l.putString("<<<")
		return i + 2
	case l.arith == 0 && strings.HasPrefix(rest, "<<"):
		if h, next, ok := parseHeredoc(l.cmd, i); ok {
			l.heredocs = append(l.heredocs, h)
			l.putString(l.cmd[i:next])
			return next - 1
		}
		l.put('<')
		return i
	case rest[0] == '<':
		l.put('<')
		return i
	default:
		l.split(rest[0])
		return i
	}
	l.putString(rest[:2])
	return i + 1
}

type heredoc struct {
	delimiter string
	stripTabs bool
	// quoted is set when the delimiter is quoted, as in <<'EOF', which keeps
	// the shell from expanding the body.
	quoted bool
	// code is set for a heredoc fed to a shell, as in bash <<EOF, whose
	// body the shell runs.
	code bool
}

// heredocBodies reads the bodies of the heredocs opened on the line before
// pos, through each one's delimiter line, and returns the index after them.
// A body is data: it is written escaped to the unquoted form and is no simple
// command. A shell that reads it runs it, so it is kept as code, and the
// $(...) and backticks in an unquoted body run as the shell expands it.
func (l *shellLexer) heredocBodies(pos int) int {
	for _, h := range l.heredocs {
		start, end := pos, len(l.cmd)
		for pos < len(l.cmd) {
			line := l.cmd[pos:]
			if n := strings.IndexByte(line, '\n'); n >= 0 {
				line = line[:n+1]
			}
			pos += len(line)
			body := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			if h.stripTabs {
				body = strings.TrimLeft(body, "\t")
			}
			if body == h.delimiter {
				end = pos - len(line)
				break
			}
		}
		switch {
		case h.code:
			l.code = append(l.code, l.cmd[start:end])
		case !h.quoted:
			l.code = append(l.code, bodySubstitutions(l.cmd[start:end])...)
		}
		for j := start; j < pos; j++ {
			if c := l.cmd[j]; j < pos-1 || c != '\n' {
				if strings.IndexByte(shellSpecial, c) >= 0 {
					l.full.WriteByte('\\')
				}
				l.full.WriteByte(c)
			}
		}
		l.full.WriteByte('\n')
	}
	l.heredocs, l.marked = nil, 0
	return pos
}

// bodySubstitutions returns the commands of the $(...) and backticks in an
// unquoted heredoc body, which the shell runs as it expands the body. A
// backslash keeps the character after it literal.
func bodySubstitutions(body string) []string {
	var code []string
	for i := 0; i < len(body); i++ {
		switch {
		case body[i] == '\\':
			i++
		case strings.HasPrefix(body[i:], "$(") && !strings.HasPrefix(body[i:], "$(("):
			end := substitutionEnd(body, i+2)
			code = append(code, body[i+2:end])
			i = end
		case body[i] == '`':
			end := quoteEnd(body, i+1, '`')
			if end < 0 {
				return code
			}
			code = append(code, body[i+1:end])
			i = end
		}
	}
	return code
}

// decode keeps the code a simple command runs itself for lexShell to split:
// the script of sh -c, su -c, env -S and script -c, the text of eval, watch
// and trap, and the commands after find -exec. It returns the command from its
// name on, without assignments, wrappers and redirections, so a pattern sees
// sudo -u deploy rm -rf / and 2>&1 git reset --hard as the command they run.
func (l *shellLexer) decode(command string) string {
	words := dropRedirections(shellWords(command))
	i, scripts := commandIndex(words)
	l.code = append(l.code, scripts...)
	if i == len(words) {
		return ""
	}
	args := words[i+1:]
	switch name := commandName(words[i]); {
	case name == "eval":
		l.code = append(l.code, unescapeWords(args))
	case name == "trap":
		if len(args) > 0 {
			l.code = append(l.code, unescapeWords(args[:1]))
		}
	case name == "watch":
		// watch runs its arguments with sh -c, after options such as -n 5.
		j := 0
		for ; j < len(args) && strings.HasPrefix(args[j], "-"); j++ {
			if args[j] == "-n" || args[j] == "-q" {
				j++
			}
		}
		if j < len(args) {
			l.code = append(l.code, unescapeWords(args[j:]))
		}
	case name == "find":
		l.code = append(l.code, findExecCommands(args)...)
	case name == "su":
		for j, w := range args {
			if script, ok := strings.CutPrefix(w, "--command="); ok {
				l.code = append(l.code, unescapeWords([]string{script}))
				break
			}
			if (w == "-c" || w == "--command") && j+1 < len(args) {
				l.code = append(l.code, unescapeWords(args[j+1:j+2]))
				break
			}
		}
	case shellNames[name]:
		if script, _ := shellScript(args); script != "" {
			l.code = append(l.code, script)
		}
	}
	return strings.Join(words[i:], " ")
}

var shellNames = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true}

// shellScript reads a shell's arguments: it returns the script of its -c
// option, and whether, with no -c and no script file, it reads its commands
// from stdin. Options before -c, with the values of -o and -O, are skipped,
// as in bash -o pipefail -c '...' and bash --norc -c -- '...'.
func shellScript(args []string) (script string, stdin bool) {
	c, s := false, false
	for j := 0; j < len(args); j++ {
		switch w := args[j]; {
		case w == "--":
			if c && j+1 < len(args) {
				return unescapeWords(args[j+1 : j+2]), false
			}
			return "", !c && (s || j+1 == len(args))
		case w == "-o" || w == "+o" || w == "-O" || w == "+O" || w == "--rcfile" || w == "--init-file":
			j++
		case strings.HasPrefix(w, "--"):
		case len(w) > 1 && (w[0] == '-' || w[0] == '+'):
			c = c || cFlag(w)
			s = s || w[0] == '-' && strings.Contains(w, "s")
			if strings.IndexByte("oO", w[len(w)-1]) >= 0 {
				j++
			}
		case c:
			return unescapeWords([]string{w}), false
		default:
			// A script file, which the shell runs instead of stdin, unless -s
			// makes the words after it arguments.
			return "", s
		}
	}
	return "", !c
}

// shellWrapper is a command that runs the command after it, as sudo does.
type shellWrapper struct {
	// values are its short options that take a value, as sudo -u USER.
	values string
	// scripts are those of values whose value is a command, as env -S.
	scripts string
	// long are its long options that take a value, as sudo --user USER, and
	// longScripts those whose value is a command, as env --split-string.
	long, longScripts []string
	// positional counts the words it reads before the command, as the lock
	// file of flock FILE COMMAND.
	positional int
}

var shellCommandWrappers = map[string]shellWrapper{
	"sudo":    {values: "ughCDprtUT", long: strings.Fields("user group host close-from chdir prompt role type other-user command-timeout chroot")},
	"doas":    {values: "uC"},
	"env":     {values: "uCS", scripts: "S", long: strings.Fields("unset chdir"), longScripts: []string{"split-string"}},
	"command": {}, "builtin": {}, "exec": {values: "a"}, "nohup": {},
	"nice":    {values: "n", long: []string{"adjustment"}},
	"timeout": {values: "sk", positional: 1, long: strings.Fields("signal kill-after")},
	"stdbuf":  {values: "ioe", long: strings.Fields("input output error")},
	// --replace, --max-lines and --eof take an optional value, which only
	// =VALUE gives them, so the word after them is the command.
	"xargs":  {values: "InPLsdEa", long: strings.Fields("max-args max-procs max-chars delimiter arg-file process-slot-var")},
	"time":   {values: "fo", long: strings.Fields("format output")},
	"setsid": {}, "caffeinate": {values: "tw"}, "chronic": {}, "unbuffer": {},
	"flock":  {values: "wE", positional: 1, long: strings.Fields("wait timeout conflict-exit-code")},
	"ionice": {values: "cnp", long: strings.Fields("class classdata pid")},
	"script": {values: "Ftc", scripts: "c", positional: 1, long: strings.Fields("log-timing timing log-in log-out log-io logging-format"), longScripts: []string{"command"}},
}

var shellKeywords = map[string]bool{
	"!": true, "if": true, "then": true, "else": true, "elif": true, "do": true, "while": true, "until": true, "coproc": true,
}

// commandIndex returns the index of a simple command's name: the first word
// that is no assignment or keyword, nor a wrapper with its options, their
// values and its positional words. It also returns the code in wrapper
// options that take one, as env -S.
func commandIndex(words []string) (int, []string) {
	var scripts []string
	for i := 0; i < len(words); i++ {
		if shellKeywords[words[i]] || shellAssignment(words[i]) {
			continue
		}
		wrapper, ok := shellCommandWrappers[commandName(words[i])]
		if !ok {
			return i, scripts
		}
		for i++; i < len(words); i++ {
			opt := words[i]
			if opt == "--" {
				i++
				break
			}
			if len(opt) < 2 || opt[0] != '-' {
				break
			}
			if strings.HasPrefix(opt, "--") {
				i, scripts = longWrapperOption(wrapper, words, i, scripts)
				continue
			}
			for k := 1; k < len(opt); k++ {
				if strings.IndexByte(wrapper.values, opt[k]) < 0 {
					continue
				}
				value := opt[k+1:]
				if value == "" && i+1 < len(words) {
					i++
					value = words[i]
				}
				if strings.IndexByte(wrapper.scripts, opt[k]) >= 0 {
					scripts = append(scripts, unescapeWords([]string{value}))
				}
				break
			}
		}
		for i < len(words) && shellAssignment(words[i]) {
			i++
		}
		i += wrapper.positional - 1
	}
	return len(words), scripts
}

// longWrapperOption reads the long option words[i] of a wrapper, and the
// value after it when it takes one, as sudo --user deploy does. git accepts a
// unique prefix of a long option, and so do these tools, so --us is --user.
// It returns the index of the option's last word.
func longWrapperOption(w shellWrapper, words []string, i int, scripts []string) (int, []string) {
	name, value, inline := strings.Cut(words[i][2:], "=")
	prefixOf := func(l string) bool { return name != "" && strings.HasPrefix(l, name) }
	script := slices.ContainsFunc(w.longScripts, prefixOf)
	if !script && !slices.ContainsFunc(w.long, prefixOf) {
		return i, scripts
	}
	if !inline && i+1 < len(words) {
		i++
		value = words[i]
	}
	if script {
		scripts = append(scripts, unescapeWords([]string{value}))
	}
	return i, scripts
}

// runsShell reports whether a simple command runs a shell that reads its
// commands from stdin, as bash <<EOF does.
func runsShell(words []string) bool {
	i, _ := commandIndex(words)
	if i == len(words) || !shellNames[commandName(words[i])] {
		return false
	}
	_, stdin := shellScript(words[i+1:])
	return stdin
}

// cFlag reports whether a shell option cluster carries -c, as in -lc.
func cFlag(w string) bool {
	return strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") && strings.Contains(w, "c")
}

// findExecCommands returns the commands after find -exec and its kin, up to
// the ; or + that ends each, in the escaped form lexShell reads again.
func findExecCommands(words []string) []string {
	var out []string
	for i := 0; i < len(words); i++ {
		switch words[i] {
		case "-exec", "-execdir", "-ok", "-okdir":
			j := i + 1
			for j < len(words) && words[j] != `\;` && words[j] != "+" {
				j++
			}
			if j > i+1 {
				out = append(out, strings.Join(words[i+1:j], " "))
			}
			i = j
		}
	}
	return out
}

func shellAssignment(word string) bool {
	eq := strings.IndexByte(word, '=')
	return eq > 0 && isShellName(word[:eq])
}

// isShellName reports whether s is a shell variable name.
func isShellName(s string) bool {
	if s == "" {
		return false
	}
	for k := range len(s) {
		c := s[k]
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || k > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// commandName is a command word without its directory or a leading backslash,
// so /usr/bin/git and \git are git.
func commandName(word string) string {
	return strings.TrimPrefix(word[strings.LastIndexByte(word, '/')+1:], `\`)
}

// shellWords splits a simple command, in the escaped form lexShell writes, at
// its unescaped spaces and tabs.
func shellWords(command string) []string {
	var words []string
	start := -1
	for i := 0; i < len(command); i++ {
		switch c := command[i]; {
		case c == ' ' || c == '\t':
			if start >= 0 {
				words = append(words, command[start:i])
				start = -1
			}
		case start < 0:
			start = i
			if c == '\\' {
				i++
			}
		case c == '\\':
			i++
		}
	}
	if start >= 0 {
		words = append(words, command[start:])
	}
	return words
}

// dropRedirections returns words without their redirections, such as
// >/dev/null, 2>&1, <in, and a heredoc's << with its delimiter, so
// 2>&1 git reset --hard reads as git reset --hard.
func dropRedirections(words []string) []string {
	var out []string
	for i := 0; i < len(words); i++ {
		n := redirection(words[i])
		if n == 0 {
			out = append(out, words[i])
		} else if n == len(words[i]) {
			// The target is the next word, as in > file.
			i++
		}
	}
	return out
}

// redirectionOperators are the shell's redirection operators, longest first.
var redirectionOperators = []string{"&>>", "<<<", "<<-", "&>", ">>", ">|", ">&", "<<", "<>", "<&", ">", "<"}

// redirection returns the length of the redirection operator a word starts
// with, after an optional file descriptor number, or 0 when it starts none.
func redirection(w string) int {
	i := 0
	for i < len(w) && w[i] >= '0' && w[i] <= '9' {
		i++
	}
	for _, op := range redirectionOperators {
		if strings.HasPrefix(w[i:], op) {
			return i + len(op)
		}
	}
	return 0
}

// unescapeWords turns escaped words back into the text they stand for, joined
// with spaces: the code in an sh -c or eval argument.
func unescapeWords(words []string) string {
	var b strings.Builder
	for n, w := range words {
		if n > 0 {
			b.WriteByte(' ')
		}
		for i := 0; i < len(w); i++ {
			if w[i] == '\\' && i+1 < len(w) {
				i++
			}
			b.WriteByte(w[i])
		}
	}
	return b.String()
}

// redirectAmpersand reports whether the & at i belongs to a redirection, as in
// 2>&1 and &>file, rather than ending a command.
func redirectAmpersand(command string, i int) bool {
	return i > 0 && (command[i-1] == '>' || command[i-1] == '<') || i+1 < len(command) && command[i+1] == '>'
}

// wholeWord reports whether the character at i is a word on its own, as the
// { and } of a group are, and not part of one like -I{}.
func wholeWord(command string, i int) bool {
	const edge = " \t\r\n;&|()"
	return (i == 0 || strings.IndexByte(edge, command[i-1]) >= 0) &&
		(i+1 == len(command) || strings.IndexByte(edge, command[i+1]) >= 0)
}

// parseHeredoc reads the << or <<- at i and its delimiter word, quoted or not,
// and returns the index after the word. Any word is a delimiter, as in bash,
// so <<'END-OF-SCRIPT' is a heredoc; a << inside arithmetic is a shift, which
// the callers keep from reaching here.
func parseHeredoc(command string, i int) (heredoc, int, bool) {
	j := i + 2
	h := heredoc{}
	if j < len(command) && command[j] == '-' {
		h.stripTabs = true
		j++
	}
	for j < len(command) && (command[j] == ' ' || command[j] == '\t') {
		j++
	}
	start := j
	for j < len(command) && strings.IndexByte(" \t\r\n;&|()<>", command[j]) < 0 {
		switch command[j] {
		case '\'', '"':
			if end := quoteEnd(command, j+1, command[j]); end > 0 {
				j = end
			}
		case '\\':
			j++
		}
		j++
	}
	if j > len(command) {
		j = len(command)
	}
	word := command[start:j]
	h.delimiter = strings.NewReplacer(`'`, "", `"`, "", `\`, "").Replace(word)
	h.quoted = h.delimiter != word
	if h.delimiter == "" {
		return heredoc{}, 0, false
	}
	return h, j, true
}

// quoteEnd returns the index of the quote that closes text starting at i,
// skipping backslash escapes (so \" in "..." and \' in $'...' do not close
// it), or -1 when none does.
func quoteEnd(command string, i int, quote byte) int {
	for ; i < len(command); i++ {
		switch command[i] {
		case '\\':
			i++
		case quote:
			return i
		}
	}
	return -1
}

// doubleQuoteEnd returns the index of the " that closes a double quote whose
// text starts at i, or -1. A $(...) or backtick inside it is skipped whole, so
// a quote in a nested command does not close it.
func doubleQuoteEnd(command string, i int) int {
	for ; i < len(command); i++ {
		switch {
		case command[i] == '\\':
			i++
		case command[i] == '"':
			return i
		case command[i] == '$' && i+1 < len(command) && command[i+1] == '(':
			i = substitutionEnd(command, i+2)
		case command[i] == '`':
			if i = quoteEnd(command, i+1, '`'); i < 0 {
				return -1
			}
		}
	}
	return -1
}

// substitutionEnd returns the index of the ) that closes a $( whose text starts
// at i, skipping quotes, comments, arithmetic and heredoc bodies, or
// len(command) when none does.
func substitutionEnd(command string, i int) int {
	depth, arith := 1, 0
	var heredocs []heredoc
	for ; i < len(command); i++ {
		switch c := command[i]; {
		case c == '\\':
			i++
		case c == '\'':
			if i = quoteEnd(command, i+1, '\''); i < 0 {
				return len(command)
			}
		case c == '"':
			if i = doubleQuoteEnd(command, i+1); i < 0 {
				return len(command)
			}
		case c == '#' && strings.IndexByte(" \t\n;&|(", command[i-1]) >= 0:
			for i+1 < len(command) && command[i+1] != '\n' {
				i++
			}
		case strings.HasPrefix(command[i:], "(("):
			arith++
			depth += 2
			i++
		case arith > 0 && strings.HasPrefix(command[i:], "))"):
			arith--
			if depth -= 2; depth == 0 {
				return i + 1
			}
			i++
		case arith == 0 && c == '<' && strings.HasPrefix(command[i:], "<<") && !strings.HasPrefix(command[i:], "<<<"):
			if h, next, ok := parseHeredoc(command, i); ok {
				heredocs = append(heredocs, h)
				i = next - 1
			}
		case c == '\n' && len(heredocs) > 0:
			l := &shellLexer{cmd: command, heredocs: heredocs}
			i = l.heredocBodies(i+1) - 1
			heredocs = nil
		case c == '(':
			depth++
		case c == ')':
			if depth--; depth == 0 {
				return i
			}
		}
	}
	return len(command)
}

// shellJoin joins an argv list into one command, quoting each argument the
// shell would split or expand, so ["bash", "-lc", "git reset --hard"] keeps
// bash's script as one word.
func shellJoin(args []string) string {
	words := make([]string, len(args))
	for i, a := range args {
		words[i] = a
		if strings.ContainsAny(a, shellSpecial) {
			words[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(words, " ")
}
