package history

import (
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultSinceWindow is how far back the CLI and the viewer look when the user
// sets no lower bound. The development machine holds 277,625 turns across all
// time but 60,882 in the last 90 days, and the store is a linear-scan JSONL
// store, so an unbounded default would make the viewer unusable on the first
// import.
const DefaultSinceWindow = 90 * 24 * time.Hour

// SkipReason explains why a session was excluded from a selection.
type SkipReason string

const (
	SkipActiveSession SkipReason = "active"
	SkipOutOfRange    SkipReason = "out_of_range"
	SkipWorkspace     SkipReason = "workspace_filter"
	SkipSourcePath    SkipReason = "source_filter"
	SkipMaxSessions   SkipReason = "max_sessions"
)

// SkippedSession pairs an excluded session with the reason, so the CLI can
// report what it left out and why.
type SkippedSession struct {
	Session SessionPreview
	Reason  SkipReason
}

// Filter is the selection criteria shared by the CLI flags and the viewer's
// import card. Build it with [NewFilter], which skips sessions an agent may
// still be writing. The zero Filter skips none.
type Filter struct {
	Since       time.Time // inclusive lower bound on activity; zero means no bound
	Until       time.Time // inclusive upper bound on activity; zero means no bound
	Workspace   string    // case-insensitive substring match on Workspace
	SourcePaths []string  // restrict to these discovered source paths; empty means any
	MaxSessions int       // cap on selected sessions, most recent first; 0 means no cap
	MaxTurns    int       // per-session turn cap applied while reading; 0 means no cap
	SkipActive  bool      // exclude in-progress sessions
}

// NewFilter returns a Filter that skips in-progress sessions.
func NewFilter() Filter {
	return Filter{SkipActive: true}
}

// SelectSessions splits previews into the ones to import and the ones skipped,
// with a reason for each. Both slices are ordered most recent first by
// LastActivityAt, so MaxSessions keeps the freshest sessions.
//
// SourcePaths restricts the selection to discovered paths. It never adds a
// root: a path outside the importer's roots matches nothing, and the caller
// reports that rather than importing something the user did not ask for.
func (f Filter) SelectSessions(in []SessionPreview) (selected []SessionPreview, skipped []SkippedSession) {
	ordered := append([]SessionPreview(nil), in...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].LastActivityAt.After(ordered[j].LastActivityAt)
	})

	src := normalizedSet(f.SourcePaths)
	ws := strings.ToLower(strings.TrimSpace(f.Workspace))

	for _, s := range ordered {
		switch {
		case f.SkipActive && s.Active:
			skipped = append(skipped, SkippedSession{s, SkipActiveSession})
		case !f.inRange(s):
			skipped = append(skipped, SkippedSession{s, SkipOutOfRange})
		case ws != "" && !strings.Contains(strings.ToLower(s.Workspace), ws):
			skipped = append(skipped, SkippedSession{s, SkipWorkspace})
		case len(src) > 0 && !matchesSourcePath(src, s.SourcePath):
			skipped = append(skipped, SkippedSession{s, SkipSourcePath})
		case f.MaxSessions > 0 && len(selected) >= f.MaxSessions:
			skipped = append(skipped, SkippedSession{s, SkipMaxSessions})
		default:
			selected = append(selected, s)
		}
	}
	return selected, skipped
}

// inRange reports whether a session overlaps [Since, Until]. A session spans
// [StartedAt, LastActivityAt]; it is in range unless it ended before Since or
// started after Until. Either bound may be zero, meaning unbounded.
func (f Filter) inRange(s SessionPreview) bool {
	if !f.Since.IsZero() && !s.LastActivityAt.IsZero() && s.LastActivityAt.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && !s.StartedAt.IsZero() && s.StartedAt.After(f.Until) {
		return false
	}
	return true
}

// matchesSourcePath accepts an exact path or any path under a named directory,
// so `--source ~/.claude/projects/foo` selects that project's transcripts.
//
// The directory comparison cleans both sides and joins with
// [filepath.Separator], so it also matches on Windows, where discovery returns
// backslash-separated paths.
func matchesSourcePath(want map[string]bool, path string) bool {
	if want[path] {
		return true
	}
	clean := filepath.Clean(path)
	for prefix := range want {
		dir := filepath.Clean(prefix)
		if clean == dir || strings.HasPrefix(clean, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func normalizedSet(paths []string) map[string]bool {
	if len(paths) == 0 {
		return nil
	}
	out := make(map[string]bool, len(paths))
	for _, p := range paths {
		if p = strings.TrimSpace(p); p != "" {
			out[p] = true
		}
	}
	return out
}
