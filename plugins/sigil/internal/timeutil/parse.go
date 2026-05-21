// Package timeutil collects small time-handling helpers shared by the
// sigil plugin's agent packages.
package timeutil

import "time"

// ParseTimestamp parses an ISO-8601 timestamp (RFC3339Nano first, then
// RFC3339) and falls back to def when the input is empty or unparseable.
func ParseTimestamp(s string, def time.Time) time.Time {
	if s == "" {
		return def
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return def
}
