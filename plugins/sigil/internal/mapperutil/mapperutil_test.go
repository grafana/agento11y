package mapperutil

import (
	"reflect"
	"testing"

	"github.com/grafana/sigil-sdk/go/sigil"
)

func TestDeterministicID(t *testing.T) {
	// Each subtest checks a different property of DeterministicID, so
	// the cases don't share a single input/output shape; subtests are
	// clearer here than a forced struct table.
	t.Run("stable for same inputs", func(t *testing.T) {
		if DeterministicID("codex", "sess", "turn") != DeterministicID("codex", "sess", "turn") {
			t.Fatal("DeterministicID not stable across calls")
		}
	})

	t.Run("preserves prefix and 24-hex tail", func(t *testing.T) {
		const wantPrefix = "codex-"
		got := DeterministicID("codex", "sess", "turn")
		if len(got) != len(wantPrefix)+24 {
			t.Fatalf("length = %d; want %d", len(got), len(wantPrefix)+24)
		}
		if got[:len(wantPrefix)] != wantPrefix {
			t.Errorf("prefix = %q; want %q", got[:len(wantPrefix)], wantPrefix)
		}
	})

	t.Run("hash tail stable across prefixes", func(t *testing.T) {
		a := DeterministicID("codex", "sess", "turn")
		b := DeterministicID("copilot", "sess", "turn")
		if a[len("codex-"):] != b[len("copilot-"):] {
			t.Errorf("hash tails differ: %q vs %q", a, b)
		}
	})

	t.Run("NUL separator prevents boundary collisions", func(t *testing.T) {
		if DeterministicID("p", "a", "bc") == DeterministicID("p", "ab", "c") {
			t.Error("DeterministicID collided across part boundaries")
		}
	})

	t.Run("different parts produce different IDs", func(t *testing.T) {
		if DeterministicID("codex", "sess", "turn") == DeterministicID("codex", "sess", "turn2") {
			t.Error("DeterministicID ignored differing parts")
		}
	})
}

func TestNormalizeStartContentMode(t *testing.T) {
	cases := []struct {
		name string
		in   sigil.ContentCaptureMode
		want sigil.ContentCaptureMode
	}{
		{"default becomes metadata-only", sigil.ContentCaptureModeDefault, sigil.ContentCaptureModeMetadataOnly},
		{"metadata-only unchanged", sigil.ContentCaptureModeMetadataOnly, sigil.ContentCaptureModeMetadataOnly},
		{"no-tool-content unchanged", sigil.ContentCaptureModeNoToolContent, sigil.ContentCaptureModeNoToolContent},
		{"full unchanged", sigil.ContentCaptureModeFull, sigil.ContentCaptureModeFull},
		{"full-with-metadata-spans unchanged", sigil.ContentCaptureModeFullWithMetadataSpans, sigil.ContentCaptureModeFullWithMetadataSpans},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeStartContentMode(tc.in); got != tc.want {
				t.Fatalf("NormalizeStartContentMode(%v) = %v; want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizePayloadContentMode(t *testing.T) {
	cases := []struct {
		name string
		in   sigil.ContentCaptureMode
		want sigil.ContentCaptureMode
	}{
		{"default becomes metadata-only", sigil.ContentCaptureModeDefault, sigil.ContentCaptureModeMetadataOnly},
		{"metadata-only unchanged", sigil.ContentCaptureModeMetadataOnly, sigil.ContentCaptureModeMetadataOnly},
		{"no-tool-content unchanged", sigil.ContentCaptureModeNoToolContent, sigil.ContentCaptureModeNoToolContent},
		{"full unchanged", sigil.ContentCaptureModeFull, sigil.ContentCaptureModeFull},
		{"full-with-metadata-spans becomes full", sigil.ContentCaptureModeFullWithMetadataSpans, sigil.ContentCaptureModeFull},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizePayloadContentMode(tc.in); got != tc.want {
				t.Fatalf("NormalizePayloadContentMode(%v) = %v; want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSortedToolDefinitions(t *testing.T) {
	cases := []struct {
		name  string
		names []string
		want  []string
	}{
		{"nil input", nil, nil},
		{"empty input", []string{}, nil},
		{"only empty names", []string{"", ""}, nil},
		{"dedup and sort", []string{"Write", "Read", "Read", "", "Bash"}, []string{"Bash", "Read", "Write"}},
		{"already sorted single", []string{"Read"}, []string{"Read"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SortedToolDefinitions(tc.names)
			if len(got) != len(tc.want) {
				t.Fatalf("SortedToolDefinitions(%v) len = %d; want %d (%+v)", tc.names, len(got), len(tc.want), got)
			}
			if tc.want == nil && got != nil {
				t.Fatalf("SortedToolDefinitions(%v) = %+v; want nil", tc.names, got)
			}
			for i, def := range got {
				if def.Name != tc.want[i] {
					t.Errorf("got[%d].Name = %q; want %q", i, def.Name, tc.want[i])
				}
				if def.Type != "function" {
					t.Errorf("got[%d].Type = %q; want function", i, def.Type)
				}
			}
		})
	}
}

func TestInferProvider(t *testing.T) {
	cases := []struct{ model, want string }{
		{"claude-sonnet-4-6", "anthropic"},
		{"claude-opus", "anthropic"},
		{"anthropic.claude-3", "anthropic"}, // substring match anywhere
		{"gpt-5", "openai"},
		{"gpt5", "openai"}, // no hyphen still matches (loose prefix)
		{"o1-preview", "openai"},
		{"o3-mini", "openai"},
		{"o4-fast", "openai"},
		{"gemini-2.5-pro", "google"},
		{"models/gemini-pro", "google"}, // substring match anywhere
		{"some-random-model", ""},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			if got := InferProvider(tc.model); got != tc.want {
				t.Errorf("InferProvider(%q) = %q; want %q", tc.model, got, tc.want)
			}
		})
	}
}

func TestCloneStringMap(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
		want map[string]string
	}{
		{"nil input", nil, nil},
		{"empty non-nil input", map[string]string{}, nil},
		{"single entry", map[string]string{"a": "1"}, map[string]string{"a": "1"}},
		{"multiple entries", map[string]string{"a": "1", "b": "2"}, map[string]string{"a": "1", "b": "2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Clone(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Clone(%v) = %v; want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestCloneIsIndependent(t *testing.T) {
	in := map[string]string{"a": "1"}
	out := Clone(in)
	out["b"] = "2"
	if _, ok := in["b"]; ok {
		t.Fatalf("mutating clone leaked into source: %v", in)
	}
	in["c"] = "3"
	if _, ok := out["c"]; ok {
		t.Fatalf("mutating source leaked into clone: %v", out)
	}
}

func TestCloneAnyMap(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want map[string]any
	}{
		{"nil input", nil, nil},
		{"empty non-nil input", map[string]any{}, nil},
		{"mixed values",
			map[string]any{"n": int64(7), "s": "x", "b": true},
			map[string]any{"n": int64(7), "s": "x", "b": true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Clone(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Clone(%v) = %v; want %v", tc.in, got, tc.want)
			}
		})
	}
}
