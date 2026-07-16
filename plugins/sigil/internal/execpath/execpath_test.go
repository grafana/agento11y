package execpath

import (
	"errors"
	"testing"
)

func TestHookCommand(t *testing.T) {
	cases := []struct {
		name    string
		goos    string
		exe     string
		exeErr  error
		suffix  string
		want    string
		wantErr bool
	}{
		{name: "plain path", goos: "linux", exe: "/usr/local/bin/agento11y", suffix: "copilot hook", want: "/usr/local/bin/agento11y copilot hook"},
		{name: "legacy sigil path", goos: "linux", exe: "/opt/homebrew/bin/sigil", suffix: "vibe hook", want: "/opt/homebrew/bin/sigil vibe hook"},
		{name: "path with spaces is quoted", goos: "darwin", exe: "/Users/Jane Doe/bin/agento11y", suffix: "vibe hook", want: "'/Users/Jane Doe/bin/agento11y' vibe hook"},
		{name: "path with single quote is escaped", goos: "darwin", exe: "/tmp/it's/agento11y", suffix: "copilot hook", want: `'/tmp/it'\''s/agento11y' copilot hook`},
		{name: "windows path uses bare name", goos: "windows", exe: `C:\Users\Jane Doe\go\bin\agento11y.exe`, suffix: "copilot hook", want: "agento11y copilot hook"},
		{name: "windows forward-slash path", goos: "windows", exe: `C:/bin/agento11y.exe`, suffix: "copilot hook", want: "agento11y copilot hook"},
		{name: "windows legacy sigil name", goos: "windows", exe: `C:\bin\sigil.exe`, suffix: "vibe hook", want: "sigil vibe hook"},
		{name: "executable error propagates", goos: "linux", exeErr: errors.New("boom"), suffix: "copilot hook", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prevExe, prevGOOS := Executable, goos
			t.Cleanup(func() { Executable, goos = prevExe, prevGOOS })
			Executable = func() (string, error) { return tc.exe, tc.exeErr }
			goos = tc.goos

			got, err := HookCommand(tc.suffix)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("HookCommand = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("HookCommand: %v", err)
			}
			if got != tc.want {
				t.Fatalf("HookCommand = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestQuote(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{in: "/usr/local/bin/agento11y", want: "/usr/local/bin/agento11y"},
		{in: "", want: "''"},
		{in: "/tmp/a b", want: "'/tmp/a b'"},
		{in: "/has/dollar$ign/agento11y", want: "'/has/dollar$ign/agento11y'"},
		{in: `/tmp/a'b`, want: `'/tmp/a'\''b'`},
		{in: `/tmp/a\b`, want: `'/tmp/a\b'`},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := Quote(tc.in); got != tc.want {
				t.Fatalf("Quote(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
