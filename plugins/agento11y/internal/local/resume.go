package local

import (
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	resumeReady       = "ready"
	resumeUnavailable = "unavailable"

	resumeUnsupportedID        = "unsupported_id"
	resumeMissingWorkspace     = "missing_workspace"
	resumeInvalidWorkspace     = "invalid_workspace"
	resumeConflictingWorkspace = "conflicting_workspace"
	resumeAmbiguousAgent       = "ambiguous_agent"
	resumeMissingRoot          = "missing_root"
)

// resumeHosts are the agent names whose native session IDs the viewer can
// hand to a resume command.
var resumeHosts = map[string]bool{"claude-code": true, "pi": true}

// ResumeInfo tells the viewer which native session to resume and from which
// directory. It is a best-effort target: nothing checks that the native
// session still exists on this machine.
type ResumeInfo struct {
	Status    string `json:"status"`
	Agent     string `json:"agent,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	CWD       string `json:"cwd,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// resumeScan collects agent and directory evidence from one conversation's
// generations. The result does not depend on record order.
type resumeScan struct {
	rootHosts  map[string]struct{}
	childHosts map[string]struct{}
	cwds       map[string]struct{}
}

func newResumeScan() *resumeScan {
	return &resumeScan{
		rootHosts:  map[string]struct{}{},
		childHosts: map[string]struct{}{},
		cwds:       map[string]struct{}{},
	}
}

func (r *resumeScan) add(gen storedGeneration) {
	if gen.AgentName == "" {
		return
	}
	host, _, suffixed := strings.Cut(gen.AgentName, "/")
	// A Claude sidechain without attribution keeps the bare agent name, so
	// the subagent tag is the only sign that it is a child.
	if suffixed || gen.Tags["subagent"] == "true" {
		r.childHosts[host] = struct{}{}
		return
	}
	r.rootHosts[host] = struct{}{}
	if cwd := gen.Tags["cwd"]; cwd != "" {
		r.cwds[cwd] = struct{}{}
	}
}

// result returns nil when no supported host recorded the conversation.
// conversationID is the native session ID for both hosts: generation IDs,
// Claude child IDs, and Pi fork parents are never substitutes.
func (r *resumeScan) result(conversationID string) *ResumeInfo {
	if !anyResumeHost(r.rootHosts) && !anyResumeHost(r.childHosts) {
		return nil
	}
	unavailable := func(reason string) *ResumeInfo {
		return &ResumeInfo{Status: resumeUnavailable, Reason: reason}
	}
	if len(r.rootHosts) == 0 {
		return unavailable(resumeMissingRoot)
	}
	host, ok := onlyKey(r.rootHosts)
	if !ok || !resumeHosts[host] {
		return unavailable(resumeAmbiguousAgent)
	}
	for child := range r.childHosts {
		if child != host {
			return unavailable(resumeAmbiguousAgent)
		}
	}
	if !nativeSessionID(conversationID) {
		return unavailable(resumeUnsupportedID)
	}
	if len(r.cwds) == 0 {
		return unavailable(resumeMissingWorkspace)
	}
	cwd, ok := enclosingWorkspace(r.cwds)
	if !ok {
		return unavailable(resumeConflictingWorkspace)
	}
	if !resumableWorkspace(cwd) {
		return unavailable(resumeInvalidWorkspace)
	}
	return &ResumeInfo{Status: resumeReady, Agent: host, SessionID: conversationID, CWD: cwd}
}

func anyResumeHost(hosts map[string]struct{}) bool {
	for host := range hosts {
		if resumeHosts[host] {
			return true
		}
	}
	return false
}

func onlyKey(set map[string]struct{}) (string, bool) {
	if len(set) != 1 {
		return "", false
	}
	for key := range set {
		return key, true
	}
	return "", false
}

// enclosingWorkspace returns the directory that contains every recorded one.
// Claude Code records the shell's directory on each line, so a session that
// cd'd into a subfolder records the folder it started in and the subfolder.
// Choosing by timestamp or file order instead would guess: imported
// timestamps are approximate.
func enclosingWorkspace(cwds map[string]struct{}) (string, bool) {
	var shortest string
	for cwd := range cwds {
		if shortest == "" || len(cwd) < len(shortest) {
			shortest = cwd
		}
	}
	prefix := shortest
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	for cwd := range cwds {
		if cwd != shortest && !strings.HasPrefix(cwd, prefix) {
			return "", false
		}
	}
	return shortest, true
}

// nativeSessionID accepts only the hyphenated 36-character UUID form both
// hosts generate. uuid.Parse alone also accepts braces, URNs, and the
// unhyphenated form, none of which the hosts look up.
func nativeSessionID(id string) bool {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return false
	}
	_, err := uuid.Parse(id)
	return err == nil
}

// resumableWorkspace requires an absolute POSIX path that a single pasted
// line can carry unchanged.
func resumableWorkspace(cwd string) bool {
	if !utf8.ValidString(cwd) || !path.IsAbs(cwd) {
		return false
	}
	return strings.IndexFunc(cwd, func(r rune) bool {
		return unicode.IsControl(r) || r == '\u2028' || r == '\u2029'
	}) < 0
}
