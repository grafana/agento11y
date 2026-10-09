package usagestats

// ServiceName identifies this binary in the envelope; the receiver dispatches
// on it.
const ServiceName = "agento11y"

// Outcome values for Event.Outcome.
const (
	OutcomeOK = "ok"
	// OutcomeLaunched is a launcher that handed off via execve. Kept distinct
	// from OutcomeOK because the target CLI's own outcome is unknowable.
	OutcomeLaunched   = "launched"
	OutcomeHelp       = "help"
	OutcomeUsageError = "usage_error"
	OutcomeError      = "error"
	OutcomePanic      = "panic"
)

// Surface values naming which dispatch branch ran. Without it an `agento11y
// claude` row cannot be told from an `agento11y claude install` row.
const (
	SurfaceCommand  = "command"
	SurfaceLauncher = "launcher"
	SurfaceHelp     = "help"
)

// ErrorKind values, derived from the exit code alone.
const (
	ErrorKindUsage   = "usage_error"
	ErrorKindRuntime = "runtime_error"
)

// CommandUnknown is recorded when argv's first token is not a known command.
// The token itself is never sent.
const CommandUnknown = "unknown"

// Event is the flat event describing one invocation. The json encoding is
// exactly what travels on the wire.
//
// Privacy invariant:
//
//   - Command, Surface, Agent, Skill, Outcome, ErrorKind and CIProvider come
//     from vocabularies compiled into this binary (the help-page registry, the
//     constants here, the launcher and hook maps, skills.Names(), the CI
//     table). A token from argv that is not a member is never copied.
//   - Flags carries names only, allowlisted, and stops at the first bare "--".
//   - No field carries an argument value, endpoint, tenant id, token, tag, user
//     id, path, or error message. There is no error_message because this
//     binary's error strings interpolate paths and endpoints.
//   - ExitCode and DurationMS are raw numbers because they describe the
//     invocation, not the user's data. A count of imported sessions or captured
//     turns would be about volume and must be bucketed instead.
//
// Adding a field means amending the first-run notice and bumping its revision,
// or existing installs keep a disclosure that no longer matches.
type Event struct {
	Service string `json:"service"`
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`

	// InstallIDPersisted false marks a throwaway id, so the receiver can keep
	// an unwritable state directory from counting as a new install every run.
	InstallID          string `json:"install_id"`
	InstallIDPersisted bool   `json:"install_id_persisted"`

	Command string `json:"command"`
	Surface string `json:"surface"`
	Agent   string `json:"agent"`
	Skill   string `json:"skill"`
	Flags   string `json:"flags"`

	Outcome   string `json:"outcome"`
	ExitCode  int    `json:"exit_code"`
	ErrorKind string `json:"error_kind"`

	// DurationMS on SurfaceLauncher measures bootstrap only, not the agent
	// session: the process is replaced at the handoff. Read it with Surface.
	DurationMS int64 `json:"duration_ms"`

	IsTTY      bool   `json:"is_tty"`
	IsCI       bool   `json:"is_ci"`
	CIProvider string `json:"ci_provider"`
}
