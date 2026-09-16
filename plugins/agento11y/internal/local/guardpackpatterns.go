package local

// These fragments match literal arguments, not shell evaluation. Substitutions
// allow one nested $() without treating its flags as outer-command options.
const (
	shellSubstitution = `(?:\$\((?:[^()]|\$\([^()]*\))*\)|\x60(?:\\.|[^\\\x60])*\x60)`
	shellBarePart     = `(?:` + shellSubstitution + `|[^\s;&|()<>\x60#"'\\]|\\[^\r\n])`
	shellQuotedWord   = `(?:'[^'\r\n]*'|"(?:\\[^\r\n]|[^"\\\r\n])*")`
	shellWordPart     = `(?:` + shellBarePart + `|` + shellQuotedWord + `)`
	shellWord         = shellWordPart + `+`

	// Redirection targets are not command flags.
	// File descriptor numbers need a preceding space; otherwise they belong to the previous word.
	// Trailing whitespace prevents a redirection target from becoming an argument.
	shellRedirection = `(?:>>?|>\||<<<|<<-?|<>|<|[<>]&|&>>?)[ \t]*` + shellWord
	shellWordGap     = `(?:(?:[ \t]+[0-9]*)?` + shellRedirection + `)*[ \t]+`
	shellArgumentGap = shellWordGap + `(?:` + shellWord + shellWordGap + `)*`

	// A final quote can close an enclosing shell command, but an adjoining quoted
	// suffix belongs to the argument. For example, --force"-with-lease" is not --force.
	shellArgumentEnd = `(?:''|"")*(?:[[:space:];&|()<>]|["']?$)`

	// A standalone -- ends option recognition, including its quoted spellings.
	shellNonDashPart = `(?:` + shellSubstitution + `|[^\s;&|()<>\x60#"'\\-]|\\[^\r\n-]|'-*[^'\r\n-][^'\r\n]*'|"-*(?:\\[^\r\n]|[^"\\\r\n-])(?:\\[^\r\n]|[^"\\\r\n])*")`
	shellOptionWord  = `(?:` + shellWordPart + `*` + shellNonDashPart + shellWordPart + `*|(?:''|"")*(?:-|\\-|'-'|"-"|---+|'---+'|"---+")(?:''|"")*|(?:''|"")+)`
	shellOptionGap   = shellWordGap + `(?:` + shellOptionWord + shellWordGap + `)*`
	rmRecursiveFlag  = `(?:--recursive|-[a-z]*r[a-z]*)`
	rmForceFlag      = `(?:--force|-[a-z]*f[a-z]*)`
)

func shellQuoted(pattern string) string {
	return `(?:` + pattern + `|'` + pattern + `'|"` + pattern + `")`
}

var gitCommandPrefix = `(?i)\bgit[ \t]+(?:(?:-[Cc][ \t]+` + shellWord + `|--(?:git-dir|work-tree|namespace|config-env)(?:=|[ \t]+)` + shellWord + `|--(?:no-pager|paginate|bare|no-replace-objects|literal-pathspecs|no-optional-locks))[ \t]+)*`
