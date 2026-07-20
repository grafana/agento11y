package copilot

import (
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// Test seams.
var (
	surfaceDetect = detectSurface
	// processInfoFn resolves a PID to its command and parent PID. Overridden
	// in tests to model a synthetic process tree without spawning `ps`.
	processInfoFn = processInfo
)

// maxSurfaceAncestry bounds the process-tree walk in detectSurface. The hook
// is reached within a couple of levels of either host (sh -> copilot, or the
// VS Code extension host), so a small depth is plenty and keeps the cost of
// the `ps` probes negligible.
const maxSurfaceAncestry = 6

// detectSurface determines which host fired the current hook. The Copilot
// hook payload carries no host identifier and a single ~/.copilot/hooks file
// is read by BOTH the Copilot CLI and Copilot Chat in VS Code, so the surface
// must be inferred at runtime. Precedence:
//
//  1. AGENTO11Y_COPILOT_HOOK_SURFACE env — an explicit override. The plugin's
//     hooks.json sets this to "copilot-cli", so anyone driving capture via
//     the plugin (rather than the shared user file) is labelled precisely.
//  2. Process ancestry — if a `copilot` binary is an ancestor of this
//     process, the Copilot CLI invoked us, so the surface is "copilot-cli".
//     This holds even when the CLI runs inside a VS Code integrated terminal,
//     because the ancestry there is sh -> copilot, not the VS Code extension
//     host.
//  3. Otherwise the only other host that fires these hooks is Copilot Chat in
//     VS Code, so the surface is "vscode".
func detectSurface() string {
	if s := envconfig.Getenv("COPILOT_HOOK_SURFACE"); s != "" {
		return s
	}
	if hasCopilotAncestor(os.Getppid(), maxSurfaceAncestry) {
		return "copilot-cli"
	}
	return "vscode"
}

// hasCopilotAncestor walks up to maxDepth ancestors starting at pid and
// reports whether any of them is a `copilot` process.
func hasCopilotAncestor(pid, maxDepth int) bool {
	for i := 0; i < maxDepth && pid > 1; i++ {
		comm, ppid, ok := processInfoFn(pid)
		if !ok {
			return false
		}
		if strings.Contains(strings.ToLower(comm), "copilot") {
			return true
		}
		pid = ppid
	}
	return false
}

// processInfo returns the command name and parent PID for pid using `ps`.
// It returns ok=false on any error or unparseable output so callers degrade
// gracefully (treating the surface as VS Code, the more common interactive
// host). Output of `ps -o ppid=,comm= -p <pid>` looks like:
//
//	"  1234 /opt/homebrew/bin/copilot"
//
// so the first field is the parent PID and the remainder is the command
// (which may be an absolute path — substring matching on "copilot" handles
// both bare-name and full-path forms).
func processInfo(pid int) (comm string, ppid int, ok bool) {
	out, err := exec.Command("ps", "-o", "ppid=,comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", 0, false
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) < 2 {
		return "", 0, false
	}
	parent, err := strconv.Atoi(fields[0])
	if err != nil {
		return "", 0, false
	}
	return strings.Join(fields[1:], " "), parent, true
}
