package local

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/history"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAppJSXParsesWithBabel(t *testing.T) {
	transcriptJS(t, "")
}

func TestTranscriptDerivationScenarios(t *testing.T) {
	script := `
const assert = require("assert").strict;
const message = (role, parts) => ({ role, parts });
const callPart = (id, name, input = {}) => ({ kind: "tool_call", tool_call: { id, name, input_json: input } });
const resultPart = (id, name, content, isError = false) => ({ kind: "tool_result", tool_result: { tool_call_id: id, name, content_json: content, is_error: isError } });
const generation = (id, input, output) => ({
  generation_id: id,
  agent_name: "cursor",
  started_at: "2026-01-01T00:00:0" + (id === "g1" ? "0" : "2") + "Z",
  completed_at: "2026-01-01T00:00:0" + (id === "g1" ? "1" : "3") + "Z",
  input,
  output,
  total_tokens: 0,
  token_buckets: {},
  parent_generation_ids: [],
});

const same = generation("g1", [message("user", [{ kind: "text", text: "question" }])], [
  message("assistant", [callPart("", "Read"), resultPart("", "Read", "line 1\nline 2")]),
]);
let turns = buildTranscript([same]);
let row = turns[0].blocks.find(block => block.kind === "work").calls[0];
assert.equal(resultBody(row.result), "line 1\nline 2");
assert.equal(resultBody(row.result).split("\n").length, 2);
assert.equal(row.failed, false);

const first = generation("g1", [message("user", [{ kind: "text", text: "question" }])], [message("assistant", [callPart("call-1", "Read")])]);
const second = generation("g2", [message("tool", [resultPart("call-1", "Read", "done")])], [message("assistant", [{ kind: "text", text: "answer" }])]);
turns = buildTranscript([first, second]);
row = turns[0].blocks.find(block => block.kind === "work").calls[0];
assert.equal(resultBody(row.result), "done");

const final = generation("g1", [message("user", [{ kind: "text", text: "question" }])], [message("assistant", [callPart("call-final", "Read")])]);
turns = buildTranscript([final]);
row = turns[0].blocks.find(block => block.kind === "work").calls[0];
assert.equal(row.result, null);
assert.equal(row.failed, false);

assert.deepEqual(
  splitPreamble("<user_info>A</user_info>\n<rules>B</rules>\nCompare <old> and <new>."),
  { preamble: "<user_info>A</user_info>\n<rules>B</rules>\n", prompt: "Compare <old> and <new>." },
);
const only = "<user_info>A</user_info>\n<rules>B</rules>";
assert.deepEqual(splitPreamble(only), { preamble: "", prompt: only });

// Harness blocks carry attributes, and pi's skill block is the common one.
const skill = '<skill name="plan" location="/x/y/SKILL.md">body</skill>';
assert.deepEqual(splitPreamble(skill + "\nfix rebase issues"), { preamble: skill + "\n", prompt: "fix rebase issues" });
assert.deepEqual(splitPreamble(skill), { preamble: "", prompt: skill });

// Markdown link targets: relative forms pass through, and a scheme that can
// run script or carry a payload renders the link with no href at all.
for (const relative of ["/panel", "./notes.md", "../up", "#anchor", "?q=1"]) {
  assert.equal(markdownURL(relative), relative);
}
assert.equal(markdownURL("https://grafana.com/"), "https://grafana.com/");
assert.equal(markdownURL("mailto:a@b.c"), "mailto:a@b.c");
for (const blocked of [
  "javascript:alert(1)",
  "JaVaScRiPt:alert(1)",
  "java\tscript:alert(1)",
  "java\nscript:alert(1)",
  "data:text/html;base64,PHNjcmlwdD4=",
  "vbscript:msgbox(1)",
  "file:///etc/passwd",
  "//evil.example.com",
  "\\\\evil.example.com",
  "",
  "   ",
]) {
  assert.equal(markdownURL(blocked), undefined, "must not render href " + JSON.stringify(blocked));
}

// Raw HTML never becomes an element, and every remote-loading tag is dropped.
assert.equal(MARKDOWN_OPTIONS.disableParsingRawHTML, true);
for (const tag of ["script", "iframe", "img", "style", "object", "embed", "form", "svg"]) {
  assert.equal(MARKDOWN_OPTIONS.overrides[tag].component, BlockedElement, tag + " must be blocked");
}
assert.equal(MARKDOWN_OPTIONS.overrides.a.component, SafeAnchor);

const metrics = buildTranscriptMetrics([same], turns);
assert.equal(metrics.usageAvailable, false);
assert.equal(metrics.totalTokens, 0);
const used = buildTranscriptMetrics([{ ...same, total_tokens: 120 }, { ...second, total_tokens: 30 }], turns);
assert.equal(used.usageAvailable, true);
assert.equal(used.totalTokens, 150);

const mixed = generation("g1", [message("user", [{ kind: "text", text: "question" }])], [message("assistant", [
  { kind: "text", text: "before" },
  { kind: "thinking", thinking: "reason" },
  callPart("a", "Read"),
  callPart("b", "Grep"),
  { kind: "text", text: "after" },
])]);
turns = buildTranscript([mixed]);
assert.deepEqual(turns[0].blocks.map(block => block.kind), ["prose", "reasoning", "work", "prose"]);

// A generation that interleaves prose and tool calls splits into several work
// blocks. Only the first one carries the generation's duration, so a merge with
// the next generation's work adds that generation's time and nothing more.
const split = { ...generation("g1", [message("user", [{ kind: "text", text: "question" }])], [message("assistant", [
  callPart("a", "Read"),
  { kind: "text", text: "middle" },
  callPart("b", "Grep"),
  { kind: "text", text: "" },
  callPart("c", "Glob"),
])]), duration_seconds: 4 };
turns = buildTranscript([split]);
assert.deepEqual(turns[0].blocks.map(block => block.kind), ["work", "prose", "work"]);
assert.deepEqual(turns[0].blocks.filter(block => block.kind === "work").map(block => block.durationSec), [4, 0]);

const splitNext = { ...generation("g2", [message("tool", [resultPart("c", "Glob", "done")])], [message("assistant", [callPart("d", "Read")])]), duration_seconds: 3 };
turns = buildTranscript([split, splitNext]);
assert.deepEqual(turns[0].blocks.filter(block => block.kind === "work").map(block => block.durationSec), [4, 3]);

const sameName = generation("g1", [message("user", [{ kind: "text", text: "question" }])], [message("assistant", [
  callPart("", "Read"),
  callPart("", "Read"),
  resultPart("", "Read", "first"),
  resultPart("", "Read", "second"),
])]);
turns = buildTranscript([sameName]);
assert.deepEqual(
  turns[0].blocks.find(block => block.kind === "work").calls.map(call => resultBody(call.result)),
  ["first", "second"],
);

const repeatFirst = generation("g1", [message("user", [{ kind: "text", text: "question" }])], [message("assistant", [callPart("", "weather")])]);
const repeatSecond = generation("g2", [message("tool", [resultPart("", "weather", "first result")])], [message("assistant", [callPart("", "weather")])]);
const repeatThird = { ...generation("g3", [message("tool", [resultPart("", "weather", "second result")])], [message("assistant", [{ kind: "text", text: "answer" }])]), started_at: "2026-01-01T00:00:04Z", completed_at: "2026-01-01T00:00:05Z" };
turns = buildTranscript([repeatFirst, repeatSecond, repeatThird]);
assert.deepEqual(
  turns[0].blocks.flatMap(block => block.kind === "work" ? block.calls : []).map(call => resultBody(call.result)),
  ["first result", "second result"],
);

const failed = generation("g1", [message("user", [{ kind: "text", text: "question" }])], [message("assistant", [
  callPart("failed-call", "Shell"),
  resultPart("failed-call", "Shell", "bad", true),
])]);
turns = buildTranscript([failed]);
assert.equal(turns[0].failedCount, 1);
assert.equal(turns[0].blocks.find(block => block.kind === "work").calls[0].failed, true);

const callErrorOnly = { ...generation("g1", [message("user", [{ kind: "text", text: "question" }])], []), call_error: "provider unavailable" };
turns = buildTranscript([callErrorOnly]);
assert.equal(turns[0].failedCount, 1);
assert.deepEqual(turns[0].blocks.map(block => block.kind), ["error"]);
assert.equal(turns[0].blocks[0].text, "provider unavailable");

const callErrorAfterResult = {
  ...generation("g1", [message("user", [{ kind: "text", text: "question" }])], [message("assistant", [
    callPart("successful-call", "Read"),
    resultPart("successful-call", "Read", "ok"),
  ])]),
  call_error: "model call failed",
};
turns = buildTranscript([callErrorAfterResult]);
const successfulRow = turns[0].blocks.find(block => block.kind === "work").calls[0];
assert.equal(successfulRow.failed, false);
assert.equal(resultBody(successfulRow.result), "ok");
assert.equal(turns[0].blocks.find(block => block.kind === "error").text, "model call failed");
assert.equal(turns[0].failedCount, 1);

const parent = generation("g1", [message("user", [{ kind: "text", text: "question" }])], []);
const child = {
  ...generation("child", [], []),
  agent_name: "cursor/subagent",
  parent_generation_ids: ["g1"],
  started_at: "2026-01-01T00:00:02Z",
  completed_at: "2026-01-01T00:00:03Z",
};
const nestedFailure = {
  ...generation("nested", [], []),
  agent_name: "cursor/nested",
  parent_generation_ids: ["child"],
  call_error: "nested model call failed",
  started_at: "2026-01-01T00:00:04Z",
  completed_at: "2026-01-01T00:00:05Z",
};
turns = buildTranscript([parent, child, nestedFailure]);
assert.equal(turns[0].failedCount, 1);
assert.equal(turns[0].blocks.find(block => block.kind === "work").subruns[0].failedCount, 1);

assert.equal(missingUsageNotice("cursor"), "No token usage was recorded for this cursor session, so token counts and cost are unavailable.");
assert.equal(missingUsageNotice(""), "No token usage was recorded for this session, so token counts and cost are unavailable.");

const large = generation("g1", [message("user", [{ kind: "text", text: "question" }])], [message("assistant", Array.from({ length: 41 }, (_, index) => callPart("call-" + index, "Read")))]);
turns = buildTranscript([large]);
assert.equal(turns[0].blocks.find(block => block.kind === "work").calls.length, 41);
console.log("TRANSCRIPT_ASSERTIONS_OK");
`
	transcriptJS(t, script)
}

func transcriptJS(t *testing.T, script string) {
	t.Helper()
	babel, err := webStatic.ReadFile("web/vendor/babel.min.js")
	require.NoError(t, err)

	dir := t.TempDir()
	babelPath := filepath.Join(dir, "babel.cjs")
	require.NoError(t, os.WriteFile(babelPath, babel, 0o600))

	scriptPath := filepath.Join(dir, "test.cjs")
	runner := `
const fs = require("fs");
const vm = require("vm");
const Babel = require(process.argv[2]);
const source = fs.readFileSync(process.argv[3], "utf8");
const compiled = Babel.transform(source, { filename: "app.jsx", presets: ["react"] }).code;
if (process.env.RUN_TRANSCRIPT_TESTS === "1") {
  const start = compiled.indexOf("function partKind(");
  const end = compiled.indexOf("// ============================================================\n// Settings", start);
  if (start < 0 || end < 0) throw new Error("transcript function region not found");
  // URL is a browser global the markdown link check relies on.
  const context = { console, require, URL, React: { createElement() {} } };
  vm.createContext(context);
  vm.runInContext(compiled.slice(start, end), context);
  vm.runInContext(fs.readFileSync(process.argv[4], "utf8"), context);
}
`
	require.NoError(t, os.WriteFile(scriptPath, []byte(runner), 0o600))

	appPath := filepath.Join(dir, "app.jsx")
	require.NoError(t, os.WriteFile(appPath, appJSX, 0o600))
	assertPath := filepath.Join(dir, "assert.cjs")
	require.NoError(t, os.WriteFile(assertPath, []byte(script), 0o600))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", scriptPath, babelPath, appPath, assertPath)
	if script != "" {
		cmd.Env = append(os.Environ(), "RUN_TRANSCRIPT_TESTS=1")
	}
	output, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "Babel/transcript checks failed for embedded web/app.jsx:\n%s", output)
	if script != "" {
		require.Contains(t, string(output), "TRANSCRIPT_ASSERTIONS_OK", "transcript assertions did not run")
	}
}

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
// display name may be written into the import UI, whether as rendered text, a
// string literal, an object key, or a comparison.
//
// The whole region is searched, minus style objects, and a name has to stand on
// its own to count (see namesAgentAt). Matching the region as a plain substring
// made any short agent id a false positive: "pi" sits inside
// "/api/v1/history/agents" and "picking", and "cursor" inside `cursor:
// "pointer"`. The word boundary kills the first kind; dropping style objects
// kills the second.
func TestViewerHasNoHardcodedHistoryAgents(t *testing.T) {
	src := string(appJSX)
	require.Contains(t, src, "/api/v1/history/agents", "the viewer must read the agent list from the registry endpoint")

	searchable := searchableImportUI(t, src)
	require.NotEmpty(t, history.Specs(), "no importers registered; this test proves nothing")
	for _, spec := range history.Specs() {
		for _, name := range []string{string(spec.ID), spec.DisplayName} {
			at := namesAgentAt(searchable, name)
			assert.Negative(t, at,
				"the import UI names the %q agent in %q; the agent list comes from GET /api/v1/history/agents",
				name, snippetAround(searchable, at))
		}
	}
}

// TestViewerHardcodedAgentGuardCatchesHardcoding pins the guard itself. A guard
// that stopped catching what it guards is worse than the false positives it was
// narrowed to avoid, and the narrowing (a word boundary, and dropping style
// objects) is what could quietly cost coverage. Each case below is planted into
// the real import UI region, in a form a frontend edit would actually take.
func TestViewerHardcodedAgentGuardCatchesHardcoding(t *testing.T) {
	caught := []struct {
		name    string
		planted string
	}{
		{"select option", `<option value="codex">Codex</option>`},
		{"segmented control entry", `{ value: "codex", label: "Codex" }`},
		{"jsx text with punctuation", `<div>Import (Codex only)</div>`},
		{"jsx text beside an interpolation", `<span>codex sessions {run.count}</span>`},
		{"identifier object key", `const icons = { codex: IconCodex };`},
		{"quoted object key", `const icons = { "codex": IconCodex };`},
		{"comparison", `if (offer.agent === "codex") return null;`},
		{"style object value", `<div style={{ backgroundImage: "url(/codex.svg)" }}/>`},
	}
	for _, tt := range caught {
		t.Run(tt.name, func(t *testing.T) {
			searchable := searchableImportUI(t, plantInImportUI(t, string(appJSX), tt.planted))
			assert.GreaterOrEqual(t, namesAgentAt(searchable, "codex"), 0,
				"the guard missed hardcoding written as %s", tt.planted)
		})
	}

	// The one construct deliberately out of scope, and the reason style objects
	// are dropped: a CSS property name is not an agent name, and "cursor" is
	// both a CSS property and an agent this repo ships a plugin for.
	searchable := searchableImportUI(t, string(appJSX))
	assert.Negative(t, namesAgentAt(searchable, "cursor"),
		`cursor: "pointer" in a style object must not read as a hardcoded agent`)
	planted := searchableImportUI(t, plantInImportUI(t, string(appJSX), `<div>Import from Cursor</div>`))
	assert.GreaterOrEqual(t, namesAgentAt(planted, "cursor"), 0,
		"dropping style objects must not hide an agent named in rendered text")
}

// searchableImportUI returns the part of the viewer this guard reads: the import
// UI region with style objects removed.
//
// The region runs from useHistoryImport to the end of the Settings history tab.
// Agent names elsewhere in the file are explanatory prose about how one agent
// records its transcripts, not a list to keep in sync, so prose that has to name
// an agent belongs outside these bounds.
func searchableImportUI(t *testing.T, src string) string {
	t.Helper()
	start := strings.Index(src, "function useHistoryImport(")
	require.Positive(t, start, "useHistoryImport not found in web/app.jsx")
	end := strings.Index(src, "function SettingsTabPanels(")
	require.Greater(t, end, start, "SettingsTabPanels not found after useHistoryImport")

	searchable := withoutStylePropertyNames(src[start:end])
	// Sentences the tab renders. They pin the strip: a style object whose braces
	// do not balance would swallow the rest of the region, and a swallowed region
	// makes every assertion below pass while checking nothing.
	for _, sentinel := range []string{"Import past sessions", "Cancel import", "Scanning…"} {
		require.Contains(t, searchable, sentinel,
			"the import UI renders %q but it is not in the searched text", sentinel)
	}
	return searchable
}

// plantInImportUI returns src with snippet inserted into the import UI region,
// just before the Settings history tab, so a guard case runs against the real
// file rather than a hand-written stand-in.
func plantInImportUI(t *testing.T, src, snippet string) string {
	t.Helper()
	at := strings.Index(src, "function SettingsHistoryTab(")
	require.Positive(t, at, "SettingsHistoryTab not found in web/app.jsx")
	return src[:at] + snippet + "\n" + src[at:]
}

// namesAgentAt returns the offset in text where the agent is named, or -1 when
// it is not. The name has to stand on its own: bounded by something other than a
// letter or a digit on both sides, so "pi" matches in `value: "pi"` and in
// "Import from Pi" but not in "picking" or "/api/v1/history/agents". The
// comparison ignores case, so a hardcoded "Pi" label is caught by an agent
// registered as "pi".
func namesAgentAt(text, name string) int {
	name = strings.TrimSpace(name)
	if name == "" {
		return -1
	}
	hay, needle := strings.ToLower(text), strings.ToLower(name)
	for off := 0; off < len(hay); {
		i := strings.Index(hay[off:], needle)
		if i < 0 {
			return -1
		}
		i += off
		if !alnumAt(hay, i-1) && !alnumAt(hay, i+len(needle)) {
			return i
		}
		off = i + 1
	}
	return -1
}

func alnumAt(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	c := s[i]
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// snippetAround returns the line at off with its neighbours trimmed, so a
// failure names the offending code instead of dumping 37 KB of JSX.
func snippetAround(text string, off int) string {
	if off < 0 || off >= len(text) {
		return ""
	}
	start := strings.LastIndexByte(text[:off], '\n') + 1
	end := strings.IndexByte(text[off:], '\n')
	if end < 0 {
		end = len(text)
	} else {
		end += off
	}
	return strings.TrimSpace(text[start:end])
}

// withoutStylePropertyNames returns src with the property names inside every
// style={{ … }} attribute blanked out. It is the one exception the guard makes,
// and it exists for one word: the import UI writes cursor: "pointer" twelve
// times, "cursor" is a plausible agent id (this repo ships plugins/cursor), and
// a CSS property name puts no agent name in front of a user.
//
// Only the names go. Values stay, so an agent named in a URL or a class name
// inside a style object is still caught, and so is everything outside the
// attribute: a style attribute ends at }} before the element's children.
//
// The viewer is served as text/babel and no JS toolchain covers it, so this
// counts braces rather than parsing. A style object holding an unbalanced brace
// inside a string would swallow the rest of the region, which is what the
// sentinels in searchableImportUI catch.
func withoutStylePropertyNames(src string) string {
	const open = "style={{"
	var out strings.Builder
	for {
		i := strings.Index(src, open)
		if i < 0 {
			out.WriteString(src)
			return out.String()
		}
		styleStart := i + len(open)
		out.WriteString(src[:styleStart])
		depth, j := 2, styleStart
		for ; j < len(src) && depth > 0; j++ {
			switch src[j] {
			case '{':
				depth++
			case '}':
				depth--
			}
		}
		out.WriteString(blankPropertyNames(src[styleStart:j]))
		src = src[j:]
	}
}

// blankPropertyNames replaces every word followed by a colon with spaces,
// keeping the rest of the text at the same offsets.
func blankPropertyNames(span string) string {
	b := []byte(span)
	for i := 0; i < len(b); {
		if !identByte(b[i]) {
			i++
			continue
		}
		word := i
		for i < len(b) && identByte(b[i]) {
			i++
		}
		after := i
		for after < len(b) && (b[after] == ' ' || b[after] == '\t' || b[after] == '\n' || b[after] == '\r') {
			after++
		}
		if after < len(b) && b[after] == ':' {
			for x := word; x < i; x++ {
				b[x] = ' '
			}
		}
	}
	return string(b)
}

func identByte(c byte) bool {
	return alnumAt(string(c), 0) || c == '_' || c == '$' || c == '-'
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
		"/assets/vendor/markdown-to-jsx.js":          "application/javascript; charset=utf-8",
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
