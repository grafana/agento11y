package hook

import (
	"testing"
	"time"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/cursor/fragment"
)

func TestToolSpanWindow(t *testing.T) {
	genEnd := time.Date(2026, 4, 28, 12, 0, 30, 0, time.UTC)
	dur := func(ms float64) *float64 { return &ms }

	cases := []struct {
		name          string
		rec           fragment.ToolRecord
		wantStarted   time.Time
		wantCompleted time.Time
	}{
		{
			name: "uses tool's own completedAt and subtracts duration",
			rec: fragment.ToolRecord{
				CompletedAt: "2026-04-28T12:00:10.500Z",
				DurationMs:  dur(2500),
			},
			wantStarted:   time.Date(2026, 4, 28, 12, 0, 8, 0, time.UTC),
			wantCompleted: time.Date(2026, 4, 28, 12, 0, 10, 500_000_000, time.UTC),
		},
		{
			name: "no duration → started equals completed",
			rec: fragment.ToolRecord{
				CompletedAt: "2026-04-28T12:00:10Z",
			},
			wantStarted:   time.Date(2026, 4, 28, 12, 0, 10, 0, time.UTC),
			wantCompleted: time.Date(2026, 4, 28, 12, 0, 10, 0, time.UTC),
		},
		{
			name: "missing completedAt falls back to gen.CompletedAt",
			rec: fragment.ToolRecord{
				DurationMs: dur(1000),
			},
			wantStarted:   genEnd.Add(-1000 * time.Millisecond),
			wantCompleted: genEnd,
		},
		{
			name: "unparseable completedAt falls back to gen.CompletedAt",
			rec: fragment.ToolRecord{
				CompletedAt: "not-a-timestamp",
				DurationMs:  dur(500),
			},
			wantStarted:   genEnd.Add(-500 * time.Millisecond),
			wantCompleted: genEnd,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotStart, gotEnd := toolSpanWindow(tc.rec, genEnd)
			if !gotStart.Equal(tc.wantStarted) {
				t.Errorf("startedAt = %s; want %s", gotStart, tc.wantStarted)
			}
			if !gotEnd.Equal(tc.wantCompleted) {
				t.Errorf("completedAt = %s; want %s", gotEnd, tc.wantCompleted)
			}
		})
	}
}

// Two tool records with different completedAt timestamps must produce
// distinct, non-overlapping windows so the UI can show the real
// CALL→TOOL→CALL→TOOL interleaving instead of stacking spans at the end.
func TestToolSpanWindow_PreservesInterleaving(t *testing.T) {
	genEnd := time.Date(2026, 4, 28, 12, 0, 30, 0, time.UTC)
	dur := func(ms float64) *float64 { return &ms }

	first := fragment.ToolRecord{
		CompletedAt: "2026-04-28T12:00:05Z",
		DurationMs:  dur(1000),
	}
	second := fragment.ToolRecord{
		CompletedAt: "2026-04-28T12:00:20Z",
		DurationMs:  dur(1000),
	}
	_, firstEnd := toolSpanWindow(first, genEnd)
	secondStart, _ := toolSpanWindow(second, genEnd)
	if !firstEnd.Before(secondStart) {
		t.Errorf("first.completedAt (%s) should precede second.startedAt (%s)", firstEnd, secondStart)
	}
}
