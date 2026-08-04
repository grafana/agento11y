package local

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBucketLaddersAgree pins the one contract between the token endpoint
// and the chart: both bucket on the same ladder, and every step divides the
// next. The client folds server points onto its own bars, so a step that
// does not divide the bar width would split a bucket across two bars. The
// viewer is served as text/babel and no JS toolchain covers it, which
// leaves this test as the only check.
func TestBucketLaddersAgree(t *testing.T) {
	for i := 1; i < len(tokenUsageIntervals); i++ {
		assert.Zero(t, tokenUsageIntervals[i]%tokenUsageIntervals[i-1],
			"%v must divide %v", tokenUsageIntervals[i-1], tokenUsageIntervals[i])
	}

	got := bucketIntervalsFromJSX(t, string(appJSX))
	want := make([]time.Duration, 0, len(tokenUsageIntervals))
	want = append(want, tokenUsageIntervals...)
	assert.Equal(t, want, got, "BUCKET_INTERVALS_MS in web/app.jsx and tokenUsageIntervals must match")
}

// bucketIntervalsFromJSX reads the BUCKET_INTERVALS_MS literal out of the
// embedded viewer and returns it as durations. The entries are arithmetic
// (`5 * 60_000`), so each one is evaluated as a product of its factors.
func bucketIntervalsFromJSX(t *testing.T, src string) []time.Duration {
	t.Helper()
	literal := regexp.MustCompile(`(?s)const BUCKET_INTERVALS_MS = \[(.*?)\]`).FindStringSubmatch(src)
	require.Len(t, literal, 2, "BUCKET_INTERVALS_MS literal not found in web/app.jsx")

	out := []time.Duration{}
	for entry := range strings.SplitSeq(literal[1], ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		ms := int64(1)
		for factor := range strings.SplitSeq(entry, "*") {
			n, err := strconv.ParseInt(strings.ReplaceAll(strings.TrimSpace(factor), "_", ""), 10, 64)
			require.NoErrorf(t, err, "entry %q", entry)
			ms *= n
		}
		out = append(out, time.Duration(ms)*time.Millisecond)
	}
	return out
}
