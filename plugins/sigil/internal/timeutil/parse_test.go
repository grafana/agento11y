package timeutil

import (
	"testing"
	"time"
)

func TestParseTimestamp(t *testing.T) {
	def := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Time
	}{
		{"2026-04-28T12:00:00Z", time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)},
		{"2026-04-28T12:00:00.123Z", time.Date(2026, 4, 28, 12, 0, 0, 123_000_000, time.UTC)},
		{"", def},
		{"garbage", def},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := ParseTimestamp(tc.in, def)
			if !got.Equal(tc.want) {
				t.Errorf("got %v; want %v", got, tc.want)
			}
		})
	}
}
