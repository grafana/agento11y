package history

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func setModTime(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func sessionIDs(sessions []SessionPreview) []string {
	out := make([]string, len(sessions))
	for i, s := range sessions {
		out[i] = s.SessionID
	}
	return out
}

type multiSessionStub struct {
	*stubImporter
	previews func(context.Context, string) ([]SessionPreview, error)
}

func (s *multiSessionStub) Previews(ctx context.Context, path string) ([]SessionPreview, error) {
	return s.previews(ctx, path)
}

func TestDiscoverWalksRootsAndSortsByActivity(t *testing.T) {
	root := t.TempDir()
	base := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	old := writeFile(t, filepath.Join(root, "a", "old.jsonl"), "x")
	recent := writeFile(t, filepath.Join(root, "b", "recent.jsonl"), "x")
	ignored := writeFile(t, filepath.Join(root, "b", "notes.txt"), "x")
	setModTime(t, old, base.Add(-48*time.Hour))
	setModTime(t, recent, base.Add(-1*time.Hour))
	setModTime(t, ignored, base)

	imp := &stubImporter{
		roots: []string{root},
		match: func(path string) bool { return strings.HasSuffix(path, ".jsonl") },
		preview: func(_ context.Context, path string) (SessionPreview, bool, error) {
			return SessionPreview{SessionID: filepath.Base(path)}, true, nil
		},
	}
	got, err := Discover(context.Background(), AgentCodex, imp, DiscoverOptions{
		Now: func() time.Time { return base },
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := []string{"recent.jsonl", "old.jsonl"}
	if ids := sessionIDs(got.Sessions); !equalStrings(ids, want) {
		t.Fatalf("session order = %v, want %v", ids, want)
	}
	for _, s := range got.Sessions {
		if s.Agent != AgentCodex {
			t.Fatalf("session %q has agent %q", s.SessionID, s.Agent)
		}
		if s.SourcePath == "" {
			t.Fatalf("session %q has no source path", s.SessionID)
		}
		if s.LastActivityAt.IsZero() {
			t.Fatalf("session %q has no last activity", s.SessionID)
		}
	}
}

func TestDiscoverExpandsMultiSessionSources(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	path := writeFile(t, filepath.Join(root, "opencode.db"), strings.Repeat("x", 100))
	setModTime(t, path, now.Add(-time.Minute))
	old := now.Add(-time.Hour)

	imp := &multiSessionStub{
		stubImporter: &stubImporter{roots: []string{path}},
		previews: func(context.Context, string) ([]SessionPreview, error) {
			return []SessionPreview{
				{SessionID: "session-b", LastActivityAt: old},
				{SessionID: "session-a", LastActivityAt: old},
				{SessionID: "session-active", LastActivityAt: now.Add(-time.Minute), SizeBytes: 7},
			}, nil
		},
	}

	got, err := Discover(context.Background(), AgentOpenCode, imp, DiscoverOptions{
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if ids := sessionIDs(got.Sessions); !equalStrings(ids, []string{"session-active", "session-a", "session-b"}) {
		t.Fatalf("session order = %v", ids)
	}
	for _, sess := range got.Sessions {
		if sess.Agent != AgentOpenCode || sess.SourcePath != path {
			t.Fatalf("session %q identity = (%q, %q)", sess.SessionID, sess.Agent, sess.SourcePath)
		}
	}
	if got.Sessions[0].SizeBytes != 7 || !got.Sessions[0].Active {
		t.Fatalf("active preview = %+v", got.Sessions[0])
	}
	for _, sess := range got.Sessions[1:] {
		if sess.SizeBytes != 0 {
			t.Fatalf("session %q inherited shared file size %d", sess.SessionID, sess.SizeBytes)
		}
		if sess.Active {
			t.Fatalf("old session %q inherited the shared file activity", sess.SessionID)
		}
	}

	again, err := Discover(context.Background(), AgentOpenCode, imp, DiscoverOptions{
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("second Discover: %v", err)
	}
	if ids := sessionIDs(again.Sessions); !equalStrings(ids, sessionIDs(got.Sessions)) {
		t.Fatalf("second session order = %v, first = %v", ids, sessionIDs(got.Sessions))
	}
}

func TestDiscoverMarksActiveSessions(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		modTime    time.Time
		wantActive bool
	}{
		{name: "written a minute ago is active", modTime: now.Add(-1 * time.Minute), wantActive: true},
		{name: "written an hour ago is idle", modTime: now.Add(-time.Hour), wantActive: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, filepath.Join(root, tc.name, "s.jsonl"), "x")
			setModTime(t, path, tc.modTime)
			imp := &stubImporter{roots: []string{filepath.Dir(path)}}
			got, err := Discover(context.Background(), AgentCodex, imp, DiscoverOptions{
				Now: func() time.Time { return now },
			})
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if len(got.Sessions) != 1 {
				t.Fatalf("found %d sessions, want 1", len(got.Sessions))
			}
			if got.Sessions[0].Active != tc.wantActive {
				t.Fatalf("Active = %v, want %v", got.Sessions[0].Active, tc.wantActive)
			}
		})
	}
}

func TestDiscoverCollectsPreviewWarnings(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "good.jsonl"), "x")
	writeFile(t, filepath.Join(root, "bad.jsonl"), "x")

	imp := &stubImporter{
		roots: []string{root},
		preview: func(_ context.Context, path string) (SessionPreview, bool, error) {
			if strings.HasSuffix(path, "bad.jsonl") {
				return SessionPreview{}, false, errors.New("schema drift")
			}
			return SessionPreview{SessionID: "good"}, true, nil
		},
	}
	got, err := Discover(context.Background(), AgentCodex, imp, DiscoverOptions{})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got.Sessions) != 1 {
		t.Fatalf("found %d sessions, want 1", len(got.Sessions))
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "schema drift") {
		t.Fatalf("warnings = %v", got.Warnings)
	}
}

func TestDiscoverSkipsUnusableFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "empty.jsonl"), "")
	writeFile(t, filepath.Join(root, "real.jsonl"), "x")

	imp := &stubImporter{
		roots: []string{root},
		preview: func(_ context.Context, path string) (SessionPreview, bool, error) {
			if strings.HasSuffix(path, "empty.jsonl") {
				return SessionPreview{}, false, nil
			}
			return SessionPreview{SessionID: "real"}, true, nil
		},
	}
	got, err := Discover(context.Background(), AgentCodex, imp, DiscoverOptions{})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if ids := sessionIDs(got.Sessions); !equalStrings(ids, []string{"real"}) {
		t.Fatalf("sessions = %v, want [real]", ids)
	}
	if len(got.Warnings) != 0 {
		t.Fatalf("an unusable file produced warnings: %v", got.Warnings)
	}
}

func TestDiscoverMissingRootIsNotAnError(t *testing.T) {
	imp := &stubImporter{roots: []string{filepath.Join(t.TempDir(), "never-ran")}}
	got, err := Discover(context.Background(), AgentCodex, imp, DiscoverOptions{})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got.Sessions) != 0 || len(got.Warnings) != 0 {
		t.Fatalf("missing root produced %d sessions and %v warnings", len(got.Sessions), got.Warnings)
	}
}

func TestDiscoverDeduplicatesOverlappingRoots(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	writeFile(t, filepath.Join(nested, "s.jsonl"), "x")

	imp := &stubImporter{roots: []string{root, nested}}
	got, err := Discover(context.Background(), AgentCodex, imp, DiscoverOptions{})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got.Sessions) != 1 {
		t.Fatalf("found %d sessions, want 1", len(got.Sessions))
	}
}

func TestDiscoverHonoursCancellation(t *testing.T) {
	root := t.TempDir()
	for i := range 5 {
		writeFile(t, filepath.Join(root, "s", string(rune('a'+i))+".jsonl"), "x")
	}
	ctx, cancel := context.WithCancel(context.Background())
	imp := &stubImporter{
		roots: []string{root},
		preview: func(_ context.Context, path string) (SessionPreview, bool, error) {
			cancel()
			return SessionPreview{SessionID: filepath.Base(path)}, true, nil
		},
	}
	if _, err := Discover(ctx, AgentCodex, imp, DiscoverOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Discover error = %v, want context.Canceled", err)
	}
}

func TestDiscoverWithoutImporter(t *testing.T) {
	if _, err := Discover(context.Background(), AgentCodex, nil, DiscoverOptions{}); err == nil {
		t.Fatal("Discover(nil importer) returned nil error")
	}
}

func TestReadPreviewWindows(t *testing.T) {
	tests := []struct {
		name      string
		lines     int
		budget    int64
		wantWhole bool
	}{
		{name: "small file is read whole", lines: 10, budget: 4096, wantWhole: true},
		{name: "large file is sampled", lines: 4000, budget: 4096, wantWhole: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			for i := range tc.lines {
				b.WriteString(strings.Repeat("x", 40))
				b.WriteString(string(rune('0' + i%10)))
				b.WriteString("\n")
			}
			path := writeFile(t, filepath.Join(t.TempDir(), "s.jsonl"), b.String())

			win, err := ReadPreviewWindows(path, tc.budget)
			if err != nil {
				t.Fatalf("ReadPreviewWindows: %v", err)
			}
			if win.Whole != tc.wantWhole {
				t.Fatalf("Whole = %v, want %v", win.Whole, tc.wantWhole)
			}
			read := int64(len(win.Head) + len(win.Tail))
			if read > tc.budget {
				t.Fatalf("read %d bytes, over the %d byte budget", read, tc.budget)
			}
			if len(win.HeadLines()) == 0 {
				t.Fatal("no head lines")
			}
			if tc.wantWhole {
				if len(win.TailLines()) != 0 {
					t.Fatal("whole-file window returned separate tail lines")
				}
				if got := len(win.HeadLines()); got != tc.lines {
					t.Fatalf("head lines = %d, want %d", got, tc.lines)
				}
				total, approx := win.EstimateTotal(len(win.HeadLines()))
				if approx || total != tc.lines {
					t.Fatalf("EstimateTotal = (%d, %v), want (%d, false)", total, approx, tc.lines)
				}
				return
			}
			if len(win.TailLines()) == 0 {
				t.Fatal("sampled window returned no tail lines")
			}
			// Every returned line must be complete, so the partial line at
			// each window boundary is dropped.
			for _, line := range append(win.HeadLines(), win.TailLines()...) {
				if len(line) != 41 {
					t.Fatalf("partial line of %d bytes leaked into the preview", len(line))
				}
			}
			total, approx := win.EstimateTotal(len(win.HeadLines()))
			if !approx {
				t.Fatal("EstimateTotal reported an exact count for a sampled window")
			}
			if total < tc.lines/2 || total > tc.lines*2 {
				t.Fatalf("EstimateTotal = %d, want roughly %d", total, tc.lines)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
