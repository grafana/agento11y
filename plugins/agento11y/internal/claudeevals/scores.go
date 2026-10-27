package claudeevals

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
)

const (
	scoreVisibilityTimeout   = 30 * time.Second
	scoreVisibilityInterval  = time.Second
	missingGenerationMessage = "generation_id was not found"
)

type scoreExportFunc func(context.Context, []agento11y.ScoreItem) (*agento11y.ExportScoresResponse, error)

func exportScores(ctx context.Context, export scoreExportFunc, scores []agento11y.ScoreItem, retryMissing bool) error {
	if retryMissing {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, scoreVisibilityTimeout)
		defer cancel()
	}
	pending := scores
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("Claude eval score export: %w; experiment left running for retry", err)
		}
		if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
			return fmt.Errorf("Claude eval score export: %w; experiment left running for retry", context.DeadlineExceeded)
		}
		result, err := export(ctx, pending)
		if err != nil {
			return err
		}
		if result == nil {
			return errors.New("Claude eval score export returned no result; experiment left running for retry")
		}
		rejected := result.Rejected()
		if result.AcceptedCount()+result.DuplicateCount() == len(pending) && len(rejected) == 0 && result.RejectedCount == 0 {
			return nil
		}
		if !retryMissing || len(result.Results) != len(pending) || len(rejected) == 0 ||
			(result.RejectedCount != 0 && result.RejectedCount != len(rejected)) ||
			result.AcceptedCount()+result.DuplicateCount()+len(rejected) != len(pending) {
			return errors.New("Claude eval score export rejected items; experiment left running for retry")
		}
		byID := make(map[string]agento11y.ScoreItem, len(pending))
		for _, score := range pending {
			byID[score.ScoreID] = score
		}
		next := make([]agento11y.ScoreItem, 0, len(rejected))
		seen := make(map[string]bool, len(result.Results))
		for _, item := range result.Results {
			score, ok := byID[item.ScoreID]
			if !ok || seen[item.ScoreID] {
				return errors.New("Claude eval score export returned inconsistent identities; experiment left running for retry")
			}
			seen[item.ScoreID] = true
			if item.Accepted || item.Status == "duplicate" {
				continue
			}
			// The API returns this message, not its internal generation_not_found reason code.
			if item.Error != missingGenerationMessage || score.GenerationID == "" {
				return fmt.Errorf("Claude eval score export rejected %s: %s; experiment left running for retry", item.ScoreID, item.Error)
			}
			next = append(next, score)
		}
		timer := time.NewTimer(scoreVisibilityInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("Claude eval score generation visibility: %w; experiment left running for retry", ctx.Err())
		case <-timer.C:
		}
		pending = next
	}
}
