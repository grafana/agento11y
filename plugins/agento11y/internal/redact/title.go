package redact

import (
	"strings"
	"unicode/utf8"
)

// A conversation title is cut from a prompt, and redaction has to run before
// the cut: a secret the cut splits no longer matches its pattern, and its
// first part would stay in the title. TitleHead gives the part of a prompt to
// redact; CutTitle and CutTitleRunes then cut the redacted text. RedactTitle
// and RedactTitleRunes do all three with tier 1 and 2, the redaction the
// history importer's Sanitizer applies to every field.

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

// RedactTitle redacts the head of text with tier 1 and 2 and cuts it to max
// bytes with CutTitle.
func RedactTitle(text string, max int) string {
	return CutTitle(strings.TrimSpace(New().Redact(TitleHead(text))), max)
}

// RedactTitleRunes is RedactTitle counting runes rather than bytes.
func RedactTitleRunes(text string, max int) string {
	return CutTitleRunes(strings.TrimSpace(New().Redact(TitleHead(text))), max)
}

// CutTitle cuts redacted text to max bytes on a rune boundary. A redaction
// marker the cut would split is cut before, with the space ahead of it. Text
// within max is returned unchanged.
func CutTitle(text string, max int) string {
	if len(text) <= max {
		return text
	}
	return cutBytes(text, beforeSplitMarker(text, max))
}

// CutTitleRunes is CutTitle counting runes rather than bytes.
func CutTitleRunes(text string, max int) string {
	if utf8.RuneCountInString(text) <= max {
		return text
	}
	cut := len(string([]rune(text)[:max]))
	return text[:beforeSplitMarker(text, cut)]
}

// beforeSplitMarker returns cut, or the start of a redaction marker that cut
// would split, with the space before the marker left out. Only a whole marker
// counts, so text the user typed that only looks like the start of one is
// never cut short.
func beforeSplitMarker(text string, cut int) int {
	for from := 0; from < cut; {
		at := strings.Index(text[from:], markerPrefix)
		if at < 0 {
			break
		}
		at += from
		if at >= cut {
			break
		}
		end := strings.IndexByte(text[at:], ']')
		if end < 0 {
			break
		}
		end += at + 1
		if end > cut {
			return len(strings.TrimRight(text[:at], " \t\r\n"))
		}
		from = end
	}
	return cut
}

// cutBytes cuts text to max bytes, dropping the last rune when the cut splits
// it. Bytes before it are left as they are.
func cutBytes(text string, max int) string {
	text = text[:max]
	for i := len(text) - 1; i >= 0 && i >= len(text)-utf8.UTFMax; i-- {
		if utf8.RuneStart(text[i]) {
			if !utf8.FullRuneInString(text[i:]) {
				return text[:i]
			}
			break
		}
	}
	return text
}

// markerPrefix starts the text redaction puts in place of a secret.
const markerPrefix = "[REDACTED:"
