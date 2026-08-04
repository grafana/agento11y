package local

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/history"
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

// TestViewerDefaultRangeMatchesImportWindow pins the one number the viewer and
// the importer have to agree on. A history import defaults to the previous 90
// days; a viewer that opened on a narrower window would show an empty list
// right after one, because everything backfilled is older than it. The viewer
// is served as text/babel with no JS toolchain, so this is the only check.
func TestViewerDefaultRangeMatchesImportWindow(t *testing.T) {
	src := string(appJSX)

	defaultRange := regexp.MustCompile(`const DEFAULT_TIME_RANGE = "([^"]+)"`).FindStringSubmatch(src)
	require.Len(t, defaultRange, 2, "DEFAULT_TIME_RANGE not found in web/app.jsx")

	pattern := fmt.Sprintf(`\{ value: %q, label: "[^"]+", ms: ([0-9 */]+) \}`, defaultRange[1])
	entry := regexp.MustCompile(pattern).FindStringSubmatch(src)
	require.Len(t, entry, 2, "no TIME_RANGES entry for %q", defaultRange[1])

	ms := int64(1)
	for factor := range strings.SplitSeq(entry[1], "*") {
		n, err := strconv.ParseInt(strings.ReplaceAll(strings.TrimSpace(factor), "_", ""), 10, 64)
		require.NoErrorf(t, err, "range value %q", entry[1])
		ms *= n
	}
	assert.Equal(t, history.DefaultSinceWindow, time.Duration(ms)*time.Millisecond)
}

// TestViewerHasNoHardcodedHistoryAgents guards the registry contract: adding an
// importer must not need a frontend edit. The viewer reads the agent list from
// the registry endpoint and renders whatever it returns, so no agent id or
// display name may be written into the import UI.
func TestViewerHasNoHardcodedHistoryAgents(t *testing.T) {
	src := string(appJSX)
	require.Contains(t, src, "/api/v1/history/agents", "the viewer must read the agent list from the registry endpoint")

	// The import UI runs from useHistoryImport to the end of the Settings
	// history tab. Agent names elsewhere in the file are explanatory prose
	// about how one agent records its transcripts, not a list to keep in sync.
	start := strings.Index(src, "function useHistoryImport(")
	require.Positive(t, start, "useHistoryImport not found in web/app.jsx")
	end := strings.Index(src, "function SettingsTabPanels(")
	require.Greater(t, end, start, "SettingsTabPanels not found after useHistoryImport")
	importUI := src[start:end]

	require.NotEmpty(t, history.Specs(), "no importers registered; this test proves nothing")
	for _, spec := range history.Specs() {
		assert.NotContains(t, importUI, string(spec.ID),
			"the import UI names the %q agent; the list comes from GET /api/v1/history/agents", spec.ID)
		assert.NotContains(t, importUI, spec.DisplayName,
			"the import UI names %q; display names come from GET /api/v1/history/agents", spec.DisplayName)
	}
}

// TestViewerServesItsOwnAssets pins the offline and privacy contract: the
// viewer renders private session data, so opening it must not reach a CDN, and
// it must work with no network. Every third-party asset ships in the binary.
func TestViewerServesItsOwnAssets(t *testing.T) {
	srv, _ := newTestServer(t)

	for _, host := range []string{"unpkg.com", "fonts.googleapis.com", "fonts.gstatic.com", "cdn.jsdelivr.net"} {
		assert.NotContains(t, string(indexHTML), host, "index.html must not load anything from %s", host)
		assert.NotContains(t, string(appCSS), host, "app.css must not load anything from %s", host)
	}

	assets := map[string]string{
		"/assets/vendor/react.production.min.js":     "application/javascript; charset=utf-8",
		"/assets/vendor/react-dom.production.min.js": "application/javascript; charset=utf-8",
		"/assets/vendor/babel.min.js":                "application/javascript; charset=utf-8",
		"/assets/fonts/inter-latin.woff2":            "font/woff2",
		"/assets/fonts/roboto-mono-latin.woff2":      "font/woff2",
	}
	for path, wantType := range assets {
		t.Run(path, func(t *testing.T) {
			// Every asset the shell references must actually be served.
			require.Contains(t, string(indexHTML)+string(appCSS), path, "nothing references %s", path)

			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			require.Equal(t, http.StatusOK, rr.Code)
			assert.Equal(t, wantType, rr.Header().Get("Content-Type"))
			assert.NotEmpty(t, rr.Body.Bytes())
		})
	}
}

// TestViewerAssetRoutesRejectTraversal covers the one attack surface the
// vendored assets add: the file name comes from the URL.
func TestViewerAssetRoutesRejectTraversal(t *testing.T) {
	srv, _ := newTestServer(t)

	for _, path := range []string{
		"/assets/vendor/../app.jsx",
		"/assets/vendor/..%2Fapp.jsx",
		"/assets/fonts/../vendor/babel.min.js",
		"/assets/vendor/nope.js",
		"/assets/fonts/inter-latin.woff2.js",
		"/assets/vendor/babel.min.js.woff2",
	} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			assert.NotEqual(t, http.StatusOK, rr.Code, "%s must not be served", path)
		})
	}
}
