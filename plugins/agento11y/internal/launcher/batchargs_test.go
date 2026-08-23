package launcher

import "testing"

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

func TestBatchCommandLineRejectsNewlinesAndNUL(t *testing.T) {
	for _, arg := range []string{"line\nfeed", "carriage\rreturn", "nul\x00byte"} {
		if _, err := batchCommandLine(batchScript, []string{arg}); err == nil {
			t.Fatalf("batchCommandLine(%q) returned nil error", arg)
		}
	}
}

func TestBatchCommandLineRejectsUnusablePaths(t *testing.T) {
	for _, script := range []string{`C:\bin\od"d.cmd`, `C:\bin\trailing\`} {
		if _, err := batchCommandLine(script, nil); err == nil {
			t.Fatalf("batchCommandLine(%q) returned nil error", script)
		}
	}
}
