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
// export idempotent across runs.
func (r SourceRef) GenerationID() string {
	return genIDPrefix + "-" + r.identityHash()[:24]
}

// SourceIdentity is the opaque, hashed ledger key for a turn. It is a full
// SHA-256 digest of the SourceRef, so the ledger file stores no raw path or
// session ID, only its hash.
type SourceIdentity string

// Identity returns the hashed ledger key for the turn.
func (r SourceRef) Identity() SourceIdentity {
	return SourceIdentity(r.identityHash())
}

func (r SourceRef) identityHash() string {
	turnIndex := strconv.Itoa(r.TurnIndex)
	if r.TurnIDStable && r.TurnID != "" {
		turnIndex = ""
	}
	return hashFields(string(r.Agent), r.SessionID, r.SourcePath, turnIndex, r.TurnID)
}

// Collision reports a mapped conversation ID claimed by more than one source.
// Two different files reusing one conversation ID would otherwise merge in the
// viewer. Hash-derived generation IDs cannot collide by construction.
type Collision struct {
	Agent     AgentID
	SessionID string   // mapped conversation ID
	Sources   []string // distinct source paths claiming the ID
}

// DetectCollisions returns the conversations claimed by more than one source
// path within one agent. The same conversation discovered twice at one path is
// not a collision; only differing source paths are reported.
func DetectCollisions(previews []SessionPreview) []Collision {
	type key struct {
		agent     AgentID
		sessionID string
	}
	paths := map[key]map[string]bool{}
	for _, p := range previews {
		conversationID := p.ConversationID
		if conversationID == "" {
			conversationID = p.SessionID
		}
		if conversationID == "" {
			continue // unidentified sessions get synthetic IDs; nothing to collide
		}
		k := key{p.Agent, conversationID}
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
