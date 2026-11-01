package local

import (
	"strings"
	"time"
)

type SkillAnalytics struct {
	Totals          SkillAnalyticsTotals  `json:"totals"`
	Rows            []ToolAnalyticsRow    `json:"rows"`
	Buckets         []ToolAnalyticsBucket `json:"buckets"`
	Workspaces      []ToolWorkspaceFacet  `json:"workspaces"`
	IntervalSeconds int64                 `json:"interval_seconds"`
	Coverage        ToolAnalyticsCoverage `json:"coverage"`
}

type SkillAnalyticsTotals struct {
	Calls           int `json:"calls"`
	Failures        int `json:"failures"`
	Skills          int `json:"skills"`
	Sessions        int `json:"sessions"`
	DurationSamples int `json:"duration_samples"`
}

func (s *Storage) SkillsToolsMetrics(opts ToolAnalyticsOptions) (SkillsToolsMetricsResponse, error) {
	observations, err := s.toolObservations()
	if err != nil {
		return SkillsToolsMetricsResponse{}, err
	}
	return SkillsToolsMetricsResponse{
		Tools:  toolAnalyticsFromObservations(observations, opts),
		Skills: skillAnalyticsFromObservations(observations, opts),
	}, nil
}

func skillAnalyticsFromObservations(observations []toolObservation, opts ToolAnalyticsOptions) SkillAnalytics {
	if opts.Interval <= 0 {
		var first, last time.Time
		for _, observation := range observations {
			if !nanosRepresentable(observation.Timestamp) ||
				!inPeriod(observation.Timestamp, opts.Since, opts.Before) ||
				!workspaceMatches(observation.Workspace, opts.Workspace) || skillObservationName(observation) == "" {
				continue
			}
			if first.IsZero() || observation.Timestamp.Before(first) {
				first = observation.Timestamp
			}
			if observation.Timestamp.After(last) {
				last = observation.Timestamp
			}
		}
		// The chart fills empty columns to the request bounds, not just
		// between marked observations.
		if !opts.Since.IsZero() {
			first = opts.Since
		}
		if !opts.Before.IsZero() {
			last = opts.Before
		}
		if !first.IsZero() && !last.IsZero() {
			opts.Interval = tokenUsageIntervalFor(last.Sub(first))
		}
	}
	aggregated := executionAnalyticsFromObservations(observations, opts, skillObservationName, skillNameKey)
	return SkillAnalytics{
		Totals: SkillAnalyticsTotals{
			Calls: aggregated.Calls, Failures: aggregated.Failures, Skills: len(aggregated.Rows),
			Sessions: aggregated.Sessions, DurationSamples: aggregated.DurationSamples,
		},
		Rows: aggregated.Rows, Buckets: aggregated.Buckets, Workspaces: aggregated.Workspaces,
		IntervalSeconds: aggregated.IntervalSeconds, Coverage: aggregated.Coverage,
	}
}

func skillObservationName(observation toolObservation) string {
	return skillNameKey(observation.SkillName)
}

func skillNameKey(name string) string {
	return strings.TrimSpace(name)
}
