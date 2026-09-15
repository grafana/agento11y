package mapperutil

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBoundJSON(t *testing.T) {
	small := json.RawMessage(`{"path":"x"}`)
	if got := BoundJSON(small, MaxToolInputBytes); string(got) != string(small) {
		t.Fatalf("payload within the cap changed: %s", got)
	}
	if got := BoundJSON(nil, MaxToolInputBytes); got != nil {
		t.Fatalf("nil payload changed: %v", got)
	}

	// Multi-byte runes straddle the cut so the boundary has to be rune-safe.
	big := json.RawMessage(`"` + strings.Repeat("é", 3000) + strings.Repeat("x", 3000) + `"`)
	got := BoundJSON(big, MaxToolInputBytes)
	var s string
	if err := json.Unmarshal(got, &s); err != nil {
		t.Fatalf("bounded payload is not a JSON string: %v: %.60s", err, got)
	}
	if !strings.HasSuffix(s, " [truncated]") {
		t.Fatalf("bounded payload lacks the marker: %.40q", s[len(s)-40:])
	}
	if !utf8.ValidString(s) {
		t.Fatal("bounded payload split a multi-byte rune")
	}
	if len(s) > MaxToolInputBytes+len(" [truncated]") {
		t.Fatalf("bounded payload is %d bytes, want at most %d plus the marker", len(s), MaxToolInputBytes)
	}
}

func TestBoundText(t *testing.T) {
	if got := BoundText("short", 10); got != "short" {
		t.Fatalf("text within the cap changed: %q", got)
	}
	// "é" is two bytes; a five-byte cap must cut back to a rune boundary.
	if got := BoundText(strings.Repeat("é", 10), 5); got != "éé [truncated]" {
		t.Fatalf("got %q, want the first two runes plus the marker", got)
	}
	if got := BoundText("abc", 0); got != " [truncated]" {
		t.Fatalf("zero cap: got %q", got)
	}
}
