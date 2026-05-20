package hook

import "testing"

func TestResolvedTimestampParsesStringEncodedEpochMillis(t *testing.T) {
	got := Payload{Timestamp: []byte(`"1747579200000"`)}.ResolvedTimestamp()
	if got != "2025-05-18T14:40:00Z" {
		t.Fatalf("ResolvedTimestamp = %q", got)
	}
}
