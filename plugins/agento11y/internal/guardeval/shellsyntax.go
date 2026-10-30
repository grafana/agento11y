package guardeval

import (
	"context"
	"path"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

const (
	shellSourceLimit = 64 << 10
	shellNodeLimit   = 8192
	shellDepthLimit  = 4
)

type shellAnalysis struct {
	invocations []shellInvocation
	incomplete  string
}

type shellInvocation struct {
	name string // Unix basename; args retains the original argv, including argv[0].
	args []shellWord
}

type shellWord struct {
	value      string
	static     bool
	rootOrHome bool
}

type shellAnalyzer struct {
	ctx    context.Context
	result shellAnalysis
	bytes  int
	nodes  int
	stop   bool
}

func analyzeShell(ctx context.Context, source string, dialect syntax.LangVariant) shellAnalysis {
	a := shellAnalyzer{ctx: ctx}
	a.parse(source, dialect, 0)
	return a.result
}

func (a *shellAnalyzer) incomplete(reason string) {
	// Use fixed messages: parser and context errors can include caller input.
	if a.result.incomplete == "" {
		a.result.incomplete = reason
	}
}

func (a *shellAnalyzer) active() bool {
	if a.ctx.Err() != nil {
		a.incomplete("shell analysis canceled")
		a.stop = true
	}
	return !a.stop
}

func (a *shellAnalyzer) parse(source string, dialect syntax.LangVariant, depth int) {
	if !a.active() {
		return
	}
	if strings.ContainsAny(source, "\r\x00") {
		a.incomplete("unsupported shell control character")
		return
	}
	if len(source) > shellSourceLimit-a.bytes {
		a.incomplete("shell source limit exceeded")
		return
	}
	a.bytes += len(source)
	if depth > shellDepthLimit {
		a.incomplete("shell nesting limit exceeded")
		return
	}
	if dialect != 0 && dialect != syntax.LangBash && dialect != syntax.LangPOSIX {
		a.incomplete("unsupported shell dialect")
		return
	}
	if !a.takeNode() { // The parser's File node is not yielded by StmtsSeq.
		return
	}
	parser := syntax.NewParser(syntax.Variant(dialect))
	// StmtsSeq can emit a final error after yield returns false (v3.14.1).
	// Calling the iterator directly avoids a range-function protocol panic.
	parser.StmtsSeq(strings.NewReader(source))(func(stmt *syntax.Stmt, err error) bool {
		if err != nil {
			a.incomplete("invalid shell syntax")
			return false
		}
		if !a.active() {
			return false
		}
		a.walk(stmt, dialect, depth)
		return a.active()
	})
}

func (a *shellAnalyzer) takeNode() bool {
	a.nodes++
	if a.nodes > shellNodeLimit {
		a.incomplete("shell node limit exceeded")
		a.stop = true
		return false
	}
	return true
}

func (a *shellAnalyzer) walk(stmt *syntax.Stmt, dialect syntax.LangVariant, depth int) {
	var stack []syntax.Node
	syntax.Walk(stmt, func(node syntax.Node) bool {
		if node == nil {
			last := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if s, ok := last.(*syntax.Stmt); ok && a.active() {
				if call, ok := s.Cmd.(*syntax.CallExpr); ok {
					a.call(call, s.Redirs, dialect, depth)
				}
			}
			return true
		}
		if !a.active() {
			return false
		}
		if !a.takeNode() {
			return false
		}
		stack = append(stack, node)
		switch n := node.(type) {
		case *syntax.SglQuoted:
			if n.Dollar {
				a.incomplete("unsupported shell quoting")
			}
		case *syntax.DblQuoted:
			if n.Dollar {
				a.incomplete("unsupported shell quoting")
			}
		case *syntax.ArithmExp, *syntax.ArithmCmd, *syntax.LetClause, *syntax.CStyleLoop:
			// Arithmetic can recursively evaluate variable contents as shell syntax.
			a.incomplete("unsupported shell arithmetic")
		case *syntax.Assign:
			if n.Index != nil {
				a.incomplete("unsupported shell arithmetic")
			}
			if n.Name != nil && shellGitExecutionEnv(n.Name.Value) {
				a.incomplete("unsupported git execution configuration")
			}
		case *syntax.ArrayElem:
			if n.Index != nil {
				a.incomplete("unsupported shell arithmetic")
			}
		case *syntax.UnaryTest:
			if n.Op == syntax.TsVarSet || n.Op == syntax.TsRefVar {
				word, ok := n.X.(*syntax.Word)
				if !ok {
					a.incomplete("unsupported shell arithmetic")
				} else if value := decodeShellWord(word, dialect); !value.static || !shellName(value.value) {
					a.incomplete("unsupported shell arithmetic")
				}
			}
		case *syntax.BinaryTest:
			switch n.Op {
			case syntax.TsEql, syntax.TsNeq, syntax.TsLeq, syntax.TsGeq, syntax.TsLss, syntax.TsGtr:
				a.incomplete("unsupported shell arithmetic")
			default:
			}
		case *syntax.DeclClause:
			a.declaration(n, dialect)
		case *syntax.ParamExp:
			if n.Index != nil {
				a.incomplete("unsupported shell arithmetic")
			}
			// Prefix enumeration does not dereference variable contents.
			if n.Excl && n.Names == 0 {
				a.incomplete("unsupported shell indirect expansion")
			}
			if n.Exp != nil && n.Exp.Op == syntax.OtherParamOps && n.Exp.Word != nil && n.Exp.Word.Lit() == "P" {
				// Prompt expansion can execute substitutions stored in variable values.
				a.incomplete("unsupported shell prompt expansion")
			}
		}
		return true
	})
}

func (a *shellAnalyzer) declaration(decl *syntax.DeclClause, dialect syntax.LangVariant) {
	options := true
	for _, arg := range decl.Args {
		if arg.Name != nil && arg.Value != nil {
			word := decodeShellWord(arg.Value, dialect)
			// Declaration builtins reparse quoted compound-array initializers.
			if strings.HasPrefix(word.value, "(") {
				a.incomplete("unsupported shell arithmetic")
			}
		}
		if !arg.Naked || arg.Name != nil {
			continue
		}
		word := decodeShellWord(arg.Value, dialect)
		if !word.static {
			a.incomplete("unsupported shell arithmetic")
			break
		}
		if options && word.value == "--" {
			options = false
			continue
		}
		if options && strings.HasPrefix(word.value, "-") {
			if strings.Contains(word.value[1:], "i") {
				a.incomplete("unsupported shell arithmetic")
			}
			if strings.Contains(word.value[1:], "n") && (decl.Variant.Value == "declare" || decl.Variant.Value == "typeset" || decl.Variant.Value == "local") {
				a.incomplete("unsupported shell indirect expansion")
			}
			continue
		}
		name, value, _ := strings.Cut(word.value, "=")
		if strings.Contains(name, "[") || strings.HasPrefix(value, "(") {
			a.incomplete("unsupported shell arithmetic")
		}
		if shellGitExecutionEnv(strings.TrimSuffix(name, "+")) {
			a.incomplete("unsupported git execution configuration")
		}
	}
}

func (a *shellAnalyzer) call(call *syntax.CallExpr, redirs []*syntax.Redirect, dialect syntax.LangVariant, depth int) {
	if len(call.Args) == 0 {
		return
	}
	args := make([]shellWord, len(call.Args))
	for i, word := range call.Args {
		if !a.active() {
			return
		}
		args[i] = decodeShellWord(word, dialect)
	}
	a.argv(args, redirs, depth)
}

func (a *shellAnalyzer) argv(args []shellWord, redirs []*syntax.Redirect, depth int) {
	for len(args) > 0 && a.active() {
		if !args[0].static || args[0].value == "" {
			a.result.invocations = append(a.result.invocations, shellInvocation{args: args})
			a.incomplete("dynamic shell executable")
			return
		}
		name := path.Base(args[0].value)
		a.result.invocations = append(a.result.invocations, shellInvocation{name: name, args: args})
		switch name {
		case "env", "sudo", "command", "exec":
			next, ok := a.shellWrapper(name, args[1:])
			if !ok {
				a.incomplete("unsupported shell wrapper")
				return
			}
			args = next
		case "bash", "sh", "dash":
			dialect := syntax.LangPOSIX
			if name == "bash" {
				dialect = syntax.LangBash
			}
			a.shellInput(args[1:], redirs, dialect, depth)
			return
		case "printf":
			if !shellPrintfData(args[1:]) {
				a.incomplete("unsupported shell printf destination")
			}
			return
		case "echo", "cat", ":", "true", "false":
			return
		case "git":
			if _, _, unknown := shellPolicyGitCommand(args[1:]); unknown {
				a.incomplete("unsupported git invocation")
			}
			return
		case "rm", "chmod", "chown", "dd", "mkfs", "wipefs", "fdisk", "parted":
			return
		case "chgrp", "cp", "mv", "mkdir", "rmdir", "touch", "ln", "unlink", "ls", "pwd", "cd", "head", "tail", "wc", "sort", "uniq", "cut", "tr", "tee", "sleep", "curl", "wget":
			for _, arg := range args[1:] {
				if !arg.static {
					a.incomplete("dynamic shell argument")
				}
			}
			return
		default:
			if strings.HasPrefix(name, "mkfs.") {
				return
			}
			a.incomplete("unknown shell executor")
			return
		}
	}
}

// Shell-spawning sudo options and env -S change how arguments are interpreted.
func (a *shellAnalyzer) shellWrapper(name string, args []shellWord) ([]shellWord, bool) {
	options := true
	for len(args) > 0 {
		if !args[0].static {
			return nil, false
		}
		v := args[0].value
		if options && v == "--" {
			args = args[1:]
			options = false
			continue
		}
		if (name == "env" || name == "sudo") && shellStaticAssignment(v) {
			key, _, _ := strings.Cut(v, "=")
			if shellGitExecutionEnv(key) {
				a.incomplete("unsupported git execution configuration")
			}
			args = args[1:]
			options = false
			continue
		}
		if !options || !strings.HasPrefix(v, "-") || v == "-" {
			break
		}
		args = args[1:]
		switch name {
		case "command":
			if v == "-v" || v == "-V" {
				return nil, true
			}
			if v != "-p" {
				return nil, false
			}
		case "env":
			if v == "-i" || v == "--ignore-environment" {
				continue
			}
			if strings.HasPrefix(v, "--unset=") || strings.HasPrefix(v, "--chdir=") {
				continue
			}
			if v == "-u" || v == "--unset" || v == "-C" || v == "--chdir" {
				if len(args) == 0 || !args[0].static {
					return nil, false
				}
				args = args[1:]
				continue
			}
			if len(v) > 2 && (v[:2] == "-u" || v[:2] == "-C") {
				continue
			}
			return nil, false
		case "exec", "sudo":
			flags, values := "cl", "a"
			if name == "sudo" {
				flags, values = "nEHSkb", "ughpCTRD"
				key, _, equals := strings.Cut(v, "=")
				switch key {
				case "--user", "--group", "--host", "--prompt", "--close-from", "--command-timeout", "--chroot", "--chdir":
					if !equals {
						if len(args) == 0 || !args[0].static {
							return nil, false
						}
						args = args[1:]
					}
					continue
				case "--non-interactive", "--preserve-env", "--set-home", "--stdin", "--background":
					if equals && key != "--preserve-env" {
						return nil, false
					}
					continue
				}
			}
			for i := 1; i < len(v); i++ {
				if strings.ContainsRune(flags, rune(v[i])) {
					continue
				}
				if !strings.ContainsRune(values, rune(v[i])) {
					return nil, false
				}
				if i == len(v)-1 {
					if len(args) == 0 || !args[0].static {
						return nil, false
					}
					args = args[1:]
				}
				break
			}
		}
	}
	return args, name != "sudo" || len(args) > 0
}

// Explicit assignments can affect later Git invocations.
func shellGitExecutionEnv(name string) bool {
	return name == "GIT_SSH_COMMAND" || name == "GIT_SSH" || strings.HasPrefix(name, "GIT_CONFIG_")
}

// printf -v evaluates array subscripts, unlike its format and data operands.
func shellPrintfData(args []shellWord) bool {
	for len(args) > 0 {
		if !args[0].static {
			return false
		}
		v := args[0].value
		args = args[1:]
		if v == "--" || !strings.HasPrefix(v, "-v") {
			return true
		}
		dest := v[2:]
		if dest == "" {
			if len(args) == 0 || !args[0].static {
				return false
			}
			dest = args[0].value
			args = args[1:]
		}
		if !shellName(dest) {
			return false
		}
	}
	return true
}

func shellStaticAssignment(s string) bool {
	name, _, ok := strings.Cut(s, "=")
	return ok && shellName(name)
}

func shellName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r != '_' && !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func (a *shellAnalyzer) shellInput(args []shellWord, redirs []*syntax.Redirect, dialect syntax.LangVariant, depth int) {
	stdin, command := false, false
	for len(args) > 0 {
		if !args[0].static {
			a.incomplete("dynamic shell script")
			return
		}
		v := args[0].value
		if !strings.HasPrefix(v, "-") && v != "+o" {
			break
		}
		args = args[1:]
		if v == "--" || v == "-" {
			break
		}
		if dialect == syntax.LangBash && (v == "--noprofile" || v == "--norc" || v == "--posix") {
			continue
		}
		if v == "-o" || v == "+o" {
			if len(args) == 0 || !args[0].static || (args[0].value != "pipefail" && args[0].value != "errexit" && args[0].value != "nounset") {
				a.incomplete("unsupported shell option")
				return
			}
			args = args[1:]
			continue
		}
		flags := "aefhkmnptuvxlC"
		if dialect == syntax.LangBash {
			flags += "BEHP"
		}
		for _, flag := range v[1:] {
			switch flag {
			case 'c':
				command = true
			case 's':
				stdin = true
			default:
				if !strings.ContainsRune(flags, flag) {
					a.incomplete("unsupported shell option")
					return
				}
			}
		}
	}
	if command {
		if len(args) == 0 || !args[0].static {
			a.incomplete("dynamic shell script")
			return
		}
		a.parse(args[0].value, dialect, depth+1)
		return
	}
	if len(args) > 0 && !stdin {
		a.incomplete("shell script file is not analyzed")
		return
	}
	var input *syntax.Redirect
	for _, r := range redirs {
		if r.N != nil {
			if r.N.Value != "" && strings.TrimLeft(r.N.Value, "0") == "" {
				input = r
			}
			continue
		}
		switch r.Op {
		case syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc, syntax.RdrIn, syntax.RdrInOut, syntax.DplIn:
			input = r
		default:
			continue
		}
	}
	if input == nil {
		a.incomplete("shell input is not literal")
		return
	}
	var body string
	var ok bool
	switch input.Op {
	case syntax.Hdoc, syntax.DashHdoc:
		body, ok = literalShellHeredoc(input)
	case syntax.WordHdoc:
		word := decodeShellWord(input.Word, dialect)
		body, ok = word.value+"\n", word.static
	default:
		ok = false
	}
	if !ok {
		a.incomplete("shell input is not literal")
		return
	}
	a.parse(body, dialect, depth+1)
}

func literalShellHeredoc(r *syntax.Redirect) (string, bool) {
	quoted := false
	for _, part := range r.Word.Parts {
		lit, ok := part.(*syntax.Lit)
		if !ok || strings.Contains(lit.Value, "\\") {
			quoted = true
		}
	}
	if r.Hdoc == nil {
		return "", true
	}
	var b strings.Builder
	for _, part := range r.Hdoc.Parts {
		lit, ok := part.(*syntax.Lit)
		if !ok {
			return "", false
		}
		if quoted {
			b.WriteString(lit.Value)
		} else {
			b.WriteString(shellUnescape(lit.Value, 'h'))
		}
	}
	return b.String(), true
}

func decodeShellWord(word *syntax.Word, dialect syntax.LangVariant) shellWord {
	var value, pattern strings.Builder
	static, unknown := true, false
	var parts func([]syntax.WordPart, bool)
	parts = func(list []syntax.WordPart, quoted bool) {
		for _, part := range list {
			switch p := part.(type) {
			case *syntax.Lit:
				mode := byte('u')
				if quoted {
					mode = 'd'
				}
				v := shellUnescape(p.Value, mode)
				value.WriteString(v)
				expanding := !quoted && shellExpandingLiteral(p.Value, dialect)
				if expanding {
					static = false
					pattern.WriteString(v)
				} else {
					pattern.WriteString(strings.ReplaceAll(v, "*", "\x00literal-star"))
				}
			case *syntax.SglQuoted:
				if p.Dollar {
					unknown = true
				}
				value.WriteString(p.Value)
				pattern.WriteString(strings.ReplaceAll(p.Value, "*", "\x00literal-star"))
			case *syntax.DblQuoted:
				if p.Dollar {
					unknown = true
				}
				parts(p.Parts, true)
			case *syntax.ParamExp:
				static = false
				if simpleShellHome(p) {
					// NUL is rejected before parsing, so this marker cannot be literal input.
					pattern.WriteByte(0)
				} else {
					unknown = true
				}
			default:
				unknown = true
			}
		}
	}
	parts(word.Parts, false)
	v, p := value.String(), pattern.String()
	tilde, tildeRoot := false, false
	if len(word.Parts) > 0 {
		if lit, ok := word.Parts[0].(*syntax.Lit); ok {
			tilde = strings.HasPrefix(lit.Value, "~")
			tildeRoot = (lit.Value == "~" && len(word.Parts) == 1) || strings.HasPrefix(lit.Value, "~/")
			if tilde {
				static = false
			}
		}
	}
	root := !unknown && (p == "/" || p == "/." || p == "/*" || p == "\x00" || p == "\x00/" || p == "\x00/." || p == "\x00/*" || (tildeRoot && (p == "~" || p == "~/" || p == "~/." || p == "~/*")))
	if unknown {
		v = ""
	}
	return shellWord{value: v, static: static && !unknown, rootOrHome: root}
}

func simpleShellHome(p *syntax.ParamExp) bool {
	return p.Param != nil && p.Param.Value == "HOME" && p.Flags == nil && !p.Excl && !p.Length && !p.Width && !p.IsSet && p.Split == syntax.OptUnset && p.GlobSubst == syntax.OptUnset && p.RcExpand == syntax.OptUnset && p.NestedParam == nil && p.Index == nil && len(p.Modifiers) == 0 && p.Slice == nil && p.Repl == nil && p.Names == 0 && p.Exp == nil
}

func shellExpandingLiteral(s string, dialect syntax.LangVariant) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if strings.ContainsRune("*?[", rune(s[i])) || (dialect != syntax.LangPOSIX && strings.ContainsRune("{}", rune(s[i]))) {
			return true
		}
	}
	return false
}

func shellUnescape(s string, mode byte) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			next := s[i+1]
			if next == '\n' {
				i++
				continue
			}
			if mode == 'u' || strings.ContainsRune("$`\\", rune(next)) || (mode == 'd' && next == '"') {
				i++
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
