package login

import (
	"context"
	"encoding/json"
	"os/exec"
	"slices"
	"time"
)

// gcxBinary is the Grafana Cloud CLI. Someone who has it configured already told
// it which stacks they work with, so login offers those instead of asking them
// to type a host it could have read.
const gcxBinary = "gcx"

// gcxStacksTimeout bounds the lookup. Reading the configuration is local work
// with no network call, so a gcx that has not answered by now is not going to;
// the stack question is about to be asked anyway, with the plain input.
const gcxStacksTimeout = 3 * time.Second

// lookPath is a package var so tests can report a gcx that is not installed.
var lookPath = exec.LookPath

// gcxStacks returns the stack origins gcx is configured for, the current
// context first, and nothing at all when gcx is absent or names no stack. It is
// a package var so tests can drive the prompt without a gcx on PATH.
var gcxStacks = readGcxStacks

// readGcxStacks asks gcx which stacks it has contexts for. Every failure
// resolves to no stacks: the list is a convenience, and login's own question
// still works without it, so a missing binary, an unreadable configuration, or
// an output shape a later gcx changed must all stay silent.
//
// The CLI is asked rather than $XDG_CONFIG_HOME/gcx/config.yaml read directly,
// because gcx merges a system file, the user file, and a .gcx.yaml in the
// working directory, and honours --config / $GCX_CONFIG over all three.
func readGcxStacks(ctx context.Context) []string {
	bin, err := lookPath(gcxBinary)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, gcxStacksTimeout)
	defer cancel()
	// -o json is the output-format flag, so the shape does not depend on gcx's
	// own agent-mode detection (which keys off CLAUDECODE and friends, and this
	// binary is launched by coding agents). Stderr is dropped with it: gcx
	// writes an unrelated hint about --json field selection there, and it would
	// land in the middle of the form.
	out, err := exec.CommandContext(ctx, bin, "config", "list-contexts", "-o", "json").Output()
	if err != nil {
		return nil
	}
	return parseGcxStacks(out)
}

// parseGcxStacks reads the origins out of `gcx config list-contexts -o json`.
//
// Two shapes in that output need handling. A context can carry no server —
// never configured, gcx reports a lone `default` context with the field absent —
// which is what makes an unconfigured gcx offer nothing. And several contexts
// can point at one stack (a token context and an OAuth context for the same
// host), so origins are deduplicated: the context name is not what login
// saves, and listing one host three times would only ask the user to pick
// between identical entries.
//
// A context for a Grafana on this machine is dropped, because the question this
// list answers asks for a Grafana Cloud URL. Typing one still works.
func parseGcxStacks(out []byte) []string {
	var doc struct {
		Contexts []struct {
			Current bool   `json:"current"`
			Server  string `json:"server"`
		} `json:"contexts"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil
	}
	var origins []string
	current := ""
	for _, c := range doc.Contexts {
		// stackOrigin rejects the empty server and normalises the rest into what
		// the printed links are built from, so a gcx server and a typed answer
		// reach the config file in the same shape.
		origin, err := stackOrigin(c.Server)
		if err != nil || isLoopbackOrigin(origin) {
			continue
		}
		if c.Current && current == "" {
			current = origin
		}
		if !slices.Contains(origins, origin) {
			origins = append(origins, origin)
		}
	}
	// The current context is the stack the user works with today, so it leads
	// the list. gcx returns the rest sorted by context name; keep that order.
	if i := slices.Index(origins, current); i > 0 {
		origins = slices.Insert(slices.Delete(origins, i, i+1), 0, current)
	}
	return origins
}
