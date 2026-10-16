package redact

import (
	"strings"
	"testing"
	"unicode/utf8"
)

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
		{name: "a marker after a non-ASCII space", in: "token [REDACTED:github-pat]", max: 12, want: "token"},
		{name: "a marker after a whole one", in: "[REDACTED:github-pat] [REDACTED:aws-access-token]", max: 30, want: "[REDACTED:github-pat]"},
		{name: "text that looks like a marker's start", in: "see [README and more", max: 8, want: "see [REA"},
		{name: "text that starts like a marker", in: "[REDACTED: notes from the incident, see arr[i]", max: 20, want: "[REDACTED: notes fro"},
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
	if got := CutTitleRunes("é token", 7); got != "é token" {
		t.Errorf("CutTitleRunes() = %q, want text within the cap unchanged", got)
	}
	// An invalid byte counts as one byte, not as the three bytes of U+FFFD,
	// which would run the cut past the text.
	if got := CutTitleRunes(strings.Repeat("\xff", 150), 100); len(got) != 100 {
		t.Errorf("CutTitleRunes() kept %d bytes, want 100", len(got))
	}
	if got := CutTitleRunes("\xff"+strings.Repeat("a", 149), 100); utf8.RuneCountInString(got) != 100 {
		t.Errorf("CutTitleRunes() kept %d runes, want 100", utf8.RuneCountInString(got))
	}
}

func TestRedactTitle(t *testing.T) {
	r := New()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "a token the cut would split",
			in:   strings.Repeat("x", 70) + " ghp_" + strings.Repeat("A", 36) + " please",
			want: strings.Repeat("x", 70) + " [REDACTED:github-pat] please",
		},
		{
			// The value matches only with its closing quote.
			name: "a key-value secret the cut would split",
			in:   strings.Repeat("x", 80) + ` "api_key": "supersecretvalue123" please`,
			want: strings.Repeat("x", 80) + ` "api_key": "`,
		},
		{
			// Regression: only the first 64 KiB were redacted, so a key whose
			// end line came later was not matched.
			name: "a private key longer than 64 KiB",
			in:   "-----BEGIN PRIVATE KEY-----\n" + strings.Repeat("MIIEvQIBADANBgkqhkiG9w0BAQEFAASC\n", 70<<10/33) + "-----END PRIVATE KEY-----\nwhat is this?",
			want: "[REDACTED:private-key]\nwhat is this?",
		},
		{
			// The pattern needs a word boundary after the key, which the cut
			// makes: the whole text has none, the cut title does.
			name: "a key the cut gives a word boundary",
			in:   strings.Repeat("x", 79) + " AKIA" + strings.Repeat("B", 16) + "XYZ please",
			want: strings.Repeat("x", 79),
		},
		{
			// One pass redacts the first key and leaves the token after it,
			// which only matches once the key is a marker.
			name: "a token only a second pass finds",
			in:   strings.Repeat("x", 20) + " SK" + strings.Repeat("0a", 16) + "ghp_" + strings.Repeat("A", 36) + " please",
			want: strings.Repeat("x", 20) + " [REDACTED:twilio-api-key][REDACTED:github-pat] please",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactTitle(tt.in, 100)
			if got != tt.want {
				t.Errorf("RedactTitle() = %q, want %q", got, tt.want)
			}
			if again := r.Redact(got); again != got {
				t.Errorf("Redact(RedactTitle()) = %q, want the title unchanged", again)
			}
		})
	}
}

// beforeSplitMarker only knows a marker by the shape of its pattern ID, so
// every ID has to have that shape.
func TestMarkersHaveTheShapeTitleCutsKnow(t *testing.T) {
	ids := []string{emailPattern.id}
	for _, p := range tier1Patterns {
		ids = append(ids, p.id)
	}
	for _, p := range tier2Patterns {
		ids = append(ids, p.id)
	}
	for _, id := range ids {
		marker := markerPrefix + id + "]"
		if got := markerLen(marker + " after"); got != len(marker) {
			t.Errorf("markerLen(%q) = %d, want %d", marker, got, len(marker))
		}
	}
}
