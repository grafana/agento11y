// Package login owns the interactive agento11y credentials flow used by both
// the explicit `agento11y login` subcommand and the auto-prompt that runs
// before `agento11y claude` / `agento11y pi` when no credentials are configured.
//
// On first setup the flow asks where sessions go, when RunOpts.OfferLocal is set
// and the platform has a local receiver. Local only writes the LOCAL setting and
// ends the flow. Grafana Cloud continues to the stack and credential questions
// below.
//
// The Cloud flow asks which Grafana stack the user is on. A stack the machine
// already knows about — saved by an earlier run, or configured in gcx — comes
// back as the answer rather than as a question: pre-filled into the input when
// there is one, offered as a list whose last entry is still that input when
// there are several.
//
// It prints that stack's coding-agent setup page and tries to open it in a
// browser. The environment block from that page goes into one paste field,
// which fills SIGIL_ENDPOINT, SIGIL_AUTH_TENANT_ID,
// SIGIL_AUTH_TOKEN, the OTLP endpoint, and OTEL_EXPORTER_OTLP_HEADERS.
// Pasting is optional: whatever the block does not carry is asked for field by
// field. The three credentials are required, the OTLP endpoint is not. A
// preferences group follows: content capture mode, session tags, guards, and
// automatic tags. The stack answer only builds the printed links and is never
// saved.
//
// The flow then asks the endpoint whether it accepts the credentials and
// writes everything to the dotenv at $XDG_CONFIG_HOME/agento11y/config.env
// (or the legacy $XDG_CONFIG_HOME/sigil/config.env when only that file
// exists; see dotenv.FilePath). Existing allowed keys that no field covers
// are preserved by the underlying writer.
//
// A value can arrive three ways: as a RunOpts field the `agento11y login`
// flags fill in, from the saved configuration (config.env plus any
// AGENTO11Y_* / SIGIL_* var already exported in the shell), or typed into the
// form. That list is also the precedence order: flags win over everything
// else, and only what is still missing is prompted for.
//
// Prompts use github.com/charmbracelet/huh, the same library gcx uses. A run
// that still has to prompt needs a terminal: without one it returns
// ErrNotInteractive, and the caller should suggest a terminal, the flags, or
// SIGIL_* env vars. A run whose credentials the endpoint rejects returns
// ErrNotVerified unless the user (or --yes) chooses to save them anyway.
package login

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/grafana/agento11y/plugins/agento11y/internal/browser"
	"github.com/grafana/agento11y/plugins/agento11y/internal/clihelp"
	"github.com/grafana/agento11y/plugins/agento11y/internal/doctor"
	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
	"github.com/grafana/agento11y/plugins/agento11y/internal/skills"
	"github.com/grafana/agento11y/plugins/agento11y/internal/usagestats"
	"golang.org/x/term"
)

// Grafana brand orange, applied throughout the prompt theme.
const grafanaOrange = lipgloss.Color("#FF671D")

// setupPagePath is the Agent Observability page that shows a coding agent's
// credentials as one environment block. A path rather than a URL, because it
// lives on the user's own Grafana stack.
const setupPagePath = "/a/grafana-agento11y-app/setup-coding-agent"

// observabilityPath is the Agent Observability app route on the user's stack,
// where captured generations, traces, and scores show up.
const observabilityPath = "/a/grafana-agento11y-app"

// placeholderStackOrigin stands in for a stack login never learned, so a
// printed link still shows which URL to open. The prompt requires a stack, so
// only a promptless run with no saved stack reaches it.
const placeholderStackOrigin = "https://<your-stack>.grafana.net"

// stackURLKey holds the stack the user named, so a re-run pre-fills the
// question and a promptless run still prints its own links. Written under the
// preferred spelling alone: no SDK reads it, only login does.
const stackURLKey = "AGENTO11Y_STACK_URL"

// setupPageURL and observabilityPageURL build the two links login prints for
// the stack origin the user gave. An empty origin falls back to the
// placeholder host.
func setupPageURL(origin string) string { return stackBase(origin) + setupPagePath }

func observabilityPageURL(origin string) string { return stackBase(origin) + observabilityPath }

func stackBase(origin string) string {
	if origin = strings.TrimSpace(origin); origin != "" {
		return strings.TrimRight(origin, "/")
	}
	return placeholderStackOrigin
}

// stackOrigin turns what the user gives for their Grafana stack (a bare host,
// an origin, or a deep link inside the stack) into the scheme and host the app
// paths are appended to. The result never becomes AGENTO11Y_ENDPOINT: the
// ingest API answers on a different regional host.
func stackOrigin(raw string) (string, error) {
	bad := errors.New("enter your Grafana Cloud URL, e.g. mystack.grafana.net")
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", bad
	}
	if !strings.Contains(s, "://") {
		s = defaultStackScheme(s) + "://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", bad
	}
	// A host is case-insensitive, so lowercase it rather than print a link
	// that carries whatever case the user typed.
	return u.Scheme + "://" + strings.ToLower(u.Host), nil
}

// defaultStackScheme picks the scheme for an answer that carries none.
// Grafana Cloud stacks are HTTPS. A loopback host is a Grafana running on
// this machine, which serves plain HTTP, so `localhost:3000` and
// `http://localhost:3000` resolve to the same origin.
func defaultStackScheme(hostport string) string {
	host, _, _ := strings.Cut(hostport, "/")
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if isLoopbackHostname(strings.Trim(host, "[]")) {
		return "http"
	}
	return "https"
}

// isLoopbackHostname reports whether a bare hostname is this machine.
func isLoopbackHostname(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isLoopbackOrigin reports whether an origin points at a Grafana on this machine.
// Such an origin is a fine answer to type, and login keeps working with one, but
// it is not something to suggest: the question asks for a Grafana Cloud URL, and
// a gcx set up for local development carries a context for a local Grafana that
// would otherwise sit in the list under that title.
func isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return isLoopbackHostname(u.Hostname())
}

// validateStack requires an answer stackOrigin can read. The stack is what
// builds the link to the credentials page, and the page is where the rest of
// the form comes from, so there is nothing useful to show without it.
func validateStack(s string) error {
	_, err := stackOrigin(s)
	return err
}

// openURL opens a URL in the user's browser. It is a package var so tests can
// record the call instead of launching anything.
var openURL = browser.Open

// docsURL points at the plugins directory so users can discover every
// agent adapter we ship (claude-code, codex, cursor, pi, …). Linked as a
// supplemental “read more” after the next-step hint.
const docsURL = "https://github.com/grafana/agento11y/tree/main/plugins"

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
	// A blurred list keeps its answer marked. huh draws every field of the
	// group, and one style for both option states makes an answered question
	// look like an unanswered one, so moving between questions does not show
	// what is set. The mark is a check rather than the cursor the focused list
	// draws: the row is an answer now, not a place the cursor sits. Dim
	// orange, because orange marks the answer on the active field too.
	chosen := faint.Foreground(grafanaOrange)
	t.Blurred.SelectSelector = chosen.SetString("✓ ")
	t.Blurred.SelectedOption = chosen
	t.Blurred.UnselectedOption = faint
	t.Blurred.NextIndicator = faint
	t.Blurred.PrevIndicator = faint
	t.Blurred.TextInput.Prompt = faint
	t.Blurred.TextInput.Text = faint
	t.Blurred.TextInput.Placeholder = faint
	t.Blurred.TextInput.Cursor = faint
	return t
}

// ProbeFunc verifies credentials against the generation-export endpoint. It
// matches doctor.ProbeConversations, which is what production uses; tests
// substitute a stub so login never touches the network. An implementation
// reports a failure as a ProbeResult, never as a nil pointer; a nil result is
// still handled, as a check that produced no verdict.
type ProbeFunc func(ctx context.Context, endpoint, tenant, token string, insecure bool) *doctor.ProbeResult

// Result distinguishes Local only from Cloud forwarding through the local daemon.
type Result struct {
	LocalMode       bool
	UsesLocalDaemon bool
}

// RunOpts controls the login flow.
type RunOpts struct {
	// ConfigPath overrides the dotenv path; empty resolves to
	// dotenv.FilePath().
	ConfigPath string

	// SkipVerify saves the credentials without asking the endpoint whether
	// it accepts them. Set by `agento11y login --no-verify`.
	SkipVerify bool

	// AssumeYes answers the "save anyway?" question that follows a failed
	// verification. Set by `agento11y login --yes`. It answers nothing else.
	AssumeYes bool

	// Probe verifies the credentials before they are written. nil resolves
	// to doctor.ProbeConversations.
	Probe ProbeFunc

	// Endpoint, TenantID, Token, and OTLPEndpoint are values the command
	// line supplied. They are not prompted for, and they outrank the process
	// environment and the existing dotenv file. Supplying Endpoint, TenantID,
	// and Token together runs login without a terminal.
	Endpoint     string
	TenantID     string
	Token        string
	OTLPEndpoint string

	// Block is an environment block from `login --env-file`. Its credential
	// lines count as command-line values, a flag still beats the block, and
	// together they must supply every required credential because nothing
	// can be prompted for. Its other settings are saved as it sets them, apart
	// from the keys blockKeys refuses.
	Block string

	// ShowNextStep prints a `Try sigil claude or sigil pi.` hint after a
	// successful save so users know what to run next. Set by the explicit
	// `agento11y login` command; left false when login auto-fires from a
	// launcher (the launcher is about to start the agent anyway, so the
	// hint would just be noise).
	ShowNextStep bool

	// OfferLocal asks where sessions should go before asking for Cloud
	// credentials. The caller leaves it false when a destination is already set.
	OfferLocal bool

	// OfferLocalDaemon adds the local-daemon choice to Cloud setup paths that
	// normally route live captures. KeepLocalSetting suppresses one-run overrides.
	OfferLocalDaemon bool

	// KeepLocalSetting leaves the saved LOCAL family unchanged after Cloud
	// setup. One-run Cloud overrides set it so later runs still use local mode.
	KeepLocalSetting bool

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
	// prompt. Callers should suggest running from an interactive shell,
	// passing the values as flags, or configuring SIGIL_* env vars / the
	// dotenv file directly.
	ErrNotInteractive = errors.New("login: cannot prompt; stdin is not a terminal")

	// ErrNotVerified indicates the endpoint did not accept the credentials
	// and the save was not overridden, so nothing was written.
	ErrNotVerified = errors.New("login: endpoint did not accept the credentials; nothing was saved")
)

// Run executes the login flow. On success the dotenv file is rewritten and
// the resolved values are also exported into the current process env so a
// subsequent in-process launcher dispatch sees them without re-loading.
func Run(ctx context.Context, opts RunOpts) (Result, error) {
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	if opts.Probe == nil {
		opts.Probe = doctor.ProbeConversations
	}
	configPath := opts.ConfigPath
	if configPath == "" {
		configPath = dotenv.FilePath()
	}

	// Command-line values are authoritative: they beat the process env and
	// the existing dotenv. Validate them before the prompt so a malformed
	// --endpoint fails right away instead of after the whole form is filled
	// in.
	fixed := fixedValues{
		endpoint:     strings.TrimSpace(opts.Endpoint),
		tenantID:     strings.TrimSpace(opts.TenantID),
		token:        strings.TrimSpace(opts.Token),
		otelEndpoint: strings.TrimSpace(opts.OTLPEndpoint),
	}
	if err := fixed.validate(); err != nil {
		return Result{}, err
	}
	var block formValues
	if opts.Block != "" {
		// An --env-file run never prompts for a value: the block and the
		// flags must hold every required value between them.
		if err := validatePastedBlock(fixed, opts.Block); err != nil {
			return Result{}, err
		}
		applyPaste(&block, fixed, opts.Block)
		fixed.endpoint = cmp.Or(fixed.endpoint, block.endpoint)
		fixed.tenantID = cmp.Or(fixed.tenantID, block.tenantID)
		fixed.token = cmp.Or(fixed.token, block.token)
		fixed.otelEndpoint = cmp.Or(fixed.otelEndpoint, block.otelEndpoint)
		if keys, flags := fixed.missingCredentials(); len(keys) > 0 {
			pronoun := "them"
			if len(keys) == 1 {
				pronoun = "it"
			}
			return Result{}, fmt.Errorf("the block does not set %s; add %s to the block or pass %s", strings.Join(keys, " and "), pronoun, strings.Join(flags, " and "))
		}
		// The block's OTLP headers embed its own tenant and token, so a flag
		// that replaces either one leaves them stale. Headers without the
		// block's own OTLP endpoint would go to whichever endpoint was saved
		// before, so they are dropped too.
		if opts.TenantID != "" || opts.Token != "" || block.otelEndpoint == "" {
			block.otlpHeadersPasted = false
		}
	}

	// The form runs only while a required credential is still missing after
	// the flags, so `--endpoint --tenant --token` configures a devcontainer,
	// an SSH session, or a script with no terminal at all.
	askUser := !fixed.completeCredentials()
	isTTY := term.IsTerminal(int(opts.Stdin.Fd()))
	if askUser && !isTTY {
		return Result{}, ErrNotInteractive
	}
	receiverSupported := local.ReceiverSupported()
	offerLocal := shouldOfferLocal(askUser, opts.OfferLocal, receiverSupported)
	offerLocalDaemon := shouldOfferLocalDaemon(askUser, opts.OfferLocalDaemon, receiverSupported, opts.KeepLocalSetting)

	// Seed prompt fields from the existing dotenv (and any SIGIL_* vars
	// already set in the process env) so re-running login — or the launcher
	// auto-prompt triggered by a partial env — shows the user's current
	// configuration instead of empty fields. Tokens are intentionally NOT
	// pre-seeded into the form field because huh's password echo would just
	// show asterisks for a value the user didn't type; we offer "Press Enter
	// to keep existing" semantics via the validator and a post-form restore
	// instead.
	existing, err := loadSeeds(configPath, opts.Logger)
	if err != nil {
		return Result{}, fmt.Errorf("login: read config %q: %w", configPath, err)
	}
	// saved is the file alone, without the shell's env: what a block's
	// missing keys and the credential copies are judged against.
	saved, err := dotenv.ReadDotenv(configPath, opts.Logger)
	if err != nil {
		return Result{}, fmt.Errorf("login: read config %q: %w", configPath, err)
	}

	// existingToken is what a submitted-empty password field falls back to.
	existingToken := existing["SIGIL_AUTH_TOKEN"]

	v := formValues{
		offerLocalDaemon: offerLocalDaemon,
		localDaemon: seedLocalDaemon(
			existing[envconfig.LegacyKey("LOCAL")],
			existing[envconfig.LegacyKey("LOCAL_FORWARD")],
		),
		endpoint:     cmp.Or(fixed.endpoint, existing["SIGIL_ENDPOINT"]),
		tenantID:     cmp.Or(fixed.tenantID, existing["SIGIL_AUTH_TENANT_ID"]),
		token:        fixed.token,
		otelEndpoint: cmp.Or(fixed.otelEndpoint, existing["SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT"]),
		contentMode:  normalizeContentMode(existing["SIGIL_CONTENT_CAPTURE_MODE"]),
		tags:         existing["SIGIL_TAGS"],
		autoTags:     seedAutoTags(existing[envconfig.LegacyKey(envconfig.AutoTagsSuffix)]),
		autoTagNames: seedAutoTagNames(existing[envconfig.LegacyKey(envconfig.AutoTagNamesSuffix)]),
		guards:       seedGuards(existing["SIGIL_GUARDS_ENABLED"], existing["SIGIL_GUARDS_FAIL_OPEN"]),
		guardTimeout: strings.TrimSpace(existing["SIGIL_GUARDS_TIMEOUT_MS"]),
		otlpHeaders:  existing["OTEL_EXPORTER_OTLP_HEADERS"],
		stackURL:     existing[stackURLKey],
	}
	if block.otlpHeadersPasted {
		v.otlpHeaders, v.otlpHeadersPasted = block.otlpHeaders, true
	}
	if opts.Block != "" {
		// --env-file shows no form, so the block's form preferences are
		// written as it spells them, and the ones it leaves out keep their
		// saved values.
		env := parseBlock(opts.Block)
		dropExport := opts.TenantID != "" || opts.Token != "" || block.otelEndpoint == ""
		v.blockSettings, v.blockRefused = blockKeys(env, true, dropExport)
		if stack, _, _ := envconfig.LookupMap(env, "STACK_URL"); stack != "" {
			v.stackURL, _ = stackOrigin(stack)
		}

		// Both automatic-tag keys are written back below, so the one the
		// block leaves out must be the saved one. Seeding it from the shell
		// would make a one-off export permanent.
		savedOn, _, _ := envconfig.LookupMap(saved, envconfig.AutoTagsSuffix)
		savedNames, savedNamesKey, _ := envconfig.LookupMap(saved, envconfig.AutoTagNamesSuffix)
		v.autoTags, v.autoTagNames = seedAutoTags(savedOn), seedAutoTagNames(savedNames)

		// The block's keys then go over those, as applyPaste puts them over
		// the form's seeds, so both runs save the same answer.
		v.autoTagsFromBlock = presetAutoTags(&v, env)
		on, _, _ := envconfig.LookupMap(env, envconfig.AutoTagsSuffix)
		names, namesKey, _ := envconfig.LookupMap(env, envconfig.AutoTagNamesSuffix)
		// The names are saved only while the switch is on (see buildUpdates).
		if on != "" {
			v.blockAutoTagKeys = append(v.blockAutoTagKeys, envconfig.PreferredKey(envconfig.AutoTagsSuffix))
		}
		if names != "" && v.autoTags {
			v.blockAutoTagKeys = append(v.blockAutoTagKeys, envconfig.PreferredKey(envconfig.AutoTagNamesSuffix))
		}
		if names != "" && on == "" && !v.autoTags {
			_, _ = fmt.Fprintf(opts.Stderr, "agento11y login: did not set %s from the block: automatic tags are off; add AGENTO11Y_AUTO_CODING_AGENT_TAGS=true to the block to turn them on.\n", namesKey)
		}
		// A saved list with no supported name seeds no names, and writing
		// that back would save every name in its place.
		if v.autoTagsFromBlock && v.autoTags && len(v.autoTagNames) == 0 {
			return Result{}, fmt.Errorf("login: the saved %s in %q has no supported name; add AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES to the block", savedNamesKey, configPath)
		}
	}

	if askUser {
		if err := promptValues(ctx, &v, fixed, existingToken, opts.Stderr, offerLocal); err != nil {
			return Result{}, err
		}
		if v.localMode {
			return enableLocalMode(configPath, opts)
		}
	}

	if strings.TrimSpace(v.token) == "" {
		v.token = existingToken
	}

	// Trim before persisting. Validators only trim locally, and
	// paste-from-terminal inputs can carry trailing newlines or spaces that
	// would otherwise corrupt SIGIL_ENDPOINT and break export requests.
	v.endpoint = strings.TrimSpace(v.endpoint)
	v.tenantID = strings.TrimSpace(v.tenantID)
	v.token = strings.TrimSpace(v.token)
	v.otelEndpoint = strings.TrimSpace(v.otelEndpoint)

	// A saved OTEL_EXPORTER_OTLP_HEADERS carries its own copy of the OTLP
	// credential and no field asks for it, so a new token would leave OTLP
	// authenticating with the old one and login would still report success:
	// otel.ExporterHeaders prefers explicit headers that carry an Authorization
	// entry, and the credential check only probes the export endpoint. Drop it,
	// so the exporter builds Basic auth from the tenant ID and the new token.
	if !v.otlpHeadersPasted && v.otlpHeaders != "" && v.token != existingToken {
		v.otlpHeaders = ""
		fmt.Fprintln(opts.Stderr, clihelp.New(opts.Stderr).Detail(
			"Removed the saved OTEL_EXPORTER_OTLP_HEADERS: it carried the previous token. "+
				"Paste the block from the setup page if OTLP needs a credential of its own."))
	}

	markCredentialCopies(&v, saved)

	// Ask the endpoint whether it accepts these credentials before the file
	// is written, so a wrong instance ID or a token without sigil:write is
	// reported here instead of showing up as an empty Conversations page.
	verdict, err := verifyCredentials(ctx, opts, v, envconfig.ParseBool(existing["SIGIL_INSECURE"]), isTTY)
	if err != nil {
		return Result{}, err
	}

	updates := buildUpdates(v, opts.KeepLocalSetting)
	if err := dotenv.WriteDotenv(configPath, updates, opts.Logger); err != nil {
		return Result{}, err
	}
	reportBlockKeys(opts.Stderr, v)
	if len(v.droppedCredentialCopies) > 0 {
		fmt.Fprintln(opts.Stderr, clihelp.New(opts.Stderr).Detail(
			"Removed the saved "+strings.Join(v.droppedCredentialCopies, " and ")+": they carried the previous credential."))
	}

	// Mirror into process env so a following in-process launcher dispatch
	// inherits the new credentials without re-loading the file.
	for key, value := range updates {
		if strings.TrimSpace(value) == "" {
			_ = os.Unsetenv(key)
			continue
		}
		_ = os.Setenv(key, value)
	}

	if opts.ShowNextStep {
		printNextStep(opts.Stderr, verdict, v.stackURL)
	}
	return Result{UsesLocalDaemon: v.localDaemonPrompted && v.localDaemon}, nil
}

func shouldOfferLocal(askUser, requested, supported bool) bool {
	return askUser && requested && supported
}

func shouldOfferLocalDaemon(askUser, requested, supported, keepLocalSetting bool) bool {
	return askUser && requested && supported && !keepLocalSetting
}

func enableLocalMode(configPath string, opts RunOpts) (Result, error) {
	updates := envconfig.ExpandAliases(map[string]string{
		envconfig.LegacyKey("LOCAL"):                  "true",
		envconfig.LegacyKey("LOCAL_FORWARD"):          "false",
		envconfig.LegacyKey(envconfig.AutoTagsSuffix): "true",
	})
	if err := dotenv.WriteDotenv(configPath, updates, opts.Logger); err != nil {
		return Result{}, err
	}
	envconfig.SetBothEnv("LOCAL", "true")
	envconfig.SetBothEnv("LOCAL_FORWARD", "false")
	envconfig.SetBothEnv(envconfig.AutoTagsSuffix, "true")
	fmt.Fprintln(opts.Stderr, clihelp.New(opts.Stderr).Success("Sessions will be captured on this machine."))
	return Result{LocalMode: true, UsesLocalDaemon: true}, nil
}

// fixedValues are the values the command line supplied. An empty field means
// the flag was absent, so the seed or the prompt decides that value instead.
type fixedValues struct {
	endpoint     string
	tenantID     string
	token        string
	otelEndpoint string
}

// completeCredentials reports whether the three required credentials all
// arrived as flags, which is what lets login run with no terminal.
func (f fixedValues) completeCredentials() bool {
	return f.endpoint != "" && f.tenantID != "" && f.token != ""
}

// missingCredentials names the required credentials the flags left empty, as
// block keys and as the flags that set them.
func (f fixedValues) missingCredentials() (keys, flags []string) {
	for _, c := range []struct{ key, flag, value string }{
		{"AGENTO11Y_ENDPOINT", "--endpoint", f.endpoint},
		{"AGENTO11Y_AUTH_TENANT_ID", "--tenant", f.tenantID},
		{"AGENTO11Y_AUTH_TOKEN", "--token-stdin", f.token},
	} {
		if c.value == "" {
			keys = append(keys, c.key)
			flags = append(flags, c.flag)
		}
	}
	return keys, flags
}

func (f fixedValues) validate() error {
	if f.endpoint != "" {
		if err := requireURL(f.endpoint); err != nil {
			return fmt.Errorf("--endpoint: %w", err)
		}
	}
	if f.otelEndpoint != "" {
		if err := requireURL(f.otelEndpoint); err != nil {
			return fmt.Errorf("--otlp-endpoint: %w", err)
		}
	}
	return nil
}

// markCredentialCopies decides whether the saved HEADERS and OTEL_AUTH_TOKEN
// are deleted. They carry their own copies of the export credential and no
// field asks for them, so moving to another endpoint or token would keep
// sending the old credential there. buildUpdates deletes them unless this run
// supplies its own, and stderr names the ones the file held.
func markCredentialCopies(v *formValues, saved map[string]string) {
	// The file, not the shell: an exported endpoint must not hide a move
	// away from the stack the saved copies belong to.
	savedValue := func(suffix string) string {
		value, _, _ := envconfig.LookupMap(saved, suffix)
		return value
	}
	v.dropCredentialCopies = v.token != savedValue("AUTH_TOKEN") ||
		v.endpoint != savedValue("ENDPOINT") ||
		// The setup page's raw key holds the OTLP endpoint when the file
		// has no branded one, as a block's does.
		v.otelEndpoint != cmp.Or(savedValue("OTEL_EXPORTER_OTLP_ENDPOINT"), strings.TrimSpace(saved["OTEL_EXPORTER_OTLP_ENDPOINT"]))
	if !v.dropCredentialCopies {
		return
	}
	for _, suffix := range blockExportCredentialSuffixes {
		_, fromBlock := v.blockSettings[envconfig.PreferredKey(suffix)]
		if !fromBlock && (saved[envconfig.PreferredKey(suffix)] != "" || saved[envconfig.LegacyKey(suffix)] != "") {
			v.droppedCredentialCopies = append(v.droppedCredentialCopies, envconfig.PreferredKey(suffix))
		}
	}
}

func canKeepCloudConnection(v formValues, fixed fixedValues, existingToken string) bool {
	if fixed.endpoint != "" || fixed.tenantID != "" || fixed.token != "" {
		return false
	}
	if _, err := stackOrigin(v.stackURL); err != nil {
		return false
	}
	return requireURL(v.endpoint) == nil &&
		strings.TrimSpace(v.tenantID) != "" &&
		strings.TrimSpace(existingToken) != ""
}

// promptValues drives the interactive part of login, updating v in place.
// Fields a flag fixed or the paste supplied are not asked again.
//
// Credential collection uses separate forms because login prints and opens the
// setup page between the stack and paste questions. A paste must also finish
// before huh binds the resulting values to the remaining fields.
func promptValues(ctx context.Context, v *formValues, fixed fixedValues, existingToken string, stderr io.Writer, offerLocal bool) error {
	// Guidance goes to stderr before huh takes over rendering. huh stays in
	// inline mode, so this text remains static scrollback above the form and
	// the URL stays selectable. The deferred escape erases it on any outcome:
	// huh clears its own render area on exit, leaving the cursor right below
	// what we printed, so cursor-up plus erase-to-end-of-screen removes exactly
	// these rows.
	printed := 0
	width := terminalWidth(stderr)
	say := func(s string) {
		fmt.Fprintln(stderr, s)
		printed += rows(s, width)
	}
	defer func() { fmt.Fprintf(stderr, "\033[%dA\033[J", printed) }()

	keepCloudConnection := canKeepCloudConnection(*v, fixed, existingToken)
	say(welcomeBanner(stderr, offerLocal, keepCloudConnection))

	if offerLocal {
		chooseLocal, err := promptDestination()
		if err != nil {
			return err
		}
		if chooseLocal {
			v.localMode = true
			return nil
		}
	}

	// The guard timeout field shows the runtime default when no value is
	// saved, so an empty seed becomes an explicit number the user can edit.
	if strings.TrimSpace(v.guardTimeout) == "" {
		v.guardTimeout = strconv.Itoa(envconfig.DefaultGuardsTimeoutMs)
	}

	if keepCloudConnection {
		change, err := promptCloudConnection(v.stackURL)
		if err != nil {
			return err
		}
		if !change {
			return runSettingsForm(v, nil)
		}
	}

	stack, err := promptStack(ctx, v.stackURL)
	if err != nil {
		return err
	}
	v.stackURL = stack
	say(setupPageLink(stderr, v.stackURL))

	// One masked input rather than a text area: the block is a credential, and
	// a terminal paste into a single-line input turns its newlines into spaces,
	// which applyPaste reads through normalizePastedBlock.
	var pasted string
	pasteForm := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("Paste from Grafana").
			Description("Paste the environment block from that page, then press Enter.").
			EchoMode(huh.EchoModePassword).
			Validate(pastedBlockValidator(fixed)).
			Value(&pasted),
	)).WithTheme(grafanaTheme())
	if err := formError(pasteForm.Run()); err != nil {
		return err
	}
	filled := applyPaste(v, fixed, pasted)

	tokenDesc := "API token → Create a token in Cloud Access Policies on the page above"
	tokenValidate := requireNonEmpty("auth token")
	if existingToken != "" {
		tokenDesc = "Press Enter to keep the existing token"
		tokenValidate = func(string) error { return nil }
	}

	var required []huh.Field
	if fixed.endpoint == "" && !filled.endpoint {
		required = append(required, huh.NewInput().
			Title("Endpoint").
			Description("Copy 'API URL' from the page above").
			Validate(requireURL).
			Value(&v.endpoint))
	}
	if fixed.tenantID == "" && !filled.tenantID {
		required = append(required, huh.NewInput().
			Title("Tenant ID").
			Description("Copy 'Instance ID' from the page above").
			Validate(requireNonEmpty("tenant id")).
			Value(&v.tenantID))
	}
	if fixed.token == "" && !filled.token {
		required = append(required, huh.NewInput().
			Title("Auth token").
			Description(tokenDesc).
			EchoMode(huh.EchoModePassword).
			Validate(tokenValidate).
			Value(&v.token))
	}
	if fixed.otelEndpoint == "" && !filled.otelEndpoint {
		required = append(required, huh.NewInput().
			Title("OTLP endpoint").
			Description("For SDK traces and metrics. Press Enter to skip.").
			Validate(allowEmptyURL).
			Value(&v.otelEndpoint))
	}

	return runSettingsForm(v, required)
}

func runSettingsForm(v *formValues, required []huh.Field) error {
	var groups []*huh.Group
	// A paste can fill every credential, and an empty group would render as a
	// blank step.
	if len(required) > 0 {
		groups = append(groups, huh.NewGroup(required...))
	}
	groups = append(groups, preferenceGroups(v)...)

	if err := formError(huh.NewForm(groups...).WithTheme(grafanaTheme()).Run()); err != nil {
		return err
	}
	v.capturePrompted = true
	v.localDaemonPrompted = v.offerLocalDaemon
	return nil
}

// preferenceGroups builds the questions that follow the credentials: what
// leaves the machine, and what it is labelled with. Every one of them is a
// list or a field, none is a pair of buttons. The group is one column of
// questions the eye runs down, a row of buttons in it reads as the form's
// submit rather than as one more answer, and huh draws the chosen button
// highlighted even while the field is blurred, which reads as the field the
// keyboard is on.
//
// Each list binds Value after Options. Options() aims the cursor and the
// viewport at whatever the field holds when it runs, and Value() afterwards
// moves the cursor alone. In that order the viewport starts at the top, since
// the string lists hold nothing yet and the automatic-tag list holds the zero
// bool, which is its first option. Bound the other way round, a saved answer
// scrolls the options above it out of sight. destinationForm binds Value
// first for the same reason: there the true option leads.
func preferenceGroups(v *formValues) []*huh.Group {
	fields := make([]huh.Field, 0, 6)
	if v.offerLocalDaemon {
		fields = append(fields, huh.NewSelect[bool]().
			Title("Local web UI").
			Description("View coding agent sessions in your browser on this machine").
			Options(localDaemonOptions()...).
			Value(&v.localDaemon))
	}
	fields = append(fields,
		huh.NewSelect[string]().
			Title("Content capture").
			Description("What leaves this machine for each generation").
			Options(contentCaptureOptions(v.contentMode)...).
			Value(&v.contentMode),
		huh.NewInput().
			Title("Session tags").
			Description("Applied to every generation, e.g. team=ai,project=demo. Press Enter to skip.").
			Validate(validateTags).
			Value(&v.tags),
		huh.NewSelect[string]().
			Title("Guards").
			Description("Pre-tool-use safety checks").
			Options(
				huh.NewOption("Disabled (default)", guardsOff),
				huh.NewOption("Enabled, fail-open — allow the action when a guard errors or times out", guardsOpen),
				huh.NewOption("Enabled, fail-closed — block the action when a guard errors or times out", guardsClosed),
			).
			Value(&v.guards),
		huh.NewInput().
			Title("Guard timeout (ms)").
			Description("How long to wait for guards before applying the fail mode. Only used when guards are enabled.").
			Validate(func(s string) error {
				// The timeout is ignored while guards are disabled, so don't
				// let a stale or invalid value block submission then.
				if v.guards == guardsOff {
					return nil
				}
				return validateGuardTimeout(s)
			}).
			Value(&v.guardTimeout),
		huh.NewSelect[bool]().
			Title("Automatic tags").
			Description("Tag every session with the user, the repository, and the branch, and turn those into metric labels so Usage and Cost can be split by person, repository, or branch.\nThe values are stored with the metrics and kept for the metric retention period; the user value is often an email address.").
			Options(autoTagSwitchOptions()...).
			Value(&v.autoTags),
	)
	return []*huh.Group{
		huh.NewGroup(fields...),
		// The names only matter once the switch is on, so this group is skipped
		// while the answer above is Off: the form asks whether to tag at all
		// first and unfolds the list after it.
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Which values to tag with").
				Description("Space toggles a value, Enter confirms. Branch names grow without bound, so drop that one if you do not need per-branch cost.").
				Options(autoTagOptions(v.autoTagNames)...).
				Validate(validateAutoTagNames).
				Value(&v.autoTagNames),
		).WithHideFunc(func() bool { return !v.autoTags }),
	}
}

// Keep No first so Options lays out both rows before Value moves the cursor to
// the default Yes answer.
func localDaemonOptions() []huh.Option[bool] {
	return []huh.Option[bool]{
		huh.NewOption("No: do not use the local web UI", false),
		huh.NewOption("Yes: use the local web UI (default)", true),
	}
}

// Keep Off first: huh matches the zero bool before binding Value and scrolls earlier options out of view.
func autoTagSwitchOptions() []huh.Option[bool] {
	return []huh.Option[bool]{
		huh.NewOption("Off — no user, repository, or branch labels", false),
		huh.NewOption("On — choose which of the three to send next (default)", true),
	}
}

const (
	destinationTitle = "Where should sessions go?"
	// The answer is not a one-way door, and a first-run reader has no way to know
	// that. login rewrites it, and the flags override it for one launch.
	destinationHelp = "You can change this later: agento11y login, or --local per launch."
)

func promptDestination() (bool, error) {
	localMode := true
	if err := formError(destinationForm(&localMode).Run()); err != nil {
		return false, err
	}
	return localMode, nil
}

// destinationForm builds the destination question.
//
// Value comes before Options: Options points both the cursor and the viewport at
// whichever option matches the value bound at that moment, and a select with no
// binding yet holds the zero bool, which is Grafana Cloud. Bind first and both
// land on Local only. Bind last and only the cursor moves back, leaving it on a
// row the viewport has already scrolled past, so the first frame shows Grafana
// Cloud alone until a key press repairs the offset.
func destinationForm(localMode *bool) *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewSelect[bool]().
			Title(destinationTitle).
			Description(destinationHelp).
			Value(localMode).
			Options(destinationOptions()...),
	)).WithTheme(grafanaTheme())
}

func destinationOptions() []huh.Option[bool] {
	return []huh.Option[bool]{
		huh.NewOption("Local only", true),
		huh.NewOption("Grafana Cloud", false),
	}
}

func promptCloudConnection(stackURL string) (bool, error) {
	change := false
	if err := formError(cloudConnectionForm(&change, stackURL).Run()); err != nil {
		return false, err
	}
	return change, nil
}

func cloudConnectionForm(change *bool, stackURL string) *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewSelect[bool]().
			Title("Grafana Cloud connection").
			Description(stackURL).
			Value(change).
			Options(
				huh.NewOption("Keep this connection (default)", false),
				huh.NewOption("Change connection", true),
			),
	)).WithTheme(grafanaTheme())
}

// manualStackChoice is the list value that opens the free-form field. Not the
// empty string: huh moves the cursor to the option whose value equals the bound
// one, so an empty sentinel would take the cursor off the URLs whenever nothing
// is bound. Not a possible origin either, since those all carry a scheme.
const manualStackChoice = "<type it in>"

// stackListRows caps how many rows the stack list shows at once. Past it the list
// scrolls, which cuts it at the bottom, so the last row goes out of sight and
// reaching it takes an arrow key or / to filter. The cap is therefore set above
// any plausible number of gcx contexts, and still low enough to leave the terminal
// the rows promptValues printed above the form.
const stackListRows = 14

// promptStack asks which Grafana stack the user is on and returns its origin.
//
// What it asks depends on how many URLs the machine already knows, from an
// earlier run and from gcx. Several are a list with a field under it. One is that
// field alone, pre-filled: a list of one says no more than the pre-filled field
// does, in three rows instead of one. None is the empty field this flow started
// with.
func promptStack(ctx context.Context, saved string) (string, error) {
	stacks := stackList(gcxStacks(ctx), saved)
	options := stackOptions(stacks)
	if len(options) == 0 {
		value, from := stackPrefill(stacks, saved)
		return promptStackInput(value, from)
	}
	return promptStackList(stacks, options)
}

// stackTitle names what this step asks for. It is the Grafana a user opens in a
// browser, and deliberately not the ingest host the setup page calls "API URL":
// the two are different hosts and login never saves this one as the endpoint.
const stackTitle = "Your Grafana Cloud URL"

// stackActionColor writes the last row of the list, the one that opens the field.
//
// #6E9FFF is the design system's link colour, and a link is what that row is: the
// only one that goes somewhere instead of answering. The colours this screen
// already spends rule the alternatives out. Orange is taken twice over, by the
// title and by the selected row, so an orange row would read as selected wherever
// the cursor really is. Plain text is what the URL rows are. Yellow is the warning
// colour and nothing here is wrong, and green is the success colour.
const stackActionColor = lipgloss.Color("#6E9FFF")

// manualStackLabel is the last row of the list, the one that opens the field. It
// opens with the same orange "> " the field's own prompt uses, so the row shows
// where it leads, and the ellipsis says it leads somewhere rather than answering.
//
// The cost of that prompt glyph: huh draws its cursor on the selected row, so this
// row reads "› > or another URL…" when the cursor is on it.
//
// Rendered per call rather than held in a package var, so it picks up the same
// colour profile as every other string this flow prints. lipgloss decides that
// from the process output, which a package var would freeze at init.
func manualStackLabel() string {
	return lipgloss.NewStyle().Foreground(grafanaOrange).Render("> ") +
		lipgloss.NewStyle().Foreground(stackActionColor).Render("or another URL…")
}

// stackListSeparator closes the column of URLs and holds the last row away from
// it. A rule the width of the longest URL, then a blank line: the rule reads as
// the bottom of the list rather than as a stub, and it can never be wider than a
// row already on screen, so it cannot wrap one.
//
// Its colour is explicit rather than faint because huh renders a label inside the
// style of its row, and faint adds no colour of its own: a faint rule would turn
// orange whenever the URL it hangs under is selected, which reads as part of the
// highlight. Adaptive, so it is not invisible on a light terminal.
func stackListSeparator(stacks []string) string {
	width := 0
	for _, stack := range stacks {
		width = max(width, lipgloss.Width(stack))
	}
	rule := lipgloss.NewStyle().
		Foreground(lipgloss.AdaptiveColor{Light: "250", Dark: "240"}).
		Render(strings.Repeat("─", width))
	return "\n" + rule + "\n"
}

// promptStackList asks the stack question as one block — the URLs this machine
// knows, and a last row for one it does not — and returns the origin of whichever
// the user settled on.
//
// One block, so Enter on a URL answers the step: huh enables Submit on the last
// field of a group and turns Enter into "go to the next field" everywhere else, so
// a field sitting under the list could only be walked into. The field the last row
// opens is a second group instead, which huh shows on its own once that row is
// chosen. Its way back is Esc, from stackKeyMap; submitting it empty comes back
// here and asks again, which is the same way back for a user who reaches for
// Enter.
func promptStackList(stacks []string, options []huh.Option[string]) (string, error) {
	for {
		choice, typed := stacks[0], ""

		list := huh.NewSelect[string]().
			Title(stackTitle).
			Options(options...).
			Value(&choice)
		// Height pads a list shorter than itself with blank rows, so it is only set
		// once the list has to scroll. Its rows are the options plus the two the
		// separator adds, and the title takes one more out of the height.
		if len(options)+2 > stackListRows {
			list = list.Height(stackListRows + 1)
		}

		typing := huh.NewInput().
			Title("Another URL").
			Description("e.g. mystack.grafana.net").
			// Empty has to pass. huh validates a field as it loses focus and then
			// refuses to leave a group holding an error, so a required-value
			// validator here is what would keep Esc from going anywhere.
			Validate(allowEmptyStack).
			Value(&typed)

		form := huh.NewForm(
			huh.NewGroup(list),
			huh.NewGroup(typing).WithHideFunc(func() bool { return choice != manualStackChoice }),
		).WithTheme(grafanaTheme()).WithKeyMap(stackKeyMap())
		if err := formError(form.Run()); err != nil {
			return "", err
		}
		if answer, ok := stackAnswer(choice, typed); ok {
			return answer, nil
		}
	}
}

// stackAnswer resolves what the form collected into one origin. It reports false
// when the user asked to type a URL and then submitted nothing, which is a request
// for the list back rather than an answer.
func stackAnswer(choice, typed string) (string, bool) {
	if choice != manualStackChoice {
		// Every listed value came from stackOrigin already.
		return choice, true
	}
	if s := strings.TrimSpace(typed); s != "" {
		// allowEmptyStack accepted it.
		origin, _ := stackOrigin(s)
		return origin, true
	}
	return "", false
}

// stackKeyMap adds Esc to the back binding of the typed-answer field, and says
// where back goes. Ctrl-C is left as the only key that abandons login.
func stackKeyMap() *huh.KeyMap {
	km := huh.NewDefaultKeyMap()
	km.Input.Prev.SetKeys("shift+tab", "esc")
	km.Input.Prev.SetHelp("esc", "back to the list")
	return km
}

// allowEmptyStack validates the field the last row opens. Empty passes, because
// promptStackList reads it as a request for the list back; anything else has to be
// a URL.
func allowEmptyStack(s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return validateStack(s)
}

// stackPrefill picks the value the free-form input opens with, and where it came
// from. The answer an earlier run saved wins: the user gave it, and stackList
// already put it first, so a gcx stack that is not it would have made a second
// entry and a select rather than reach this.
func stackPrefill(stacks []string, saved string) (string, stackSource) {
	switch {
	case saved != "":
		return saved, stackSourceSaved
	case len(stacks) > 0:
		return stacks[0], stackSourceGcx
	default:
		return "", stackSourceNone
	}
}

// stackSource says where the value pre-filled into the stack input came from.
// The description tells the user, because a field that answers itself has to say
// on whose authority.
type stackSource int

const (
	// stackSourceNone is an empty field: nothing to pre-fill, or a user who
	// asked to type a stack the list did not hold.
	stackSourceNone stackSource = iota
	// stackSourceSaved is the answer an earlier run wrote to the config file.
	stackSourceSaved
	// stackSourceGcx is the one stack gcx is configured for.
	stackSourceGcx
)

func stackDescription(from stackSource) string {
	switch from {
	case stackSourceGcx:
		return "Found in your gcx config. Press Enter to use it."
	case stackSourceSaved:
		return "Press Enter to keep it, or type another stack."
	default:
		return "e.g. mystack.grafana.net"
	}
}

// promptStackInput asks for a stack in a free-form field and returns its origin.
func promptStackInput(value string, from stackSource) (string, error) {
	answer := value
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title(stackTitle).
			Description(stackDescription(from)).
			Validate(validateStack).
			Value(&answer),
	)).WithTheme(grafanaTheme())
	if err := formError(form.Run()); err != nil {
		return "", err
	}
	// validateStack already accepted it.
	origin, _ := stackOrigin(answer)
	return origin, nil
}

// stackList orders the stacks the question can answer itself with: the one an
// earlier run saved, then the ones gcx is configured for, each host once.
// origins arrive deduplicated from parseGcxStacks.
//
// The saved stack leads because it is the likeliest answer, and because huh
// scrolls its list to the selected option while its viewport is no taller than
// the list: a cursor further down hides every row above it, so a re-run would
// show the stacks after the saved one and none of the stacks before it.
func stackList(origins []string, saved string) []string {
	stacks := make([]string, 0, len(origins)+1)
	if saved != "" {
		stacks = append(stacks, saved)
	}
	for _, origin := range origins {
		if origin != saved {
			stacks = append(stacks, origin)
		}
	}
	return stacks
}

// stackOptions builds the list of URLs to offer, with a last row that opens the
// free-form field. Fewer than two URLs gets no list at all, which is what sends
// promptStack to the pre-filled field instead.
//
// The separator belongs to the label above it rather than to a row of its own:
// huh has no unselectable row, and a label that opened with the separator instead
// would leave the cursor pointing at a rule.
func stackOptions(stacks []string) []huh.Option[string] {
	if len(stacks) < 2 {
		return nil
	}
	options := make([]huh.Option[string], 0, len(stacks)+1)
	for i, stack := range stacks {
		label := stack
		if i == len(stacks)-1 {
			label += stackListSeparator(stacks)
		}
		options = append(options, huh.NewOption(label, stack))
	}
	return append(options, huh.NewOption(manualStackLabel(), manualStackChoice))
}

// setupPageLink renders the link naming the coding-agent setup page for origin,
// and opens that page in a browser on the way. Opening is best effort and
// unreported: a machine with no browser still gets a URL to open by hand.
func setupPageLink(w io.Writer, origin string) string {
	target := setupPageURL(origin)
	// The placeholder host resolves to nothing, so only a real origin is opened.
	if origin != "" {
		_ = openURL(target)
	}
	return clihelp.New(w).Heading("Get your credentials at:") + "\n" + target + "\n"
}

// pasteFilled records which values a pasted block supplied, so the form skips
// the fields that already hold an answer.
type pasteFilled struct {
	endpoint     bool
	tenantID     bool
	token        bool
	otelEndpoint bool
	otlpHeaders  bool
}

// applyPaste fills the credential fields from a pasted environment block and
// reports which ones it supplied. A value fixed on the command line is never
// overwritten. Both spellings are read, and AGENTO11Y_* beats SIGIL_* inside
// one block.
//
// OTLP headers are kept as pasted rather than rebuilt from the tenant ID and
// token: the OTLP Basic-auth user is the stack ID, which can differ from the
// ingest tenant ID. They follow --otlp-endpoint rather than a fixed value of
// their own, because a user who points login at their own collector must not
// get a Grafana Cloud token written next to it.
//
// applyPaste fills the form from a pasted block: its credentials, and its
// preferences as answers the user can still change. The block's other
// settings are recorded to be written (see blockKeys).
func applyPaste(v *formValues, fixed fixedValues, block string) pasteFilled {
	env := parseBlock(block)
	var filled pasteFilled
	filled.endpoint = setPasted(&v.endpoint, fixed.endpoint, brandedPaste(env, "ENDPOINT"))
	filled.tenantID = setPasted(&v.tenantID, fixed.tenantID, brandedPaste(env, "AUTH_TENANT_ID"))
	filled.token = setPasted(&v.token, fixed.token, brandedPaste(env, "AUTH_TOKEN"))
	filled.otelEndpoint = setPasted(&v.otelEndpoint, fixed.otelEndpoint,
		cmp.Or(brandedPaste(env, "OTEL_EXPORTER_OTLP_ENDPOINT"), env["OTEL_EXPORTER_OTLP_ENDPOINT"]))
	// OTEL_EXPORTER_OTLP_HEADERS has no branded spelling: it is the raw
	// OpenTelemetry variable, and the block ships it as such.
	filled.otlpHeaders = setPasted(&v.otlpHeaders, fixed.otelEndpoint, env["OTEL_EXPORTER_OTLP_HEADERS"])
	v.otlpHeadersPasted = filled.otlpHeaders
	v.autoTagsFromBlock = presetAutoTags(v, env)
	presetFormPreferences(v, env)
	dropExport := fixed.tenantID != "" || fixed.token != "" || fixed.otelEndpoint != "" ||
		strings.TrimSpace(cmp.Or(brandedPaste(env, "OTEL_EXPORTER_OTLP_ENDPOINT"), env["OTEL_EXPORTER_OTLP_ENDPOINT"])) == ""
	v.blockSettings, v.blockRefused = blockKeys(env, false, dropExport)
	return filled
}

// parseBlock reads a pasted or piped block. An unreadable one is reported by
// validatePastedBlock, which runs first.
func parseBlock(block string) map[string]string {
	env, _ := dotenv.ParseDotenv(strings.NewReader(normalizePastedBlock(block)))
	return env
}

// blockPreferenceSuffixes are the automatic tag switch and names, which decide
// the user, repository, and branch labels on the Coding agents page. They have
// rules of their own (presetAutoTags), so blockKeys leaves them out.
var blockPreferenceSuffixes = []string{envconfig.AutoTagsSuffix, envconfig.AutoTagNamesSuffix}

// presetAutoTags puts the block's automatic-tag answers over the ones v
// holds, and reports whether the block set either key. A key the block leaves
// out keeps the seeded answer.
func presetAutoTags(v *formValues, env map[string]string) bool {
	on, _, _ := envconfig.LookupMap(env, envconfig.AutoTagsSuffix)
	names, _, _ := envconfig.LookupMap(env, envconfig.AutoTagNamesSuffix)
	if on != "" {
		v.autoTags = envconfig.ParseBool(on)
	}
	if names != "" {
		v.autoTagNames = seedAutoTagNames(names)
	}
	return on != "" || names != ""
}

// validateBlockPreferences applies the form's rules to the automatic-tag keys
// a block set: a boolean switch, and at least one supported name. The error
// names the key and leaves the value out, as for a credential.
func validateBlockPreferences(env map[string]string) error {
	if raw, key, _ := envconfig.LookupMap(env, envconfig.AutoTagsSuffix); raw != "" {
		if _, ok := envconfig.ParseBoolValue(raw); !ok {
			return fmt.Errorf("the block's %s is not usable: use true or false", key)
		}
	}
	if raw, key, _ := envconfig.LookupMap(env, envconfig.AutoTagNamesSuffix); raw != "" {
		if enabled, unknown := envconfig.ParseAutoTags(raw); len(unknown) > 0 || len(enabled) == 0 {
			return fmt.Errorf("the block's %s is not usable: use a comma-separated list of user, repo, and branch, or all", key)
		}
	}
	// The form's own rules, checked on every spelling the block carries,
	// since both are written.
	checks := []struct {
		suffix, usage string
		ok            func(string) bool
	}{
		{"CONTENT_CAPTURE_MODE", "use full, metadata_only, no_tool_content, or full_with_metadata_spans", func(raw string) bool {
			return normalizeContentMode(raw) == strings.ToLower(raw)
		}},
		{"TAGS", "use comma-separated key=value pairs", func(raw string) bool { return validateTags(raw) == nil }},
		{"GUARDS_ENABLED", "use true or false", func(raw string) bool { _, ok := envconfig.ParseBoolValue(raw); return ok }},
		{"GUARDS_FAIL_OPEN", "use true or false", func(raw string) bool { _, ok := envconfig.ParseBoolValue(raw); return ok }},
		{"GUARDS_TIMEOUT_MS", "use a positive whole number of milliseconds", func(raw string) bool { return validateGuardTimeout(raw) == nil }},
		{"STACK_URL", "use the https stack URL, such as https://example.grafana.net", func(raw string) bool {
			origin, err := stackOrigin(raw)
			return err == nil && requireBlockURL(origin) == nil
		}},
	}
	for _, check := range checks {
		for _, key := range []string{envconfig.PreferredKey(check.suffix), envconfig.LegacyKey(check.suffix)} {
			if raw := strings.TrimSpace(env[key]); raw != "" && !check.ok(raw) {
				return fmt.Errorf("the block's %s is not usable: %s", key, check.usage)
			}
		}
	}
	return nil
}

// blockReadSuffixes are the branded keys login reads from a block (see
// applyPaste).
var blockReadSuffixes = []string{"ENDPOINT", "AUTH_TENANT_ID", "AUTH_TOKEN", "OTEL_EXPORTER_OTLP_ENDPOINT"}

// blockRefusalReasons are the keys a block never sets, in either spelling,
// and why, which stderr names one line per reason:
// the binary the agent plugins launch, the directory the local app loads its
// web UI from, and the hosts the local app answers, which guard its session
// list and config API; where usage stats and the SDKs' test-suite requests
// (which carry the user's own service account token) go, including
// GRAFANA_URL, which those requests fall back to, and the experiment links
// the SDKs print; the agent name, which guard rules and dashboards filter on;
// and the user ID, which would give everyone set up from the block the same
// user. Whoever can edit a shared block could otherwise run code on, read the
// sessions of, or collect requests from every machine set up from it. A block
// chooses the stack it reports to, and nothing else that leaves the machine.
// LOCAL and LOCAL_FORWARD are written by login itself on every Cloud
// setup, so a block's value would not survive the save.
var blockRefusalReasons = map[string]string{
	"BIN":                            "a block cannot choose what agento11y runs, loads, or answers",
	"LOCAL_WEB_DIR":                  "a block cannot choose what agento11y runs, loads, or answers",
	"LOCAL_ALLOWED_HOSTS":            "a block cannot choose what agento11y runs, loads, or answers",
	"ANONYMOUS_USAGE_STATS_ENDPOINT": "a block cannot send requests anywhere but its stack",
	"CONTROL_ENDPOINT":               "a block cannot send requests anywhere but its stack",
	"GRAFANA_URL":                    "a block cannot send requests anywhere but its stack",
	"EXPERIMENT_URL_TEMPLATE":        "a block cannot choose the links agento11y prints",
	"AGENT_NAME":                     "a block cannot rename the agent that guard rules and dashboards match",
	"USER_ID":                        "a block cannot give everyone who logs in with it the same user",
	"LOCAL":                          "login sets these itself",
	"LOCAL_FORWARD":                  "login sets these itself",
}

const (
	blockReasonDowngrade  = "a block cannot turn off redaction or TLS"
	blockReasonUsageStats = "a block cannot turn on usage stats, which go to Grafana, not its stack"
	blockReasonKeyName    = "a setting name must be upper case"
)

// blockRefusal is a key login did not take from a block, and why.
type blockRefusal struct{ key, reason string }

// blockKeyName is what a setting's name may hold. Windows reads environment
// names without case, and Unicode case folding does not map every letter onto
// ASCII (a dotless ı uppercases to I), so a name in any other form could reach
// a denied setting there while every check here read it as another key.
var blockKeyName = regexp.MustCompile(`^[A-Z0-9_]+$`)

// blockDowngrade reports whether a value would turn off a protection the
// user's machine keeps by default: secret redaction of prompts, TLS to the
// OTLP endpoint or, over gRPC, to the generation endpoint, or a usage stats
// opt-out. The masked paste
// field never shows a block's lines, so login refuses these instead of saving
// them.
func blockDowngrade(key, suffix, value string) bool {
	switch {
	case suffix == "REDACT_INPUT_MESSAGES":
		return !envconfig.ParseBoolDefault(value, true)
	case suffix == "INSECURE", suffix == "OTEL_EXPORTER_OTLP_INSECURE", key == "OTEL_EXPORTER_OTLP_INSECURE":
		return envconfig.ParseBool(value)
	case suffix == usagestats.EnvSuffix:
		// Usage stats go to Grafana, not the block's stack. Turning them off,
		// or to log, which sends nothing, is still saved.
		return usagestats.ResolveMode(usagestats.ShellEnv{Mode: value}, nil) == usagestats.ModeEnabled
	}
	return false
}

// blockFormSuffixes are the settings form's preferences other than automatic
// tags and the stack URL, which have their own handling. A pasted block fills
// them in as answers the user can still change (presetFormPreferences);
// --env-file, which shows no form, writes the ones the block sets.
var blockFormSuffixes = []string{"CONTENT_CAPTURE_MODE", "TAGS", "GUARDS_ENABLED", "GUARDS_FAIL_OPEN", "GUARDS_TIMEOUT_MS"}

// blockExportCredentialSuffixes carry a copy of the export credential, like
// the block's OTLP headers: OTEL_AUTH_TOKEN wins over the token for OTLP, and
// HEADERS can carry an Authorization value. They follow the headers' rule, so
// a flag that replaces the tenant or the token drops them, and so does a block
// without its own OTLP endpoint.
var blockExportCredentialSuffixes = []string{"OTEL_AUTH_TOKEN", "HEADERS"}

// blockDefaultValues are the PROTOCOL and AUTH_MODE values the setup page's
// block always carries. They are the defaults, so login does not write them
// over a saved choice such as PROTOCOL=otel; any other value is written.
var blockDefaultValues = map[string]string{"PROTOCOL": "http", "AUTH_MODE": "basic"}

// blockKeys sorts a block's keys by what login does with them. settings are
// the keys login writes: every key the config file accepts, apart from the
// credential, OTLP, automatic tag, and stack lines, which login reads into
// the form, and, unless writeFormKeys is set, the form preferences, which a
// paste fills into the form instead. A branded key is written once, under
// its AGENTO11Y_ spelling with the value envconfig.LookupMap resolves, so both
// paths save the same thing when a block carries both spellings. Blank values
// and the setup page's defaults are skipped. refused are the keys a block
// cannot set (blockRefusalReasons). Keys the config file
// does not accept are dropped, as the loader would drop them.
func blockKeys(env map[string]string, writeFormKeys, dropExportCredentials bool) (settings map[string]string, refused []blockRefusal) {
	readIntoForm := map[string]bool{"OTEL_EXPORTER_OTLP_ENDPOINT": true, "OTEL_EXPORTER_OTLP_HEADERS": true}
	for _, s := range slices.Concat(blockReadSuffixes, blockPreferenceSuffixes, []string{"STACK_URL"}) {
		readIntoForm[envconfig.PreferredKey(s)], readIntoForm[envconfig.LegacyKey(s)] = true, true
	}
	settings = map[string]string{}
	for _, key := range slices.Sorted(maps.Keys(env)) {
		suffix := brandSuffix(key)
		// A branded key writes its family's resolved value, so the downgrade
		// check reads that one; checking each spelling's own value would let
		// a safe second spelling carry a refused first one through.
		value := strings.TrimSpace(env[key])
		if suffix != "" {
			value, _, _ = envconfig.LookupMap(env, suffix)
		}
		switch {
		case !blockKeyName.MatchString(key):
			// Name it when it reads as one of ours once upper-cased, the way
			// Windows reads it; any other line is dropped, as before.
			if upper := strings.ToUpper(key); brandSuffix(upper) != "" || strings.HasPrefix(upper, "OTEL_") {
				refused = append(refused, blockRefusal{key, blockReasonKeyName})
			}
		case !dotenv.AllowedDotenvKey(key) || readIntoForm[key]:
		case suffix == "" && !strings.HasPrefix(key, "OTEL_"):
			// A bare AGENTO11Y_ or SIGIL_ names no setting.
		case blockRefusalReasons[suffix] != "":
			refused = append(refused, blockRefusal{key, blockRefusalReasons[suffix]})
		case suffix == usagestats.EnvSuffix && blockDowngrade(key, suffix, value):
			refused = append(refused, blockRefusal{key, blockReasonUsageStats})
		case blockDowngrade(key, suffix, value):
			refused = append(refused, blockRefusal{key, blockReasonDowngrade})
		case suffix == "":
			// A raw OTEL_ key, which has no second spelling.
			if value != "" {
				settings[key] = value
			}
		case dropExportCredentials && slices.Contains(blockExportCredentialSuffixes, suffix):
		case slices.Contains(blockFormSuffixes, suffix) && !writeFormKeys:
		default:
			if value == "" || strings.EqualFold(value, blockDefaultValues[suffix]) {
				continue
			}
			if slices.Contains(blockFormSuffixes, suffix) {
				value = formPreferenceValue(suffix, value)
			}
			settings[envconfig.PreferredKey(suffix)] = value
		}
	}
	if len(settings) == 0 {
		settings = nil
	}
	return settings, refused
}

// brandSuffix is key without its AGENTO11Y_ or SIGIL_ prefix, or "" for a key
// with neither.
func brandSuffix(key string) string {
	for _, prefix := range []string{"AGENTO11Y_", "SIGIL_"} {
		if suffix, ok := strings.CutPrefix(key, prefix); ok {
			return suffix
		}
	}
	return ""
}

// formPreferenceValue normalizes a form preference the way the form saves its
// answer. validateBlockPreferences has already accepted raw.
func formPreferenceValue(suffix, raw string) string {
	raw = strings.TrimSpace(raw)
	switch suffix {
	case "CONTENT_CAPTURE_MODE":
		return normalizeContentMode(raw)
	case "GUARDS_ENABLED", "GUARDS_FAIL_OPEN":
		return strconv.FormatBool(envconfig.ParseBool(raw))
	}
	return raw
}

// presetFormPreferences puts the block's form preferences over the answers v
// holds, so a pasted block's values are the form's defaults, which the user
// can still change. A key the block leaves out keeps the seeded answer.
func presetFormPreferences(v *formValues, env map[string]string) {
	if mode, _, _ := envconfig.LookupMap(env, "CONTENT_CAPTURE_MODE"); mode != "" {
		v.contentMode = normalizeContentMode(mode)
	}
	if tags, _, _ := envconfig.LookupMap(env, "TAGS"); tags != "" {
		v.tags = tags
	}
	enabled, _, _ := envconfig.LookupMap(env, "GUARDS_ENABLED")
	failOpen, _, _ := envconfig.LookupMap(env, "GUARDS_FAIL_OPEN")
	if enabled != "" || failOpen != "" {
		on, open := v.guards != guardsOff, v.guards != guardsClosed
		if enabled != "" {
			on = envconfig.ParseBool(enabled)
		}
		if failOpen != "" {
			open = envconfig.ParseBool(failOpen)
		}
		v.guards = seedGuards(strconv.FormatBool(on), strconv.FormatBool(open))
	}
	if timeout, _, _ := envconfig.LookupMap(env, "GUARDS_TIMEOUT_MS"); timeout != "" {
		v.guardTimeout = timeout
	}
	// A stack the user already chose in this run stays: the block came from
	// that stack's setup page, and the form has no field to show a swap.
	if stack, _, _ := envconfig.LookupMap(env, "STACK_URL"); stack != "" && v.stackURL == "" {
		v.stackURL, _ = stackOrigin(stack)
	}
}

// reportBlockKeys names, on stderr, the settings a block set beyond the
// credentials, and the keys it could not set with the reason for each, so a
// team that shares a block can see what login took from it. Values stay out,
// as they do in every block error.
func reportBlockKeys(w io.Writer, v formValues) {
	set := slices.Concat(slices.Collect(maps.Keys(v.blockSettings)), v.blockAutoTagKeys)
	if len(set) > 0 {
		slices.Sort(set)
		_, _ = fmt.Fprintf(w, "agento11y login: set %s from the block.\n", strings.Join(set, ", "))
	}
	var reasons []string
	byReason := map[string][]string{}
	for _, r := range v.blockRefused {
		if _, seen := byReason[r.reason]; !seen {
			reasons = append(reasons, r.reason)
		}
		byReason[r.reason] = append(byReason[r.reason], r.key)
	}
	for _, reason := range reasons {
		_, _ = fmt.Fprintf(w, "agento11y login: did not set %s from the block: %s.\n", strings.Join(byReason[reason], ", "), reason)
	}
}

// setPasted writes value into field unless a flag fixed that value or the
// block carried none, and reports whether the field now holds the pasted one.
func setPasted(field *string, fixed, value string) bool {
	if fixed != "" || strings.TrimSpace(value) == "" {
		return false
	}
	*field = strings.TrimSpace(value)
	return true
}

// brandedPaste resolves one alias family from a pasted block, preferring the
// AGENTO11Y_ spelling over the legacy SIGIL_ one.
func brandedPaste(env map[string]string, suffix string) string {
	return cmp.Or(env[envconfig.PreferredKey(suffix)], env[envconfig.LegacyKey(suffix)])
}

// pastedBlockValidator binds validatePastedBlock to the values the flags
// fixed, which is the shape huh's Validate takes.
func pastedBlockValidator(fixed fixedValues) func(string) error {
	return func(s string) error { return validatePastedBlock(fixed, s) }
}

// validatePastedBlock checks a block before the form accepts it. An empty box
// is fine: the user can type the values instead. A block that fills nothing is
// rejected. Every value it does fill runs the validator the matching field
// applies to typed input, because the paste replaces those fields and nothing
// else would check them.
//
// Values a flag fixed are skipped: `login --endpoint …` has to accept a block
// whose endpoint slot is still a placeholder, because that slot is not what
// the user came for.
func validatePastedBlock(fixed fixedValues, s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	env, err := dotenv.ParseDotenv(strings.NewReader(normalizePastedBlock(s)))
	if err != nil {
		return fmt.Errorf("could not read the block: %w", err)
	}
	// A block whose every line a flag overrode still carries credentials,
	// so this check ignores the flags.
	if applyPaste(&formValues{}, fixedValues{}, s) == (pasteFilled{}) {
		return errors.New("no credentials in this block. Expected lines like AGENTO11Y_ENDPOINT=…, AGENTO11Y_AUTH_TENANT_ID=…, AGENTO11Y_AUTH_TOKEN=…")
	}
	var probe formValues
	filled := applyPaste(&probe, fixed, s)
	for _, c := range []struct {
		supplied bool
		name     string
		value    string
		validate func(string) error
	}{
		{filled.endpoint, "endpoint", probe.endpoint, requireBlockURL},
		{filled.tenantID, "tenant ID", probe.tenantID, requireNonEmpty("tenant id")},
		{filled.token, "token", probe.token, requireNonEmpty("auth token")},
		{filled.otelEndpoint, "OTLP endpoint", probe.otelEndpoint, requireBlockURL},
		{filled.otlpHeaders, "OTLP headers", probe.otlpHeaders, nil},
	} {
		if !c.supplied {
			continue
		}
		if strings.HasPrefix(c.value, "op://") {
			return fmt.Errorf("the %s is a 1Password reference. Resolve it first, for example: op inject -i <file> | agento11y login --env-file -", c.name)
		}
		if looksLikePlaceholder(c.value) {
			// The value is left out: it can sit next to a real secret, and
			// a block from stdin prints this error to a log.
			return fmt.Errorf("the %s is still a placeholder. Fill it in on the page, then copy the block again", c.name)
		}
		if c.validate != nil {
			if err := c.validate(c.value); err != nil {
				return fmt.Errorf("the block's %s is not usable: %w", c.name, err)
			}
		}
	}
	return validateBlockPreferences(env)
}

// looksLikePlaceholder reports whether a pasted value still carries a <…>
// slot. The setup page renders every value it does not have yet that way, so a
// block copied before the token was created arrives with one.
func looksLikePlaceholder(value string) bool {
	open := strings.Index(value, "<")
	return open >= 0 && strings.Contains(value[open:], ">")
}

// pastedAssignmentRe matches a `KEY=` at the start of the text or after
// whitespace, with the optional `export ` prefix the block may carry.
var pastedAssignmentRe = regexp.MustCompile(`(?:^|\s)(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)=`)

// normalizePastedBlock puts every assignment on a line of its own. The paste
// field is a single-line input, and a terminal paste into one of those arrives
// with each newline replaced by a space, which the loader would read as one
// unusable key. Cutting before every key the loader accepts restores the
// lines. A block that kept its newlines loses only its comments.
//
// Only accepted keys cut, so a value keeps its spaces:
// OTEL_EXPORTER_OTLP_HEADERS="Authorization=Basic …" holds no such key and
// stays whole. A key right after `#` is commented out and does not cut
// either, so a block that is only `# KEY=…` stays dead. Text before the first assignment becomes its own line, which is
// what keeps a leading `# copied from Grafana` from commenting out the block
// once the newline after it is gone.
func normalizePastedBlock(block string) string {
	// In a block that kept its newlines, a comment must stay dead: drop
	// comment lines and ` #` tails before the regex below, which would
	// otherwise start a live assignment at a `KEY=` inside them. A flattened
	// paste is one line, so a leading comment there is handled below instead.
	if strings.Contains(strings.TrimSpace(block), "\n") {
		var lines []string
		for line := range strings.SplitSeq(block, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "#") {
				lines = append(lines, cutInlineComment(line))
			}
		}
		block = strings.Join(lines, "\n")
	}
	var cuts []int
	for _, m := range pastedAssignmentRe.FindAllStringSubmatchIndex(block, -1) {
		if !dotenv.AllowedDotenvKey(block[m[2]:m[3]]) {
			continue
		}
		start := m[0]
		// The regex opens with (?:^|\s); drop the whitespace byte it consumed.
		if strings.IndexByte(" \t\n\f\r", block[start]) >= 0 {
			start++
		}
		if strings.HasSuffix(strings.TrimRight(block[:start], " \t"), "#") {
			continue
		}
		cuts = append(cuts, start)
	}
	if len(cuts) == 0 {
		return block
	}
	lines := make([]string, 0, len(cuts)+1)
	if head := strings.TrimSpace(block[:cuts[0]]); head != "" {
		lines = append(lines, head)
	}
	for i, start := range cuts {
		end := len(block)
		if i+1 < len(cuts) {
			end = cuts[i+1]
		}
		lines = append(lines, strings.TrimSpace(block[start:end]))
	}
	return strings.Join(lines, "\n")
}

// cutInlineComment drops a ` #` comment from the end of a line where the
// loader would drop it: after the closing quote of a quoted value, or anywhere
// in an unquoted one (see dotenv.ParseDotenv).
func cutInlineComment(line string) string {
	_, rest, ok := strings.Cut(line, "=")
	if !ok {
		return line
	}
	value := strings.TrimLeft(rest, " \t")
	from := len(line) - len(value)
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') {
		if end := strings.IndexByte(value[1:], value[0]); end >= 0 {
			from += end + 2
		}
	}
	// A tab before the comment counts as a space, as ParseDotenv reads it.
	if i := strings.Index(strings.ReplaceAll(line[from:], "\t", " "), " #"); i >= 0 {
		return line[:from+i]
	}
	return line
}

// autoTagOptions lists the values the automatic-tag switch can attach, with
// the source of each one, and preselects the ones already configured. Every
// option is described because these become metric labels: a user reading the
// list has to be able to tell what leaves the machine.
func autoTagOptions(selected []string) []huh.Option[string] {
	labels := map[envconfig.AutoTag]string{
		envconfig.AutoTagUser:   "user — configured user id, host agent account, or OS user",
		envconfig.AutoTagRepo:   "repo — owner/name from the origin remote",
		envconfig.AutoTagBranch: "branch — branch checked out in the session directory",
	}
	options := make([]huh.Option[string], 0, len(envconfig.AutoTagOrder))
	for _, name := range envconfig.AutoTagOrder {
		opt := huh.NewOption(labels[name], string(name))
		if slices.Contains(selected, string(name)) {
			opt = opt.Selected(true)
		}
		options = append(options, opt)
	}
	return options
}

// validateAutoTagNames requires at least one name once the switch is on. An
// empty list would be the same as turning the switch off, and silently
// treating it as "all" would attach values the user just deselected.
func validateAutoTagNames(selected []string) error {
	if len(selected) == 0 {
		return errors.New("select at least one value, or turn automatic tags off")
	}
	return nil
}

// seedAutoTags starts the automatic-tag question on On when nothing is saved.
// A saved false stays off: a rerun must not switch the setting back on.
func seedAutoTags(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return true
	}
	return envconfig.ParseBool(raw)
}

// seedAutoTagNames turns a saved allowlist into the preselected names. An
// unset allowlist means every supported name, which is what an absent
// AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES resolves to at runtime.
//
// A saved allowlist that names nothing supported ("team", or a lone comma)
// attaches no tags at runtime. This function preselects nothing for it. Ticking
// all three names instead would tell the user that user, repo and branch are
// on, and pressing Enter would then write exactly that config. The checklist
// rejects an empty submission, so the user picks names or answers No.
func seedAutoTagNames(raw string) []string {
	enabled, _ := envconfig.ParseAutoTags(raw)
	if len(enabled) == 0 {
		if strings.TrimSpace(raw) != "" {
			return nil
		}
		enabled = envconfig.AllAutoTags()
	}
	names := make([]string, 0, len(enabled))
	for _, name := range envconfig.AutoTagOrder {
		if enabled[name] {
			names = append(names, string(name))
		}
	}
	return names
}

// contentCaptureOptions lists the capture modes the form offers. Only
// metadata_only and full are offered. The advanced no_tool_content and
// full_with_metadata_spans modes are still honoured if already set — append
// the current one so re-running login preserves it instead of silently
// downgrading to the first option.
func contentCaptureOptions(current string) []huh.Option[string] {
	options := []huh.Option[string]{
		huh.NewOption("Metadata only — no prompts, responses, or tool I/O (default)", contentModeMetadataOnly),
		huh.NewOption("Full — capture everything", contentModeFull),
	}
	switch current {
	case contentModeNoToolContent:
		options = append(options, huh.NewOption("No tool content — capture generations, drop tool args and results", contentModeNoToolContent))
	case contentModeFullWithMetadataSpans:
		options = append(options, huh.NewOption("Full to ingest, metadata-only spans — keep content off OTel traces", contentModeFullWithMetadataSpans))
	}
	return options
}

// formError maps a huh outcome onto the package sentinels.
func formError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, huh.ErrUserAborted):
		return ErrAborted
	default:
		return fmt.Errorf("login form: %w", err)
	}
}

// verifyOutcome is how the saved configuration was checked, which decides
// what the closing hint tells the user to do next.
type verifyOutcome int

const (
	// verifyFailed means the endpoint did not accept the credentials and the
	// save was not overridden, so nothing is written. It is the zero value so
	// an outcome that was never set cannot read as success.
	verifyFailed verifyOutcome = iota
	// verifyPassed means the endpoint accepted the credentials.
	verifyPassed
	// verifySkipped means --no-verify suppressed the check.
	verifySkipped
	// verifyOverridden means the check failed and the user saved anyway.
	verifyOverridden
)

// verifyCredentials asks the generation-export endpoint whether it accepts
// the collected credentials, using the same request the exporter sends. It
// returns verifyPassed, verifySkipped, or verifyOverridden when the caller
// may write the file, and verifyFailed with ErrNotVerified when it may not.
// A failure is reported and then offered as a choice: an interactive user can
// save anyway, and --yes makes that choice up front for scripts. Without
// either, nothing is written.
//
// Only an OK result passes. That is stricter than `agento11y doctor`, which
// leaves a 4xx other than 401/403 healthy because the minimal probe body
// ({}) can draw a benign 400 or 415 from an endpoint that validates the body
// before auth. Login is about to persist these values, so it reports every
// non-success status and lets the user decide.
func verifyCredentials(ctx context.Context, opts RunOpts, v formValues, insecure, canPrompt bool) (verifyOutcome, error) {
	if opts.SkipVerify {
		return verifySkipped, nil
	}

	res := opts.Probe(ctx, v.endpoint, v.tenantID, v.token, insecure)
	logVerdict(opts.Logger, v.endpoint, v.tenantID, res)
	if res != nil && res.OK {
		fmt.Fprintln(opts.Stderr, clihelp.New(opts.Stderr).Success("The endpoint accepted these credentials."))
		return verifyPassed, nil
	}

	fmt.Fprintln(opts.Stderr, describeProbeFailure(opts.Stderr, res, v.endpoint, v.tenantID))
	if opts.AssumeYes {
		return verifyOverridden, nil
	}
	if !canPrompt {
		return verifyFailed, ErrNotVerified
	}

	save := false
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Save anyway?").
				Description("Yes writes these values to the config file even though the check failed. No writes nothing.").
				Affirmative("Yes").
				Negative("No").
				Value(&save),
		),
	).WithTheme(grafanaTheme())
	if err := formError(form.Run()); err != nil {
		return verifyFailed, err
	}
	if !save {
		return verifyFailed, ErrNotVerified
	}
	return verifyOverridden, nil
}

// logVerdict records the credential check in the debug log. The diagnostics
// go to opts.Stderr, which the launcher auto-login points at a terminal the
// user may have already scrolled past and which defaults to io.Discard, so
// the log is the only durable account of why a run saved nothing. The token
// is never logged.
func logVerdict(logger *log.Logger, endpoint, tenantID string, res *doctor.ProbeResult) {
	if logger == nil {
		return
	}
	if res == nil {
		logger.Printf("login: credential check returned no result: endpoint=%s tenant=%s", endpoint, tenantID)
		return
	}
	logger.Printf("login: credential check: endpoint=%s tenant=%s status=%d ok=%v message=%q",
		endpoint, tenantID, res.StatusCode, res.OK, res.Message)
}

// describeProbeFailure turns a failed probe into a diagnostic that names the
// value to look at. A 401 or 403 cannot be pinned on one value: Grafana
// Cloud checks the tenant ID and the token together as one Basic credential,
// so both are named and the most common cause — a token without the
// sigil:write scope — is called out. The classification uses doctor's own
// predicates so login's wording cannot drift from what `agento11y doctor`
// calls the same result.
func describeProbeFailure(w io.Writer, res *doctor.ProbeResult, endpoint, tenantID string) string {
	r := clihelp.New(w)
	warn := r.Warning
	faint := r.Detail

	var lines []string
	switch {
	case res == nil:
		lines = []string{
			warn("Could not check the credentials: the check returned no result."),
		}
	case res.AuthFailure():
		lines = []string{
			warn(fmt.Sprintf("The endpoint rejected these credentials (HTTP %d).", res.StatusCode)),
			faint(fmt.Sprintf("Tenant ID %q and the auth token are checked as one pair, so either can cause this.", tenantID)),
			faint("The likeliest cause is a token without the sigil:write scope."),
		}
	case res.NoResponse():
		lines = []string{warn("Could not reach " + endpoint + ".")}
		if msg := strings.TrimSpace(res.Message); msg != "" {
			lines = append(lines, faint(msg))
		}
		lines = append(lines, faint("Check the endpoint URL and this machine's network access."))
	default:
		lines = []string{
			warn(fmt.Sprintf("The endpoint answered HTTP %d.", res.StatusCode)),
			faint("Request URL: " + res.URL),
		}
		if strings.TrimSpace(res.Message) != "" {
			lines = append(lines, faint(strings.TrimSpace(res.Message)))
		}
	}
	return strings.Join(lines, "\n")
}

// printNextStep emits the post-login hint: what to run, how to diagnose the
// configuration that was just written, where the data shows up, and where to
// read more. Commands are bold so the eye lands on what to type;
// surrounding copy and URLs are faint so the lines read as secondary
// suggestions rather than another banner.
//
// The diagnostic line depends on how the save ended. Every outcome names
// `agento11y doctor`, which probes both endpoints with the file that was just
// written, but a save that skipped or overrode verification says why to run
// it now rather than only if the data does not appear.
func printNextStep(w io.Writer, outcome verifyOutcome, origin string) {
	r := clihelp.New(w)
	faint := r.Detail
	cmd := r.Name
	link := r.Detail
	fmt.Fprintln(w,
		faint("Now you can try ")+
			cmd("agento11y claude")+
			faint(" or ")+
			cmd("agento11y pi")+
			faint(" to launch a coding agent."),
	)
	switch outcome {
	case verifyPassed:
		fmt.Fprintln(w, faint("Run ")+cmd("agento11y doctor")+faint(" if the data does not appear."))
	case verifySkipped:
		fmt.Fprintln(w, faint("Verification was skipped. Run ")+cmd("agento11y doctor")+faint(" if the configuration does not work."))
	case verifyOverridden:
		fmt.Fprintln(w, faint("The endpoint did not accept these credentials. Run ")+cmd("agento11y doctor")+faint(" to check them again."))
	case verifyFailed:
		// Unreachable: a failed check that was not overridden writes
		// nothing, so there is no saved configuration to hint about.
	}
	// Credentials are saved, but a coding agent still has to be wired up. The
	// skill walks that part. Doctor prints the same command.
	fmt.Fprintln(w, faint("Setting up a coding agent? Run ")+cmd(skills.SetupCodingAgentCommand)+faint("."))
	fmt.Fprintln(w, faint("View observability data at ")+link(observabilityPageURL(origin)))
	fmt.Fprintln(w, faint("Read documentation at ")+link(docsURL))
}

// seededSuffixes are the alias families loadSeeds resolves from the dotenv
// file and overlays from the process env. Package-level so tests can iterate
// it to clear both spellings hermetically per case.
var seededSuffixes = []string{
	"ENDPOINT",
	"AUTH_TENANT_ID",
	"AUTH_TOKEN",
	"OTEL_EXPORTER_OTLP_ENDPOINT",
	// INSECURE is never prompted for. It is resolved here so the credential
	// check receives the same arguments the exporter and `agento11y doctor`
	// resolve for themselves. It cannot change login's own request:
	// the value only picks a scheme for an endpoint that has none, and every
	// endpoint login probes already passed requireURL.
	"INSECURE",
	"CONTENT_CAPTURE_MODE",
	"TAGS",
	"LOCAL",
	"LOCAL_FORWARD",
	envconfig.AutoTagsSuffix,
	envconfig.AutoTagNamesSuffix,
	"GUARDS_ENABLED",
	"GUARDS_FAIL_OPEN",
	"GUARDS_TIMEOUT_MS",
}

// Content capture mode labels mirror agento11y.ContentCaptureMode.String() so
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
	localMode           bool
	offerLocalDaemon    bool
	localDaemon         bool
	localDaemonPrompted bool
	endpoint            string
	tenantID            string
	token               string
	otelEndpoint        string
	// otlpHeaders is the OTEL_EXPORTER_OTLP_HEADERS value to persist: the one on
	// file, or the one a pasted block replaced it with. otlpHeadersPasted marks
	// the second case, the only one where the header is known to carry the token
	// being saved.
	otlpHeaders       string
	otlpHeadersPasted bool
	// stackURL is the Grafana stack the user named. It builds the printed links
	// and is saved so a re-run pre-fills the question, but it is never the
	// ingest endpoint: that host is different.
	stackURL     string
	contentMode  string
	tags         string
	guards       string
	guardTimeout string
	// autoTags is the automatic-tag switch, and autoTagNames the names it
	// applies to. Holding every supported name and holding none of them both
	// mean "all" to buildUpdates, which then writes no allowlist at all.
	autoTags     bool
	autoTagNames []string

	// capturePrompted records that the form ran, which is the only way the
	// preference fields hold something the user chose. The keys are written
	// only when it is set, so a promptless run driven by flags leaves
	// whatever is on disk alone.
	capturePrompted bool

	// autoTagsFromBlock records that a block set an automatic-tag key (see
	// presetAutoTags). The automatic-tag answers are then written even when
	// the form did not run, because the block chose them.
	autoTagsFromBlock bool

	// blockSettings are the keys a block set that login writes as the block
	// spells them, and blockRefused the ones it could not set (see blockKeys).
	blockSettings map[string]string
	blockRefused  []blockRefusal
	// blockAutoTagKeys are the automatic tag keys a block set that login
	// saves; presetAutoTags writes them through the form's own rules.
	blockAutoTagKeys []string
	// dropCredentialCopies deletes the saved HEADERS and OTEL_AUTH_TOKEN,
	// which carry a copy of the export credential, because the endpoint, the
	// OTLP endpoint or the token changed in this run.
	dropCredentialCopies bool
	// droppedCredentialCopies names the saved copies that go, reported once
	// the file is written.
	droppedCredentialCopies []string
}

// buildUpdates maps the form values onto the dotenv keys WriteDotenv expects.
// Every managed value is written under both branded spellings (and empty
// values delete both) so old binaries that only read SIGIL_* keep working.
// Keys absent from the returned map keep whatever the file already holds.
//
// The capture settings are written only when capturePrompted says the form
// ran. A promptless run leaves them out: their values then come from seeds, and
// a seed can be an AGENTO11Y_* variable exported in the current shell, so
// writing them would turn a one-off `agento11y claude --tag session=demo` into
// a permanent config entry the user never saw. The automatic-tag keys are
// also written when autoTagsFromBlock is set, through the same lines, so an
// environment block saves exactly what the form would for the same answers.
//
// Unless KeepLocalSetting is set, every Cloud setup writes LOCAL=false. A
// submitted local-daemon answer writes LOCAL and LOCAL_FORWARD together.
//
// Content capture mode, the guard-enabled flag, and the automatic-tag switch
// are always written explicitly so a downgrade (e.g. full back to
// metadata_only, or enabled back to disabled) actually takes effect instead of
// being silently preserved.
//
// Answering No to automatic tags deletes the allowlist as well as writing the
// switch. Two things go wrong if login keeps it. Doctor warns about an
// allowlist the switch cannot use. And a later run that turns the switch on
// from the shell reads that allowlist, so it is narrowed to the names the user
// picked before turning the feature off.
//
// When guards are enabled the timeout and fail mode are always written too,
// so clearing the timeout field deletes the key (the runtime default then
// applies) rather than leaving a stale value behind. While guards are off
// only the disabled flag is written, leaving any prior timeout/fail-mode
// untouched and inert.
//
// OTEL_EXPORTER_OTLP_HEADERS is written like the rest, empty deleting the key.
// No field asks for it, so Run resolves it from the file and the paste before
// calling this.
//
// AGENTO11Y_STACK_URL is added after the aliases are expanded, so it gets one
// spelling rather than two, and only when it holds something: a promptless run
// seeds it from the file and must not delete a stack it never asked about.
func buildUpdates(v formValues, keepLocalSetting bool) map[string]string {
	updates := map[string]string{
		"SIGIL_ENDPOINT":                    v.endpoint,
		"SIGIL_AUTH_TENANT_ID":              v.tenantID,
		"SIGIL_AUTH_TOKEN":                  v.token,
		"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT": v.otelEndpoint,                   // "" deletes
		"OTEL_EXPORTER_OTLP_HEADERS":        strings.TrimSpace(v.otlpHeaders), // "" deletes
	}
	if !keepLocalSetting {
		localMode := false
		if v.localDaemonPrompted {
			localMode = v.localDaemon
			updates[envconfig.LegacyKey("LOCAL_FORWARD")] = strconv.FormatBool(localMode)
		}
		updates[envconfig.LegacyKey("LOCAL")] = strconv.FormatBool(localMode)
	}
	if v.capturePrompted || v.autoTagsFromBlock {
		updates[envconfig.LegacyKey(envconfig.AutoTagsSuffix)] = strconv.FormatBool(v.autoTags)
		// Writing no allowlist means every name, so a user who kept all of them
		// also picks up a name a later version adds. A narrowed selection is
		// written out; "" deletes a list from a previous run. The switch being
		// off deletes it as well, so no inert key survives.
		names := ""
		if v.autoTags {
			names = autoTagNamesValue(v.autoTagNames)
		}
		updates[envconfig.LegacyKey(envconfig.AutoTagNamesSuffix)] = names
	}
	if v.capturePrompted {
		updates["SIGIL_CONTENT_CAPTURE_MODE"] = normalizeContentMode(v.contentMode)
		updates["SIGIL_TAGS"] = strings.TrimSpace(v.tags) // "" deletes
		switch v.guards {
		case guardsOpen, guardsClosed:
			updates["SIGIL_GUARDS_ENABLED"] = "true"
			if v.guards == guardsOpen {
				updates["SIGIL_GUARDS_FAIL_OPEN"] = "true"
			} else {
				updates["SIGIL_GUARDS_FAIL_OPEN"] = "false"
			}
			// Empty deletes, so a cleared field falls back to the runtime
			// default instead of keeping a stale timeout from a previous
			// config.
			updates["SIGIL_GUARDS_TIMEOUT_MS"] = strings.TrimSpace(v.guardTimeout)
		default:
			updates["SIGIL_GUARDS_ENABLED"] = "false"
		}
	}
	if v.dropCredentialCopies {
		for _, suffix := range blockExportCredentialSuffixes {
			updates[envconfig.PreferredKey(suffix)] = "" // deletes; a block's own copy below replaces it
		}
	}
	maps.Copy(updates, v.blockSettings)
	out := envconfig.ExpandAliases(updates)
	if v.stackURL != "" {
		out[stackURLKey] = v.stackURL
	}
	return out
}

// autoTagNamesValue renders the selected names as the allowlist to persist,
// in AutoTagOrder rather than click order so re-running login does not churn
// the file. A selection covering every supported name is written as "", which
// deletes the key: the switch alone already means all of them.
func autoTagNamesValue(selected []string) string {
	names := make([]string, 0, len(envconfig.AutoTagOrder))
	for _, name := range envconfig.AutoTagOrder {
		if slices.Contains(selected, string(name)) {
			names = append(names, string(name))
		}
	}
	if len(names) == len(envconfig.AutoTagOrder) {
		return ""
	}
	return strings.Join(names, ",")
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

func seedLocalDaemon(localRaw, forwardRaw string) bool {
	localRaw = strings.TrimSpace(localRaw)
	if localRaw == "" {
		return true
	}
	localMode, ok := envconfig.ParseBoolValue(localRaw)
	if !ok || localMode {
		return localMode
	}

	// Cloud setup can write LOCAL=false without asking this question.
	// LOCAL_FORWARD=false distinguishes a saved No answer.
	forwardRaw = strings.TrimSpace(forwardRaw)
	if forwardRaw == "" {
		return true
	}
	forward, ok := envconfig.ParseBoolValue(forwardRaw)
	return ok && forward
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
func loadSeeds(configPath string, logger *log.Logger) (map[string]string, error) {
	fileEnv, err := dotenv.ReadDotenv(configPath, logger)
	if err != nil {
		return nil, err
	}
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
	// Neither of these has an alias family, and both are read from the file
	// alone. login writes back every key it resolves here, so overlaying the
	// process env would turn a one-off shell export into a saved value.
	seeds["OTEL_EXPORTER_OTLP_HEADERS"] = strings.TrimSpace(fileEnv["OTEL_EXPORTER_OTLP_HEADERS"])
	seeds[stackURLKey] = strings.TrimSpace(fileEnv[stackURLKey])
	return seeds, nil
}

func requireURL(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("endpoint URL is required")
	}
	u, err := url.Parse(s)
	if err != nil {
		// The parse error quotes the input, or part of it such as the port,
		// and the input can carry a secret from a block; say nothing of it.
		return errors.New("invalid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("endpoint URL must start with http:// or https://")
	}
	if u.Host == "" {
		return errors.New("endpoint URL must include a host")
	}
	return nil
}

// requireBlockURL is requireURL for an endpoint a block supplies, which must
// use https unless it is on this machine. A plain http endpoint would send the
// block's credentials and every session in cleartext, as the INSECURE setting
// a block cannot set would; a user who wants one passes it as a flag.
func requireBlockURL(s string) error {
	if err := requireURL(s); err != nil {
		return err
	}
	if u, _ := url.Parse(strings.TrimSpace(s)); strings.EqualFold(u.Scheme, "http") && !envconfig.IsLocalEndpoint(s) {
		return errors.New("a block's endpoint must use https unless it is on this machine; pass it as a flag to use http")
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

// welcomeBanner returns the rendered banner promptValues prints first.
func welcomeBanner(w io.Writer, offerLocal, configured bool) string {
	subtitle := "Let's connect your Grafana stack."
	if configured {
		subtitle = "Update your Agent Observability settings."
	}
	if offerLocal {
		subtitle = "Choose where to keep your sessions."
	}
	r := clihelp.New(w)
	return r.Heading("Welcome to Grafana Agent Observability") + "\n" + r.Detail(subtitle) + "\n"
}

// rows reports how far Fprintln(w, s) advances the cursor, which is what
// promptValues has to erase afterwards. Every embedded newline counts, plus
// the one Fprintln adds, plus one row for each time a line is longer than the
// terminal and soft-wraps. A width of 0 means the width is unknown and no
// wrapping is assumed.
//
// Counting newlines alone is not enough: the setup link is 75 columns with the
// placeholder host and passes 80 with a long stack name, so a wrapped row would
// survive the erase on a standard terminal.
func rows(s string, width int) int {
	n := 0
	for line := range strings.SplitSeq(s, "\n") {
		n++
		if width <= 0 {
			continue
		}
		// A line filling the last column does not wrap on its own, hence
		// width-1: the terminal defers the wrap until one more rune arrives.
		if printable := lipgloss.Width(line); printable > width {
			n += (printable - 1) / width
		}
	}
	return n
}

// terminalWidth reports the column count of the terminal behind w, or 0 when
// w is not a terminal.
func terminalWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	width, _, err := term.GetSize(int(f.Fd()))
	if err != nil || width <= 0 {
		return 0
	}
	return width
}
