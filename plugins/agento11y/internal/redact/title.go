package redact

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// A conversation title is cut from a prompt, and redaction has to run before
// the cut: a secret the cut splits no longer matches its pattern, and its
// first part would stay in the title. The whole prompt is redacted, because a
// secret that starts inside a title, such as a private key, can end anywhere
// after it. CutTitle and CutTitleRunes then cut the redacted text.
// RedactTitle and RedactTitleRunes do both with tier 1 and 2, the redaction
// the history importer's Sanitizer applies to every field.

// titleRedactPasses bounds how often RedactTitle redacts a cut title again.
// Each pass that changes it replaces a secret with a marker, so one or two
// are enough; the bound only guards against a pattern that would rewrite a
// marker.
const titleRedactPasses = 4

// RedactTitle redacts text with tier 1 and 2 and cuts it to max bytes with
// CutTitle. The title it returns is one a further pass of Redact leaves
// unchanged, as the Sanitizer's own pass over the title needs. A cut can make
// a match the whole text did not have, such as a key a pattern needs a word
// boundary after, and one pass can leave a secret only the next finds.
func RedactTitle(text string, max int) string {
	return redactTitle(text, func(t string) string { return CutTitle(t, max) })
}

// RedactTitleRunes is RedactTitle counting runes rather than bytes.
func RedactTitleRunes(text string, max int) string {
	return redactTitle(text, func(t string) string { return CutTitleRunes(t, max) })
}

func redactTitle(text string, cut func(string) string) string {
	r := New()
	title := cut(strings.TrimSpace(r.Redact(strings.TrimSpace(text))))
	for range titleRedactPasses {
		again := r.Redact(title)
		if again == title {
			break
		}
		title = cut(strings.TrimSpace(again))
	}
	return title
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

// CutTitleRunes is CutTitle counting runes rather than bytes. An invalid byte
// counts as one rune, as utf8.RuneCountInString counts it.
func CutTitleRunes(text string, max int) string {
	cut := 0
	for range max {
		if cut == len(text) {
			return text
		}
		_, size := utf8.DecodeRuneInString(text[cut:])
		cut += size
	}
	if cut == len(text) {
		return text
	}
	return text[:beforeSplitMarker(text, cut)]
}

// beforeSplitMarker returns cut, or the start of a redaction marker that cut
// would split, with the space before the marker left out. Only a whole marker
// counts, so text the user typed that only looks like one is never cut short.
func beforeSplitMarker(text string, cut int) int {
	// A marker the cut splits starts before it, so its prefix ends within
	// len(markerPrefix)-1 bytes after it.
	window := text[:min(len(text), cut+len(markerPrefix)-1)]
	for from := 0; ; {
		i := strings.Index(window[from:], markerPrefix)
		if i < 0 {
			return cut
		}
		at := from + i
		n := markerLen(text[at:])
		if n > 0 && at+n > cut {
			return len(strings.TrimRightFunc(text[:at], unicode.IsSpace))
		}
		from = at + max(n, 1)
		if from >= len(window) {
			return cut
		}
	}
}

// markerLen returns the length of the redaction marker text starts with, or 0
// when text does not start with one. A marker is markerPrefix, a pattern ID of
// lowercase letters, digits and hyphens, and "]".
func markerLen(text string) int {
	if !strings.HasPrefix(text, markerPrefix) {
		return 0
	}
	for i := len(markerPrefix); i < len(text); i++ {
		switch c := text[i]; {
		case c == ']':
			if i == len(markerPrefix) {
				return 0
			}
			return i + 1
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-':
		default:
			return 0
		}
	}
	return 0
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
