package claudecode

import (
	"context"
	"log"
	"slices"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/emit"
)

// recordClients picks the client for each generation and tool execution so
// its metrics are recorded once across hook runs. A hook whose export fails
// keeps its offset and pending tools, so the next hook sends the same records
// again. Records whose metrics an earlier hook delivered go through a second
// client that records none.
type recordClients struct {
	metrics *agento11y.Client
	// quietConfig builds the client without metrics on first use.
	quietConfig agento11y.Config
	quiet       *agento11y.Client
	// earlier holds the keys from state.Session.MetricsRecorded.
	earlier map[string]bool
	// recordedAt holds when this hook finished recording each key's metrics.
	recordedAt map[string]time.Time
}

func newRecordClients(client *agento11y.Client, cfg agento11y.Config, earlier []string) *recordClients {
	set := make(map[string]bool, len(earlier))
	for _, key := range earlier {
		set[key] = true
	}
	return &recordClients{metrics: client, quietConfig: cfg, earlier: set, recordedAt: map[string]time.Time{}}
}

func generationRecordKey(id string) string { return "generation:" + id }

func toolRecordKey(key string) string { return "tool:" + key }

// forRecord returns the client to send the record with key through, and a
// function to call once the record has ended, which is when its metrics are
// recorded.
func (c *recordClients) forRecord(key string) (*agento11y.Client, func()) {
	if !c.earlier[key] {
		return c.metrics, func() { c.recordedAt[key] = time.Now() }
	}
	if c.quiet == nil {
		c.quiet = agento11y.NewClient(emit.WithoutMetrics(c.quietConfig))
	}
	return c.quiet, func() {}
}

// flush flushes both clients. The client without metrics is flushed only
// when this hook used it.
func (c *recordClients) flush(ctx context.Context) error {
	if err := c.metrics.Flush(ctx); err != nil {
		return err
	}
	if c.quiet == nil {
		return nil
	}
	return c.quiet.Flush(ctx)
}

// shutdownQuiet shuts down the client without metrics, if this hook used it.
// The caller owns the client with metrics.
func (c *recordClients) shutdownQuiet(ctx context.Context) {
	if c.quiet != nil {
		_ = c.quiet.Shutdown(ctx)
	}
}

// keys returns the earlier keys, plus this hook's keys recorded before
// deliveredAt, when the newest delivered metrics were collected. A record
// made after that has no delivered metrics, so the retry records it again
// and the turn is still counted once.
func (c *recordClients) keys(deliveredAt time.Time) []string {
	keys := make([]string, 0, len(c.earlier)+len(c.recordedAt))
	for key := range c.earlier {
		keys = append(keys, key)
	}
	for key, at := range c.recordedAt {
		if !at.After(deliveredAt) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

// rememberRecordedMetrics saves which records already have their metrics
// when a hook stops before its export succeeds. Like discardSessionTools, it
// reloads the persisted state so the hook's uncommitted changes stay unsaved.
func rememberRecordedMetrics(sessionID string, keys []string, logger *log.Logger) {
	persisted := state.Load(sessionID)
	if slices.Equal(persisted.MetricsRecorded, keys) {
		return
	}
	persisted.MetricsRecorded = keys
	if err := state.Save(sessionID, persisted); err != nil {
		logger.Printf("save recorded metrics: %v", err)
	}
}
