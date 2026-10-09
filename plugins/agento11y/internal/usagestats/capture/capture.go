// Package capture holds the facts about the running invocation that are only
// known partway through it.
//
// A deliberate leaf: it imports nothing from agento11y, so any package can
// become a writer and recording a fact does not pull in the HTTP exporter.
// State is process-global because the invocation is.
package capture

import (
	"sync"
	"time"
)

// Invocation is the recorded shape of one CLI invocation. The zero value means
// "nothing recorded yet", never "succeeded".
type Invocation struct {
	Start    time.Time
	Command  string
	Surface  string
	Agent    string
	Skill    string
	Flags    string
	ExitCode int

	// Completed means the dispatcher returned; its absence detects a panic
	// without calling recover().
	Completed bool
	// Launched means the execve handoff was reached, so it is terminal.
	Launched bool
	// Suppressed means this invocation reports nothing.
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

// SetDispatch records which branch won and what it resolved from argv. This
// package does no validation: callers must never pass a raw argv token.
func SetDispatch(surface, command, agent, flags string) {
	mu.Lock()
	defer mu.Unlock()
	cur.Surface, cur.Command, cur.Agent, cur.Flags = surface, command, agent, flags
}

// SetSkill records a skill name, already validated by the caller.
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

// Suppress marks this invocation as reporting nothing. One-way, so a later
// branch cannot re-enable reporting for a hook that already opted out.
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

// Reset clears the state for tests; without it one test leaks into the next.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	cur = Invocation{}
}

// Duration returns how long the invocation has run, or 0 with no recorded
// start — measuring from the zero time would report ~2000 years.
func (i Invocation) Duration(now time.Time) time.Duration {
	if i.Start.IsZero() {
		return 0
	}
	return now.Sub(i.Start)
}
