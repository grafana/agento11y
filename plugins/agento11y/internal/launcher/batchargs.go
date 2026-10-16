package launcher

import (
	"fmt"
	"strings"
	"unicode"
)

// batchCommandLine quotes arguments for cmd.exe instead of the C runtime rules
// used by os/exec. cmd.exe interprets batch shims even when no shell was
// requested. The rules follow the ones the Rust standard library settled on
// after CVE-2024-24576.
//
// The logic is pure string handling and builds on every platform, so its
// tests run everywhere rather than only on the Windows runner.
func batchCommandLine(script string, args []string) (string, error) {
	// cmd.exe expands %NAME% in the path too, and a quote or a trailing
	// backslash would end the quoted path early.
	if strings.ContainsAny(script, `"%`) || strings.HasSuffix(script, `\`) {
		return "", fmt.Errorf("cannot run batch file %q: cmd.exe cannot safely receive a path with a quote, a percent sign, or a trailing backslash", script)
	}

	var line strings.Builder
	line.WriteString(`cmd.exe /e:ON /v:OFF /d /c ""`)
	line.WriteString(script)
	line.WriteByte('"')
	for _, arg := range args {
		if strings.ContainsAny(arg, "\x00\r\n") {
			return "", fmt.Errorf("cannot pass an argument with a newline or NUL through batch file %q; pass multiline text in a file or on stdin", script)
		}
		line.WriteByte(' ')
		appendBatchArg(&line, arg)
	}
	line.WriteByte('"')
	return line.String(), nil
}

func appendBatchArg(line *strings.Builder, arg string) {
	quote := arg == "" || strings.HasSuffix(arg, `\`)
	for _, r := range arg {
		if unicode.IsControl(r) || r <= unicode.MaxASCII && !isBatchUnquoted(r) {
			quote = true
			break
		}
	}
	if quote {
		line.WriteByte('"')
	}

	// Byte by byte: every character rewritten here is ASCII, and copying the
	// other bytes keeps Windows text that is not valid UTF-8, which range
	// would replace with U+FFFD.
	backslashes := 0
	for i := 0; i < len(arg); i++ {
		c := arg[i]
		switch c {
		case '\\':
			backslashes++
			line.WriteByte(c)
			continue
		case '"':
			// Double the run of backslashes that precedes the quote, then
			// double the quote itself, which is how a batch file reads a
			// literal one.
			line.WriteString(strings.Repeat(`\`, backslashes))
			line.WriteByte('"')
		case '%':
			// %cd:~,% is the current directory cut to a zero-length
			// substring, so the pair collapses to the percent sign itself
			// and cmd.exe never sees %NAME% to expand.
			line.WriteString(`%%cd:~,`)
		}
		backslashes = 0
		line.WriteByte(c)
	}
	if quote {
		line.WriteString(strings.Repeat(`\`, backslashes))
		line.WriteByte('"')
	}
}

// isBatchUnquoted reports whether cmd.exe leaves r alone in an unquoted
// argument. Every other ASCII character forces the argument to be quoted.
func isBatchUnquoted(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(`#$*+-./:?@\_`, r)
}
