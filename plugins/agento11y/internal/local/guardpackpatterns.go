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

	// An adjoining quoted suffix belongs to the argument: --force"-with-lease" is not --force.
	shellArgumentEnd = `(?:''|"")*(?:[[:space:];&|()<>]|["']?$)`

	// A standalone -- ends option recognition, including its quoted spellings.
	shellNonDashPart = `(?:` + shellSubstitution + `|[^\s;&|()<>\x60#"'\\-]|\\[^\r\n-]|'-*[^'\r\n-][^'\r\n]*'|"-*(?:\\[^\r\n]|[^"\\\r\n-])(?:\\[^\r\n]|[^"\\\r\n])*")`
	shellOptionWord  = `(?:` + shellWordPart + `*` + shellNonDashPart + shellWordPart + `*|(?:''|"")*(?:-|\\-|'-'|"-"|---+|'---+'|"---+")(?:''|"")*|(?:''|"")+)`
	shellOptionGap   = shellWordGap + `(?:` + shellOptionWord + shellWordGap + `)*`
	rmRecursiveFlag  = `(?:--recursive|-[a-z]*r[a-z]*)`

	// The end of a command: a separator, a comment, or the end of the text.
	shellCommandEnd = `[ \t]*(?:[;&|()\r\n#]|$)`
	rmForceFlag     = `(?:--force|-[a-z]*f[a-z]*)`
)

// shellCommandStart matches what can come before a command's name: nothing,
// or separators and words with no quote in them, where a backslash escapes
// the next character (a newline too), ending in an unescaped separator. A
// newline is no separator here: the shell ends a command there and each line
// is judged as its own simple command, so the match cannot run on into the
// body of a heredoc below the line. A wrapper in front of
// the name (sudo, pkexec, ssh host, docker exec c) does not hide it, while a
// quoted message cannot reach it: in the subject as written its quote stops
// the match, and in the unquoted and simple command subjects its spaces are
// escaped (git\ reset\ --hard), so no separator sits before the name. A word
// cannot start with #, which starts a comment; a # inside a word, as in a#b,
// is part of it. A directory or a backslash on the name itself, as in
// /usr/bin/git or \git, is part of the name.
var shellCommandStart = `^(?:(?:[ \t;&|()<>]|(?:[^'"\\\s;&|()<>#]|\\[\s\S])(?:[^'"\\\s;&|()<>]|\\[\s\S])*)*?[ \t;&|()<>])?(?:\\|[^\s;&|()<>'"]*/)?`

func shellQuoted(pattern string) string {
	return `(?:` + pattern + `|'` + pattern + `'|"` + pattern + `")`
}

// Git clean consumes the argument after -e/--exclude even when it is --.
// In short clusters, e consumes the remaining letters as its value.
// Value-taking options cannot also match the single-argument alternatives.
var (
	gitCleanExclude   = shellQuoted(`(?:-[diqnxX]*e|--exclude)`) + `(?:''|"")*`
	gitCleanFlag      = shellQuoted(`(?:-[diqnxX]+|--(?:no-)?(?:quiet|dry-run|interactive))`) + `(?:''|"")*`
	gitCleanValue     = shellQuoted(`(?:` + shellQuoted(`-[diqnxX]*e`) + shellWord + `|` + shellQuoted(`--exclude=`) + shellWordPart + `*)`)
	gitCleanPath      = `(?:` + shellSubstitution + `|[^\s;&|()<>\x60#"'\\-]|\\[^\r\n-]|'[^-'\r\n][^'\r\n]*'|"(?:\\[^\r\n-]|[^-"\\\r\n])(?:\\[^\r\n]|[^"\\\r\n])*")` + shellWordPart + `*|(?:''|"")+`
	gitCleanOptionGap = shellWordGap + `(?:(?:` + gitCleanExclude + shellWordGap + shellWord + `|` + gitCleanFlag + `|` + gitCleanValue + `|` + gitCleanPath + `)` + shellWordGap + `)*`
)

var gitCommandPrefix = `(?i)` + shellCommandStart + `git[ \t]+(?:(?:-[Cc][ \t]+` + shellWord + `|--(?:git-dir|work-tree|namespace|config-env)(?:=|[ \t]+)` + shellWord + `|--(?:attr-source|super-prefix|list-cmds)=` + shellWord + `|--exec-path(?:=` + shellWord + `)?|-[pP]|--(?:no-pager|paginate|bare|no-replace-objects|literal-pathspecs|glob-pathspecs|noglob-pathspecs|icase-pathspecs|no-optional-locks|no-lazy-fetch|no-advice))[ \t]+)*`

// gitForce is --force and the prefixes git accepts for it where it is the
// only option starting with --f, as in git clean and git checkout.
const gitForce = `--f(?:o(?:r(?:ce?)?)?)?`

// gitWholeTree is a pathspec for the whole working tree, which git checkout
// overwrites with the index or the named commit. The \* forms are a quoted *
// as the subjects write it.
const gitWholeTree = `(?:\.|\./|\./\\?\*|\\?\*|:/[.*]?)`

// git restore writes the working tree unless --staged (-S) is its only mode,
// so restoring a path without -S, or with -S and --worktree (-W), throws away
// uncommitted work. git accepts a unique prefix of a long option, so --st is
// --staged and --w is --worktree.
const (
	gitRestoreStaged   = `(?:--st[a-z]*|(?-i:-[a-zA-Z]*S[a-zA-Z]*))`
	gitRestoreWorktree = `(?:--w[a-z]*|(?-i:-[a-zA-Z]*W[a-zA-Z]*))`
)

// gitRestoreWorktreeWords matches every word of a git restore up to the end of
// its command, so a --staged anywhere after restore stops the match. A word is
// a path, - (paths read from stdin), a short option cluster without S or h, or
// a long option that does not start with --st or --h (git restore --help only
// prints its usage), with or without a value. No word
// at all counts too: xargs git restore gets its paths from stdin.
var (
	gitRestoreOption        = shellQuoted(`(?:(?-i:-[a-gi-zA-RT-Z]+)|--(?:[a-gi-rt-z][a-z-]*|s(?:[a-su-z][a-z-]*)?)(?:=`+shellWordPart+`*)?|--|-)`) + `(?:''|"")*`
	gitRestoreWorktreeWords = `(?:` + shellWordGap + `(?:` + gitRestoreOption + `|(?:` + gitCleanPath + `)))*` +
		`(?:(?:[ \t]+[0-9]*)?` + shellRedirection + `)*` + shellCommandEnd
)
