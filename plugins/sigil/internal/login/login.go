// Package login owns the interactive Sigil credentials flow used by both
// the explicit `sigil login` subcommand and the auto-prompt that runs
// before `sigil claude` / `sigil pi` when no credentials are configured.
//
// The flow prompts for SIGIL_ENDPOINT, SIGIL_AUTH_TENANT_ID, SIGIL_AUTH_TOKEN
// and an optional OTel OTLP endpoint, then writes them to the standard
// dotenv at $XDG_CONFIG_HOME/sigil/config.env. Existing allowed keys not
// covered by prompts (e.g. SIGIL_TAGS, SIGIL_CONTENT_CAPTURE_MODE) are
// preserved by the underlying writer.
//
// Prompts use github.com/charmbracelet/huh, the same library gcx uses. The
// flow is interactive-only: callers without a TTY receive ErrNotInteractive
// and should either run from a terminal or set SIGIL_* env vars / write
// $XDG_CONFIG_HOME/sigil/config.env directly.
package login

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/dotenv"
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
	// dotenv.FilePath("sigil").
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
		configPath = dotenv.FilePath("sigil")
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

	// Banner is printed once, on stderr, BEFORE huh takes over rendering.
	// huh stays in inline mode so this text remains static terminal
	// scrollback above the form (the URL stays selectable, redraws inside
	// huh's render area don't clobber the selection above it).
	banner, bannerLines := welcomeBanner()
	fmt.Fprintln(opts.Stderr, banner)

	form := huh.NewForm(huh.NewGroup(
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
	)).WithTheme(grafanaTheme())
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

	updates := map[string]string{
		"SIGIL_ENDPOINT":                    endpoint,
		"SIGIL_AUTH_TENANT_ID":              tenantID,
		"SIGIL_AUTH_TOKEN":                  token,
		"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT": otelEndpoint, // "" deletes
	}
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
			cmd.Render("sigil claude")+
			faint.Render(" or ")+
			cmd.Render("sigil pi")+
			faint.Render(" to launch a coding agent."),
	)
	fmt.Fprintln(w, faint.Render("View observability data at ")+link.Render(observabilityURL))
	fmt.Fprintln(w, faint.Render("Read documentation at ")+link.Render(docsURL))
}

// seededKeys are the SIGIL_* keys loadSeeds reads from the dotenv file
// and overlays from the process env. Package-level so tests can iterate
// it to clear the env hermetically per case.
var seededKeys = []string{
	"SIGIL_ENDPOINT",
	"SIGIL_AUTH_TENANT_ID",
	"SIGIL_AUTH_TOKEN",
	"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT",
}

// loadSeeds returns initial values for the login form. It starts from the
// dotenv at configPath and overlays non-empty process env values for the
// keys we prompt for. This matches dotenv.ApplyEnv's precedence (process
// env wins over the file) so when the launcher auto-prompts because one
// SIGIL_* var is missing, the other vars already set in the user's shell
// pre-fill the form instead of appearing empty.
func loadSeeds(configPath string, logger *log.Logger) map[string]string {
	seeds := dotenv.LoadDotenv(configPath, logger)
	for _, k := range seededKeys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			seeds[k] = v
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
