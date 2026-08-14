package history

import (
	"fmt"
	"testing"
	"time"
)

// cursorCreated is the session start every case here is checked against.
var cursorCreated = time.Date(2026, 1, 12, 8, 30, 0, 0, time.UTC)

// cursorNewID builds the 50-hex OpenAI ID: a version byte, an eight-byte group
// ID, four bytes of Unix seconds, then randomness.
func cursorNewID(prefix string, at time.Time) string {
	return fmt.Sprintf("%s_01101c93959630f101%08x7a1b2c3d4e5f60718293a4b5", prefix, at.Unix())
}

// cursorOldID builds the 48-hex form Cursor wrote in 2025, which puts the same
// four bytes at the front and has no group ID.
func cursorOldID(prefix string, at time.Time) string {
	return fmt.Sprintf("%s_%08xb81881999235dbea9783ee810f14079f7584b414", prefix, at.Unix())
}

func TestCursorClockDatesTheIDsThatCarryATime(t *testing.T) {
	first := cursorCreated.Add(12 * time.Second)
	second := cursorCreated.Add(4 * time.Minute)

	tests := []struct {
		name    string
		created time.Time
		ids     []string
		lookup  string
		want    time.Time
	}{
		{
			name:    "the current layout, from a tool call ID",
			created: cursorCreated,
			ids:     []string{"call_quk1E4T6bsSwIEOeoUeuavFk\n" + cursorNewID("fc", first)},
			lookup:  "call_quk1E4T6bsSwIEOeoUeuavFk\n" + cursorNewID("fc", first),
			want:    first,
		},
		{
			name:    "the 2025 layout, which holds the seconds at the front",
			created: cursorCreated,
			ids:     []string{cursorOldID("rs", first), cursorOldID("fc", second)},
			lookup:  cursorOldID("fc", second),
			want:    second,
		},
		{
			// The session's own clock can be a little ahead of the provider's.
			name:    "an ID issued just before the session's createdAt",
			created: cursorCreated,
			ids:     []string{cursorNewID("rs", cursorCreated.Add(-30*time.Second))},
			lookup:  cursorNewID("rs", cursorCreated.Add(-30*time.Second)),
			want:    cursorCreated.Add(-30 * time.Second),
		},
		{
			name:    "an Anthropic tool call ID carries no time",
			created: cursorCreated,
			ids:     []string{"toolu_01Q9x7cjWCXN7c9v49MAxbiT"},
			lookup:  "toolu_01Q9x7cjWCXN7c9v49MAxbiT",
		},
		{
			name:    "a Gemini tool call ID carries no time",
			created: cursorCreated,
			ids:     []string{"0_tool_3958561d-2026-424f-80db-a7361354f"},
			lookup:  "0_tool_3958561d-2026-424f-80db-a7361354f",
		},
		{
			name:    "an ID with no underscore in it",
			created: cursorCreated,
			ids:     []string{"6920d7bb3f6c5d1e40a27b8c9d0e1f20"},
			lookup:  "6920d7bb3f6c5d1e40a27b8c9d0e1f20",
		},
		{
			// Six days before the session was created. The offset holds
			// something else, and reading it as a time would date the turn
			// before the conversation it is in.
			name:    "a value outside the window is not a time",
			created: cursorCreated,
			ids:     []string{cursorNewID("fc", cursorCreated.Add(-6*24*time.Hour))},
			lookup:  cursorNewID("fc", cursorCreated.Add(-6*24*time.Hour)),
		},
		{
			name:    "a store with no createdAt has nothing to check a time against",
			created: time.Time{},
			ids:     []string{cursorNewID("fc", first)},
			lookup:  cursorNewID("fc", first),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := newCursorClock(tt.created, tt.ids)
			got, ok := clock.lookup(tt.lookup)
			if tt.want.IsZero() {
				if ok {
					t.Fatalf("lookup dated %q as %s, want it left undated", tt.lookup, got)
				}
				return
			}
			if !ok {
				t.Fatalf("lookup did not date %q, want %s", tt.lookup, tt.want)
			}
			if !got.Equal(tt.want) {
				t.Errorf("lookup = %s, want %s", got, tt.want)
			}
		})
	}
}

// TestCursorClockPicksTheLayoutThatDatesTheMost covers the reason the offset is
// chosen per session rather than per ID. Eight random hex digits fall in the
// window about once in 1600 IDs, so a long session holds stray matches at the
// offset the provider does not use. A vote over the session means one of them
// cannot re-date every turn.
func TestCursorClockPicksTheLayoutThatDatesTheMost(t *testing.T) {
	real := []string{
		cursorNewID("rs", cursorCreated.Add(time.Minute)),
		cursorNewID("fc", cursorCreated.Add(2*time.Minute)),
		cursorNewID("fc", cursorCreated.Add(3*time.Minute)),
	}
	// The 2025 layout at the offset the current one uses for randomness: read at
	// offset 0 it dates, read at offset 18 it does not.
	stray := cursorOldID("fc", cursorCreated.Add(90*time.Second))

	clock := newCursorClock(cursorCreated, append([]string{stray}, real...))
	for _, id := range real {
		if _, ok := clock.lookup(id); !ok {
			t.Errorf("the majority layout left %q undated", id)
		}
	}
	if got, ok := clock.lookup(stray); ok {
		t.Errorf("the odd ID out was dated %s by the majority's layout, want it left undated", got)
	}
}

func TestCursorClockMaxIsTheLatestTime(t *testing.T) {
	last := cursorCreated.Add(9 * time.Minute)
	ids := []string{
		cursorNewID("rs", cursorCreated.Add(time.Minute)),
		"call_Kj3mNp7QrStUvWxYzAbCdEfG\n" + cursorNewID("fc", last),
		"toolu_01Q9x7cjWCXN7c9v49MAxbiT",
	}
	clock := newCursorClock(cursorCreated, ids)
	got, ok := clock.max(ids)
	if !ok {
		t.Fatal("max found no time in IDs that carry two")
	}
	if !got.Equal(last) {
		t.Errorf("max = %s, want %s", got, last)
	}

	if _, ok := newCursorClock(cursorCreated, nil).max(nil); ok {
		t.Error("max reported a time for a session with no IDs")
	}
}
