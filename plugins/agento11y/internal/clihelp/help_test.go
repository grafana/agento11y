package clihelp

import (
	"bytes"
	"flag"
	"strings"
	"testing"

	"github.com/muesli/termenv"
)

func TestRender(t *testing.T) {
	for _, tt := range []struct {
		name, noColor, term string
		width               int
		stacked             bool
	}{
		{name: "redirected", width: 80},
		{name: "no color", noColor: "1", width: 80},
		{name: "dumb terminal", term: "dumb", width: 80},
		{name: "narrow", width: 20, stacked: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("NO_COLOR", tt.noColor)
			t.Setenv("TERM", tt.term)
			var out bytes.Buffer
			r := New(&out)
			r.width = tt.width
			r.Render(Page{Command: "agento11y test", Summary: "Test command.", Usage: []string{"agento11y test [flags]"}, Sections: []Section{{Title: "Commands", Rows: []Row{{Name: "start", Description: "Start capture."}}}}})
			got := out.String()
			for _, want := range []string{"Test command.", "Usage:", "agento11y test [flags]", "Commands:", "Start capture."} {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q in %q", want, got)
				}
			}
			if strings.Contains(got, "Run `agento11y test --help`") {
				t.Fatalf("help refers back to itself: %q", got)
			}
			if strings.Contains(got, "\x1b") {
				t.Fatalf("redirected help contains ANSI: %q", got)
			}
			if tt.stacked && !strings.Contains(got, "  start\n    Start capture.") {
				t.Fatalf("not stacked: %q", got)
			}
		})
	}
}

func TestDestinationIsolation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	terminal := New(&stderr)
	terminal.styles.SetColorProfile(termenv.TrueColor)
	if !strings.Contains(terminal.Heading("heading"), "\x1b") {
		t.Fatal("terminal style missing")
	}
	redirected := New(&stdout)
	if strings.Contains(redirected.Heading("heading"), "\x1b") {
		t.Fatal("other destination changed redirected output")
	}
}

func TestFlags(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("token", "", "access `value`")
	fs.String("since", "90d", "time window")
	fs.Bool("json", false, "JSON output")
	fs.Bool("hidden", false, "compatibility")
	rows := Flags(fs, map[string]bool{"hidden": true}, map[string]string{"token": "secret"})
	want := []Row{{"--json", "JSON output"}, {"--since string", "time window (default: \"90d\")"}, {"--token secret", "access value"}, {"--help, -h", "Show help."}}
	if len(rows) != len(want) {
		t.Fatalf("rows: %v", rows)
	}
	for i := range rows {
		if rows[i] != want[i] {
			t.Errorf("row %d: got %v, want %v", i, rows[i], want[i])
		}
	}
}

func TestUsageError(t *testing.T) {
	var out bytes.Buffer
	UsageError(&out, Page{Command: "agento11y skills show", Usage: []string{"agento11y skills show <name>"}}, "name required")
	want := "agento11y skills show: name required\nusage: agento11y skills show <name>\nRun `agento11y skills show --help` for help.\n"
	if out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
}
