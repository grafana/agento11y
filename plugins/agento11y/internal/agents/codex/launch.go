package codex

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/launcher"
	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
)

const (
	// marketplaceRepo is the source argument passed to
	// `codex plugin marketplace add`.
	marketplaceRepo = "grafana/agento11y"
	// marketplaceAlias is the marketplace name declared in
	// .agents/plugins/marketplace.json. A local snapshot that predates the
	// sigil rename exposes the plugin only under legacyMarketplaceAlias; see
	// pluginKeys for the keys that follow from that.
	marketplaceAlias       = "agento11y"
	legacyMarketplaceAlias = "grafana-sigil"
	// PluginName is the codex plugin name declared in
	// plugins/codex/.codex-plugin/plugin.json; legacyPluginName is the
	// pre-rename name. Codex has no rename mechanism: once the local
	// marketplace snapshot is upgraded past the rename, a legacy install
	// stops resolving and its config.toml entry lingers as an orphan, so the
	// launcher migrates it (remove legacy + add current) itself.
	PluginName       = "agento11y-codex"
	legacyPluginName = "sigil-codex"

	updateCheckTTL = 24 * time.Hour
)

// Test seams.
var (
	lookPath   = exec.LookPath
	execFn     = launcher.DefaultExec
	runInstall = defaultRunInstall
	runUpdate  = defaultRunUpdate
	runSteps   = launcher.RunSteps
	runFirst   = launcher.RunFirst
	runOutput  = launcher.Output
	pluginList = defaultPluginList
)

// Launch resolves the `codex` binary on PATH, ensures the codex plugin
// is registered and enabled in codex's plugin store (running
// `codex plugin marketplace add` + `codex plugin add` once if it is not),
// and then exec's codex with the supplied args. When localEnv is non-nil,
// the child receives local-mode SIGIL_ENDPOINT,
// SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT and placeholder auth values so it talks
// to the in-process receiver instead of Grafana Cloud.
func Launch(ctx context.Context, args []string, localEnv *local.LaunchEnv, _ io.Reader, _, stderr io.Writer, logger *log.Logger, binaryVersion string) error {
	return launcher.Bootstrap(ctx, launcher.BootstrapSpec{
		BinName:     "codex",
		PluginLabel: PluginName,
		LookPath:    lookPath,
		ExecFn:      execFn,
		Args:        args,
		Env:         local.Environ(localEnv),
		Logger:      logger,
		Stderr:      stderr,
		// Treat a failed `codex plugin list` the same as missing: codex's
		// own CLI is the source of truth and will fail loudly if something
		// is genuinely wrong.
		Probe:       pluginInstalled,
		ProbeErrLog: "codex plugin list probe",
		Install:     runInstall,
		InstallRecoveryHint: func(w io.Writer) {
			fmt.Fprintf(w,
				"          codex plugin marketplace add %s\n"+
					"          codex plugin add %s@%s\n",
				marketplaceRepo, PluginName, marketplaceAlias)
		},
		// One-time trust step the launcher cannot automate: codex requires
		// the user to open /hooks inside the TUI and accept each hook after
		// the plugin is installed (or reinstalled under a new name).
		PostInstallHint: func(w io.Writer) {
			fmt.Fprintf(w,
				"agento11y: open /hooks inside codex and trust the agento11y hooks\n"+
					"           to start exporting turns.\n")
		},
		Update: runUpdate,
		UpdateRecoveryHint: func(w io.Writer) {
			fmt.Fprintf(w,
				"          codex plugin marketplace upgrade\n"+
					"          codex plugin add %s@%s\n",
				PluginName, marketplaceAlias)
		},
		UpdateTTL:     updateCheckTTL,
		BinaryVersion: binaryVersion,
	})
}

// Status reports whether the codex plugin is installed and enabled. It
// reuses the read-only `codex plugin list` probe and never installs, updates,
// or writes update-check state — `agento11y doctor` relies on this. codex's plugin
// list does not expose a version, so version is always empty (best-effort).
func Status(ctx context.Context) (installed bool, version string, err error) {
	bin, err := lookPath("codex")
	if err != nil {
		return false, "", err
	}
	installed, err = pluginInstalled(ctx, bin)
	return installed, "", err
}

func defaultRunInstall(ctx context.Context, bin string, w io.Writer) error {
	if err := runSteps(ctx, bin, w, [][]string{{"plugin", "marketplace", "add", marketplaceRepo}}); err != nil {
		return err
	}
	return ensurePlugin(ctx, bin, w, "")
}

func defaultRunUpdate(ctx context.Context, bin string, w io.Writer) error {
	// Snapshot the key that answers today. The upgrade below can drop the name
	// or the alias that key uses, and ensurePlugin then lands on a different
	// one; codex treats that as a new hook identity, so the user has to
	// re-trust the hooks.
	installedKey := ""
	if out, err := pluginList(ctx, bin); err == nil {
		installedKey = enabledKey(out)
	}
	if err := runSteps(ctx, bin, w, [][]string{{"plugin", "marketplace", "upgrade"}}); err != nil {
		return err
	}
	return ensurePlugin(ctx, bin, w, installedKey)
}

// ensurePlugin registers the plugin under the first key of pluginKeys that
// codex accepts, then reconciles the state that key implies.
//
// After a successful current-name add it removes the legacy entry: the
// snapshot is post-rename at that point, so a leftover legacy entry can
// never resolve again and would sit in config.toml as an orphan. Removal is
// best-effort and exits 0 even when nothing is registered, so it is safe on
// fresh installs.
//
// installedKey is the key that answered before this call, empty on a fresh
// install. When the winning key differs, the install moved and codex sees a
// new hook identity, so print the one-time /hooks re-trust hint naming the
// key the user will now find there.
func ensurePlugin(ctx context.Context, bin string, w io.Writer, installedKey string) error {
	keys := pluginKeys()
	candidates := make([][]string, 0, len(keys))
	for _, k := range keys {
		candidates = append(candidates, []string{"plugin", "add", k})
	}
	idx, err := runFirst(ctx, bin, w, candidates)
	if err != nil {
		return err
	}
	key := keys[idx]
	// Only the legacy plugin name resolving means the snapshot still predates
	// the rename: that install is live, so leave it alone.
	if strings.HasPrefix(key, legacyPluginName+"@") {
		return nil
	}
	if installedKey != "" && installedKey != key {
		fmt.Fprintf(w,
			"agento11y: migrated %s to %s — open /hooks inside codex and\n"+
				"           trust the %s hooks to keep exporting turns.\n",
			installedKey, key, PluginName)
	}
	_, _ = runOutput(ctx, bin, "plugin", "remove", legacyPluginName+"@"+legacyMarketplaceAlias)
	return nil
}

// pluginKeys lists the `<plugin>@<marketplace>` keys the launcher installs and
// accepts, in preference order. codex derives plugin keys from the marketplace
// manifest and does not document whether `plugin marketplace upgrade` re-keys
// an existing registration to the manifest's new name, so a registration made
// before the rename may keep answering to legacyMarketplaceAlias while already
// serving the renamed plugin. That middle key is therefore a first-class state
// rather than a fallback.
//
// legacyPluginName@marketplaceAlias is deliberately absent: the alias comes
// from the manifest name, so a snapshot that still lists legacyPluginName is
// named legacyMarketplaceAlias and the pair cannot occur. Counting it would
// suppress bootstrap for a key codex can never resolve, and removing it would
// delete an unrelated marketplace's plugin.
func pluginKeys() []string {
	return []string{
		PluginName + "@" + marketplaceAlias,
		PluginName + "@" + legacyMarketplaceAlias,
		legacyPluginName + "@" + legacyMarketplaceAlias,
	}
}

// defaultPluginList shells out to `codex plugin list` and returns the raw
// output. Output is a human-formatted table but stable: each plugin row
// starts with the `<plugin>@<marketplace>` key followed by a status column
// like `installed, enabled` (verified against codex 0.144.6).
func defaultPluginList(ctx context.Context, bin string) ([]byte, error) {
	return launcher.Output(ctx, bin, "plugin", "list")
}

// pluginInstalled reports whether any key in pluginKeys is registered and
// enabled in codex's plugin store. A legacy install counts: while the local
// marketplace snapshot predates the rename it keeps working as-is, and the
// periodic update path performs the migration once
// `codex plugin marketplace upgrade` pulls a post-rename copy.
//
// We shell out to `codex plugin list` because it's the source of truth and
// doesn't depend on the user's $XDG_CONFIG_HOME layout. Anything other than
// an `(installed, enabled)` marker is treated as not installed —
// `(installed, disabled)` shouldn't be silently re-enabled in the user's
// face but re-running install on codex's side is idempotent and surfaces
// the disabled state to the user.
func pluginInstalled(ctx context.Context, bin string) (bool, error) {
	out, err := pluginList(ctx, bin)
	if err != nil {
		return false, err
	}
	return enabledKey(out) != "", nil
}

// enabledKey returns the first key of pluginKeys that `codex plugin list`
// marks installed and enabled; empty when none is.
func enabledKey(out []byte) string {
	for _, key := range pluginKeys() {
		if keyEnabled(out, key) {
			return key
		}
	}
	return ""
}

// keyEnabled reports whether `codex plugin list` output marks the exact
// `<plugin>@<marketplace>` key as installed and enabled.
func keyEnabled(out []byte, key string) bool {
	needle := []byte(key)
	for line := range bytes.SplitSeq(out, []byte{'\n'}) {
		// Anchor on the first whitespace-delimited token so suffix collisions
		// like `my-agento11y-codex@agento11y` or
		// `agento11y-codex@agento11y-staging` don't satisfy the check.
		fields := bytes.Fields(line)
		if len(fields) == 0 || !bytes.Equal(fields[0], needle) {
			continue
		}
		if bytes.Contains(line, []byte("installed, enabled")) {
			return true
		}
	}
	return false
}
