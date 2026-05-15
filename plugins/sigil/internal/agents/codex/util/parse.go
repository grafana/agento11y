package util

import "time"

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
