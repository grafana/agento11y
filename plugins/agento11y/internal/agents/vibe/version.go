package vibe

import (
	"context"
	"log"
	"regexp"
	"strconv"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/launcher"
)

// hookTypeSet is the `type` name vibe accepts for each of the three events it
// fires. Vibe 2.21.0 renamed all three at once, and the name is validated
// against an enum (HookType in vibe/core/hooks/models.py), so an entry
// spelled for the other generation is rejected. Which set to write is
// therefore a function of the installed vibe.
type hookTypeSet struct {
	postAgent string
	preTool   string
	postTool  string
}

var (
	// currentHookTypes is what vibe 2.21.0 and later accept.
	currentHookTypes = hookTypeSet{postAgent: "post_agent", preTool: "pre_tool", postTool: "post_tool"}
	// preRenameHookTypes is what vibe accepted before 2.21.0, while hooks were
	// still experimental.
	preRenameHookTypes = hookTypeSet{postAgent: "post_agent_turn", preTool: "before_tool", postTool: "after_tool"}
)

// renameMajor and renameMinor are the first vibe release carrying
// currentHookTypes.
const (
	renameMajor = 2
	renameMinor = 21
)

// versionProbeTimeout caps the `vibe --version` call. Vibe answers it from
// argparse, before it imports its config stack, but a frozen build still pays
// its own bootstrap first, so the budget covers a cold start rather than the
// warm case. The probe sits in front of an exec the user is waiting on, so it
// cannot be unbounded either. On timeout the caller writes current-generation
// types, which a pre-2.21.0 vibe then rejects with a warning per entry; the
// next launch usually gets a warm answer and repairs the file.
const versionProbeTimeout = 5 * time.Second

// vibeVersionRE matches the major.minor of `vibe --version` output, which
// argparse renders as "<prog> <version>". A suffix (2.21.0rc1, 2.21.0.dev0)
// is left to the caller: only major.minor decides the type set.
var vibeVersionRE = regexp.MustCompile(`([0-9]+)\.([0-9]+)`)

// Test seam.
var vibeVersionOutput = func(ctx context.Context, bin string) ([]byte, error) {
	return launcher.Output(ctx, bin, "--version")
}

// hookTypesFor reports the type set the vibe on PATH accepts.
//
// Every failure path returns currentHookTypes: no vibe on PATH (doctor runs
// without it), a probe that errors or times out, or output we cannot parse.
// Guessing current is the safer default, because a pre-2.21.0 vibe also needs
// VIBE_ENABLE_EXPERIMENTAL_HOOKS, so its users had to opt in explicitly and
// are the rarer case.
func hookTypesFor(ctx context.Context, logger *log.Logger) hookTypeSet {
	bin, err := lookPath("vibe")
	if err != nil {
		return currentHookTypes
	}
	ctx, cancel := context.WithTimeout(ctx, versionProbeTimeout)
	defer cancel()
	out, err := vibeVersionOutput(ctx, bin)
	if err != nil {
		logf(logger, "vibe --version: %v", err)
		return currentHookTypes
	}
	types, ok := hookTypesForVersion(string(out))
	if !ok {
		logf(logger, "vibe --version: cannot parse %q", string(out))
	}
	return types
}

// hookTypesForVersion maps `vibe --version` output to a type set. ok is false
// when the output carries no recognizable version, in which case the caller
// gets currentHookTypes and something to log.
func hookTypesForVersion(out string) (types hookTypeSet, ok bool) {
	m := vibeVersionRE.FindStringSubmatch(out)
	if m == nil {
		return currentHookTypes, false
	}
	major, err := strconv.Atoi(m[1])
	if err != nil {
		return currentHookTypes, false
	}
	minor, err := strconv.Atoi(m[2])
	if err != nil {
		return currentHookTypes, false
	}
	if major < renameMajor || (major == renameMajor && minor < renameMinor) {
		return preRenameHookTypes, true
	}
	return currentHookTypes, true
}

// logf writes to logger when the caller has one. Status has no logger, so the
// version probe has to tolerate a nil one.
func logf(logger *log.Logger, format string, args ...any) {
	if logger == nil {
		return
	}
	logger.Printf(format, args...)
}
