package hook

import (
	"encoding/json"
	"testing"
)

func TestResolvedTimestampParsesStringEncodedEpochMillis(t *testing.T) {
	got := Payload{Timestamp: []byte(`"1747579200000"`)}.ResolvedTimestamp()
	if got != "2025-05-18T14:40:00Z" {
		t.Fatalf("ResolvedTimestamp = %q", got)
	}
}

func TestToolInput(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "snake_case tool_input wins",
			raw:  `{"tool_input":{"a":1},"toolInput":{"b":2},"toolArgs":{"c":3}}`,
			want: `{"a":1}`,
		},
		{
			name: "legacy camelCase toolInput when no snake_case",
			raw:  `{"toolInput":{"b":2},"toolArgs":{"c":3}}`,
			want: `{"b":2}`,
		},
		{
			name: "documented toolArgs when neither input present",
			raw:  `{"toolArgs":{"c":3}}`,
			want: `{"c":3}`,
		},
		{
			name: "missing returns nil",
			raw:  `{}`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p Payload
			if err := json.Unmarshal([]byte(tt.raw), &p); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got := string(p.ToolInput())
			if got != tt.want {
				t.Fatalf("ToolInput() = %q, want %q", got, tt.want)
			}
		})
	}
}
