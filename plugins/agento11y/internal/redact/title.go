package redact

import (
	"strings"
	"unicode/utf8"
)

// A conversation title is cut from a prompt, and redaction has to run before
// the cut: a secret the cut splits no longer matches its pattern, and its
// first part would stay in the title. TitleHead gives the part of a prompt to
// redact; CutTitle and CutTitleRunes then cut the redacted text.

// TitleScanBytes bounds how much of a prompt is redacted to make a
// conversation title from it. A secret that starts inside a title is far
// shorter, so it is still matched whole, and a large paste does not pay for
// redacting the rest.
const TitleScanBytes = 64 << 10

// TitleHead returns the part of text that can reach a title: text trimmed and
// cut to TitleScanBytes on a rune boundary.
func TitleHead(text string) string {
	text = strings.TrimSpace(text)
	if len(text) > TitleScanBytes {
		text = cutBytes(text, TitleScanBytes)
	}
	return text
}

// CutTitle cuts redacted text to max bytes on a rune boundary. A redaction
// marker the cut would leave partial, such as "[REDACTED:gith", is dropped
// with the space before it. Text within max is returned unchanged.
func CutTitle(text string, max int) string {
	if len(text) <= max {
		return text
	}
	return dropPartialMarker(cutBytes(text, max))
}

// CutTitleRunes is CutTitle counting runes rather than bytes.
func CutTitleRunes(text string, max int) string {
	if utf8.RuneCountInString(text) <= max {
		return text
	}
	return dropPartialMarker(string([]rune(text)[:max]))
}

func cutBytes(text string, max int) string {
	text = text[:max]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}

// dropPartialMarker drops the start of a redaction marker at the end of s, and
// the space before it.
func dropPartialMarker(s string) string {
	at := strings.LastIndexByte(s, '[')
	if at < 0 {
		return s
	}
	tail := s[at:]
	if strings.Contains(tail, "]") || !(strings.HasPrefix(tail, markerPrefix) || strings.HasPrefix(markerPrefix, tail)) {
		return s
	}
	return strings.TrimRight(s[:at], " \t\r\n")
}

// markerPrefix starts the text redaction puts in place of a secret.
const markerPrefix = "[REDACTED:"
