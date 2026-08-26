package local

import (
	"os"
	"strconv"
	"strings"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// Guard selector values exposed to the viewer. One control encodes both the
// enabled flag and the fail mode, mapping onto the three branded GUARDS_* keys.
// These are the API/UI values defined by the Settings design; they differ from
// the labels sigil login uses internally.
const (
	guardsOff        = "off"
	guardsFailOpen   = "failopen"
	guardsFailClosed = "failclosed"
)

type Theme string

const (
	themeDark   Theme = "dark"
	themeLight  Theme = "light"
	themeSystem Theme = "system"
)

func normalizeTheme(raw Theme) Theme {
	switch Theme(strings.ToLower(strings.TrimSpace(string(raw)))) {
	case themeLight:
		return themeLight
	case themeSystem:
		return themeSystem
	default:
		return themeDark
	}
}

func parseTheme(env map[string]string) Theme {
	raw, _, _ := envconfig.LookupMap(env, "THEME")
	return normalizeTheme(Theme(raw))
}

// tokenMask stands in for AGENTO11Y_AUTH_TOKEN in the live preview. The stored
// token is never sent to the browser, so the preview shows this marker to
// signal a token is present without leaking the value.
const tokenMask = "<set>"

// Tag is one session-tag key=value pair as edited in the UI.
type Tag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Settings is the subset of config.env the local viewer's Settings page
// edits. It is hydrated from the dotenv file by ParseSettings and mapped back
// to AGENTO11Y_* keys by Updates.
//
// The auth token is write-only: Token is never populated on read (the daemon
// never sends the stored token to the browser). TokenSet reports whether a
// token exists on disk so the UI can show "set" state and the preview can
// render a masked marker. The token is tri-state on write: a new Token value
// replaces it, TokenCleared removes it, and otherwise the stored token is left
// untouched.
type Settings struct {
	Theme        Theme  `json:"theme"`
	Endpoint     string `json:"endpoint"`
	TenantID     string `json:"tenantId"`
	OtlpEndpoint string `json:"otlpEndpoint"`
	TokenSet     bool   `json:"tokenSet"`
	Token        string `json:"token"`
	TokenCleared bool   `json:"tokenCleared"`
	// OtlpHeaders mirrors OTEL_EXPORTER_OTLP_HEADERS, which carries its own
	// copy of the OTLP credential: Basic auth over `id:token`, where the id
	// can differ from the ingest tenant ID and the token from AUTH_TOKEN.
	// It is write-only and tri-state like
	// the token: OtlpHeadersSet reports that a value is on disk, a new
	// OtlpHeaders replaces it, OtlpHeadersCleared deletes it, and otherwise
	// the key is omitted so the stored value survives a save. No form field
	// edits it; the connect flow fills it from a pasted block, disconnect
	// clears it, and a write that replaces or removes the auth token drops it
	// too, because a stale value would keep authenticating OTLP with a token
	// the user has replaced or removed (otel.ExporterHeaders prefers an
	// explicit Authorization header over the synthesized one).
	OtlpHeaders        string `json:"otlpHeaders"`
	OtlpHeadersSet     bool   `json:"otlpHeadersSet"`
	OtlpHeadersCleared bool   `json:"otlpHeadersCleared"`
	Capture            string `json:"capture"`
	Tags               []Tag  `json:"tags"`
	Guards             string `json:"guards"`
	GuardTimeout       string `json:"guardTimeout"`
	Debug              bool   `json:"debug"`
	AutoUpdate         bool   `json:"autoUpdate"`
	UserID             string `json:"userId"`
	// LocalForward mirrors AGENTO11Y_LOCAL_FORWARD: when on, a `--local`
	// daemon also forwards the telemetry it captures to Grafana Cloud. The
	// local viewer always keeps full content; Capture reduces the forwarded
	// copy (full vs metadata_only) and, being the shared
	// CONTENT_CAPTURE_MODE key, also applies to non-local Cloud sessions.
	LocalForward bool `json:"localForward"`
}

// ParseSettings hydrates Settings from a dotenv map (as returned by
// dotenv.LoadDotenv). Each field resolves its alias family preferred-first
// (AGENTO11Y_* over SIGIL_*). Unset or unrecognised values fall back to the
// same effective defaults the plugins apply at runtime, so a config.env with
// none of these keys yields the default configuration. Boolean parsing reuses
// envconfig so the viewer and the hook runtime agree on what counts as
// enabled.
func ParseSettings(env map[string]string) Settings {
	fam := func(suffix string) string {
		v, _, _ := envconfig.LookupMap(env, suffix)
		return v
	}
	return Settings{
		Theme:        parseTheme(env),
		Endpoint:     fam("ENDPOINT"),
		TenantID:     fam("AUTH_TENANT_ID"),
		OtlpEndpoint: fam("OTEL_EXPORTER_OTLP_ENDPOINT"),
		TokenSet:     fam("AUTH_TOKEN") != "",
		// Token and OtlpHeaders are intentionally left empty: both carry a
		// credential, and neither is read back to the browser.
		OtlpHeadersSet: strings.TrimSpace(env["OTEL_EXPORTER_OTLP_HEADERS"]) != "",
		Capture:        parseCaptureMode(fam("CONTENT_CAPTURE_MODE")),
		Tags:           parseTags(fam("TAGS")),
		Guards:         seedGuards(fam("GUARDS_ENABLED"), fam("GUARDS_FAIL_OPEN")),
		GuardTimeout:   fam("GUARDS_TIMEOUT_MS"),
		Debug:          envconfig.ParseBoolDefault(fam("DEBUG"), false),
		// AUTO_UPDATE is opt-out: unset means enabled. This matches
		// updatecheck.Disabled (only explicit falsey values disable updates).
		AutoUpdate:   envconfig.ParseBoolDefault(fam("AUTO_UPDATE"), true),
		UserID:       fam("USER_ID"),
		LocalForward: envconfig.ParseBoolDefault(fam("LOCAL_FORWARD"), false),
	}
}

// Updates returns preferred and unbranded updates. An empty value signals a
// deletion: WriteDotenv removes that key and dotenv.RenderManaged drops it from
// the preview. The returned map therefore covers the complete set of decisions
// for the page-managed keys (write a value, or delete it); any key not listed
// here is left untouched on disk.
func (s Settings) Updates() map[string]string {
	u := map[string]string{
		"AGENTO11Y_THEME":                       string(normalizeTheme(s.Theme)),
		"AGENTO11Y_TAGS":                        renderTags(s.Tags),
		"AGENTO11Y_ENDPOINT":                    strings.TrimSpace(s.Endpoint),
		"AGENTO11Y_AUTH_TENANT_ID":              strings.TrimSpace(s.TenantID),
		"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": strings.TrimSpace(s.OtlpEndpoint),
	}

	// Capture mode is written only when explicitly set. Leaving it unset keeps
	// the runtime defaults intact (metadata_only for Cloud, full for --local via
	// env.go), so saving unrelated settings never silently forces a capture mode
	// onto config.env.
	if c := parseCaptureMode(s.Capture); c != "" {
		u["AGENTO11Y_CONTENT_CAPTURE_MODE"] = c
	}

	// The token is write-only and tri-state: an explicit reset deletes it, a
	// freshly entered value replaces it, and otherwise the key is omitted so
	// WriteDotenv preserves whatever is already on disk.
	switch {
	case s.TokenCleared:
		u["AGENTO11Y_AUTH_TOKEN"] = ""
	case strings.TrimSpace(s.Token) != "":
		u["AGENTO11Y_AUTH_TOKEN"] = strings.TrimSpace(s.Token)
	}

	// OTEL_EXPORTER_OTLP_HEADERS has no branded spelling.
	if v, ok := s.otlpHeadersUpdate(); ok {
		u["OTEL_EXPORTER_OTLP_HEADERS"] = v
	}

	switch s.Guards {
	case guardsFailOpen, guardsFailClosed:
		u["AGENTO11Y_GUARDS_ENABLED"] = "true"
		if s.Guards == guardsFailOpen {
			u["AGENTO11Y_GUARDS_FAIL_OPEN"] = "true"
		} else {
			u["AGENTO11Y_GUARDS_FAIL_OPEN"] = "false"
		}
		u["AGENTO11Y_GUARDS_TIMEOUT_MS"] = guardTimeoutValue(s.GuardTimeout)
	default:
		// Disabled: clear the enabled flag and remove the fail mode and
		// timeout so a later re-enable starts from the documented defaults
		// rather than a stale value.
		u["AGENTO11Y_GUARDS_ENABLED"] = "false"
		u["AGENTO11Y_GUARDS_FAIL_OPEN"] = ""
		u["AGENTO11Y_GUARDS_TIMEOUT_MS"] = ""
	}

	// AGENTO11Y_DEBUG is opt-in: write it only when on, delete otherwise.
	if s.Debug {
		u["AGENTO11Y_DEBUG"] = "true"
	} else {
		u["AGENTO11Y_DEBUG"] = ""
	}

	// AGENTO11Y_AUTO_UPDATE is opt-out: unset means enabled, so write false only
	// when disabled and delete the key otherwise.
	if s.AutoUpdate {
		u["AGENTO11Y_AUTO_UPDATE"] = ""
	} else {
		u["AGENTO11Y_AUTO_UPDATE"] = "false"
	}

	// AGENTO11Y_LOCAL_FORWARD is written explicitly in both directions rather
	// than deleted when off, the same trick AGENTO11Y_AUTO_UPDATE uses above.
	// The local daemon materializes its own environment from config.env at boot
	// and falls back to it, so a deleted key cannot express "off": only an
	// explicit false on disk turns a running daemon's forwarding off.
	if s.LocalForward {
		u["AGENTO11Y_LOCAL_FORWARD"] = "true"
	} else {
		u["AGENTO11Y_LOCAL_FORWARD"] = "false"
	}

	u["AGENTO11Y_USER_ID"] = strings.TrimSpace(s.UserID)

	return u
}

// otlpHeadersUpdate decides what a write does to OTEL_EXPORTER_OTLP_HEADERS:
// the value to persist, and whether the key is part of the write at all. An
// omitted key leaves the stored value on disk.
//
// The headers are tri-state like the token, plus one rule that ties them to
// it: a write that replaces or removes the auth token and carries no headers
// of its own deletes them. A saved header set carries its own copy of the OTLP
// credential and otel.ExporterHeaders prefers it over the Basic auth it
// synthesizes from the tenant ID and the token, so keeping it would authenticate
// OTLP with the token the user just replaced or removed. This is the same rule
// `agento11y login` applies when a new token is entered and no block was pasted.
//
// A write that carries headers is the connect flow pasting a block, and those
// headers win: they came from the same block as the token.
func (s Settings) otlpHeadersUpdate() (string, bool) {
	switch {
	case s.OtlpHeadersCleared:
		return "", true
	case strings.TrimSpace(s.OtlpHeaders) != "":
		return strings.TrimSpace(s.OtlpHeaders), true
	case s.TokenCleared || strings.TrimSpace(s.Token) != "":
		return "", true
	default:
		return "", false
	}
}

// previewUpdates returns the preferred keys to render in the live config.env
// preview. It mirrors Updates but never exposes a credential: a stored or
// freshly entered token is shown masked, and so are the OTLP headers, which
// carry a credential of their own. The panel signals that the key is present
// without leaking the value.
func (s Settings) previewUpdates() map[string]string {
	u := s.Updates()
	if !s.TokenCleared && (strings.TrimSpace(s.Token) != "" || s.TokenSet) {
		u["AGENTO11Y_AUTH_TOKEN"] = tokenMask
	} else {
		delete(u, "AGENTO11Y_AUTH_TOKEN")
	}
	// The headers are masked when the file keeps a value: one this write
	// carries, or the stored one when the write does not touch the key.
	if v, ok := s.otlpHeadersUpdate(); (ok && v != "") || (!ok && s.OtlpHeadersSet) {
		u["OTEL_EXPORTER_OTLP_HEADERS"] = tokenMask
	} else {
		delete(u, "OTEL_EXPORTER_OTLP_HEADERS")
	}
	return u
}

// parseCaptureMode maps a raw value onto one of the SDK's canonical capture
// mode strings using agento11y.ContentCaptureMode as the single source of truth,
// or "" when the mode is unset, the "default" alias, or unrecognised.
//
// Returning "" for an unset mode is deliberate: Updates omits the key in that
// case so the runtime defaults stand (metadata_only for Cloud,
// full for --local via env.go). Forcing a value here would let an unrelated
// save silently enable full content capture for Cloud sessions.
func parseCaptureMode(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var m agento11y.ContentCaptureMode
	if err := m.UnmarshalText([]byte(raw)); err != nil || m == agento11y.ContentCaptureModeDefault {
		return ""
	}
	return m.String()
}

// parseTags splits the SIGIL_TAGS CSV into ordered key=value pairs, dropping
// malformed entries (no '=', empty key, or empty value) so the parsed set
// matches what envconfig.ParseExtraTags applies at runtime. Order is preserved
// so reloading the page does not shuffle the user's tags. Always returns a
// non-nil slice so the JSON encoding is [] rather than null.
func parseTags(raw string) []Tag {
	raw = strings.TrimSpace(raw)
	tags := []Tag{}
	if raw == "" {
		return tags
	}
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if !ok || k == "" || v == "" {
			continue
		}
		tags = append(tags, Tag{Key: k, Value: v})
	}
	return tags
}

// renderTags serialises tags as the CSV key=value form SIGIL_TAGS expects,
// dropping pairs with an empty key or value (envconfig.ParseExtraTags would
// drop them at read time anyway). Returns "" when nothing remains so the key
// is deleted rather than written empty.
func renderTags(tags []Tag) string {
	parts := make([]string, 0, len(tags))
	for _, t := range tags {
		k := strings.TrimSpace(t.Key)
		v := strings.TrimSpace(t.Value)
		if k == "" || v == "" {
			continue
		}
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ",")
}

// guardTimeoutValue returns the AGENTO11Y_GUARDS_TIMEOUT_MS value to persist: the
// trimmed input when it is a positive integer other than the default, else ""
// so the key is dropped and the runtime default applies. A non-numeric,
// non-positive, or default value is treated as "use default".
func guardTimeoutValue(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 || n == envconfig.DefaultGuardsTimeoutMs {
		return ""
	}
	return raw
}

// seedGuards derives the guard selector value from the persisted enabled and
// fail-open keys. Fail-open defaults to true (matching the plugins), so an
// enabled-but-unspecified config seeds the fail-open option.
func seedGuards(enabledRaw, failOpenRaw string) string {
	if !envconfig.ParseBoolDefault(enabledRaw, false) {
		return guardsOff
	}
	if envconfig.ParseBoolDefault(failOpenRaw, true) {
		return guardsFailOpen
	}
	return guardsFailClosed
}

// displayConfigPath collapses the user's home prefix to ~ for display so the
// viewer shows ~/.config/agento11y/config.env rather than an absolute path.
func displayConfigPath(path string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, ok := strings.CutPrefix(path, home+string(os.PathSeparator)); ok {
			return "~" + string(os.PathSeparator) + rel
		}
	}
	return path
}
