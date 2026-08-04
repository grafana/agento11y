package history

import (
	"path/filepath"
	"testing"
	"time"
)

var filterNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func preview(id string, lastActivity time.Time, opts ...func(*SessionPreview)) SessionPreview {
	p := SessionPreview{
		Agent:          AgentClaudeCode,
		SessionID:      id,
		SourcePath:     "/projects/" + id + ".jsonl",
		StartedAt:      lastActivity.Add(-time.Hour),
		LastActivityAt: lastActivity,
		TurnCount:      3,
	}
	for _, opt := range opts {
		opt(&p)
	}
	return p
}

func withWorkspace(ws string) func(*SessionPreview) {
	return func(p *SessionPreview) { p.Workspace = ws }
}

func withPath(path string) func(*SessionPreview) {
	return func(p *SessionPreview) { p.SourcePath = path }
}

func TestSelectSessions(t *testing.T) {
	recent := preview("recent", filterNow.Add(-2*time.Hour))
	older := preview("older", filterNow.Add(-30*24*time.Hour))
	ancient := preview("ancient", filterNow.Add(-200*24*time.Hour))
	active := preview("active", filterNow)
	active.Active = true
	repoA := preview("repo-a", filterNow.Add(-3*time.Hour), withWorkspace("/src/Alpha"))
	repoB := preview("repo-b", filterNow.Add(-4*time.Hour), withWorkspace("/src/beta"))

	tests := []struct {
		name        string
		filter      Filter
		in          []SessionPreview
		wantIDs     []string
		wantSkipped map[string]SkipReason
	}{
		{
			name:    "most recent first",
			filter:  NewFilter(),
			in:      []SessionPreview{ancient, recent, older},
			wantIDs: []string{"recent", "older", "ancient"},
		},
		{
			name:        "an active session is skipped by default",
			filter:      NewFilter(),
			in:          []SessionPreview{active, recent},
			wantIDs:     []string{"recent"},
			wantSkipped: map[string]SkipReason{"active": SkipActiveSession},
		},
		{
			name:    "the zero filter keeps an active session",
			filter:  Filter{},
			in:      []SessionPreview{active},
			wantIDs: []string{"active"},
		},
		{
			name:        "since excludes sessions that ended earlier",
			filter:      Filter{Since: filterNow.Add(-DefaultSinceWindow)},
			in:          []SessionPreview{recent, older, ancient},
			wantIDs:     []string{"recent", "older"},
			wantSkipped: map[string]SkipReason{"ancient": SkipOutOfRange},
		},
		{
			name:        "until excludes sessions that started later",
			filter:      Filter{Until: filterNow.Add(-10 * 24 * time.Hour)},
			in:          []SessionPreview{recent, older},
			wantIDs:     []string{"older"},
			wantSkipped: map[string]SkipReason{"recent": SkipOutOfRange},
		},
		{
			name:        "workspace matches case-insensitively on a substring",
			filter:      Filter{Workspace: "alpha"},
			in:          []SessionPreview{repoA, repoB},
			wantIDs:     []string{"repo-a"},
			wantSkipped: map[string]SkipReason{"repo-b": SkipWorkspace},
		},
		{
			name:        "source paths restrict to the named files",
			filter:      Filter{SourcePaths: []string{"/projects/recent.jsonl"}},
			in:          []SessionPreview{recent, older},
			wantIDs:     []string{"recent"},
			wantSkipped: map[string]SkipReason{"older": SkipSourcePath},
		},
		{
			name:   "a source directory selects the files under it",
			filter: Filter{SourcePaths: []string{"/projects/keep"}},
			in: []SessionPreview{
				preview("in", filterNow.Add(-time.Hour), withPath("/projects/keep/a.jsonl")),
				preview("out", filterNow.Add(-2*time.Hour), withPath("/projects/drop/b.jsonl")),
			},
			wantIDs:     []string{"in"},
			wantSkipped: map[string]SkipReason{"out": SkipSourcePath},
		},
		{
			// Discovery returns paths built with filepath.Join, so the
			// directory match has to use the platform separator rather than a
			// literal slash.
			name:   "a source directory selects the files under it on any platform",
			filter: Filter{SourcePaths: []string{filepath.Join("projects", "keep")}},
			in: []SessionPreview{
				preview("in", filterNow.Add(-time.Hour), withPath(filepath.Join("projects", "keep", "a.jsonl"))),
				preview("out", filterNow.Add(-2*time.Hour), withPath(filepath.Join("projects", "drop", "b.jsonl"))),
			},
			wantIDs:     []string{"in"},
			wantSkipped: map[string]SkipReason{"out": SkipSourcePath},
		},
		{
			name:        "max sessions keeps the freshest",
			filter:      Filter{MaxSessions: 1},
			in:          []SessionPreview{ancient, recent, older},
			wantIDs:     []string{"recent"},
			wantSkipped: map[string]SkipReason{"older": SkipMaxSessions, "ancient": SkipMaxSessions},
		},
		{
			name:    "no input selects nothing",
			filter:  NewFilter(),
			wantIDs: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			selected, skipped := tc.filter.SelectSessions(tc.in)
			if got := sessionIDs(selected); !equalStrings(got, tc.wantIDs) {
				t.Fatalf("selected = %v, want %v", got, tc.wantIDs)
			}
			gotSkipped := map[string]SkipReason{}
			for _, s := range skipped {
				gotSkipped[s.Session.SessionID] = s.Reason
			}
			if len(gotSkipped) != len(tc.wantSkipped) {
				t.Fatalf("skipped = %v, want %v", gotSkipped, tc.wantSkipped)
			}
			for id, reason := range tc.wantSkipped {
				if gotSkipped[id] != reason {
					t.Fatalf("session %q skipped for %q, want %q", id, gotSkipped[id], reason)
				}
			}
		})
	}
}

func TestSelectSessionsDoesNotMutateInput(t *testing.T) {
	in := []SessionPreview{
		preview("a", filterNow.Add(-10*time.Hour)),
		preview("b", filterNow.Add(-1*time.Hour)),
	}
	NewFilter().SelectSessions(in)
	if in[0].SessionID != "a" || in[1].SessionID != "b" {
		t.Fatalf("SelectSessions reordered its input: %v", sessionIDs(in))
	}
}

func TestDefaultSinceWindowIsNinetyDays(t *testing.T) {
	if DefaultSinceWindow != 90*24*time.Hour {
		t.Fatalf("DefaultSinceWindow = %v, want 90 days", DefaultSinceWindow)
	}
}
