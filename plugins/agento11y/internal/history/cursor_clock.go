package history

import (
	"strconv"
	"strings"
	"time"
)

// The Cursor store timestamps the session and nothing inside it. The IDs it
// stores do carry a time, because some providers put the issue time in the ID:
//
//	rs_01c1bfbb08e4cacd01692101e712a4819b8f3bcc211529dcd8
//	   |--- 8 bytes ---||- secs -|
//	rs_6895cde1b81881999235dbea9783ee810f14079f7584b414
//	   |- secs -|
//
// Both are OpenAI IDs, from two Cursor eras. The 50-hex form puts four bytes of
// Unix seconds at hex offset 18, after a version byte and an eight-byte group
// ID; the 48-hex form puts them at the front. Anthropic's toolu_01Q9x7cj… and
// Gemini's 0_tool_<uuid> carry no time, so a session on those models has no
// clock here at all.
//
// This is not a documented format, and it is not Cursor's. It is checked rather
// than trusted: an offset is used only when the times it produces sit around the
// session's own createdAt, and the offset is chosen per session by which one
// dates the most IDs. On 127 real stores that dated 92% of the turns in the
// sessions with more than four of them, and every session on a GPT model.

// cursorIDSecondsOffsets are the hex offsets to try, newest layout first. An ID
// is 4 bytes of Unix seconds at one of them, and something else at the other.
var cursorIDSecondsOffsets = []int{18, 0}

// cursorIDSecondsWidth is the hex width of the seconds field: 4 bytes.
const cursorIDSecondsWidth = 8

// A decoded time is accepted only inside this window around the session's
// createdAt. It rejects the other offset's bytes, which are a group ID or
// randomness and decode to 1970 or 2106, and it bounds the damage when a provider
// changes the layout: 8 random hex digits fall in the window about once in 1600
// IDs, so a single stray hit must not outvote the real offset.
const (
	cursorIDSkew    = time.Hour
	cursorIDHorizon = 30 * 24 * time.Hour
)

// cursorClock dates a turn from the provider IDs its messages carry.
//
// The zero value is a clock with no evidence: [cursorClock.lookup] finds
// nothing, and the caller falls back to interpolated windows.
type cursorClock struct {
	times map[string]time.Time
}

// newCursorClock picks the layout that dates the most of ids and decodes them
// with it. created is the session start the times are checked against; a store
// with no createdAt gets an empty clock, because there is then nothing to check
// a decoded time against.
func newCursorClock(created time.Time, ids []string) cursorClock {
	if created.IsZero() || len(ids) == 0 {
		return cursorClock{}
	}
	earliest, latest := created.Add(-cursorIDSkew), created.Add(cursorIDHorizon)

	best, bestCount := 0, 0
	for _, offset := range cursorIDSecondsOffsets {
		count := 0
		for _, field := range ids {
			for _, id := range cursorSplitIDs(field) {
				if _, ok := cursorIDTime(id, offset, earliest, latest); ok {
					count++
				}
			}
		}
		if count > bestCount {
			best, bestCount = offset, count
		}
	}
	if bestCount == 0 {
		return cursorClock{}
	}

	times := make(map[string]time.Time, bestCount)
	for _, field := range ids {
		for _, id := range cursorSplitIDs(field) {
			if ts, ok := cursorIDTime(id, best, earliest, latest); ok {
				times[id] = ts
			}
		}
	}
	return cursorClock{times: times}
}

// cursorSplitIDs is the IDs in one ID field. A toolCallId holds two of them
// joined by a newline, one per API the provider answers on, and a time is in at
// most one of the two.
func cursorSplitIDs(field string) []string {
	if field == "" {
		return nil
	}
	var out []string
	for id := range strings.SplitSeq(field, "\n") {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// lookup dates one ID field.
func (c cursorClock) lookup(field string) (time.Time, bool) {
	if len(c.times) == 0 {
		return time.Time{}, false
	}
	for _, id := range cursorSplitIDs(field) {
		if ts, ok := c.times[id]; ok {
			return ts, true
		}
	}
	return time.Time{}, false
}

// max is the latest time in ids, which for the tail of a session is when the
// session stopped. ok is false when none of them carries one.
func (c cursorClock) max(ids []string) (time.Time, bool) {
	var latest time.Time
	for _, id := range ids {
		if ts, ok := c.lookup(id); ok && ts.After(latest) {
			latest = ts
		}
	}
	return latest, !latest.IsZero()
}

// cursorIDTime reads the seconds field at one offset of one ID. It reports false
// for an ID that is too short, is not hex there, or dates outside the window,
// which is every ID a provider issues without a time in it.
func cursorIDTime(id string, offset int, earliest, latest time.Time) (time.Time, bool) {
	_, body, ok := strings.Cut(id, "_")
	if !ok || len(body) < offset+cursorIDSecondsWidth {
		return time.Time{}, false
	}
	secs, err := strconv.ParseUint(body[offset:offset+cursorIDSecondsWidth], 16, 64)
	if err != nil {
		return time.Time{}, false
	}
	ts := time.Unix(int64(secs), 0).UTC()
	if ts.Before(earliest) || ts.After(latest) {
		return time.Time{}, false
	}
	return ts, true
}
