package launcher

import (
	"slices"
	"strings"
	"testing"
)

const batchScript = `C:\bin\agent.cmd`

// batchPrefix is the fixed head of every generated command line: cmd.exe, the
// flags, and the quoted script inside the outer quote pair.
const batchPrefix = `cmd.exe /e:ON /v:OFF /d /c ""` + batchScript + `"`

func TestBatchCommandLine(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "no arguments",
			want: batchPrefix + `"`,
		},
		{
			name: "plain argument stays unquoted",
			args: []string{"plain"},
			want: batchPrefix + ` plain"`,
		},
		{
			name: "empty argument survives as an empty pair",
			args: []string{""},
			want: batchPrefix + ` """`,
		},
		{
			name: "shell metacharacters are quoted",
			args: []string{"foo&bar", "pipe|redirect<in>out", "caret^bang!", "space arg"},
			want: batchPrefix + ` "foo&bar" "pipe|redirect<in>out" "caret^bang!" "space arg""`,
		},
		{
			// Each percent sign becomes itself followed by a zero-length
			// substring of %cd%, so cmd.exe finds no %NAME% to expand.
			name: "percent signs cannot expand a variable",
			args: []string{"%PATH%"},
			want: batchPrefix + ` "%%cd:~,%PATH%%cd:~,%""`,
		},
		{
			name: "quotes are doubled",
			args: []string{`say "hello"`},
			want: batchPrefix + ` "say ""hello""""`,
		},
		{
			name: "trailing backslash is doubled so it cannot escape the closing quote",
			args: []string{`trailing\`},
			want: batchPrefix + ` "trailing\\""`,
		},
		{
			name: "backslashes before a quote are doubled",
			args: []string{`c:\dir\"x`},
			want: batchPrefix + ` "c:\dir\\""x""`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := batchCommandLine(batchScript, tt.args)
			if err != nil {
				t.Fatalf("batchCommandLine: %v", err)
			}
			if got != tt.want {
				t.Fatalf("batchCommandLine =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

// batchRoundTripArgs are arguments a batch shim must hand to its program
// unchanged, chosen around the characters cmd.exe and the C runtime treat
// specially.
var batchRoundTripArgs = []string{
	"plain", "", "space arg", "tab\there", "--model=gpt-5", "naïve ✓",
	`say "hello world"`, `"`, `""`, `a\"b`, `a\\"b`, `trailing\`, `trailing\\`, `c:\dir\"x`, `\\server\share\`,
	"%PATH%", "%%", "100%", "%1", "%~dp0", "!PATH!", "caret^bang!",
	"foo&bar", "pipe|redirect<in>out", "(paren)", `x" & echo injected & "`,
}

// TestBatchCommandLineRoundTrip undoes the two things cmd.exe does to the
// generated line and parses the rest the way the program behind the shim
// does. The Windows handoff test runs the same arguments through a real shim.
func TestBatchCommandLineRoundTrip(t *testing.T) {
	prefix := `cmd.exe /e:ON /v:OFF /d /c ""` + batchScript + `"`
	for _, arg := range batchRoundTripArgs {
		t.Run(arg, func(t *testing.T) {
			line, err := batchCommandLine(batchScript, []string{arg, "sentinel"})
			if err != nil {
				t.Fatalf("batchCommandLine: %v", err)
			}
			// /c strips the outer quote pair, and each %cd:~,% expands to
			// nothing, leaving the percent sign before it.
			if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, `"`) {
				t.Fatalf("unexpected line shape: %s", line)
			}
			command := line[len(`cmd.exe /e:ON /v:OFF /d /c "`) : len(line)-1]
			if at, ok := unquotedCmdMetacharacter(command); ok {
				t.Fatalf("cmd.exe would interpret %q outside quotes in %s", command[at], command)
			}
			argText := strings.ReplaceAll(line[len(prefix):len(line)-1], `%%cd:~,%`, `%`)
			if got, want := parseMSVCArgs(argText), []string{arg, "sentinel"}; !slices.Equal(got, want) {
				t.Fatalf("parsed %q from %s, want %q", got, argText, want)
			}
		})
	}
}

// TestBatchCommandLineKeepsWTF8 pins that an argument Go read from Windows as
// WTF-8, here an unpaired surrogate, reaches the command line byte for byte.
func TestBatchCommandLineKeepsWTF8(t *testing.T) {
	const surrogate = "\xed\xa0\x80"
	line, err := batchCommandLine(batchScript, []string{"a" + surrogate})
	if err != nil {
		t.Fatal(err)
	}
	if want := batchPrefix + ` a` + surrogate + `"`; line != want {
		t.Fatalf("batchCommandLine = %q, want %q", line, want)
	}
}

// unquotedCmdMetacharacter reports the first character cmd.exe would act on in
// command, tracking its quote state the way cmd.exe does: every " toggles it.
func unquotedCmdMetacharacter(command string) (int, bool) {
	quoted := false
	for i := 0; i < len(command); i++ {
		switch c := command[i]; {
		case c == '"':
			quoted = !quoted
		case !quoted && strings.IndexByte("&|<>^()", c) >= 0:
			return i, true
		}
	}
	return 0, false
}

// parseMSVCArgs splits an argument string with the Microsoft C runtime rules
// in use since 2008, which Node.js, Rust, and Python programs share. Inside
// quotes, "" is one literal quote and quoting continues. Go programs parse
// with older rules, so a Go helper cannot stand in for them.
func parseMSVCArgs(s string) []string {
	var args []string
	var cur []byte
	quoted, started := false, false
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case (c == ' ' || c == '\t') && !quoted:
			if started {
				args = append(args, string(cur))
				cur, started = cur[:0], false
			}
			i++
		case c == '\\':
			n := 0
			for ; i < len(s) && s[i] == '\\'; i++ {
				n++
			}
			if i < len(s) && s[i] == '"' {
				cur = append(cur, strings.Repeat(`\`, n/2)...)
				if n%2 == 1 {
					cur = append(cur, '"')
					i++
				}
			} else {
				cur = append(cur, strings.Repeat(`\`, n)...)
			}
			started = true
		case c == '"':
			if quoted && i+1 < len(s) && s[i+1] == '"' {
				cur = append(cur, '"')
				i++
			} else {
				quoted = !quoted
			}
			started = true
			i++
		default:
			cur = append(cur, c)
			started = true
			i++
		}
	}
	if started {
		args = append(args, string(cur))
	}
	return args
}

func TestBatchCommandLineRejectsNewlinesAndNUL(t *testing.T) {
	for _, arg := range []string{"line\nfeed", "carriage\rreturn", "nul\x00byte"} {
		if _, err := batchCommandLine(batchScript, []string{arg}); err == nil {
			t.Fatalf("batchCommandLine(%q) returned nil error", arg)
		}
	}
}

func TestBatchCommandLineRejectsUnusablePaths(t *testing.T) {
	for _, script := range []string{`C:\bin\od"d.cmd`, `C:\bin\trailing\`, `C:\Users\100%PATH%\agent.cmd`} {
		if _, err := batchCommandLine(script, nil); err == nil {
			t.Fatalf("batchCommandLine(%q) returned nil error", script)
		}
	}
}
