package usagestats

// ServiceName identifies this binary in the event envelope; the usage-stats
// receiver dispatches on it.
const ServiceName = "agento11y"

// Outcome values for Event.Outcome.
const (
	// OutcomeOK is a command that ran to completion and exited 0.
	OutcomeOK = "ok"
	// OutcomeLaunched is a launcher that handed off via execve. The target
	// CLI's own outcome is unknowable from here: the process image is gone.
	// Kept distinct from OutcomeOK so nobody aggregates a handoff with a
	// completed command.
	OutcomeLaunched = "launched"
	// OutcomeHelp is a help page printed on request.
	OutcomeHelp = "help"
	// OutcomeUsageError is exit 2: an unknown command, verb, or flag, or
	// missing arguments. The command never ran.
	OutcomeUsageError = "usage_error"
	// OutcomeError is exit 1: the command ran and failed.
	OutcomeError = "error"
	// OutcomePanic is an unrecovered panic.
	OutcomePanic = "panic"
)

// Surface values for Event.Surface, naming which dispatch branch ran. Without
// this an `agento11y claude` row cannot be told from an `agento11y claude
// install` row, because both carry agent="claude".
const (
	SurfaceCommand  = "command"
	SurfaceLauncher = "launcher"
	SurfaceHelp     = "help"
)

// ErrorKind values for Event.ErrorKind, derived from the exit code alone.
const (
	ErrorKindUsage   = "usage_error"
	ErrorKindRuntime = "runtime_error"
)

// CommandUnknown is the placeholder recorded when argv's first token is not a
// known command. The token itself is never sent: an unknown command is, by
// definition, a string this binary does not control.
const CommandUnknown = "unknown"

// Event is the flat wide event describing one agento11y invocation. Field
// names follow the usage-stats JSON schema (snake_case); the json encoding of
// this struct is exactly what travels on the wire (see Export).
//
// Privacy invariant, stated as separate rules because one blanket sentence
// about "no sensitive data" is a promise that has to be re-audited against the
// whole struct every time a field is added, and such sentences are reliably
// false by the third field:
//
//   - Command, Surface, Agent, Skill, Outcome, ErrorKind and CIProvider are
//     drawn from CLOSED vocabularies compiled into this binary: the
//     publicHelpPages() key set, the constants above, the launchers/agents map
//     keys, skills.Names(), and the CI provider table. A token from argv that
//     is not a member of its vocabulary is replaced — never copied. This is a
//     structural property of the builder, not a convention: no code path
//     concatenates argv into an event.
//   - Flags carries flag NAMES only, intersected with an allowlist built from
//     the flag sets this binary defines, and stops at the first bare "--" so
//     arguments forwarded to the wrapped CLI cannot reach it.
//   - No field carries an argument value, a flag value, an endpoint, a tenant
//     id, a token, a tag key or value, a user id, a repository, a branch, a
//     working directory, a file path, or an error message. Several of those
//     were considered and rejected; see the plan and the PR for which and why.
//     In particular there is no error_message: this binary's error strings
//     interpolate paths and endpoints.
//   - ExitCode and DurationMS are raw numbers, and that is fine: they describe
//     the invocation, not how much of anyone's data it touched. Do not
//     generalise this to "numbers are fine" — a raw count of imported sessions
//     or captured turns would describe the user's volume and must be bucketed
//     if it is ever wanted.
//
// Adding a field means amending the first-run notice AND bumping its revision,
// or existing installations keep the disclosure they were shown while
// collection changes underneath it.
type Event struct {
	// Envelope.
	Service string `json:"service"`
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`

	// Per-installation identity. Persisted=false marks a throwaway id used
	// because the state directory could not be written, so the receiver can
	// exclude those rows from installation counts rather than counting one
	// machine many times.
	InstallID          string `json:"install_id"`
	InstallIDPersisted bool   `json:"install_id_persisted"`

	// What ran. Closed vocabularies, per the invariant above.
	Command string `json:"command"`
	Surface string `json:"surface"`
	Agent   string `json:"agent"`
	Skill   string `json:"skill"`
	Flags   string `json:"flags"`

	Outcome   string `json:"outcome"`
	ExitCode  int    `json:"exit_code"`
	ErrorKind string `json:"error_kind"`

	// DurationMS is the whole invocation. On SurfaceLauncher it measures
	// bootstrap only — plugin install or refresh, an optional login prompt, an
	// optional local daemon start — because the process is replaced at the
	// handoff and the agent session that follows is not ours to time. Read it
	// together with Surface, never as a session length.
	DurationMS int64 `json:"duration_ms"`

	// Execution context. CI variables are read for presence only: their values
	// carry repository names, URLs, and sometimes tokens.
	IsTTY      bool   `json:"is_tty"`
	IsCI       bool   `json:"is_ci"`
	CIProvider string `json:"ci_provider"`
}
