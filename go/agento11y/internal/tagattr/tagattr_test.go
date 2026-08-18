package tagattr_test

import (
	"testing"

	"go.opentelemetry.io/otel/attribute"

	"github.com/grafana/agento11y/go/agento11y/internal/tagattr"
)

func TestAttributes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		tags map[string]string
		want []attribute.KeyValue
	}{
		{name: "no tags"},
		{name: "empty map", tags: map[string]string{}},
		{
			name: "sorted by key",
			tags: map[string]string{"team": "sigil", "env": "dev"},
			want: []attribute.KeyValue{
				attribute.String("agento11y.tag.env", "dev"),
				attribute.String("agento11y.tag.team", "sigil"),
			},
		},
		{
			name: "keys and values trimmed",
			tags: map[string]string{"  team  ": "  sigil  "},
			want: []attribute.KeyValue{attribute.String("agento11y.tag.team", "sigil")},
		},
		{
			name: "a key that trims to nothing is dropped",
			tags: map[string]string{"   ": "orphan", "env": "dev"},
			want: []attribute.KeyValue{attribute.String("agento11y.tag.env", "dev")},
		},
		{
			name: "nothing survives",
			tags: map[string]string{"": "orphan"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := tagattr.Attributes("agento11y.tag.", tc.tags)
			if len(got) != len(tc.want) {
				t.Fatalf("Attributes() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("attribute %d = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
