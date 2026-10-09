package redact

import "strings"

// TitleScanBytes bounds how much of a prompt is redacted to make a
// conversation title from it. A secret that starts inside a title is far
// shorter, so it is still matched whole, and a large paste does not pay for
// redacting the rest.
const TitleScanBytes = 64 << 10

// TitleHead returns the part of text that can reach a title: text trimmed and
// cut to TitleScanBytes. Redact it before cutting it to a title's length: a
// secret the cut splits no longer matches its pattern, and its first part
// would stay in the title.
func TitleHead(text string) string {
	text = strings.TrimSpace(text)
	if len(text) > TitleScanBytes {
		text = text[:TitleScanBytes]
	}
	return text
}

// TrimPartialMarker drops a redaction marker that cutting s left partial, such
// as "[REDACTED:gith" or "[REDAC", from its end, and trims what is left. Call
// it on a title just cut from redacted text.
func TrimPartialMarker(s string) string {
	if at := strings.LastIndexByte(s, '['); at >= 0 {
		tail := s[at:]
		if !strings.Contains(tail, "]") && (strings.HasPrefix(tail, markerPrefix) || strings.HasPrefix(markerPrefix, tail)) {
			s = s[:at]
		}
	}
	return strings.TrimSpace(s)
}

// markerPrefix starts the text redaction puts in place of a secret.
const markerPrefix = "[REDACTED:"
