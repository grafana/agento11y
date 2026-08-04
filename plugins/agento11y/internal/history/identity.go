package history

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

// genIDPrefix prefixes every imported generation ID so a backfilled generation
// is distinguishable from a live one at a glance and can never collide with a
// live ID, which uses a different scheme.
const genIDPrefix = "histgen"

// hashFields joins parts with NUL so ("a","bc") and ("ab","c") cannot collide.
func hashFields(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// GenerationID derives the stable generation ID for a turn. Re-importing the
// same source turn always yields the same ID, and that stability is what makes
// export idempotent across runs. TurnIndex is included so two turns in one
// session never collide even when the agent gives them no native turn ID.
func (r SourceRef) GenerationID() string {
	return genIDPrefix + "-" + hashFields(
		string(r.Agent), r.SessionID, r.SourcePath, strconv.Itoa(r.TurnIndex), r.TurnID,
	)[:24]
}

// SourceIdentity is the opaque, hashed ledger key for a turn. It is a full
// SHA-256 digest of the SourceRef, so the ledger file stores no raw path or
// session ID, only its hash.
type SourceIdentity string

// Identity returns the hashed ledger key for the turn.
func (r SourceRef) Identity() SourceIdentity {
	return SourceIdentity(hashFields(
		string(r.Agent), r.SessionID, r.SourcePath, strconv.Itoa(r.TurnIndex), r.TurnID,
	))
}

// Collision reports a native session ID claimed by more than one source.
// Imported generations keep the native session ID as the conversation ID, so
// two different files reusing one ID would silently merge into one
// conversation. Hash-derived generation IDs cannot collide by construction.
// Detection therefore lives at the session-ID layer, where reuse is real.
type Collision struct {
	Agent     AgentID
	SessionID string
	Sources   []string // distinct source paths claiming the ID
}

// DetectCollisions returns the sessions whose native ID is claimed by more than
// one source path within one agent. The same session discovered twice at the
// same path is not a collision; only differing source paths are reported.
func DetectCollisions(previews []SessionPreview) []Collision {
	type key struct {
		agent     AgentID
		sessionID string
	}
	paths := map[key]map[string]bool{}
	for _, p := range previews {
		if p.SessionID == "" {
			continue // unidentified sessions get synthetic IDs; nothing to collide
		}
		k := key{p.Agent, p.SessionID}
		if paths[k] == nil {
			paths[k] = map[string]bool{}
		}
		paths[k][p.SourcePath] = true
	}

	out := make([]Collision, 0, len(paths))
	for k, srcSet := range paths {
		if len(srcSet) <= 1 {
			continue
		}
		srcs := make([]string, 0, len(srcSet))
		for s := range srcSet {
			srcs = append(srcs, s)
		}
		sort.Strings(srcs)
		out = append(out, Collision{Agent: k.agent, SessionID: k.sessionID, Sources: srcs})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}
