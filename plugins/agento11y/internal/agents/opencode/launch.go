// Package opencode implements the opencode launcher adapter for the
// consolidated agento11y binary. The dispatcher in cmd/agento11y routes
// `agento11y opencode [-- args...]` here.
//
// Unlike the hook adapters, this adapter owns the user's terminal: it
// bootstraps the @grafana/agento11y-opencode plugin in opencode's global
// config on first use, then hands the terminal to the opencode process. The
// opencode telemetry plugin itself runs in-process inside opencode through
// opencode's TypeScript plugin API; the launcher only handles install,
// refresh, and shared env injection.
//
// OpenCode 1.x and 2.x differ at every one of those steps, so the adapter
// reads the host version first (version.go) and refuses a release it does not
// know before it changes anything:
//
//   - 1.x (from 1.3.13) installs with `opencode plugin <pkg> --global`, reads
//     the singular `plugin` key of the first global config file, and
//     refreshes the plugin once a day.
//   - 2.x (from 2.0.22) installs with `opencode plugin add <pkg>`, reads the
//     legacy `plugin` key before the native `plugins` key in both
//     opencode.json and opencode.jsonc, and gets no automatic refresh, because
//     `opencode plugin update` starts or contacts OpenCode's shared background
//     service. Session commands (the TUI, `run`, `mini`) gain --standalone so
//     the session's private server inherits this launcher's environment; every
//     other command runs exactly as typed.
package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agentinstall"
	"github.com/grafana/agento11y/plugins/agento11y/internal/launcher"
	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
	"github.com/tailscale/hujson"
)

const (
	// PluginSource is the npm spec passed to OpenCode's plugin installer.
	PluginSource = "@grafana/agento11y-opencode"
	// PluginName is the package.json `name` of the plugin. Used to detect
	// versioned npm specs (e.g. `@grafana/agento11y-opencode@0.6.0`) in the
	// config probe.
	PluginName = "@grafana/agento11y-opencode"
	// legacyPluginName is the pre-rename package name. Existing configs may
	// still reference it; treating it as installed avoids registering the
	// plugin twice under both names.
	legacyPluginName = "@grafana/sigil-opencode"
	// pluginID is the id the package's server entry declares
	// (plugins/opencode/src/server.ts). OpenCode 2 turns a loaded plugin off
	// by id, not by package, so `-grafana.agento11y` in a plugins list
	// disables capture.
	pluginID = "grafana.agento11y"

	updateCheckTTL = 24 * time.Hour
)

// ErrCLINotFound means the OpenCode binary is not available on PATH for the
// current user. Callers can defer setup until the host is installed.
var ErrCLINotFound = errors.New("opencode CLI not found")

func init() {
	agentinstall.Register(agentinstall.Spec{
		Name:          "opencode",
		Install:       Install,
		IsMissingHost: func(err error) bool { return errors.Is(err, ErrCLINotFound) },
	})
}

// Test seams.
var (
	lookPath      = exec.LookPath
	execFn        = launcher.DefaultExec
	runInstall    = defaultRunInstall
	runUpdate     = defaultRunUpdate
	runInstallV2  = defaultRunInstallV2
	configDirFn   = defaultConfigDir
	configDirV2Fn = defaultConfigDirV2
	cacheDirFn    = defaultCacheDir
	writeConfig   = writeConfigAtomic
)

// configFileNames lists the basenames opencode recognises for its
// global config, in precedence order. The docs advertise both .json
// and .jsonc; users may pick either, so we probe both. OpenCode 2 loads
// both, in this order (packages/core/src/config/discovery.ts).
var configFileNames = []string{"opencode.json", "opencode.jsonc"}

// ArgumentError reports forwarded OpenCode arguments the launcher refuses
// because they would move the session to a backend whose environment the
// launcher does not set. Callers report it as a usage error.
type ArgumentError struct {
	// Reason says what was refused and how to get past it.
	Reason string
}

func (e *ArgumentError) Error() string { return e.Reason }

// Invocation is an OpenCode launch whose host and arguments have been
// validated. Prepare builds it without changing any file, prompting, or
// running anything but `opencode --version`, so a caller can validate before
// its own setup (login prompt, local receiver) and drop the launch on error.
type Invocation struct {
	bin        string
	generation hostGeneration
	// args are the forwarded arguments, with --standalone appended for an
	// OpenCode 2 session.
	args []string
	// direct marks an OpenCode 2 command that is not a session: help,
	// version, and maintenance such as `plugin list` or `serve`.
	direct bool
}

// Prepare resolves `opencode` on PATH, checks that it is a supported release,
// and validates the forwarded arguments for that release. OpenCode 1
// arguments are forwarded unchanged. OpenCode 2 session arguments gain
// --standalone unless they already ask for it, and --server or a negated
// --standalone is an *ArgumentError. An unsupported release is a
// *VersionError.
func Prepare(ctx context.Context, args []string) (*Invocation, error) {
	bin, err := lookPath("opencode")
	if err != nil {
		return nil, fmt.Errorf("%w on PATH: %w", ErrCLINotFound, err)
	}
	generation, err := detectHost(ctx, bin)
	if err != nil {
		return nil, err
	}
	inv := &Invocation{bin: bin, generation: generation, args: slices.Clone(args)}
	if generation == generationV2 {
		inv.args, inv.direct, err = planV2Args(args)
		if err != nil {
			return nil, err
		}
	}
	return inv, nil
}

// Direct reports whether the invocation is an OpenCode 2 help, version, or
// maintenance command. Launch runs such a command as typed, and callers skip
// their session setup for it: no login prompt and no local receiver.
func (inv *Invocation) Direct() bool { return inv.direct }

// Launch hands the terminal to opencode. A direct invocation is exec'd as is,
// with no config change and no local-mode environment. A session first
// migrates a legacy plugin entry and
// installs the plugin when the global config lacks it; OpenCode 1 also
// refreshes it once a day. When localEnv is non-nil, the child receives
// local-mode endpoint and placeholder auth values so it talks to the local
// receiver instead of Grafana Cloud.
func (inv *Invocation) Launch(ctx context.Context, localEnv *local.LaunchEnv, stderr io.Writer, logger *log.Logger, binaryVersion string) error {
	if inv.direct {
		return launcher.Exec(execFn, inv.bin, "opencode", inv.args, local.Environ(nil))
	}
	if inv.generation == generationV2 {
		return inv.launchV2(ctx, localEnv, stderr, logger)
	}
	return inv.launchV1(ctx, localEnv, stderr, logger, binaryVersion)
}

// Launch prepares and runs one `agento11y opencode` invocation. See Prepare
// and Invocation.Launch.
func Launch(ctx context.Context, args []string, localEnv *local.LaunchEnv, _ io.Reader, _, stderr io.Writer, logger *log.Logger, binaryVersion string) error {
	inv, err := Prepare(ctx, args)
	if err != nil {
		return err
	}
	return inv.Launch(ctx, localEnv, stderr, logger, binaryVersion)
}

// resolved is the Bootstrap LookPath for an invocation whose binary Prepare
// already found.
func (inv *Invocation) resolved(string) (string, error) { return inv.bin, nil }

// launchV1 ensures the plugin is registered in OpenCode 1's global config
// (running `opencode plugin @grafana/agento11y-opencode --global` if it is
// not), refreshes it daily, and execs opencode with the forwarded args.
func (inv *Invocation) launchV1(ctx context.Context, localEnv *local.LaunchEnv, stderr io.Writer, logger *log.Logger, binaryVersion string) error {
	// Rewrite a pre-rename @grafana/sigil-opencode config entry to the new
	// package name before probing: the old package is frozen on npm, so a
	// legacy entry would otherwise stay pinned to the last pre-rename release
	// forever. Best-effort — a failure falls through to the legacy
	// refresh-skip below.
	// Launch keeps legacy migration best-effort so an interactive OpenCode
	// session can still start with its existing plugin if a rewrite fails.
	_ = migrateLegacyConfig(stderr, logger)

	// The periodic refresh installs PluginSource. When the config still
	// references the legacy package name (because the migration above could
	// not rewrite it), that would register the plugin a second time under the
	// new name, so skip the refresh for legacy installs and leave the existing
	// entry alone.
	update := runUpdate
	if src, found, err := installedPluginSource(); err == nil && found && stripNpmVersion(src) == legacyPluginName {
		update = nil
	}
	return launcher.Bootstrap(ctx, launcher.BootstrapSpec{
		BinName:     "opencode",
		PluginLabel: PluginSource,
		LookPath:    inv.resolved,
		ExecFn:      execFn,
		Args:        inv.args,
		Env:         local.Environ(localEnv),
		Logger:      logger,
		Stderr:      stderr,
		// Surface a config-file probe failure on stderr too so the user can
		// see why we're falling through to install on a file we couldn't
		// read. Treat the case like a missing plugin — opencode's installer
		// will fail loudly if the file is genuinely broken.
		Probe:           func(context.Context, string) (bool, error) { return pluginInstalled() },
		ProbeErrLog:     "opencode config probe",
		ProbeErrEcho:    true,
		RegisterMessage: fmt.Sprintf("agento11y: installing %s into opencode\n", PluginSource),
		Install:         runInstall,
		InstallRecoveryHint: func(w io.Writer) {
			fmt.Fprintf(w, "          opencode plugin %s --global\n", PluginSource)
		},
		Update: update,
		UpdateRecoveryHint: func(w io.Writer) {
			fmt.Fprintf(w, "          opencode plugin %s --global --force\n", PluginSource)
		},
		UpdateTTL:     updateCheckTTL,
		BinaryVersion: binaryVersion,
	})
}

// launchV2 ensures the plugin is registered in OpenCode 2's global config
// (running `opencode plugin add @grafana/agento11y-opencode` if it is not)
// and execs opencode with the planned args. There is no periodic refresh:
// `opencode plugin update` inspects plugins through ServerConnection.resolve()
// (packages/cli/src/commands/handlers/plugin/inventory.ts), which connects to
// or starts the shared background service, and a standalone launch must not
// start that service on the user's behalf. `opencode plugin add` needs no
// service (handlers/plugin/add.ts writes the global config itself).
func (inv *Invocation) launchV2(ctx context.Context, localEnv *local.LaunchEnv, stderr io.Writer, logger *log.Logger) error {
	// Best-effort, as for OpenCode 1: a failed rewrite leaves the legacy entry
	// in place, and the probe still counts it as installed.
	_ = migrateLegacyConfigV2(stderr, logger)
	return launcher.Bootstrap(ctx, launcher.BootstrapSpec{
		BinName:     "opencode",
		PluginLabel: PluginSource,
		LookPath:    inv.resolved,
		ExecFn:      execFn,
		Args:        inv.args,
		Env:         local.Environ(localEnv),
		Logger:      logger,
		Stderr:      stderr,
		Probe: func(context.Context, string) (bool, error) {
			reg, err := readRegistrationV2()
			if err != nil {
				return false, err
			}
			// A plugin the user turned off stays off: adding it again would
			// append an entry after their selector and turn it back on.
			if reg.disabled() {
				fmt.Fprintf(stderr, "agento11y: %s is turned off by %q in %s; this session runs without agento11y capture\n",
					PluginName, reg.disabledBy, reg.disabledIn)
			}
			return reg.found(), nil
		},
		ProbeErrLog:     "opencode config probe",
		ProbeErrEcho:    true,
		RegisterMessage: fmt.Sprintf("agento11y: installing %s into opencode\n", PluginSource),
		Install:         runInstallV2,
		InstallRecoveryHint: func(w io.Writer) {
			fmt.Fprintf(w, "          opencode plugin add %s\n", PluginSource)
		},
	})
}

// Install registers the OpenCode plugin without starting OpenCode or prompting
// for Agent Observability credentials. The returned value is true only when
// this invocation registered the plugin. The host version is checked before
// any config is touched, so an unsupported release returns a *VersionError
// and a missing host returns ErrCLINotFound with no change made.
func Install(ctx context.Context, stdout io.Writer, logger *log.Logger) (bool, error) {
	bin, err := lookPath("opencode")
	if err != nil {
		return false, fmt.Errorf("%w; install OpenCode or run this in the developer's user context", ErrCLINotFound)
	}
	generation, err := detectHost(ctx, bin)
	if err != nil {
		return false, err
	}
	// A legacy config must be rewritten before it can count as installed. If
	// the rewrite fails, returning an error keeps fleet reconciliation retrying
	// instead of silently leaving OpenCode pinned to the frozen package.
	if err := migrateLegacyConfigFor(generation, stdout, logger); err != nil {
		return false, fmt.Errorf("migrate legacy OpenCode plugin configuration: %w", err)
	}
	installed, probeErr := pluginRegistered(generation)
	if probeErr == nil && installed {
		return false, nil
	}
	install := runInstall
	if generation == generationV2 {
		install = runInstallV2
	}
	if err := install(ctx, bin, stdout); err != nil {
		return false, err
	}
	return true, nil
}

func defaultRunInstall(ctx context.Context, bin string, w io.Writer) error {
	return launcher.RunSteps(ctx, bin, w, [][]string{
		{"plugin", PluginSource, "--global"},
	})
}

func defaultRunUpdate(ctx context.Context, bin string, w io.Writer) error {
	return launcher.RunSteps(ctx, bin, w, [][]string{
		{"plugin", PluginSource, "--global", "--force"},
	})
}

// defaultRunInstallV2 runs OpenCode 2's installer. It takes no --global (the
// command always writes the global config) and no --force.
func defaultRunInstallV2(ctx context.Context, bin string, w io.Writer) error {
	return launcher.RunSteps(ctx, bin, w, [][]string{
		{"plugin", "add", PluginSource},
	})
}

// pluginRegistered reports whether the plugin (or its legacy name) is in the
// global config the given release reads. An OpenCode 2 plugin the user turned
// off counts as registered, so an install does not turn it back on.
func pluginRegistered(generation hostGeneration) (bool, error) {
	if generation == generationV2 {
		reg, err := readRegistrationV2()
		return reg.found(), err
	}
	return pluginInstalled()
}

// OpenCode 2 command tree, from packages/cli/src/commands/commands.ts. The
// launcher classifies forwarded arguments the way the host's effect/cli
// parser does (effect/src/cli/internal/lexer.ts and parser.ts), so the
// session check agrees with what OpenCode will run.
var (
	// v2Subcommands are the root's subcommands and aliases (`update` is
	// `upgrade`). A first positional argument that is not one of them is the
	// root TUI's directory argument.
	v2Subcommands = map[string]bool{
		"upgrade": true, "update": true, "uninstall": true, "acp": true, "api": true,
		"debug": true, "auth": true, "mcp": true, "plugin": true, "models": true,
		"stats": true, "mini": true, "run": true, "session": true, "service": true,
		"reload": true, "pair": true, "serve": true,
	}
	// v2SessionCommands start an agent session on a server picked with
	// --standalone or --server: the root TUI (""), run, and mini. Their
	// handlers call ServerConnection.resolve({server, standalone}).
	v2SessionCommands = map[string]bool{"": true, "run": true, "mini": true}
	// v2ActionFlags are effect/cli's built-in global actions (help, version,
	// wizard, completions). They are accepted at every command level and
	// print and exit before any handler runs.
	v2ActionFlags = map[string]bool{"help": true, "h": true, "version": true, "v": true, "wizard": true, "completions": true}
	// v2RootValueFlags take a value at the root level, which consumes the
	// next positional token when the value is not given inline.
	v2RootValueFlags = map[string]bool{"server": true, "session": true, "s": true, "prompt": true, "log-level": true, "completions": true}
	// v2RootBoolFlags are boolean at the root level, including global
	// settings. effect/cli lets a boolean flag consume a following
	// true/false literal.
	v2RootBoolFlags = map[string]bool{
		"standalone": true, "auto": true, "yolo": true, "dangerously-skip-permissions": true,
		"continue": true, "c": true, "print-logs": true, "help": true, "h": true,
		"version": true, "v": true, "wizard": true,
	}
	// cliNegativeNumber is a token effect/cli lexes as a value, not an option.
	cliNegativeNumber = regexp.MustCompile(`^-(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?$`)
)

// cliToken is one lexed command-line token, as effect/cli's lexer produces.
type cliToken struct {
	flag bool
	long bool
	// name is the option name without dashes, or the positional value.
	name string
	// value is an inline `=value`; hasValue says one was given.
	value    string
	hasValue bool
}

// lexCLI tokenizes arguments before `--` like effect/cli: `--name[=value]`,
// `-x[=value]`, clusters such as `-abc` as one option per character, and
// everything else (including `-`, and negative numbers) as a value.
func lexCLI(args []string) []cliToken {
	var tokens []cliToken
	for _, arg := range args {
		switch {
		case !strings.HasPrefix(arg, "-") || cliNegativeNumber.MatchString(arg) || arg == "-":
			tokens = append(tokens, cliToken{name: arg})
		case strings.HasPrefix(arg, "--"):
			name, value, ok := strings.Cut(arg[2:], "=")
			tokens = append(tokens, cliToken{flag: true, long: true, name: name, value: value, hasValue: ok})
		default:
			if name, value, ok := strings.Cut(arg[1:], "="); ok {
				tokens = append(tokens, cliToken{flag: true, name: name, value: value, hasValue: true})
				continue
			}
			for _, ch := range arg[1:] {
				tokens = append(tokens, cliToken{flag: true, name: string(ch)})
			}
		}
	}
	return tokens
}

// effect/cli's boolean literals (Schema.TrueLiterals and FalseLiterals).
var (
	trueLiterals  = []string{"true", "yes", "on", "1", "y"}
	falseLiterals = []string{"false", "no", "off", "0", "n"}
)

func isTrueLiteral(s string) bool  { return slices.Contains(trueLiterals, s) }
func isFalseLiteral(s string) bool { return slices.Contains(falseLiterals, s) }

// planV2Args decides how OpenCode 2 runs the forwarded arguments. A help,
// version, or other action flag, or any subcommand other than run and mini,
// is direct: it runs unchanged. A session gains a trailing --standalone (before
// any `--`, where no later token can be read as its value), unless it already
// asks for one. --server and a false --standalone are refused.
func planV2Args(args []string) (planned []string, direct bool, err error) {
	opts, trailing := args, []string(nil)
	if i := slices.Index(args, "--"); i >= 0 {
		opts, trailing = args[:i], args[i:]
	}
	tokens := lexCLI(opts)
	if hasActionFlag(tokens) || !v2SessionCommands[v2Command(tokens)] {
		return slices.Clone(args), true, nil
	}
	standalone, err := sessionStandalone(tokens)
	if err != nil {
		return nil, false, err
	}
	planned = slices.Clone(opts)
	if !standalone {
		planned = append(planned, "--standalone")
	}
	return append(planned, trailing...), false, nil
}

// hasActionFlag reports a built-in action anywhere before `--`. Negating a
// boolean action (`--no-help`) still runs it, because the action ignores its
// value.
func hasActionFlag(tokens []cliToken) bool {
	for _, t := range tokens {
		if t.flag && v2ActionFlags[flagName(t)] {
			return true
		}
	}
	return false
}

// flagName returns the option a flag token resolves to, folding a negated
// long boolean (`--no-continue`) onto its name. effect/cli only negates a
// canonical name, never a one-letter alias.
func flagName(t cliToken) string {
	if t.long {
		if negated, ok := strings.CutPrefix(t.name, "no-"); ok && len(negated) > 1 && v2RootBoolFlags[negated] {
			return negated
		}
	}
	return t.name
}

// v2Command returns the subcommand the host selects, or "" for the root TUI.
// Only the first positional token can select a subcommand; values consumed
// by a root flag (`-s run` names a session "run") are skipped first.
func v2Command(tokens []cliToken) string {
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		if !t.flag {
			if !v2Subcommands[t.name] {
				return ""
			}
			if t.name == "update" {
				return "upgrade"
			}
			return t.name
		}
		if t.hasValue || i+1 >= len(tokens) || tokens[i+1].flag {
			continue
		}
		name := flagName(t)
		next := tokens[i+1].name
		switch {
		case v2RootValueFlags[name]:
			i++
		case v2RootBoolFlags[name] && (isTrueLiteral(next) || isFalseLiteral(next)):
			i++
		}
	}
	return ""
}

// sessionStandalone reports whether session arguments already select a
// private server, and refuses those that select another one. --server and
// --standalone exist only as long options, and a token starting with `--`
// is always an option, never another flag's value.
func sessionStandalone(tokens []cliToken) (bool, error) {
	standalone := false
	for i, t := range tokens {
		if !t.flag || !t.long {
			continue
		}
		switch t.name {
		case "server":
			return false, &ArgumentError{Reason: "--server attaches the session to a server whose settings agento11y cannot see; agento11y runs OpenCode 2 sessions with --standalone so their private server inherits agento11y's capture settings. Drop --server, or run opencode directly to use that server without agento11y capture"}
		case "no-standalone":
			return false, standaloneNegated("--no-standalone")
		case "standalone":
			value := t.value
			if !t.hasValue && i+1 < len(tokens) && !tokens[i+1].flag {
				value = tokens[i+1].name
			}
			if isFalseLiteral(value) {
				spelled := "--standalone " + value
				if t.hasValue {
					spelled = "--standalone=" + value
				}
				return false, standaloneNegated(spelled)
			}
			standalone = true
		}
	}
	return standalone, nil
}

func standaloneNegated(spelled string) error {
	return &ArgumentError{Reason: fmt.Sprintf("%[1]s attaches the session to OpenCode's background service, whose settings agento11y cannot see; agento11y runs OpenCode 2 sessions with --standalone so their private server inherits agento11y's capture settings. Drop %[1]s, or run opencode directly to use the background service without agento11y capture", spelled)}
}

// migrateLegacyConfigFor runs the legacy-name migration for the config files
// and keys the given release reads.
func migrateLegacyConfigFor(generation hostGeneration, stderr io.Writer, logger *log.Logger) error {
	if generation == generationV2 {
		return migrateLegacyConfigV2(stderr, logger)
	}
	return migrateLegacyConfig(stderr, logger)
}

// migrateLegacyConfig rewrites legacy @grafana/sigil-opencode entries in
// opencode's global config to the renamed @grafana/agento11y-opencode
// package. The old package is frozen on npm (releases continue only under
// the new name), so a legacy entry stays pinned to the last pre-rename
// release forever. OpenCode installs the npm plugins listed in its config at
// startup, so rewriting the entry is enough — no install command needed.
// Version pins are dropped deliberately: pre-rename versions do not exist
// under the new package name.
//
// The rewrite goes through hujson so comments, formatting, and tuple options
// survive, and the file is replaced atomically (temp file + rename) so a
// failure never leaves a half-written config. Failures are logged and never
// block Launch — patch/write failures also print a manual-recovery hint on
// stderr, and Launch's legacy refresh-skip keeps the frozen install working.
// Install returns such a failure so managed reconciliation does not report a
// legacy plugin as converged.
//
// This is the OpenCode 1 migration: it reads the first global config file and
// its `plugin` key. migrateLegacyConfigV2 covers OpenCode 2.
func migrateLegacyConfig(stderr io.Writer, logger *log.Logger) error {
	path, data, err := readConfigFile()
	if err != nil {
		logger.Printf("opencode legacy migration: %v", err)
		return err
	}
	if data == nil {
		return nil
	}
	v, err := hujson.Parse(data)
	if err != nil {
		err = fmt.Errorf("parse %s: %w", path, err)
		logger.Printf("opencode legacy migration: %v", err)
		return err
	}
	ops, err := legacyPluginOps(v)
	if err != nil {
		err = fmt.Errorf("scan %s: %w", path, err)
		logger.Printf("opencode legacy migration: %v", err)
		return err
	}
	if ops == nil {
		return nil
	}
	return applyLegacyPatch(path, v, ops, stderr, logger)
}

// migrateLegacyConfigV2 is the OpenCode 2 migration. It covers every global
// file the host loads (opencode.json, then opencode.jsonc) and both keys
// (the legacy `plugin` list, then the native `plugins` list, whose object
// entries name the package in `package`). The first legacy entry in that
// order becomes the bare renamed package and keeps its options; every other
// legacy entry is removed. When the renamed package is already configured
// anywhere, every legacy entry is removed, so the two never load side by
// side. Entries for the current package keep their pins and options.
func migrateLegacyConfigV2(stderr io.Writer, logger *log.Logger) error {
	docs, err := readConfigFilesV2()
	if err != nil {
		logger.Printf("opencode legacy migration: %v", err)
		return err
	}
	type parsedDoc struct {
		path    string
		value   hujson.Value
		entries []pluginEntryV2
	}
	var parsed []parsedDoc
	hasNew, hasLegacy := false, false
	for _, doc := range docs {
		v, err := hujson.Parse(doc.data)
		if err != nil {
			err = fmt.Errorf("parse %s: %w", doc.path, err)
			logger.Printf("opencode legacy migration: %v", err)
			return err
		}
		std := v.Clone()
		std.Standardize()
		entries, err := pluginEntriesV2(std.Pack())
		if err != nil {
			err = fmt.Errorf("scan %s: %w", doc.path, err)
			logger.Printf("opencode legacy migration: %v", err)
			return err
		}
		for _, e := range entries {
			if e.remove {
				continue
			}
			switch stripNpmVersion(e.target) {
			case legacyPluginName:
				hasLegacy = true
			case PluginName:
				hasNew = true
			}
		}
		parsed = append(parsed, parsedDoc{path: doc.path, value: v, entries: entries})
	}
	if !hasLegacy {
		return nil
	}
	kept := hasNew
	for _, doc := range parsed {
		var refs []legacyRef
		for _, e := range doc.entries {
			if e.remove || stripNpmVersion(e.target) != legacyPluginName {
				continue
			}
			refs = append(refs, legacyRef{pointer: e.pointer, spec: e.spec, keep: !kept})
			kept = true
		}
		if len(refs) == 0 {
			continue
		}
		ops, err := legacyPatch(refs)
		if err != nil {
			err = fmt.Errorf("scan %s: %w", doc.path, err)
			logger.Printf("opencode legacy migration: %v", err)
			return err
		}
		if err := applyLegacyPatch(doc.path, doc.value, ops, stderr, logger); err != nil {
			return err
		}
	}
	return nil
}

// applyLegacyPatch announces, applies, and atomically writes one migration.
func applyLegacyPatch(path string, v hujson.Value, ops []byte, stderr io.Writer, logger *log.Logger) error {
	fmt.Fprintf(stderr, "agento11y: migrating %s to %s in %s\n", legacyPluginName, PluginName, path)
	if err := v.Patch(ops); err != nil {
		err = fmt.Errorf("patch %s: %w", path, err)
		logger.Printf("opencode legacy migration: %v", err)
		printMigrationRecoveryHint(stderr, err, path)
		return err
	}
	if err := writeConfig(path, v.Pack()); err != nil {
		err = fmt.Errorf("write %s: %w", path, err)
		logger.Printf("opencode legacy migration: %v", err)
		printMigrationRecoveryHint(stderr, err, path)
		return err
	}
	return nil
}

// printMigrationRecoveryHint tells the user how to finish the rename by hand
// when the automatic rewrite failed, mirroring Bootstrap's recovery wording.
func printMigrationRecoveryHint(w io.Writer, err error, path string) {
	fmt.Fprintf(w,
		"agento11y: migration of %s failed: %v\n"+
			"agento11y: continuing with the installed version. To migrate manually, edit\n"+
			"          %s and replace %s with %s.\n",
		legacyPluginName, err, path, legacyPluginName, PluginName)
}

// legacyPluginOps builds the RFC 6902 operations rewriting legacy
// @grafana/sigil-opencode entries in the parsed config: the first legacy
// entry is replaced with the bare renamed package (a tuple entry keeps its
// options — only element 0 is replaced), any further legacy entries are
// removed, and when the new name is already present every legacy entry is
// removed instead. Matching is version-insensitive. Returns nil when the
// config has no legacy entry.
func legacyPluginOps(v hujson.Value) ([]byte, error) {
	std := v.Clone()
	std.Standardize()
	var c opencodeConfig
	if err := json.Unmarshal(std.Pack(), &c); err != nil {
		return nil, err
	}
	var legacy []legacyRef
	hasNew := false
	for i, raw := range c.Plugin {
		name, isTuple, ok := pluginEntryName(raw)
		if !ok {
			continue
		}
		switch stripNpmVersion(name) {
		case legacyPluginName:
			ref := legacyRef{pointer: fmt.Sprintf("/plugin/%d", i)}
			if isTuple {
				ref.spec = "/0"
			}
			legacy = append(legacy, ref)
		case PluginName:
			hasNew = true
		}
	}
	if len(legacy) == 0 {
		return nil, nil
	}
	legacy[0].keep = !hasNew
	return legacyPatch(legacy)
}

// legacyRef locates one legacy entry in a config document.
type legacyRef struct {
	// pointer is the entry's RFC 6901 path, e.g. "/plugin/0".
	pointer string
	// spec addresses the entry's package spec inside it: "" for a string
	// entry, "/0" for a [spec, options] tuple, "/package" for an object.
	spec string
	// keep rewrites the spec to the renamed package instead of removing the
	// entry.
	keep bool
}

// legacyPatch builds the RFC 6902 operations for refs, which must be in
// document order within each list.
func legacyPatch(refs []legacyRef) ([]byte, error) {
	type patchOp struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value string `json:"value,omitempty"`
	}
	var ops []patchOp
	// Higher indices first: a removal shifts every entry after it, so
	// operating back-to-front keeps every remaining path valid.
	for _, ref := range slices.Backward(refs) {
		if !ref.keep {
			ops = append(ops, patchOp{Op: "remove", Path: ref.pointer})
			continue
		}
		ops = append(ops, patchOp{Op: "replace", Path: ref.pointer + ref.spec, Value: PluginName})
	}
	return json.Marshal(ops)
}

// writeConfigAtomic replaces path with content through a temp file in the
// same directory plus rename, so a crash never leaves a half-written config.
// The original file's permission bits are preserved when they can be read.
func writeConfigAtomic(path string, content []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("rename to %s: %w", path, err)
	}
	return nil
}

// opencodeConfig is the subset of opencode's global config this launcher
// inspects. The vendored @opencode-ai/plugin types declare
// `plugin?: Array<string | [string, PluginOptions]>` — strings name the
// plugin module, two-element arrays carry plugin-specific options.
type opencodeConfig struct {
	Plugin []json.RawMessage `json:"plugin"`
}

// pluginInstalled reports whether the @grafana/agento11y-opencode plugin is
// already registered in opencode's global config. The config lives in
// $XDG_CONFIG_HOME/opencode (default $HOME/.config/opencode) as either
// opencode.json or opencode.jsonc. A missing file means opencode has
// never been configured with any plugins — treat as not installed.
//
// The file contents are parsed as JSONC: opencode's docs explicitly
// support comments and trailing commas (the very first config example
// in the docs has a trailing comma), so a strict json.Unmarshal would
// reject perfectly valid configs and trap us in a reinstall loop.
//
// This is the OpenCode 1 probe; readRegistrationV2 covers OpenCode 2.
func pluginInstalled() (bool, error) {
	_, found, err := installedPluginSource()
	return found, err
}

// Status reports whether the @grafana/agento11y-opencode plugin is registered
// in opencode's global config, and which version is installed. It reuses the
// read-only config probe and never installs or updates — `agento11y doctor`
// relies on this. The version comes from the package opencode installed into
// its cache, because the config entry is usually unpinned; a pinned spec
// (`@grafana/agento11y-opencode@1.2.3`) is the fallback. The cache belongs to
// opencode, so an absent or unreadable tree means the version is unknown, not
// an error.
//
// When opencode is on PATH, Status reads its version and answers with that
// release's config keys and cache layout; an unsupported release is an error,
// which doctor reports as unknown install state. Without opencode on PATH it
// tries the OpenCode 1 probe, then the OpenCode 2 one. An OpenCode 2 plugin
// the user turned off is reported as an error naming the selector that did.
func Status(ctx context.Context) (installed bool, version string, err error) {
	generation := generationUnknown
	if bin, lookErr := lookPath("opencode"); lookErr == nil {
		if generation, err = detectHost(ctx, bin); err != nil {
			return false, "", err
		}
	}
	switch generation {
	case generationV1:
		return statusV1()
	case generationV2:
		return statusV2()
	case generationUnknown:
	}
	installed, version, err = statusV1()
	if err != nil || installed {
		return installed, version, err
	}
	return statusV2()
}

func statusV1() (bool, string, error) {
	source, found, err := installedPluginSource()
	if err != nil {
		return false, "", err
	}
	if !found {
		return false, "", nil
	}
	if version := cachedVersion(source); version != "" {
		return true, version, nil
	}
	return true, versionFromNpmSpec(source), nil
}

func statusV2() (bool, string, error) {
	reg, err := readRegistrationV2()
	if err != nil {
		return false, "", err
	}
	if !reg.found() {
		return false, "", nil
	}
	if reg.disabled() {
		return false, "", fmt.Errorf("%s is configured in %s but turned off by %q in %s", PluginName, reg.path, reg.disabledBy, reg.disabledIn)
	}
	if version := cachedVersionV2(reg.source); version != "" {
		return true, version, nil
	}
	return true, versionFromNpmSpec(reg.source), nil
}

// cachedVersion returns the version of the plugin opencode installed for a
// config entry, or "" when it can't be read. opencode installs each plugin
// spec into its own cache directory named after the spec:
// <cache>/packages/<spec>/node_modules/<name>. Builds before opencode's cache
// migration installed straight under <cache>/node_modules/<name>, so that
// layout is probed second. This is the OpenCode 1 layout; cachedVersionV2
// reads OpenCode 2's.
func cachedVersion(source string) string {
	cache, err := cacheDirFn()
	if err != nil {
		return ""
	}
	name := stripNpmVersion(source)
	// A scoped name and a spec both contain a `/`; convert it to the OS
	// separator so `@scope/pkg` is two directories on Windows too.
	pkg := filepath.FromSlash(name)
	candidates := []string{
		filepath.Join(cache, "packages", filepath.FromSlash(cachedPackageSpec(source)), "node_modules", pkg),
		filepath.Join(cache, "node_modules", pkg),
	}
	for _, dir := range candidates {
		if version := packageVersion(dir); version != "" {
			return version
		}
	}
	return ""
}

// cachedVersionV2 returns the version OpenCode 2 installed for a config
// entry, or "" when it can't be read. OpenCode 2's npm service
// (packages/util/src/npm.ts) installs each spec into
// <cache>/npm/<key>/<generation>/node_modules/<name>, where key is the spec
// with a bare registry name normalized to <name>@latest and the active
// generation is the highest numeric directory. A key directory without
// generations resolves from <cache>/npm/<key>/node_modules/<name>, as the
// host's resolver does. OpenCode 1's cache paths are not consulted: a stale
// 1.x install there is not what OpenCode 2 loads.
func cachedVersionV2(source string) string {
	cache, err := cacheDirFn()
	if err != nil {
		return ""
	}
	root := filepath.Join(cache, "npm", filepath.FromSlash(npmCacheKey(source, runtime.GOOS == "windows")))
	if generation := latestGeneration(root); generation != "" {
		root = filepath.Join(root, generation)
	}
	return packageVersion(filepath.Join(root, "node_modules", filepath.FromSlash(stripNpmVersion(source))))
}

// npmCacheKey is OpenCode 2's cache directory name for a registry spec:
// `<name>@<spec>`, with `latest` for a bare name, and with characters Windows
// forbids in a path replaced by `_` on Windows.
func npmCacheKey(source string, windows bool) string {
	key := cachedPackageSpec(source)
	if !windows {
		return key
	}
	return strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"|?*`, r) {
			return '_'
		}
		return r
	}, key)
}

// latestGeneration returns the highest all-digit entry in dir, compared as
// numbers, or "" when there is none.
func latestGeneration(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	latest := ""
	for _, entry := range entries {
		name := entry.Name()
		if name == "" || strings.Trim(name, "0123456789") != "" {
			continue
		}
		if latest == "" || numericLess(latest, name) {
			latest = name
		}
	}
	return latest
}

// numericLess compares two all-digit strings as numbers of any size.
func numericLess(a, b string) bool {
	trim := func(s string) string {
		if t := strings.TrimLeft(s, "0"); t != "" {
			return t
		}
		return "0"
	}
	a, b = trim(a), trim(b)
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

// cachedPackageSpec returns the spec opencode installs a config entry as, which
// is also the name of the cache directory it installs into. opencode rewrites a
// bare package name to `<name>@latest` and passes anything else through, so a
// pinned spec and a dist tag keep their own directories.
func cachedPackageSpec(source string) string {
	if versionFromNpmSpec(source) == "" {
		return source + "@latest"
	}
	return source
}

// packageVersion reads the `version` a package directory's package.json
// declares. A missing, unreadable, or malformed file means unknown — the file
// belongs to opencode, so doctor never turns it into an error.
func packageVersion(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return ""
	}
	return pkg.Version
}

// installedPluginSource reads opencode's global config and returns the plugin
// entry source string that matches @grafana/agento11y-opencode (or its legacy
// @grafana/sigil-opencode name), if any. The config
// lives in $XDG_CONFIG_HOME/opencode (default $HOME/.config/opencode) as either
// opencode.json or opencode.jsonc; a missing file means no plugins are
// configured. The file is parsed as JSONC because opencode's docs allow
// comments and trailing commas.
func installedPluginSource() (source string, found bool, err error) {
	path, data, err := readConfigFile()
	if err != nil {
		return "", false, err
	}
	if data == nil {
		return "", false, nil
	}
	std, err := hujson.Standardize(data)
	if err != nil {
		return "", false, fmt.Errorf("parse %s: %w", path, err)
	}
	var c opencodeConfig
	if err := json.Unmarshal(std, &c); err != nil {
		return "", false, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, raw := range c.Plugin {
		name, _, ok := pluginEntryName(raw)
		if !ok {
			continue
		}
		if sourceMatchesPlugin(name) {
			return name, true, nil
		}
	}
	return "", false, nil
}

// pluginEntryName decodes one plugin-array entry, which is either a plain
// string naming the plugin module or a [name, options] tuple. Returns the
// entry's name (version pin included, if any), whether the entry was a
// tuple, and whether decoding succeeded.
func pluginEntryName(raw json.RawMessage) (name string, tuple, ok bool) {
	if err := json.Unmarshal(raw, &name); err == nil {
		return name, false, true
	}
	var asTuple []json.RawMessage
	if err := json.Unmarshal(raw, &asTuple); err != nil || len(asTuple) == 0 {
		return "", false, false
	}
	if err := json.Unmarshal(asTuple[0], &name); err != nil {
		return "", false, false
	}
	return name, true, true
}

// readConfigFile locates and reads opencode's global config, probing the
// recognised basenames in precedence order. A missing file returns
// ("", nil, nil) — opencode has never been configured. OpenCode 1 only.
func readConfigFile() (path string, data []byte, err error) {
	dir, err := configDirFn()
	if err != nil {
		return "", nil, err
	}
	for _, name := range configFileNames {
		candidate := filepath.Join(dir, name)
		b, err := os.ReadFile(candidate)
		if err == nil {
			return candidate, b, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		return "", nil, fmt.Errorf("read %s: %w", candidate, err)
	}
	return "", nil, nil
}

// configDoc is one global OpenCode config file.
type configDoc struct {
	path string
	data []byte
}

// readConfigFilesV2 reads every global config file OpenCode 2 loads, in load
// order: opencode.json, then opencode.jsonc. Missing files are skipped.
func readConfigFilesV2() ([]configDoc, error) {
	dir, err := configDirV2Fn()
	if err != nil {
		return nil, err
	}
	var docs []configDoc
	for _, name := range configFileNames {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		docs = append(docs, configDoc{path: path, data: data})
	}
	return docs, nil
}

// pluginEntryV2 is one OpenCode 2 plugin list entry after normalization.
type pluginEntryV2 struct {
	// target is the package spec, path, or selector the entry names.
	target string
	// remove marks a `-selector` string, which turns matching loaded plugins
	// off (packages/core/src/config/plugin/source.ts).
	remove bool
	// pointer and spec locate the entry for a rewrite; see legacyRef.
	pointer string
	spec    string
}

// pluginEntriesV2 returns one standardized document's plugin entries the way
// OpenCode 2 normalizes them (packages/core/src/config/normalize.ts): every
// valid legacy `plugin` entry, then every valid native `plugins` entry. A
// legacy entry is a string or a [spec, options] tuple; a native one is a
// string or {package, options}. The host drops entries of any other shape, and
// a key that is not a list, with a diagnostic, so they are skipped here too.
func pluginEntriesV2(std []byte) ([]pluginEntryV2, error) {
	var c struct {
		Plugin  json.RawMessage `json:"plugin"`
		Plugins json.RawMessage `json:"plugins"`
	}
	if err := json.Unmarshal(std, &c); err != nil {
		return nil, err
	}
	var entries []pluginEntryV2
	for i, raw := range jsonList(c.Plugin) {
		if e, ok := legacyEntryV2(raw); ok {
			e.pointer = fmt.Sprintf("/plugin/%d", i)
			entries = append(entries, e)
		}
	}
	for i, raw := range jsonList(c.Plugins) {
		if e, ok := nativeEntryV2(raw); ok {
			e.pointer = fmt.Sprintf("/plugins/%d", i)
			entries = append(entries, e)
		}
	}
	return entries, nil
}

func jsonList(raw json.RawMessage) []json.RawMessage {
	var list []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &list) != nil {
		return nil
	}
	return list
}

func isJSONObject(raw json.RawMessage) bool {
	var obj map[string]json.RawMessage
	return json.Unmarshal(raw, &obj) == nil && obj != nil
}

func stringEntryV2(s string) pluginEntryV2 {
	if selector, ok := strings.CutPrefix(s, "-"); ok {
		return pluginEntryV2{target: selector, remove: true}
	}
	return pluginEntryV2{target: s}
}

func legacyEntryV2(raw json.RawMessage) (pluginEntryV2, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return stringEntryV2(s), true
	}
	var tuple []json.RawMessage
	if json.Unmarshal(raw, &tuple) != nil || len(tuple) != 2 || json.Unmarshal(tuple[0], &s) != nil || !isJSONObject(tuple[1]) {
		return pluginEntryV2{}, false
	}
	// The host rewrites a tuple to {package, options}, which never removes.
	return pluginEntryV2{target: s, spec: "/0"}, true
}

func nativeEntryV2(raw json.RawMessage) (pluginEntryV2, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return stringEntryV2(s), true
	}
	var obj struct {
		Package *string         `json:"package"`
		Options json.RawMessage `json:"options"`
	}
	if !isJSONObject(raw) || json.Unmarshal(raw, &obj) != nil || obj.Package == nil {
		return pluginEntryV2{}, false
	}
	if obj.Options != nil && !isJSONObject(obj.Options) {
		return pluginEntryV2{}, false
	}
	return pluginEntryV2{target: *obj.Package, spec: "/package"}, true
}

// registration is where OpenCode 2's global config registers the plugin.
type registration struct {
	// source is the first configured spec naming the package or its legacy
	// name, in host load order. OpenCode keeps the first of two specs that
	// load the same plugin id and reports the later one as a duplicate.
	source string
	// path is the file that configures source.
	path string
	// disabledBy is the selector entry, as written, that left the plugin
	// turned off, and disabledIn the file holding it. Both are empty when the
	// plugin is on.
	disabledBy, disabledIn string
}

func (r registration) found() bool    { return r.source != "" }
func (r registration) disabled() bool { return r.disabledBy != "" }

// readRegistrationV2 replays OpenCode 2's plugin operations
// (packages/core/src/plugin/supervisor.ts) over the global config files, in
// load order, for this one plugin. A package entry loads the plugin; a
// `-selector` turns it off when it is already loaded and the selector matches
// its id (`*`, a `prefix.*` pattern, or the id); a later selector entry
// without the dash, or another entry for the package, turns it back on.
func readRegistrationV2() (registration, error) {
	docs, err := readConfigFilesV2()
	if err != nil {
		return registration{}, err
	}
	var reg registration
	loaded, enabled := false, false
	for _, doc := range docs {
		std, err := hujson.Standardize(doc.data)
		if err != nil {
			return registration{}, fmt.Errorf("parse %s: %w", doc.path, err)
		}
		entries, err := pluginEntriesV2(std)
		if err != nil {
			return registration{}, fmt.Errorf("parse %s: %w", doc.path, err)
		}
		for _, e := range entries {
			switch {
			case e.remove:
				if loaded && selectorMatches(e.target, pluginID) {
					enabled = false
					reg.disabledBy, reg.disabledIn = "-"+e.target, doc.path
				}
			case loaded && selectorMatches(e.target, pluginID):
				enabled = true
			case e.target == "*" || strings.HasSuffix(e.target, ".*") || strings.HasPrefix(e.target, "opencode."):
				// Selects already-loaded plugins; loads nothing.
			case sourceMatchesPlugin(e.target):
				if !loaded {
					reg.source, reg.path, loaded = e.target, doc.path, true
				}
				enabled = true
			}
		}
	}
	if enabled {
		reg.disabledBy, reg.disabledIn = "", ""
	}
	return reg, nil
}

// selectorMatches is OpenCode 2's plugin selector match: `*` matches every
// id, `prefix.*` every id starting with `prefix.`, anything else one id.
func selectorMatches(selector, id string) bool {
	if selector == "*" {
		return true
	}
	if strings.HasSuffix(selector, ".*") {
		return strings.HasPrefix(id, strings.TrimSuffix(selector, "*"))
	}
	return selector == id
}

// versionFromNpmSpec returns the pinned version of a scoped npm spec, e.g.
// "1.2.3" from "@grafana/agento11y-opencode@1.2.3". The leading `@` of a scoped
// package is part of the name, so only a later `@` separates the version.
// Returns "" for an unpinned spec.
func versionFromNpmSpec(spec string) string {
	at := strings.LastIndex(spec, "@")
	if at <= 0 {
		return ""
	}
	return spec[at+1:]
}

// sourceMatchesPlugin returns true when a plugin entry identifies the
// @grafana/agento11y-opencode package (or its legacy @grafana/sigil-opencode
// name), accounting for optional `@<version>` suffixes (e.g.
// `@grafana/agento11y-opencode@0.6.0`, `@grafana/agento11y-opencode@next`).
func sourceMatchesPlugin(source string) bool {
	if source == "" {
		return false
	}
	name := stripNpmVersion(source)
	return name == PluginName || name == legacyPluginName
}

// stripNpmVersion returns the package name portion of an npm spec,
// stripping the trailing `@<version>` segment if present. Scoped
// packages start with `@scope/...`; the leading `@` (index 0) is part of
// the name, not a version separator, so we look for the LAST `@` after
// index 0.
func stripNpmVersion(spec string) string {
	at := strings.LastIndex(spec, "@")
	if at <= 0 {
		return spec
	}
	return spec[:at]
}

// defaultConfigDir returns the directory holding opencode's global
// config, honouring $XDG_CONFIG_HOME (default $HOME/.config). Errors
// resolving the user's home directory are surfaced so callers don't
// probe a path silently rooted at CWD. OpenCode 1 reads
// $OPENCODE_CONFIG_DIR as an extra config directory, not as the global one,
// so this ignores it.
func defaultConfigDir() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "opencode"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir for opencode config: %w", err)
	}
	return filepath.Join(home, ".config", "opencode"), nil
}

// defaultConfigDirV2 returns OpenCode 2's global config directory, which
// `opencode plugin add` writes and the runtime reads: $OPENCODE_CONFIG_DIR
// when set (packages/cli/src/index.ts makes it the Global service's config
// root), else the XDG directory OpenCode 1 uses too.
func defaultConfigDirV2() (string, error) {
	if dir := os.Getenv("OPENCODE_CONFIG_DIR"); dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", fmt.Errorf("resolve OPENCODE_CONFIG_DIR: %w", err)
		}
		return abs, nil
	}
	return defaultConfigDir()
}

// defaultCacheDir returns the directory opencode installs plugin packages
// into, honouring $XDG_CACHE_HOME (default $HOME/.cache). This is the cache
// directory, not the config one: opencode keeps its config in
// $XDG_CONFIG_HOME/opencode and the installed packages here. Both OpenCode
// lines use it; OPENCODE_CONFIG_DIR does not move it.
func defaultCacheDir() (string, error) {
	if dir := os.Getenv("XDG_CACHE_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "opencode"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir for opencode cache: %w", err)
	}
	return filepath.Join(home, ".cache", "opencode"), nil
}
