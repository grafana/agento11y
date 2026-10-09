package redact

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTitleHead(t *testing.T) {
	if got := TitleHead("  fix the bug \n"); got != "fix the bug" {
		t.Errorf("TitleHead() = %q, want it trimmed", got)
	}
	if got := TitleHead(strings.Repeat("a", TitleScanBytes+10)); len(got) != TitleScanBytes {
		t.Errorf("TitleHead() kept %d bytes, want %d", len(got), TitleScanBytes)
	}
	// A rune that straddles the bound is dropped, not split.
	got := TitleHead(strings.Repeat("a", TitleScanBytes-1) + "é")
	if !utf8.ValidString(got) || len(got) != TitleScanBytes-1 {
		t.Errorf("TitleHead() = %d bytes, valid UTF-8 %v; want the é dropped", len(got), utf8.ValidString(got))
	}
}

func TestCutTitle(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{name: "within the cap", in: "fix the bug ", max: 100, want: "fix the bug "},
		{name: "a cut keeps what it lands on", in: "aaaa bbbb", max: 5, want: "aaaa "},
		{name: "a whole marker stays", in: "token [REDACTED:github-pat] please", max: 27, want: "token [REDACTED:github-pat]"},
		{name: "a marker cut after its id starts", in: "token [REDACTED:github-pat] please", max: 20, want: "token"},
		{name: "a marker cut inside its prefix", in: "token [REDACTED:github-pat] please", max: 12, want: "token"},
		{name: "a lone bracket a cut left", in: "token [REDACTED:github-pat]", max: 7, want: "token"},
		{name: "a bracket that is not a marker", in: "check arr[i] now", max: 11, want: "check arr[i"},
		{name: "a title that was only the marker", in: "[REDACTED: note] more", max: 5, want: ""},
		{name: "keeps a rune whole", in: strings.Repeat("a", 4) + "é", max: 5, want: "aaaa"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CutTitle(tt.in, tt.max); got != tt.want {
				t.Errorf("CutTitle(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
			}
		})
	}
}

func TestCutTitleRunes(t *testing.T) {
	if got := CutTitleRunes(strings.Repeat("é", 150), 100); utf8.RuneCountInString(got) != 100 {
		t.Errorf("CutTitleRunes() kept %d runes, want 100", utf8.RuneCountInString(got))
	}
	if got := CutTitleRunes("é token [REDACTED:github-pat]", 12); got != "é token" {
		t.Errorf("CutTitleRunes() = %q, want the partial marker dropped", got)
	}
}
