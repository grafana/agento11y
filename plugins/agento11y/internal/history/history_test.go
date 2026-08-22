package history

import (
	"context"
	"iter"
	"maps"
	"slices"
	"testing"
)

// stubImporter is a registry and discovery test double. Each field replaces one
// method of the Importer contract.
type stubImporter struct {
	roots   []string
	match   func(path string) bool
	preview func(ctx context.Context, path string) (SessionPreview, bool, error)
	turns   func(ctx context.Context, sess SessionPreview) iter.Seq2[HistoricalGeneration, error]
}

func (s *stubImporter) Roots() []string { return s.roots }

func (s *stubImporter) Match(path string) bool {
	if s.match == nil {
		return true
	}
	return s.match(path)
}

func (s *stubImporter) Preview(ctx context.Context, path string) (SessionPreview, bool, error) {
	if s.preview == nil {
		return SessionPreview{SessionID: path}, true, nil
	}
	return s.preview(ctx, path)
}

func (s *stubImporter) Turns(ctx context.Context, sess SessionPreview) iter.Seq2[HistoricalGeneration, error] {
	if s.turns == nil {
		return func(func(HistoricalGeneration, error) bool) {}
	}
	return s.turns(ctx, sess)
}

// isolateRegistry empties the process-wide registry for one test and restores
// it afterwards, so a registration test cannot see (or disturb) the real
// claude-code and codex entries.
func isolateRegistry(t *testing.T) {
	t.Helper()
	registryMu.Lock()
	savedRegistry := maps.Clone(registry)
	savedAliases := maps.Clone(aliases)
	registry = map[AgentID]registration{}
	aliases = map[string]AgentID{}
	registryMu.Unlock()
	t.Cleanup(func() {
		registryMu.Lock()
		registry = savedRegistry
		aliases = savedAliases
		registryMu.Unlock()
	})
}

func registerStub(id AgentID, display string, aliases ...string) {
	Register(AgentSpec{ID: id, DisplayName: display, Aliases: aliases}, func() Importer {
		return &stubImporter{}
	})
}

func TestResolve(t *testing.T) {
	isolateRegistry(t)
	registerStub(AgentClaudeCode, "Claude Code", "claude")
	registerStub(AgentCodex, "Codex", "openai-codex")

	tests := []struct {
		name string
		raw  string
		want AgentID
		ok   bool
	}{
		{name: "canonical id", raw: "claude-code", want: AgentClaudeCode, ok: true},
		{name: "canonical id is case insensitive", raw: "Claude-Code", want: AgentClaudeCode, ok: true},
		{name: "alias", raw: "claude", want: AgentClaudeCode, ok: true},
		{name: "alias with surrounding space", raw: "  codex ", want: AgentCodex, ok: true},
		{name: "second agent alias", raw: "openai-codex", want: AgentCodex, ok: true},
		{name: "unknown name", raw: "cursor", want: "", ok: false},
		{name: "empty name", raw: "", want: "", ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Resolve(tc.raw)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("Resolve(%q) = (%q, %v), want (%q, %v)", tc.raw, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestSpecsAreSortedAndCarryAliases(t *testing.T) {
	isolateRegistry(t)
	registerStub(AgentCodex, "Codex", "openai-codex")
	registerStub(AgentClaudeCode, "Claude Code", "claude", "claudecode")

	specs := Specs()
	gotIDs := make([]AgentID, len(specs))
	for i, s := range specs {
		gotIDs[i] = s.ID
	}
	wantIDs := []AgentID{AgentClaudeCode, AgentCodex}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("Specs() ids = %v, want %v", gotIDs, wantIDs)
	}
	if !slices.Equal(AgentIDs(), wantIDs) {
		t.Fatalf("AgentIDs() = %v, want %v", AgentIDs(), wantIDs)
	}
	if got := specs[0].Aliases; !slices.Equal(got, []string{"claude", "claudecode"}) {
		t.Fatalf("claude-code aliases = %v", got)
	}
	if specs[0].DisplayName != "Claude Code" {
		t.Fatalf("claude-code display name = %q", specs[0].DisplayName)
	}
}

// TestSpecsAreNotAliasedToTheRegistry pins that a caller cannot reach the
// registry's own alias slice. The CLI sorts the aliases before printing them,
// which would otherwise reorder the registered spec for the process lifetime.
func TestSpecsAreNotAliasedToTheRegistry(t *testing.T) {
	isolateRegistry(t)
	registerStub(AgentCodex, "Codex", "openai-codex")

	specs := Specs()
	specs[0].Aliases[0] = "mutated"
	if got, _ := Resolve("openai-codex"); got != AgentCodex {
		t.Fatalf("mutating a returned spec changed the registry: Resolve = %q", got)
	}
	if again := Specs(); again[0].Aliases[0] != "openai-codex" {
		t.Fatalf("Specs()[0].Aliases[0] = %q after a caller mutated an earlier copy", again[0].Aliases[0])
	}
	spec, ok := Spec(AgentCodex)
	if !ok {
		t.Fatal("the registered agent disappeared")
	}
	spec.Aliases[0] = "mutated again"
	if final, _ := Spec(AgentCodex); final.Aliases[0] != "openai-codex" {
		t.Fatalf("Spec().Aliases[0] = %q after a caller mutated an earlier copy", final.Aliases[0])
	}
}

func TestRegisterPanics(t *testing.T) {
	tests := []struct {
		name    string
		prepare func()
		call    func()
	}{
		{
			name:    "duplicate canonical id",
			prepare: func() { registerStub(AgentCodex, "Codex") },
			call:    func() { registerStub(AgentCodex, "Codex again") },
		},
		{
			name:    "alias claimed by another agent",
			prepare: func() { registerStub(AgentCodex, "Codex", "shared") },
			call:    func() { registerStub(AgentClaudeCode, "Claude Code", "shared") },
		},
		{
			name:    "alias collides with a canonical id",
			prepare: func() { registerStub(AgentCodex, "Codex") },
			call:    func() { registerStub(AgentClaudeCode, "Claude Code", "codex") },
		},
		{
			name: "empty id",
			call: func() { registerStub("", "No ID") },
		},
		{
			name: "nil factory",
			call: func() { Register(AgentSpec{ID: "x", DisplayName: "X"}, nil) },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateRegistry(t)
			if tc.prepare != nil {
				tc.prepare()
			}
			defer func() {
				if recover() == nil {
					t.Fatal("Register() did not panic")
				}
			}()
			tc.call()
		})
	}
}

func TestRegisterKeepsTheFirstEntryWhenADuplicatePanics(t *testing.T) {
	isolateRegistry(t)
	registerStub(AgentCodex, "Codex", "openai-codex")

	func() {
		defer func() { _ = recover() }()
		Register(AgentSpec{ID: AgentCodex, DisplayName: "Replacement"}, func() Importer { return &stubImporter{} })
	}()

	spec, ok := Spec(AgentCodex)
	if !ok {
		t.Fatal("codex disappeared after a rejected duplicate registration")
	}
	if spec.DisplayName != "Codex" {
		t.Fatalf("display name = %q, want the original %q", spec.DisplayName, "Codex")
	}
}

func TestNewImporter(t *testing.T) {
	isolateRegistry(t)
	registerStub(AgentCodex, "Codex")

	if _, ok := NewImporter(AgentCodex); !ok {
		t.Fatal("NewImporter(codex) reported no importer")
	}
	if _, ok := NewImporter(AgentClaudeCode); ok {
		t.Fatal("NewImporter(claude-code) returned an importer that was never registered")
	}
}

// TestRealImportersAreRegistered guards the wiring the CLI, the HTTP API, and
// the viewer all read from. A missing init here makes an agent invisible
// everywhere at once.
func TestRealImportersAreRegistered(t *testing.T) {
	for _, id := range []AgentID{AgentClaudeCode, AgentCodex, AgentOpenCode} {
		spec, ok := Spec(id)
		if !ok {
			t.Fatalf("no spec registered for %q", id)
		}
		if spec.DisplayName == "" {
			t.Fatalf("%q has no display name", id)
		}
		if _, ok := NewImporter(id); !ok {
			t.Fatalf("no importer registered for %q", id)
		}
	}
}
