package redact

import (
	"strings"
	"testing"
)

func TestTitleHead(t *testing.T) {
	if got := TitleHead("  fix the bug \n"); got != "fix the bug" {
		t.Errorf("TitleHead() = %q, want it trimmed", got)
	}
	if got := TitleHead(strings.Repeat("a", TitleScanBytes+10)); len(got) != TitleScanBytes {
		t.Errorf("TitleHead() kept %d bytes, want %d", len(got), TitleScanBytes)
	}
}

func TestTrimPartialMarker(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "a whole marker stays", in: "token [REDACTED:github-pat] please", want: "token [REDACTED:github-pat] please"},
		{name: "a marker cut after its id starts", in: "token [REDACTED:gith", want: "token"},
		{name: "a marker cut inside its prefix", in: "token [REDAC", want: "token"},
		{name: "a lone bracket a cut left", in: "token [", want: "token"},
		{name: "a bracket that is not a marker", in: "check arr[i", want: "check arr[i"},
		{name: "no bracket", in: "fix the bug ", want: "fix the bug"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TrimPartialMarker(tt.in); got != tt.want {
				t.Errorf("TrimPartialMarker(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
