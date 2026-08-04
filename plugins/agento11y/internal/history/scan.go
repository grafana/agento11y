package history

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
)

// PreviewByteBudget caps how much of one session file [Importer.Preview] may
// read. A preview runs over every session on the machine behind an interactive
// request: on the development machine that is 14,507 Claude transcripts
// totalling 5.3 GB, so reading whole files makes the preview time out before it
// renders. A file at or under the budget is read whole and counted exactly;
// a larger one is sampled from both ends and its turn count is approximate.
const PreviewByteBudget = 1 << 20 // 1 MiB

// previewMetadataLines caps how many lines at each end of a preview window are
// JSON-decoded. Session identity sits on the first line and the last activity
// on the last, so a handful covers a file that opens with a few bookkeeping
// records, while decoding every line in a 1 MiB window costs more than the
// rest of discovery together.
const previewMetadataLines = 32

// fallbackTurnID is a stable, content-free turn identifier for a source turn
// that carries no native ID. Generation IDs hash (session, turn index, turn
// ID), so the ordinal keeps anonymous turns distinct without leaking source
// content.
func fallbackTurnID(index int) string {
	return fmt.Sprintf("history-turn-%06d", index)
}

// defaultActiveWindow is how recently a session file must have been written for
// the session to count as in-progress, and so be skipped by default. It is a
// heuristic: JSONL transcripts carry no lock, so "written recently" is the only
// portable signal that an agent may still be appending.
const defaultActiveWindow = 5 * time.Minute

// isActiveMod reports whether modTime falls inside window before now, that is
// whether the file looks like it is still being written. A zero modTime or a
// non-positive window means not active.
func isActiveMod(modTime, now time.Time, window time.Duration) bool {
	if window <= 0 || modTime.IsZero() {
		return false
	}
	return !modTime.After(now) && now.Sub(modTime) < window
}

// walkFiles returns the paths of regular files under root for which match
// reports true. An unreadable entry is skipped rather than fatal, and a missing
// root yields no files and no error, so discovery degrades gracefully when an
// agent has never run on this machine.
func walkFiles(ctx context.Context, root string, match func(path string) bool) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return nil // skip the unreadable entry or subtree, keep scanning
		}
		if d.IsDir() || !match(path) {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return out, err
	}
	return out, nil
}

// hasTokenUsage reports whether any token count is non-zero, that is whether
// the source carried real usage rather than the zero value.
func hasTokenUsage(u agento11y.TokenUsage) bool {
	return u.InputTokens > 0 || u.OutputTokens > 0 || u.TotalTokens > 0 ||
		u.CacheReadInputTokens > 0 || u.CacheWriteInputTokens > 0 || u.ReasoningTokens > 0
}

// PreviewWindows is a bounded sample of a session file: the first lines, the
// last lines, and the file's size and modification time. Whole is true when
// Head holds the entire file, in which case Tail is empty and any count taken
// from Head is exact.
type PreviewWindows struct {
	Head    []byte
	Tail    []byte
	Size    int64
	ModTime time.Time
	Whole   bool
}

// ReadPreviewWindows reads at most budget bytes of path: the whole file when it
// fits, otherwise a head window and a tail window of half the budget each. A
// non-positive budget uses [PreviewByteBudget].
//
// Importers use it to build a metadata-only [SessionPreview] without decoding
// the file. See the budget note on [Importer.Preview].
func ReadPreviewWindows(path string, budget int64) (PreviewWindows, error) {
	if budget <= 0 {
		budget = PreviewByteBudget
	}
	f, err := os.Open(path)
	if err != nil {
		return PreviewWindows{}, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return PreviewWindows{}, err
	}
	win := PreviewWindows{Size: info.Size(), ModTime: info.ModTime()}

	if win.Size <= budget {
		data := make([]byte, win.Size)
		n, err := readFullAt(f, data, 0)
		if err != nil {
			return PreviewWindows{}, err
		}
		win.Head = data[:n]
		win.Whole = true
		return win, nil
	}

	half := budget / 2
	head := make([]byte, half)
	n, err := readFullAt(f, head, 0)
	if err != nil {
		return PreviewWindows{}, err
	}
	win.Head = head[:n]

	tail := make([]byte, half)
	n, err = readFullAt(f, tail, win.Size-half)
	if err != nil {
		return PreviewWindows{}, err
	}
	win.Tail = tail[:n]
	return win, nil
}

// readFullAt fills buf from off, tolerating short reads. It returns how many
// bytes were read; a read that stops at end of file is not an error.
func readFullAt(f *os.File, buf []byte, off int64) (int, error) {
	read := 0
	for read < len(buf) {
		n, err := f.ReadAt(buf[read:], off+int64(read))
		read += n
		if err != nil {
			if errors.Is(err, io.EOF) {
				return read, nil
			}
			return read, err
		}
	}
	return read, nil
}

// HeadLines returns the complete lines in the head window. When the window
// covers only part of the file the trailing partial line is dropped, so every
// returned line is decodable.
func (w PreviewWindows) HeadLines() [][]byte {
	lines := splitLines(w.Head)
	if w.Whole || len(lines) == 0 {
		return lines
	}
	return lines[:len(lines)-1]
}

// TailLines returns the complete lines in the tail window. The leading partial
// line is dropped for the same reason [PreviewWindows.HeadLines] drops the
// trailing one. A whole-file window has no separate tail.
func (w PreviewWindows) TailLines() [][]byte {
	if w.Whole {
		return nil
	}
	lines := splitLines(w.Tail)
	if len(lines) == 0 {
		return nil
	}
	return lines[1:]
}

func splitLines(data []byte) [][]byte {
	if len(data) == 0 {
		return nil
	}
	raw := bytes.Split(data, []byte{'\n'})
	out := make([][]byte, 0, len(raw))
	for _, line := range raw {
		out = append(out, bytes.TrimRight(line, "\r"))
	}
	// A file ending in a newline produces a trailing empty element that is not
	// a line. Anything else is a real (possibly partial) final line.
	if len(out) > 0 && len(out[len(out)-1]) == 0 {
		out = out[:len(out)-1]
	}
	return out
}

// EstimateTotal scales a count taken from the head window to the whole file.
// It is used only for a window that does not cover the file, where the exact
// count would cost a full read. approx is true whenever the result is scaled,
// so the caller can set SessionPreview.ApproxTurns.
func (w PreviewWindows) EstimateTotal(headCount int) (total int, approx bool) {
	if w.Whole {
		return headCount, false
	}
	if headCount == 0 || len(w.Head) == 0 {
		return headCount, true
	}
	scaled := float64(headCount) * float64(w.Size) / float64(len(w.Head))
	return int(scaled), true
}
