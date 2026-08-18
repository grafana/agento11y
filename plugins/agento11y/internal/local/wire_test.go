package local

import (
	"encoding/json"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStoredUsageInputSemantics pins that the input-semantics marker
// survives the stored shape in every encoding a record can carry it in:
// the proto-JSON enum name the exporter writes, the enum number the SDK's
// Go JSON shape writes, and the same number quoted. A record written
// before the field existed must still decode to unspecified, which is
// what keeps the provider-name heuristic in charge of legacy data.
func TestStoredUsageInputSemantics(t *testing.T) {
	cases := []struct {
		name string
		json string
		want agento11y.TokenUsage
	}{
		{
			name: "proto-json enum name and int64 strings",
			json: `{"input_tokens":"175","output_tokens":"18","total_tokens":"193","cache_read_input_tokens":"15","input_semantics":"TOKEN_INPUT_SEMANTICS_INCLUSIVE"}`,
			want: agento11y.TokenUsage{
				InputTokens:          175,
				OutputTokens:         18,
				TotalTokens:          193,
				CacheReadInputTokens: 15,
				InputSemantics:       agento11y.TokenInputSemanticsInclusive,
			},
		},
		{
			name: "go json enum number",
			json: `{"input_tokens":175,"output_tokens":18,"total_tokens":193,"cache_read_input_tokens":15,"input_semantics":1}`,
			want: agento11y.TokenUsage{
				InputTokens:          175,
				OutputTokens:         18,
				TotalTokens:          193,
				CacheReadInputTokens: 15,
				InputSemantics:       agento11y.TokenInputSemanticsInclusive,
			},
		},
		{
			name: "quoted enum number",
			json: `{"input_tokens":"10","input_semantics":"1"}`,
			want: agento11y.TokenUsage{InputTokens: 10, InputSemantics: agento11y.TokenInputSemanticsInclusive},
		},
		{
			name: "explicit unspecified enum name",
			json: `{"input_tokens":"10","input_semantics":"TOKEN_INPUT_SEMANTICS_UNSPECIFIED"}`,
			want: agento11y.TokenUsage{InputTokens: 10},
		},
		{
			name: "null marker",
			json: `{"input_tokens":"10","input_semantics":null}`,
			want: agento11y.TokenUsage{InputTokens: 10},
		},
		{
			// Every record written before the marker existed. It must decode
			// exactly as it does today: unspecified, so the bucket split
			// falls back to the provider-name heuristic.
			name: "legacy record without the field",
			json: `{"input_tokens":"175","output_tokens":"18","total_tokens":"193","cache_read_input_tokens":"15"}`,
			want: agento11y.TokenUsage{
				InputTokens:          175,
				OutputTokens:         18,
				TotalTokens:          193,
				CacheReadInputTokens: 15,
			},
		},
		{
			// An enum name a newer exporter knows and this build does not
			// must not cost the line; unspecified is the safe degradation.
			name: "unknown enum name degrades to unspecified",
			json: `{"input_tokens":"10","input_semantics":"TOKEN_INPUT_SEMANTICS_FUTURE"}`,
			want: agento11y.TokenUsage{InputTokens: 10},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var u storedUsage
			require.NoError(t, json.Unmarshal([]byte(tc.json), &u))
			assert.Equal(t, tc.want, u.toSDK())

			// Round trip: re-encoding the stored shape and decoding it
			// again must preserve the marker, so a record this build
			// writes back is read the same way it was read.
			encoded, err := json.Marshal(u)
			require.NoError(t, err)
			var again storedUsage
			require.NoError(t, json.Unmarshal(encoded, &again))
			assert.Equal(t, tc.want, again.toSDK())
		})
	}
}
