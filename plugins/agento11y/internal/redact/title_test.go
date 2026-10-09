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
	// An invalid byte earlier in the text does not shorten the head.
	if got := TitleHead("hello \xff" + strings.Repeat("a", TitleScanBytes)); len(got) != TitleScanBytes {
		t.Errorf("TitleHead() kept %d bytes, want %d", len(got), TitleScanBytes)
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
		{name: "a marker cut right after its bracket", in: "token [REDACTED:github-pat]", max: 7, want: "token"},
		{name: "text that looks like a marker's start", in: "see [README and more", max: 8, want: "see [REA"},
		{name: "a bracket the user typed", in: "check arr[i] now", max: 10, want: "check arr["},
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
		t.Errorf("CutTitleRunes() = %q, want the split marker cut before", got)
	}
}

func TestRedactTitle(t *testing.T) {
	// Redacted before the cut, so neither a token nor a key-value secret the
	// cut would split keeps its first part.
	token := strings.Repeat("x", 70) + " ghp_" + strings.Repeat("A", 36) + " please"
	if got := RedactTitle(token, 100); got != strings.Repeat("x", 70)+" [REDACTED:github-pat] please" {
		t.Errorf("RedactTitle(token) = %q", got)
	}
	kv := strings.Repeat("x", 80) + ` "api_key": "supersecretvalue123" please`
	if got := RedactTitle(kv, 100); strings.Contains(got, "superse") || strings.Contains(got, "[REDACT") {
		t.Errorf("RedactTitle(key-value) = %q, want the value and its split marker gone", got)
	}
}
