// Package login owns the interactive Sigil credentials flow used by both
// the explicit `sigil login` subcommand and the auto-prompt that runs
// before `sigil claude` / `sigil pi` when no credentials are configured.
//
// The flow prompts for the connection details (SIGIL_ENDPOINT,
// SIGIL_AUTH_TENANT_ID, SIGIL_AUTH_TOKEN and an optional OTel OTLP endpoint)
// followed by an optional preferences group (content capture mode, session
// tags, and guards), then writes them to the standard dotenv at
// $XDG_CONFIG_HOME/agento11y/config.env (or the legacy
// $XDG_CONFIG_HOME/sigil/config.env when only that file exists; see
// dotenv.FilePath). Existing allowed keys not covered by
// prompts are preserved by the underlying writer.
//
// Prompts use github.com/charmbracelet/huh, the same library gcx uses. The
// flow is interactive-only: callers without a TTY receive ErrNotInteractive
// and should either run from a terminal or set SIGIL_* env vars / write
// $XDG_CONFIG_HOME/agento11y/config.env directly.
package login

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"golang.org/x/term"
)

// Grafana brand orange, applied throughout the prompt theme.
const grafanaOrange = lipgloss.Color("#FF671D")

const pluginURL = "https://<your-stack>.grafana.net/plugins/grafana-sigil-app"

// observabilityURL points at the AI Observability plugin route on the
// user’s Grafana stack — the page where captured generations, traces, and
// scores show up after a `sigil claude` / `sigil pi` session.
const observabilityURL = "https://<your-stack>.grafana.net/a/grafana-sigil-app"

// docsURL points at the plugins directory so users can discover every
// agent adapter we ship (claude-code, codex, cursor, pi, …). Linked as a
// supplemental “read more” after the next-step hint.
const docsURL = "https://github.com/grafana/sigil-sdk/tree/main/plugins"

var (
	bannerBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(grafanaOrange).
			Padding(0, 1).
			MarginBottom(1)
	bannerTitle    = lipgloss.NewStyle().Bold(true)
	bannerSubtitle = lipgloss.NewStyle().Faint(true)
	bannerLabel    = lipgloss.NewStyle().Faint(true)
	bannerURL      = lipgloss.NewStyle().Underline(true)
)

// grafanaTheme returns a huh theme tinted with Grafana orange for the
// active field only. Inactive (blurred) fields drop ThemeCharm's blue
// accents in favour of a faint neutral tone so the focused step is the
// single visual focal point.
func grafanaTheme() *huh.Theme {
	t := huh.ThemeCharm()
	orange := lipgloss.NewStyle().Foreground(grafanaOrange)
	faint := lipgloss.NewStyle().Faint(true)

	// Focused (active) field: Grafana orange accents.
	t.Focused.Title = t.Focused.Title.Foreground(grafanaOrange).Bold(true)
	t.Focused.NoteTitle = t.Focused.NoteTitle.Foreground(grafanaOrange)
	t.Focused.Directory = orange
	t.Focused.SelectSelector = orange.SetString("› ")
	t.Focused.NextIndicator = orange
	t.Focused.PrevIndicator = orange
	t.Focused.SelectedOption = orange
	t.Focused.SelectedPrefix = orange.SetString("✓ ")
	t.Focused.FocusedButton = t.Focused.FocusedButton.Background(grafanaOrange)
	// TextInput.Prompt is applied as a style to bubbles' default "> " prompt.
	// Using SetString here would prepend an extra glyph and produce a
	// double-arrow `› >` on the active row.
	t.Focused.TextInput.Prompt = orange
	t.Focused.TextInput.Cursor = t.Focused.TextInput.Cursor.Foreground(grafanaOrange)

	// Blurred (inactive) fields: kill the inherited blue, use faint instead
	// so completed and upcoming steps fade into the background.
	t.Blurred.Title = faint.Bold(true)
	t.Blurred.NoteTitle = faint
	t.Blurred.Description = faint
	t.Blurred.SelectSelector = faint
	t.Blurred.SelectedOption = faint
	t.Blurred.UnselectedOption = faint
	t.Blurred.NextIndicator = faint
	t.Blurred.PrevIndicator = faint
	t.Blurred.TextInput.Prompt = faint
	t.Blurred.TextInput.Text = faint
	t.Blurred.TextInput.Placeholder = faint
	t.Blurred.TextInput.Cursor = faint
	return t
}

// RunOpts controls the login flow.
type RunOpts struct {
	// ConfigPath overrides the dotenv path; empty resolves to
	// dotenv.FilePath().
	ConfigPath string

	// ShowNextStep prints a `Try sigil claude or sigil pi.` hint after a
	// successful save so users know what to run next. Set by the explicit
	// `sigil login` command; left false when login auto-fires from a
	// launcher (the launcher is about to start the agent anyway, so the
	// hint would just be noise).
	ShowNextStep bool

	// Stdin is consulted for the TTY check. nil resolves to os.Stdin.
	Stdin *os.File

	// Stderr receives the welcome banner and, when ShowNextStep is set,
	// the post-save hint. The huh form renders on /dev/tty, not here.
	Stderr io.Writer

	// Logger records dotenv read/write diagnostics.
	Logger *log.Logger
}

// Sentinels callers can branch on.
var (
	// ErrAborted indicates the user pressed Ctrl-C / Esc out of the form.
	ErrAborted = errors.New("login: user aborted")

	// ErrNotInteractive indicates stdin is not a terminal so we cannot
	// prompt. Callers should suggest running from an interactive shell or
	// configuring SIGIL_* env vars / the dotenv file directly.
	ErrNotInteractive = errors.New("login: cannot prompt; stdin is not a terminal")
)

// Run executes the login flow. On success the dotenv file is rewritten and
// the resolved values are also exported into the current process env so a
// subsequent in-process launcher dispatch sees them without re-loading.
func Run(_ context.Context, opts RunOpts) error {
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	configPath := opts.ConfigPath
	if configPath == "" {
		configPath = dotenv.FilePath()
	}

	if !term.IsTerminal(int(opts.Stdin.Fd())) {
		return ErrNotInteractive
	}

	// Seed prompt fields from the existing dotenv (and any SIGIL_* vars
	// already set in the process env) so re-running login — or the launcher
	// auto-prompt triggered by a partial env — shows the user's current
	// configuration instead of empty fields. Tokens are intentionally NOT
	// pre-seeded into the form field because huh's password echo would just
	// show asterisks for a value the user didn't type; we offer "Press Enter
	// to keep existing" semantics via the validator and a post-form restore
	// instead.
	existing := loadSeeds(configPath, opts.Logger)
	endpoint := existing["SIGIL_ENDPOINT"]
	tenantID := existing["SIGIL_AUTH_TENANT_ID"]
	existingToken := existing["SIGIL_AUTH_TOKEN"]
	var token string
	otelEndpoint := existing["SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT"]

	tokenDesc := "API token → Create a token in Cloud Access Policies on the page above"
	tokenValidate := requireNonEmpty("auth token")
	if existingToken != "" {
		tokenDesc = "Press Enter to keep existing token"
		tokenValidate = func(string) error { return nil }
	}

	// Optional preferences. These default to the same values the plugin
	// resolves when the keys are absent, so leaving the second group at its
	// defaults is a no-op rather than a behaviour change.
	contentMode := normalizeContentMode(existing["SIGIL_CONTENT_CAPTURE_MODE"])
	tags := existing["SIGIL_TAGS"]
	guards := seedGuards(existing["SIGIL_GUARDS_ENABLED"], existing["SIGIL_GUARDS_FAIL_OPEN"])
	guardTimeout := strings.TrimSpace(existing["SIGIL_GUARDS_TIMEOUT_MS"])
	if guardTimeout == "" {
		guardTimeout = strconv.Itoa(envconfig.DefaultGuardsTimeoutMs)
	}

	// Only metadata_only and full are offered. The advanced no_tool_content
	// and full_with_metadata_spans modes are still honoured if already set —
	// append the current one so re-running login preserves it instead of
	// silently downgrading to the first option.
	contentOptions := []huh.Option[string]{
		huh.NewOption("Metadata only — no prompts, responses, or tool I/O (default)", contentModeMetadataOnly),
		huh.NewOption("Full — capture everything", contentModeFull),
	}
	switch contentMode {
	case contentModeNoToolContent:
		contentOptions = append(contentOptions, huh.NewOption("No tool content — capture generations, drop tool args and results", contentModeNoToolContent))
	case contentModeFullWithMetadataSpans:
		contentOptions = append(contentOptions, huh.NewOption("Full to ingest, metadata-only spans — keep content off OTel traces", contentModeFullWithMetadataSpans))
	}

	// Banner is printed once, on stderr, BEFORE huh takes over rendering.
	// huh stays in inline mode so this text remains static terminal
	// scrollback above the form (the URL stays selectable, redraws inside
	// huh's render area don't clobber the selection above it).
	banner, bannerLines := welcomeBanner()
	fmt.Fprintln(opts.Stderr, banner)

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Endpoint").
				Description("Copy 'API URL' from the page above").
				Validate(requireURL).
				Value(&endpoint),
			huh.NewInput().
				Title("Tenant ID").
				Description("Copy 'Instance ID' from the page above").
				Validate(requireNonEmpty("tenant id")).
				Value(&tenantID),
			huh.NewInput().
				Title("Auth token").
				Description(tokenDesc).
				EchoMode(huh.EchoModePassword).
				Validate(tokenValidate).
				Value(&token),
			huh.NewInput().
				Title("OTLP endpoint").
				Description("For SDK traces and metrics. Press Enter to skip.").
				Validate(allowEmptyURL).
				Value(&otelEndpoint),
		),
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Content capture").
				Description("What leaves this machine for each generation").
				Options(contentOptions...).
				Value(&contentMode),
			huh.NewInput().
				Title("Session tags").
				Description("Applied to every generation, e.g. team=ai,project=demo. Press Enter to skip.").
				Validate(validateTags).
				Value(&tags),
			huh.NewSelect[string]().
				Title("Guards").
				Description("Pre-tool-use safety checks").
				Options(
					huh.NewOption("Disabled (default)", guardsOff),
					huh.NewOption("Enabled, fail-open — allow the action when a guard errors or times out", guardsOpen),
					huh.NewOption("Enabled, fail-closed — block the action when a guard errors or times out", guardsClosed),
				).
				Value(&guards),
			huh.NewInput().
				Title("Guard timeout (ms)").
				Description("How long to wait for guards before applying the fail mode. Only used when guards are enabled.").
				Validate(func(s string) error {
					// The timeout is ignored while guards are disabled, so don't
					// let a stale or invalid value block submission then.
					if guards == guardsOff {
						return nil
					}
					return validateGuardTimeout(s)
				}).
				Value(&guardTimeout),
		),
	).WithTheme(grafanaTheme())
	formErr := form.Run()

	// Erase the banner regardless of outcome. The banner is one-shot
	// onboarding guidance; leaving it in the terminal after the form
	// exits is clutter. After huh's inline-mode exit the cursor is back
	// at the row right below the banner, so cursor-up by bannerLines +
	// erase-to-end-of-screen scrubs exactly the banner.
	fmt.Fprintf(opts.Stderr, "\033[%dA\033[J", bannerLines)

	if formErr != nil {
		if errors.Is(formErr, huh.ErrUserAborted) {
			return ErrAborted
		}
		return fmt.Errorf("login form: %w", formErr)
	}
	if strings.TrimSpace(token) == "" {
		token = existingToken
	}

	// Trim before persisting. Validators only trim locally, and
	// paste-from-terminal inputs can carry trailing newlines or spaces that
	// would otherwise corrupt SIGIL_ENDPOINT and break export requests.
	endpoint = strings.TrimSpace(endpoint)
	tenantID = strings.TrimSpace(tenantID)
	token = strings.TrimSpace(token)
	otelEndpoint = strings.TrimSpace(otelEndpoint)

	updates := buildUpdates(formValues{
		endpoint:     endpoint,
		tenantID:     tenantID,
		token:        token,
		otelEndpoint: otelEndpoint,
		contentMode:  normalizeContentMode(contentMode),
		tags:         strings.TrimSpace(tags),
		guards:       guards,
		guardTimeout: guardTimeout,
	})
	if err := dotenv.WriteDotenv(configPath, updates, opts.Logger); err != nil {
		return err
	}

	// Mirror into process env so a following in-process launcher dispatch
	// inherits the new credentials without re-loading the file.
	for k, v := range updates {
		if strings.TrimSpace(v) == "" {
			_ = os.Unsetenv(k)
			continue
		}
		_ = os.Setenv(k, v)
	}

	if opts.ShowNextStep {
		printNextStep(opts.Stderr)
	}
	return nil
}

// printNextStep emits the post-login hint: what to run, where the data
// shows up, and where to read more. Commands are bold orange so the eye
// lands on what to type; surrounding copy and URLs are faint so the lines
// read as secondary suggestions rather than another banner.
func printNextStep(w io.Writer) {
	faint := lipgloss.NewStyle().Faint(true)
	cmd := lipgloss.NewStyle().Bold(true).Foreground(grafanaOrange)
	link := lipgloss.NewStyle().Faint(true).Underline(true)
	fmt.Fprintln(w,
		faint.Render("Now you can try ")+
			cmd.Render("agento11y claude")+
			faint.Render(" or ")+
			cmd.Render("agento11y pi")+
			faint.Render(" to launch a coding agent."),
	)
	fmt.Fprintln(w, faint.Render("View observability data at ")+link.Render(observabilityURL))
	fmt.Fprintln(w, faint.Render("Read documentation at ")+link.Render(docsURL))
}

// seededSuffixes are the alias families loadSeeds resolves from the dotenv
// file and overlays from the process env. Package-level so tests can iterate
// it to clear both spellings hermetically per case.
var seededSuffixes = []string{
	"ENDPOINT",
	"AUTH_TENANT_ID",
	"AUTH_TOKEN",
	"OTEL_EXPORTER_OTLP_ENDPOINT",
	"CONTENT_CAPTURE_MODE",
	"TAGS",
	"GUARDS_ENABLED",
	"GUARDS_FAIL_OPEN",
	"GUARDS_TIMEOUT_MS",
}

// Content capture mode labels mirror sigil.ContentCaptureMode.String() so
// the value we persist round-trips through the SDK's UnmarshalText and the
// plugin's envconfig.ResolveContentMode without translation.
const (
	contentModeFull                  = "full"
	contentModeNoToolContent         = "no_tool_content"
	contentModeMetadataOnly          = "metadata_only"
	contentModeFullWithMetadataSpans = "full_with_metadata_spans"
)

// Guard select values. The single select encodes both the enabled flag and
// the fail mode so the form has one fewer field than the three underlying
// SIGIL_GUARDS_* keys.
const (
	guardsOff    = "off"
	guardsOpen   = "open"
	guardsClosed = "closed"
)

// formValues holds the resolved field values the form produced. It exists so
// buildUpdates can be unit-tested without driving the huh TUI.
type formValues struct {
	endpoint     string
	tenantID     string
	token        string
	otelEndpoint string
	contentMode  string
	tags         string
	guards       string
	guardTimeout string
}

// buildUpdates maps the form values onto the dotenv keys WriteDotenv expects.
// Every managed value is written under both branded spellings (and empty
// values delete both) so old binaries that only read SIGIL_* keep working.
// Content capture mode and the guard-enabled flag are always written
// explicitly so a downgrade (e.g. full back to metadata_only, or enabled back
// to disabled) actually takes effect instead of being silently preserved.
// When guards are enabled the timeout and fail mode are always written too,
// so clearing the timeout field deletes the key (the runtime default then
// applies) rather than leaving a stale value behind. While guards are off
// only the disabled flag is written, leaving any prior timeout/fail-mode
// untouched and inert.
func buildUpdates(v formValues) map[string]string {
	updates := map[string]string{
		"SIGIL_ENDPOINT":                    v.endpoint,
		"SIGIL_AUTH_TENANT_ID":              v.tenantID,
		"SIGIL_AUTH_TOKEN":                  v.token,
		"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT": v.otelEndpoint, // "" deletes
		"SIGIL_CONTENT_CAPTURE_MODE":        normalizeContentMode(v.contentMode),
		"SIGIL_TAGS":                        strings.TrimSpace(v.tags), // "" deletes
	}
	switch v.guards {
	case guardsOpen, guardsClosed:
		updates["SIGIL_GUARDS_ENABLED"] = "true"
		if v.guards == guardsOpen {
			updates["SIGIL_GUARDS_FAIL_OPEN"] = "true"
		} else {
			updates["SIGIL_GUARDS_FAIL_OPEN"] = "false"
		}
		// Empty deletes, so a cleared field falls back to the runtime default
		// instead of keeping a stale timeout from a previous config.
		updates["SIGIL_GUARDS_TIMEOUT_MS"] = strings.TrimSpace(v.guardTimeout)
	default:
		updates["SIGIL_GUARDS_ENABLED"] = "false"
	}
	return envconfig.ExpandAliases(updates)
}

// normalizeContentMode maps a raw (possibly stale or empty) value onto one of
// the four known modes, falling back to metadata_only — the same default the
// plugin applies when SIGIL_CONTENT_CAPTURE_MODE is unset or unparseable.
func normalizeContentMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case contentModeFull:
		return contentModeFull
	case contentModeNoToolContent:
		return contentModeNoToolContent
	case contentModeFullWithMetadataSpans:
		return contentModeFullWithMetadataSpans
	default:
		return contentModeMetadataOnly
	}
}

// seedGuards derives the guard select value from the persisted enabled and
// fail-open keys. Fail-open defaults to true (matching the plugin), so an
// enabled-but-unspecified config seeds the fail-open option.
func seedGuards(enabledRaw, failOpenRaw string) string {
	if !envconfig.ParseBoolDefault(enabledRaw, false) {
		return guardsOff
	}
	if envconfig.ParseBoolDefault(failOpenRaw, true) {
		return guardsOpen
	}
	return guardsClosed
}

// validateTags accepts an empty value (tags are optional) and otherwise
// requires each comma-separated entry to be key=value with a non-empty key
// and value. The plugin reads SIGIL_TAGS through envconfig.ParseExtraTags,
// which drops pairs with an empty value, so rejecting them here keeps login
// from persisting tags that would never attach to a generation.
func validateTags(s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, val, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(val) == "" {
			return fmt.Errorf("tag %q must be key=value with a non-empty key and value", part)
		}
	}
	return nil
}

// validateGuardTimeout accepts an empty value (the plugin default applies) and
// otherwise requires a positive whole number of milliseconds.
func validateGuardTimeout(s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return errors.New("timeout must be a positive whole number of milliseconds")
	}
	return nil
}

// loadSeeds returns initial values for the login form. It starts from the
// dotenv at configPath and overlays non-empty process env values for the
// keys we prompt for. This matches dotenv.ApplyEnv's precedence (process
// env wins over the file) so when the launcher auto-prompts because one
// SIGIL_* var is missing, the other vars already set in the user's shell
// pre-fill the form instead of appearing empty.
// loadSeeds resolves each seeded family as shell over file, preferred
// spelling first within each source, and keys the result by the legacy
// SIGIL_* name — the form's internal key space.
func loadSeeds(configPath string, logger *log.Logger) map[string]string {
	fileEnv := dotenv.LoadDotenv(configPath, logger)
	seeds := map[string]string{}
	for _, suffix := range seededSuffixes {
		preferred, legacy := envconfig.PreferredKey(suffix), envconfig.LegacyKey(suffix)
		for _, v := range []string{
			strings.TrimSpace(os.Getenv(preferred)),
			strings.TrimSpace(os.Getenv(legacy)),
			strings.TrimSpace(fileEnv[preferred]),
			strings.TrimSpace(fileEnv[legacy]),
		} {
			if v != "" {
				seeds[legacy] = v
				break
			}
		}
	}
	return seeds
}

func requireURL(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("endpoint URL is required")
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("endpoint URL must start with http:// or https://")
	}
	if u.Host == "" {
		return errors.New("endpoint URL must include a host")
	}
	return nil
}

func allowEmptyURL(s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return requireURL(s)
}

func requireNonEmpty(field string) func(string) error {
	return func(s string) error {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("%s is required", field)
		}
		return nil
	}
}

// welcomeBanner returns the rendered banner box plus the cursor advance
// (in terminal lines) that Fprintln(banner) would produce. The line count
// is used by login.Run to issue ANSI escapes that erase the banner once
// the huh form exits.
func welcomeBanner() (string, int) {
	lines := []string{
		bannerTitle.Render("Welcome to Grafana AI Observability"),
		bannerSubtitle.Render("Let's connect your Grafana stack."),
		"",
		bannerLabel.Render("Get credentials at:"),
		bannerURL.Render(pluginURL),
	}
	rendered := bannerBox.Render(strings.Join(lines, "\n"))
	// Fprintln(rendered) appends one extra \n past whatever the rendered
	// string already ends with, so the cursor advances one more line than
	// the embedded newline count.
	return rendered, strings.Count(rendered, "\n") + 1
}
