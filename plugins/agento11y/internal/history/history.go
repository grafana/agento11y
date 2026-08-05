// Package history imports pre-existing local coding-agent sessions into
// agento11y as backfilled, live-like data.
//
// The package owns everything an importer does not: the agent registry,
// filesystem discovery, selection filtering, redaction and truncation, the
// idempotency ledger, and backdated export. A per-agent importer supplies only
// four small methods (see [Importer]); everything else is shared.
//
// Nothing here decodes prompt, response, thinking, or tool text outside
// [Importer.Turns]. Discovery reads a bounded window of each session file to
// count turns and pick out metadata, but no part of those bytes is decoded into
// content, returned, exported, or stored; the ledger is content-free by
// construction. A user can therefore inspect what would be imported without any
// of it being surfaced or kept.
package history

import (
	"context"
	"fmt"
	"iter"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
)

// AgentID is the canonical agent name. It matches the AgentName each live
// adapter already emits (claudecode's mapper writes "claude-code", codex's
// writes "codex"), so an imported generation carries the same agent name as a
// live one.
type AgentID string

// The agents with a registered importer. They are declared here rather than in
// their importer files so callers can name them without importing the agent
// package and triggering its init.
const (
	AgentClaudeCode AgentID = "claude-code"
	AgentCodex      AgentID = "codex"
	AgentPi         AgentID = "pi"
)

// SourceRef locates a single historical turn on disk. It is content-free: it
// carries identity (which session, file, and turn) but never prompt, response,
// or tool payloads. It is the input to both the deterministic generation ID and
// the hashed ledger key, so its fields must stay stable across releases.
type SourceRef struct {
	Agent      AgentID
	SessionID  string // native session or conversation ID
	SourcePath string // file the session was read from
	TurnIndex  int    // 0-based position within the session
	TurnID     string // native turn or message ID when the agent provides one
}

// SessionPreview is the metadata-only view of a discovered session. It is what
// the CLI picker and the viewer's import card render, so it deliberately
// excludes prompt and response snippets.
type SessionPreview struct {
	Agent          AgentID
	SessionID      string
	Title          string
	Workspace      string // workspace or repo path, when known
	SourcePath     string // file the session lives in
	TurnCount      int
	ApproxTurns    bool // TurnCount could not be counted within the preview budget
	SizeBytes      int64
	StartedAt      time.Time
	LastActivityAt time.Time
	Active         bool // the source file was written recently, so an agent may still be appending
}

// QualityReport records the approximations an importer had to make for a single
// turn. A minimal form ships with the generation as metadata so dashboards can
// flag backfilled-but-approximate data; the detailed form stays local.
type QualityReport struct {
	ApproxStartedAt   bool // StartedAt was synthesized, not read from the source
	ApproxCompletedAt bool // CompletedAt was synthesized
	ApproxUsage       bool // token usage was missing or estimated
	MissingModel      bool // no model name was recoverable
	Truncated         bool // the sanitizer truncated a content field
	Notes             []string
}

// HistoricalGeneration is one historical model turn, normalized into the SDK's
// generation shape and ready for redaction and backdated export. Gen carries
// the turn content, Source identifies where it came from, and Quality records
// the approximations.
type HistoricalGeneration struct {
	Source  SourceRef
	Gen     agento11y.Generation
	Quality QualityReport
}

// Discovery is the result of scanning one agent's history. Warnings collects
// non-fatal problems (an unreadable file, schema drift) so the caller can show
// them without aborting the whole import.
type Discovery struct {
	Agent    AgentID
	Sessions []SessionPreview
	Warnings []string
}

// ImportResult is the outcome of one import run.
type ImportResult struct {
	Agent      AgentID
	Sessions   int  // sessions selected for import
	Imported   int  // generations successfully exported
	Skipped    int  // generations already recorded in the ledger
	Failed     int  // generations that errored during export
	Collisions int  // native session IDs claimed by more than one source
	DryRun     bool // nothing was decoded, exported, or stored
	// Warnings say why a session or a batch failed. Failed alone cannot be
	// acted on: it does not distinguish an unreadable file from a schema
	// change. Like [Discovery.Warnings], they name sessions and paths, never
	// session content.
	Warnings []string
}

// Importer reads one agent's local history. The framework owns walking,
// warning collection, active-session detection, sorting, filtering, redaction,
// the ledger, and export, so an importer for an agent that already has a Go
// live mapper is four small methods.
//
// Register an implementation from its own file's init with [Register].
type Importer interface {
	// Roots returns the directories to scan. A missing root is not an error:
	// discovery yields nothing for an agent that never ran on this machine.
	Roots() []string

	// Match reports whether a file found under a root is one of this agent's
	// session files. It is given the full path.
	Match(path string) bool

	// Preview returns session metadata only. It MUST NOT read the whole file
	// and MUST NOT return prompt, response, thinking, or tool text: a preview
	// runs over every session on the machine behind an interactive request, and
	// the result is rendered to the user before they choose what to import. The
	// window it reads may contain content; none of it may leave the method.
	//
	// The budget is [PreviewByteBudget]: read a head window and a tail window
	// whose combined size stays within it, plus os.Stat metadata. Set
	// SessionPreview.ApproxTurns when the turn count could not be established
	// inside that budget. ok is false for a file that is not a usable session.
	Preview(ctx context.Context, path string) (preview SessionPreview, ok bool, err error)

	// Turns yields the session's turns in order. It is consumed lazily and one
	// turn at a time, so an implementation must not build the whole session in
	// memory: rollouts of several hundred megabytes exist. Content is raw here;
	// the framework runs [Sanitizer] once before export.
	Turns(ctx context.Context, sess SessionPreview) iter.Seq2[HistoricalGeneration, error]
}

// AgentSpec is the static, content-free description of a supported agent.
// Registration carries it, so adding an importer needs no edit to a separate
// table that could be forgotten.
type AgentSpec struct {
	// ID is the canonical agent name, matching the live adapter's AgentName.
	ID AgentID
	// DisplayName is the human-readable label for the CLI and the viewer.
	DisplayName string
	// Aliases are additional names the CLI and the HTTP API accept. They
	// resolve to ID; responses always report ID.
	Aliases []string
}

// registration pairs a spec with its importer factory.
type registration struct {
	spec    AgentSpec
	factory func() Importer
}

var (
	registryMu sync.RWMutex
	registry   = map[AgentID]registration{}
	aliases    = map[string]AgentID{}
)

// Register wires an importer for an agent. Call it from the importer file's
// init.
//
// It panics on an empty ID, a nil factory, a duplicate canonical ID, or an
// alias already claimed by another agent. A silent no-op would leave a
// half-registered agent invisible to the CLI, the API, and the viewer with no
// failing test to show for it.
func Register(spec AgentSpec, factory func() Importer) {
	if strings.TrimSpace(string(spec.ID)) == "" {
		panic("history: Register with empty agent ID")
	}
	if factory == nil {
		panic(fmt.Sprintf("history: Register %q with nil importer factory", spec.ID))
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[spec.ID]; dup {
		panic(fmt.Sprintf("history: duplicate importer registration for %q", spec.ID))
	}
	for _, alias := range spec.Aliases {
		key := normalizeAgentKey(alias)
		if key == "" {
			continue
		}
		if owner, taken := aliases[key]; taken && owner != spec.ID {
			panic(fmt.Sprintf("history: alias %q already registered for %q", alias, owner))
		}
		if _, isCanonical := registry[AgentID(key)]; isCanonical {
			panic(fmt.Sprintf("history: alias %q collides with canonical agent ID", alias))
		}
		aliases[key] = spec.ID
	}
	spec.Aliases = append([]string(nil), spec.Aliases...)
	registry[spec.ID] = registration{spec: spec, factory: factory}
}

func normalizeAgentKey(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// Resolve maps a user-supplied agent name (canonical ID or registered alias,
// any case) to its canonical ID. ok is false when nothing is registered under
// that name.
func Resolve(raw string) (AgentID, bool) {
	key := normalizeAgentKey(raw)
	if key == "" {
		return "", false
	}
	registryMu.RLock()
	defer registryMu.RUnlock()
	if _, ok := registry[AgentID(key)]; ok {
		return AgentID(key), true
	}
	id, ok := aliases[key]
	return id, ok
}

// Spec returns the registered spec for an agent. Its Aliases are a copy: a
// caller that sorts or edits them must not reach the registry's own slice.
func Spec(id AgentID) (AgentSpec, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	reg, ok := registry[id]
	return copySpec(reg.spec), ok
}

// Specs returns every registered spec sorted by ID, so the CLI usage line, the
// HTTP agents endpoint, and the viewer selector all list agents in one order.
// Each spec's Aliases are a copy; see [Spec].
func Specs() []AgentSpec {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]AgentSpec, 0, len(registry))
	for _, reg := range registry {
		out = append(out, copySpec(reg.spec))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func copySpec(spec AgentSpec) AgentSpec {
	spec.Aliases = append([]string(nil), spec.Aliases...)
	return spec
}

// AgentIDs returns every registered canonical ID, sorted.
func AgentIDs() []AgentID {
	specs := Specs()
	out := make([]AgentID, len(specs))
	for i, s := range specs {
		out[i] = s.ID
	}
	return out
}

// NewImporter constructs the importer registered for an agent. ok is false
// when no importer is registered under that ID.
func NewImporter(id AgentID) (Importer, bool) {
	registryMu.RLock()
	reg, ok := registry[id]
	registryMu.RUnlock()
	if !ok {
		return nil, false
	}
	return reg.factory(), true
}
