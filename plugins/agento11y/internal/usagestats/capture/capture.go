// Package capture holds the facts about the running invocation that usage
// statistics need but that are only known partway through it: when it started,
// which dispatch branch won, whether it should report at all, and how it
// ended.
//
// It is a true leaf. It imports nothing from agento11y, not even the
// usagestats package it serves, so any package can become a writer without an
// import-graph audit — and so recording a fact does not drag the HTTP exporter
// into the importing package. Writers will eventually live in internal/entry,
// internal/local, and internal/agents/*.
//
// The state is process-global because the invocation is. There is exactly one
// per process, it is written from whichever branch runs, and it is read once
// at exit by a caller that has no way to receive a value threaded through
// fifteen return paths.
package capture

import (
	"sync"
	"time"
)

// Invocation is the recorded shape of one CLI invocation. The zero value means
// "nothing recorded yet", which the event builder must treat as an unknown
// command rather than as a successful one.
type Invocation struct {
	// Start is when the process began, set as early as possible so Duration
	// covers the whole invocation.
	Start time.Time
	// Command is the resolved command path, drawn from the help-page registry.
	Command string
	// Surface names the dispatch branch: command, launcher, or help.
	Surface string
	// Agent is the matched launcher or hook agent name.
	Agent string
	// Skill is a bundled skill name, already validated against skills.Names().
	Skill string
	// Flags is the sorted, comma-joined list of allowlisted flag names.
	Flags string
	// ExitCode is the process exit code, set on the paths that exit non-zero.
	ExitCode int
	// Completed records that the dispatcher returned without panicking. Its
	// absence is how a panic is detected without calling recover() and
	// changing what the runtime prints.
	Completed bool
	// Launched records that the execve handoff was reached. The target CLI's
	// own outcome is unknowable afterwards, so this is terminal.
	Launched bool
	// Suppressed means this invocation reports nothing at all.
	Suppressed bool
}

var (
	mu  sync.Mutex
	cur Invocation
)

// SetStart records the process start time.
func SetStart(t time.Time) {
	mu.Lock()
	defer mu.Unlock()
	cur.Start = t
}

// SetDispatch records which branch won and what it resolved from argv. Every
// value here must already be a member of a closed vocabulary: this package
// does no validation and no redaction, because it cannot — it does not know
// what the vocabularies are. Callers are responsible for never passing a raw
// argv token.
func SetDispatch(surface, command, agent, flags string) {
	mu.Lock()
	defer mu.Unlock()
	cur.Surface, cur.Command, cur.Agent, cur.Flags = surface, command, agent, flags
}

// SetSkill records a bundled skill name, already validated by the caller.
func SetSkill(skill string) {
	mu.Lock()
	defer mu.Unlock()
	cur.Skill = skill
}

// SetExit records the exit code.
func SetExit(code int) {
	mu.Lock()
	defer mu.Unlock()
	cur.ExitCode = code
}

// SetCompleted records that the dispatcher returned normally.
func SetCompleted() {
	mu.Lock()
	defer mu.Unlock()
	cur.Completed = true
}

// SetLaunched records that the execve handoff was reached.
func SetLaunched() {
	mu.Lock()
	defer mu.Unlock()
	cur.Launched = true
}

// Suppress marks this invocation as reporting nothing. It is one-way: once
// suppressed an invocation stays suppressed, so a later dispatch branch cannot
// accidentally re-enable reporting for a hook that already opted out.
func Suppress() {
	mu.Lock()
	defer mu.Unlock()
	cur.Suppressed = true
}

// Snapshot returns a copy of the recorded invocation.
func Snapshot() Invocation {
	mu.Lock()
	defer mu.Unlock()
	return cur
}

// Reset clears the state. It exists for tests: the state is process-global, so
// without it one test's invocation leaks into the next.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	cur = Invocation{}
}

// Duration returns how long the invocation has run, or 0 when no start time
// was recorded. Zero is reported rather than a duration measured from the
// zero time, which would be ~2000 years and would poison any aggregate.
func (i Invocation) Duration(now time.Time) time.Duration {
	if i.Start.IsZero() {
		return 0
	}
	return now.Sub(i.Start)
}
