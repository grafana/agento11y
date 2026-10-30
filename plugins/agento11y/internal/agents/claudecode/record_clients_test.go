package claudecode

import (
	"slices"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
)

// Only records made before the newest delivered metrics were collected are
// remembered: a record made after it has no delivered metrics, so the retry
// must record it again. Earlier hooks' keys always stay.
func TestRecordClientsKeysCoverOnlyDeliveredRecords(t *testing.T) {
	clients := newRecordClients(nil, agento11y.Config{}, []string{"generation:earlier"})

	_, recordedFirst := clients.forRecord("generation:first")
	recordedFirst()
	deliveredAt := time.Now()
	_, recordedLater := clients.forRecord("tool:later")
	time.Sleep(time.Millisecond)
	recordedLater()

	if got, want := clients.keys(deliveredAt), []string{"generation:earlier", "generation:first"}; !slices.Equal(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	if got, want := clients.keys(time.Time{}), []string{"generation:earlier"}; !slices.Equal(got, want) {
		t.Fatalf("keys with nothing delivered = %v, want %v", got, want)
	}
}
