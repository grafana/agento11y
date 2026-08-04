package history

import (
	"strings"
	"testing"
)

func baseRef() SourceRef {
	return SourceRef{
		Agent:      AgentClaudeCode,
		SessionID:  "session-1",
		SourcePath: "/home/u/.claude/projects/p/session-1.jsonl",
		TurnIndex:  3,
		TurnID:     "req_abc",
	}
}

func TestGenerationIDIsStableAndPrefixed(t *testing.T) {
	ref := baseRef()
	first := ref.GenerationID()
	if first != ref.GenerationID() {
		t.Fatal("GenerationID is not stable for one ref")
	}
	if !strings.HasPrefix(first, genIDPrefix+"-") {
		t.Fatalf("GenerationID %q lacks the %q prefix", first, genIDPrefix)
	}
	if len(first) != len(genIDPrefix)+1+24 {
		t.Fatalf("GenerationID %q has length %d", first, len(first))
	}
}

func TestIdentityAndGenerationIDVaryWithEveryField(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*SourceRef)
	}{
		{name: "agent", mutate: func(r *SourceRef) { r.Agent = AgentCodex }},
		{name: "session id", mutate: func(r *SourceRef) { r.SessionID = "session-2" }},
		{name: "source path", mutate: func(r *SourceRef) { r.SourcePath = "/other.jsonl" }},
		{name: "turn index", mutate: func(r *SourceRef) { r.TurnIndex = 4 }},
		{name: "turn id", mutate: func(r *SourceRef) { r.TurnID = "req_xyz" }},
	}
	base := baseRef()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			other := base
			tc.mutate(&other)
			if other.GenerationID() == base.GenerationID() {
				t.Fatalf("changing %s did not change the generation ID", tc.name)
			}
			if other.Identity() == base.Identity() {
				t.Fatalf("changing %s did not change the ledger key", tc.name)
			}
		})
	}
}

func TestIdentityLeaksNoSourceText(t *testing.T) {
	ref := baseRef()
	key := string(ref.Identity())
	for _, secret := range []string{ref.SessionID, ref.SourcePath, ref.TurnID} {
		if strings.Contains(key, secret) {
			t.Fatalf("ledger key %q contains the source value %q", key, secret)
		}
	}
	if len(key) != 64 {
		t.Fatalf("ledger key has length %d, want a 64-character SHA-256 digest", len(key))
	}
}

func TestFieldJoiningCannotCollide(t *testing.T) {
	a := SourceRef{Agent: AgentCodex, SessionID: "ab", SourcePath: "c"}
	b := SourceRef{Agent: AgentCodex, SessionID: "a", SourcePath: "bc"}
	if a.Identity() == b.Identity() {
		t.Fatal("adjacent fields collided; the join separator is not doing its job")
	}
}

func TestDetectCollisions(t *testing.T) {
	tests := []struct {
		name     string
		previews []SessionPreview
		want     []Collision
	}{
		{
			name: "one id in two files collides",
			previews: []SessionPreview{
				{Agent: AgentCodex, SessionID: "s1", SourcePath: "/b.jsonl"},
				{Agent: AgentCodex, SessionID: "s1", SourcePath: "/a.jsonl"},
			},
			want: []Collision{{
				Agent: AgentCodex, SessionID: "s1",
				Sources: []string{"/a.jsonl", "/b.jsonl"},
			}},
		},
		{
			name: "the same file twice is not a collision",
			previews: []SessionPreview{
				{Agent: AgentCodex, SessionID: "s1", SourcePath: "/a.jsonl"},
				{Agent: AgentCodex, SessionID: "s1", SourcePath: "/a.jsonl"},
			},
		},
		{
			name: "the same id under two agents is not a collision",
			previews: []SessionPreview{
				{Agent: AgentCodex, SessionID: "s1", SourcePath: "/a.jsonl"},
				{Agent: AgentClaudeCode, SessionID: "s1", SourcePath: "/b.jsonl"},
			},
		},
		{
			name: "sessions with no id are ignored",
			previews: []SessionPreview{
				{Agent: AgentCodex, SourcePath: "/a.jsonl"},
				{Agent: AgentCodex, SourcePath: "/b.jsonl"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectCollisions(tc.previews)
			if len(got) != len(tc.want) {
				t.Fatalf("found %d collisions, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i].Agent != tc.want[i].Agent || got[i].SessionID != tc.want[i].SessionID {
					t.Fatalf("collision %d = %+v, want %+v", i, got[i], tc.want[i])
				}
				if !equalStrings(got[i].Sources, tc.want[i].Sources) {
					t.Fatalf("collision %d sources = %v, want %v", i, got[i].Sources, tc.want[i].Sources)
				}
			}
		})
	}
}
