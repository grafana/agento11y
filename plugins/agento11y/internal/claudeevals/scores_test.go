package claudeevals

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/stretchr/testify/require"
)

func pendingScoreResponse(t *testing.T) *agento11y.ExportScoresResponse {
	t.Helper()
	data, err := os.ReadFile("testdata/scores-generation-pending.json")
	require.NoError(t, err)
	var result agento11y.ExportScoresResponse
	require.NoError(t, json.Unmarshal(data, &result))
	return &result
}

func visibilityScores() []agento11y.ScoreItem {
	return []agento11y.ScoreItem{
		{ScoreID: "score-accepted", GenerationID: "generation-accepted"},
		{ScoreID: "score-pending", GenerationID: "generation-pending", Metadata: map[string]any{"stable": "value"}},
		{ScoreID: "score-duplicate", GenerationID: "generation-duplicate"},
	}
}

func TestScoreVisibilityRetriesOnlyPending(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "duplicate"}[duplicate], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				scores := visibilityScores()
				calls := 0
				started := time.Now()
				err := exportScores(t.Context(), func(_ context.Context, batch []agento11y.ScoreItem) (*agento11y.ExportScoresResponse, error) {
					calls++
					if calls == 1 {
						require.Equal(t, scores, batch)
						return pendingScoreResponse(t), nil
					}
					require.Equal(t, scores[1:2], batch)
					item := agento11y.ExportScoreResult{ScoreID: batch[0].ScoreID, Accepted: true, Status: "accepted"}
					if duplicate {
						item.Accepted, item.Status = false, "duplicate"
					}
					return &agento11y.ExportScoresResponse{Results: []agento11y.ExportScoreResult{item}}, nil
				}, scores, true)
				require.NoError(t, err)
				require.Equal(t, 2, calls)
				require.Equal(t, scoreVisibilityInterval, time.Since(started))
			})
		})
	}
}

func TestScoreVisibilityDoesNotRetryOtherFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		edit      func(*agento11y.ExportScoresResponse, []agento11y.ScoreItem)
		http      bool
		transport bool
	}{
		{name: "HTTP mode", http: true},
		{name: "internal reason is not wire message", edit: func(r *agento11y.ExportScoresResponse, _ []agento11y.ScoreItem) {
			r.Results[1].Error = "generation_not_found"
		}},
		{name: "other rejection", edit: func(r *agento11y.ExportScoresResponse, _ []agento11y.ScoreItem) {
			r.Results[1].Error = "trial was not found"
		}},
		{name: "mixed rejection", edit: func(r *agento11y.ExportScoresResponse, _ []agento11y.ScoreItem) {
			r.Results[0].Accepted, r.Results[0].Status, r.Results[0].Error = false, "rejected", "trial was not found"
			r.Accepted, r.RejectedCount = 0, 2
		}},
		{name: "unlinked score", edit: func(_ *agento11y.ExportScoresResponse, s []agento11y.ScoreItem) { s[1].GenerationID = "" }},
		{name: "unknown identity", edit: func(r *agento11y.ExportScoresResponse, _ []agento11y.ScoreItem) { r.Results[1].ScoreID = "unknown" }},
		{name: "repeated identity", edit: func(r *agento11y.ExportScoresResponse, _ []agento11y.ScoreItem) {
			r.Results[1].ScoreID = r.Results[0].ScoreID
		}},
		{name: "missing item", edit: func(r *agento11y.ExportScoresResponse, _ []agento11y.ScoreItem) { r.Results = r.Results[:2] }},
		{name: "inconsistent rejection count", edit: func(r *agento11y.ExportScoresResponse, _ []agento11y.ScoreItem) { r.RejectedCount = 2 }},
		{name: "transport failure", transport: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			scores := visibilityScores()
			response := pendingScoreResponse(t)
			if tc.edit != nil {
				tc.edit(response, scores)
			}
			err := exportScores(t.Context(), func(context.Context, []agento11y.ScoreItem) (*agento11y.ExportScoresResponse, error) {
				calls++
				if tc.transport {
					return nil, errors.New("transport failed")
				}
				return response, nil
			}, scores, !tc.http)
			require.Error(t, err)
			require.Equal(t, 1, calls)
		})
	}
}

func TestScoreVisibilityDeadlineAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name           string
		timeout        time.Duration
		parentDeadline bool
		cancel         bool
	}{
		{name: "visibility budget", timeout: 30 * time.Second},
		{name: "earlier caller deadline", timeout: 1500 * time.Millisecond, parentDeadline: true},
		{name: "canceled while pending", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tc.parentDeadline {
					var cancelTimeout context.CancelFunc
					ctx, cancelTimeout = context.WithTimeout(ctx, tc.timeout)
					defer cancelTimeout()
				}
				started := time.Now()
				calls := 0
				err := exportScores(ctx, func(ctx context.Context, batch []agento11y.ScoreItem) (*agento11y.ExportScoresResponse, error) {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
					calls++
					require.Equal(t, visibilityScores()[1:2], batch)
					if tc.cancel {
						cancel()
					}
					response := pendingScoreResponse(t)
					response.Results = response.Results[1:2]
					response.Accepted, response.Duplicates = 0, 0
					return response, nil
				}, visibilityScores()[1:2], true)
				if tc.cancel {
					require.ErrorIs(t, err, context.Canceled)
					require.Equal(t, 1, calls)
				} else {
					require.ErrorIs(t, err, context.DeadlineExceeded)
					require.Equal(t, tc.timeout, time.Since(started))
					require.LessOrEqual(t, calls, 30)
				}
			})
		})
	}
}
