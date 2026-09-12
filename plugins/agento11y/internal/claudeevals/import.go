// Package claudeevals imports completed `claude plugin eval` JSON documents.
// It never runs Claude or regrades results. Trajectory capture reads only
// retained trace files below an explicitly authorized root.
package claudeevals

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/go/agento11y/experiments"
	"github.com/grafana/agento11y/plugins/agento11y/internal/redact"
)

const maxDocumentBytes = 32 << 20

type document struct {
	SchemaVersion   int      `json:"schemaVersion"`
	ClaudeVersion   string   `json:"claudeVersion"`
	StartedAt       string   `json:"startedAt"`
	Partial         bool     `json:"partial"`
	PartialReason   string   `json:"partialReason"`
	CostUSD         *float64 `json:"costUsd"`
	DurationSeconds *float64 `json:"durationSeconds"`
	Suite           struct {
		Ablation   string   `json:"ablation"`
		Threshold  *float64 `json:"threshold"`
		Model      string   `json:"modelOverride"`
		JudgeModel string   `json:"judgeModel"`
		Plugins    []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"plugins"`
	} `json:"suite"`
	Aggregates aggregates `json:"aggregates"`
	Cases      []evalCase `json:"cases"`
}

type aggregates struct {
	CasesTotal      *int     `json:"casesTotal,omitempty"`
	CasesPassed     *int     `json:"casesPassed,omitempty"`
	OverallScore    *float64 `json:"overallScore,omitempty"`
	OverallPassRate *float64 `json:"overallPassRate,omitempty"`
	MeanDelta       *float64 `json:"meanDelta,omitempty"`
	Score           *float64 `json:"score,omitempty"`
	PassRate        *float64 `json:"passRate,omitempty"`
	ScoreWithout    *float64 `json:"scoreWithout,omitempty"`
	PassRateWithout *float64 `json:"passRateWithout,omitempty"`
	Delta           *float64 `json:"delta,omitempty"`
}

type evalCase struct {
	Name       string               `json:"name"`
	Prompt     string               `json:"promptMarkdown"`
	Runs       int                  `json:"runsPerCase"`
	Graders    []definition         `json:"graders"`
	Aggregates aggregates           `json:"aggregates"`
	Arms       map[string][]attempt `json:"arms"`
}

type definition struct {
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	Weight   float64         `json:"weight"`
	Markdown string          `json:"graderMarkdown,omitempty"`
	Config   json.RawMessage `json:"config,omitempty"`
}

type attempt struct {
	TracePath          string          `json:"tracePath"`
	Score              *float64        `json:"score"`
	Passed             *bool           `json:"passed"`
	CostUSD            *float64        `json:"costUsd"`
	JudgeCostUSD       *float64        `json:"judgeCostUsd"`
	DurationSeconds    *float64        `json:"durationSeconds"`
	StartedAt          string          `json:"startedAt"`
	Error              string          `json:"error"`
	Aborted            json.RawMessage `json:"aborted"`
	SkippedPaidGraders bool            `json:"skippedPaidGraders"`
	Graders            []grader        `json:"graders"`
}

type grader struct {
	Name        string          `json:"name"`
	Passed      *bool           `json:"passed"`
	Weight      float64         `json:"weight"`
	Scored      *bool           `json:"scored"`
	WithOnly    bool            `json:"withOnly"`
	Explanation string          `json:"explanation"`
	Evidence    json.RawMessage `json:"evidence,omitempty"`
	JudgeVotes  []bool          `json:"judgeVotes,omitempty"`
}

type Options struct {
	Name           string
	IncludeContent bool
	TraceRoot      string
}

// Plan contains only the selected export fields. In particular, local paths,
// arbitrary suite configuration/environment, and transcripts never enter it.
type Plan struct {
	ComparisonID      string `json:"comparison_id"`
	Partial           bool   `json:"partial"`
	IncludeContent    bool   `json:"include_content"`
	IncludeTrajectory bool   `json:"include_trajectory"`
	Runs              []Run  `json:"runs"`
}

type Run struct {
	Arm    string                            `json:"arm"`
	Create agento11y.CreateExperimentRequest `json:"create"`
	Trials []Trial                           `json:"trials"`
	Status agento11y.ExperimentStatus        `json:"status"`
}

type Trial struct {
	Trajectory *Trajectory                  `json:"trajectory,omitempty"`
	Create     agento11y.UpsertTrialRequest `json:"create"`
	Update     agento11y.UpdateTrialRequest `json:"update"`
	Scores     []agento11y.ScoreItem        `json:"scores"`
}

func stableID(prefix string, value any) string {
	data, _ := json.Marshal(value)
	return fmt.Sprintf("%s-%x", prefix, sha256.Sum256(data))
}

// Parse validates the entire document before any remote writes. Unknown JSON
// fields are ignored as required by Claude's additive schemaVersion=1 contract.
func Parse(reader io.Reader, opts Options) (*Plan, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxDocumentBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDocumentBytes {
		return nil, errors.New("Claude eval result exceeds 32 MiB")
	}
	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("invalid Claude eval JSON: %w", err)
	}
	if doc.SchemaVersion != 1 {
		return nil, fmt.Errorf("unsupported Claude eval schemaVersion %d (expected 1)", doc.SchemaVersion)
	}
	if err := doc.validate(); err != nil {
		return nil, err
	}
	// Include all original fields in the identity, but never export them. JSON
	// whitespace and object key order do not turn a retry into another run.
	var original any
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	if err := dec.Decode(&original); err != nil {
		return nil, err
	}
	identity := []any{original, opts.IncludeContent, opts.Name}
	if opts.TraceRoot != "" {
		identity = append(identity, "trajectory-v1")
	}
	id := stableID("claude-eval", identity)
	plan := &Plan{ComparisonID: id, Partial: doc.Partial, IncludeContent: opts.IncludeContent, IncludeTrajectory: opts.TraceRoot != ""}
	seenSessions := map[string]bool{}
	pluginNames := make([]string, 0, len(doc.Suite.Plugins))
	for _, plugin := range doc.Suite.Plugins {
		pluginNames = append(pluginNames, plugin.Name)
	}
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = "Claude plugin eval: " + strings.Join(pluginNames, ", ")
	}
	if len(pluginNames) == 0 && opts.Name == "" {
		name = "Claude plugin eval"
	}
	type caseDefinition struct {
		Name, Prompt string
		Graders      []definition
	}
	definitions := make([]caseDefinition, 0, len(doc.Cases))
	for _, c := range doc.Cases {
		definitions = append(definitions, caseDefinition{c.Name, c.Prompt, c.Graders})
	}
	suiteID := stableID("claude-suite", pluginNames)
	suiteVersion := stableID("suite", definitions)
	for _, arm := range []string{"with", "without"} {
		run := Run{Arm: arm, Status: agento11y.ExperimentStatusCompleted}
		planned := 0
		for _, c := range doc.Cases {
			planned += len(c.Arms[arm])
		}
		run.Create = agento11y.CreateExperimentRequest{
			RunID: id + "-" + arm, Name: name + " [" + arm + "]", Source: agento11y.ExperimentSourceExternal,
			SuiteID: suiteID, SuiteVersion: suiteVersion, PlannedTrialCount: &planned,
			Tags:      []string{"claude-plugin-eval", "arm:" + arm},
			Candidate: map[string]any{"agent_name": strings.Join(pluginNames, ", ") + " [" + arm + "]", "model_name": doc.Suite.Model},
			Metadata: map[string]any{
				"framework": "claude-plugin-eval", "comparison_id": id, "arm": arm,
				"claude_version": doc.ClaudeVersion, "source_started_at": doc.StartedAt,
				"plugins": doc.Suite.Plugins, "ablation": doc.Suite.Ablation, "judge_model": doc.Suite.JudgeModel,
				"threshold": *doc.Suite.Threshold, "partial": doc.Partial, "partial_reason": doc.PartialReason,
				"source_aggregates": doc.Aggregates, "source_cost_usd": doc.CostUSD, "source_duration_seconds": doc.DurationSeconds,
				"include_content": opts.IncludeContent, "include_trajectory": plan.IncludeTrajectory,
			},
		}
		if doc.Partial {
			// Claude 2.1.269's runsPerCase is the case default, not the
			// effective --runs override. A partial's planned count is unknown.
			run.Create.PlannedTrialCount = nil
			run.Status = agento11y.ExperimentStatusFailed
			run.Create.Tags = append(run.Create.Tags, "partial")
		}
		for _, c := range doc.Cases {
			for index, a := range c.Arms[arm] {
				trialID := stableID("trial", []any{run.Create.RunID, c.Name, index + 1})
				caseID := stableID("case", []string{suiteID, c.Name})
				snapshot := &agento11y.TestCaseSnapshot{TestCaseID: caseID, Name: c.Name, SuiteID: suiteID, SuiteVersion: suiteVersion}
				if opts.IncludeContent {
					snapshot.Input = map[string]any{"prompt": c.Prompt}
					snapshot.Expected = map[string]any{"graders": c.Graders}
				}
				t := Trial{Create: agento11y.UpsertTrialRequest{
					TrialID: trialID, TestCaseID: caseID, Attempt: index + 1, Status: "running", TestCase: snapshot,
					Metadata: map[string]any{"arm": arm, "source_started_at": a.StartedAt, "source_case_aggregates": c.Aggregates, "skipped_paid_graders": a.SkippedPaidGraders, "judge_cost_usd": a.JudgeCostUSD},
				}, Update: agento11y.UpdateTrialRequest{Status: "completed", Cost: a.CostUSD}}
				if a.DurationSeconds != nil {
					ms := int(math.Round(*a.DurationSeconds * 1000))
					t.Update.DurationMillis = &ms
				}
				if a.Error != "" || isAborted(a) {
					t.Update.Status = "failed"
					t.Update.Error = "Claude eval attempt ended abnormally"
					if opts.IncludeContent {
						if a.Error != "" {
							t.Update.Error = a.Error
						}
						t.Create.Metadata["aborted"] = a.Aborted
					}
				}
				// The released SDK/server contract selects `final` as the
				// headline. Every grader is namespaced so none can replace it.
				// Skipped paid graders are not comparable headline scores.
				scoreKey := "final"
				if a.SkippedPaidGraders {
					scoreKey = "claude_score_incomplete"
					run.Status = agento11y.ExperimentStatusFailed
				}
				t.Scores = append(t.Scores, agento11y.ScoreItem{
					ScoreID: stableID("score", []string{trialID, "aggregate"}), TrialID: trialID,
					EvaluatorID: "claude-plugin-eval", EvaluatorVersion: doc.ClaudeVersion,
					ScoreKey: scoreKey, Value: agento11y.NumberScoreValue(*a.Score), Passed: a.Passed,
					Metadata: map[string]any{"skipped_paid_graders": a.SkippedPaidGraders},
				})
				for _, g := range a.Graders {
					var def definition
					for _, candidate := range c.Graders {
						if candidate.Name == g.Name {
							def = candidate
							break
						}
					}
					score := agento11y.ScoreItem{
						ScoreID: stableID("score", []string{trialID, "grader", g.Name}), TrialID: trialID,
						EvaluatorID: "claude-plugin-eval." + def.Type, EvaluatorVersion: stableID("grader", def),
						ScoreKey: "grader." + g.Name, Value: agento11y.BoolScoreValue(*g.Passed), Passed: g.Passed,
						Metadata: map[string]any{"weight": g.Weight, "scored": *g.Scored, "with_only": g.WithOnly, "judge_votes": g.JudgeVotes},
					}
					if opts.IncludeContent {
						score.Explanation = g.Explanation
						score.Metadata["evidence"] = g.Evidence
					}
					t.Scores = append(t.Scores, score)
				}
				if plan.IncludeTrajectory {
					trajectory, err := readTrajectory(opts.TraceRoot, a.TracePath, a, c.Prompt, stableID("gen", trialID), c.Name+" ["+arm+"]")
					if err != nil {
						return nil, fmt.Errorf("case %s %s attempt %d: %w", c.Name, arm, index+1, err)
					}
					g := &trajectory.Generation
					if seenSessions[g.ConversationID] {
						return nil, errors.New("multiple trials reference the same source session")
					}
					seenSessions[g.ConversationID] = true
					g.Tags = map[string]string{"experiment_id": run.Create.RunID, "test_case_id": caseID, "trial_id": trialID, "framework": "claude-plugin-eval", "arm": arm}
					g.Metadata["experiment_id"], g.Metadata["test_case_id"], g.Metadata["trial_id"] = run.Create.RunID, caseID, trialID
					g.TraceID, g.SpanID = traceID(g.ID).String(), spanID(g.ID).String()
					t.Trajectory = trajectory
					t.Create.ConversationID, t.Create.TraceID, t.Create.SpanID = g.ConversationID, g.TraceID, g.SpanID
					t.Update.ConversationID, t.Update.TraceID, t.Update.SpanID = g.ConversationID, g.TraceID, g.SpanID
					input, output := int(g.Usage.InputTokens), int(g.Usage.OutputTokens)
					t.Update.InputTokens, t.Update.OutputTokens = &input, &output
					t.Create.Metadata["agent_token_usage"] = g.Usage
					t.Create.Metadata["trace_sha256"] = trajectory.SHA256
					for i := range t.Scores {
						t.Scores[i].GenerationID, t.Scores[i].ConversationID = g.ID, g.ConversationID
						t.Scores[i].TraceID, t.Scores[i].SpanID = g.TraceID, g.SpanID
					}
					if !opts.IncludeContent {
						g.Input, g.Output = nil, nil
						for i := range trajectory.Activities {
							trajectory.Activities[i].Arguments, trajectory.Activities[i].Result = nil, nil
						}
					}
				}
				run.Trials = append(run.Trials, t)
			}
		}
		if len(run.Trials) > 0 {
			plan.Runs = append(plan.Runs, run)
		}
	}
	if len(plan.Runs) == 0 {
		return nil, errors.New("Claude eval result contains no attempts to import")
	}
	// Redact every selected field, including names, before previews or export.
	selected, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(redact.New().RedactJSON(selected), plan); err != nil {
		return nil, err
	}
	for _, run := range plan.Runs {
		for _, trial := range run.Trials {
			if trial.Trajectory != nil {
				if err := agento11y.ValidateGeneration(trial.Trajectory.Generation); err != nil {
					return nil, fmt.Errorf("invalid trajectory for %s: %w", trial.Create.TrialID, err)
				}
			}
		}
	}
	return plan, nil
}

func isAborted(a attempt) bool { return len(a.Aborted) != 0 && string(a.Aborted) != "null" }

func (d document) validate() error {
	if d.ClaudeVersion == "" || len(d.Cases) == 0 {
		return errors.New("Claude version and at least one case are required")
	}
	if _, err := time.Parse(time.RFC3339Nano, d.StartedAt); err != nil {
		return errors.New("invalid Claude eval startedAt")
	}
	if d.Suite.Threshold == nil || *d.Suite.Threshold < 0 || *d.Suite.Threshold > 1 {
		return errors.New("suite.threshold must be between 0 and 1")
	}
	if d.Suite.Ablation != "none" && d.Suite.Ablation != "with-without" {
		return errors.New("unsupported suite.ablation")
	}
	seen := map[string]bool{}
	for _, c := range d.Cases {
		if strings.TrimSpace(c.Name) == "" || seen[c.Name] || c.Runs < 1 || c.Runs > 50 {
			return errors.New("case names must be unique and runsPerCase must be 1..50")
		}
		seen[c.Name] = true
		defs := map[string]bool{}
		for _, g := range c.Graders {
			if g.Name == "" || g.Type == "" || defs[g.Name] || g.Weight <= 0 {
				return errors.New("invalid or duplicate grader definition")
			}
			defs[g.Name] = true
		}
		for arm, attempts := range c.Arms {
			if arm != "with" && arm != "without" {
				return errors.New("unsupported eval arm")
			}
			if arm == "without" && d.Suite.Ablation == "none" {
				return errors.New("without arm in a single-arm eval")
			}
			if len(attempts) > 50 {
				return errors.New("more than 50 attempts in one case arm")
			}
			for _, a := range attempts {
				if a.Score == nil || *a.Score < 0 || *a.Score > 1 || a.Passed == nil {
					return errors.New("each attempt needs a score in 0..1 and a passed verdict")
				}
				for _, value := range []*float64{a.CostUSD, a.JudgeCostUSD, a.DurationSeconds} {
					if value != nil && (*value < 0 || *value > 1e9) {
						return errors.New("invalid attempt cost or duration")
					}
				}
				graders := map[string]bool{}
				for _, g := range a.Graders {
					if !defs[g.Name] || graders[g.Name] || g.Passed == nil || g.Scored == nil || g.Weight <= 0 {
						return errors.New("invalid, duplicate, or undefined attempt grader")
					}
					graders[g.Name] = true
				}
			}
		}
		if !d.Partial && (len(c.Arms["with"]) == 0 || (d.Suite.Ablation == "with-without" && len(c.Arms["without"]) == 0)) {
			return errors.New("complete eval is missing an arm")
		}
	}
	return nil
}

// Export leaves an interrupted upload running so replaying the same file can
// resume it. Source partials are finalized as failed, not successful benchmarks.
func (p *Plan) Export(ctx context.Context, client *experiments.Client, telemetry *Telemetry, output io.Writer) error {
	if p.IncludeTrajectory && telemetry == nil {
		return errors.New("trajectory import requires an initialized OTLP exporter")
	}
	for _, run := range p.Runs {
		remote, err := client.UpsertExperiment(ctx, run.Create)
		if err != nil {
			return fmt.Errorf("upsert %s: %w", run.Arm, err)
		}
		if remote.Status != string(agento11y.ExperimentStatusRunning) {
			if remote.Status != string(run.Status) {
				return fmt.Errorf("experiment %s has conflicting terminal status %s", run.Create.RunID, remote.Status)
			}
			fmt.Fprintf(output, "Already imported: %s\n", client.ExperimentURL(run.Create.RunID))
			continue
		}
		count := 0
		for _, trial := range run.Trials {
			if _, err := client.UpsertTrial(ctx, run.Create.RunID, trial.Create); err != nil {
				return fmt.Errorf("create trial %s: %w", trial.Create.TrialID, err)
			}
			if trial.Trajectory != nil {
				if err := telemetry.export(ctx, trial.Trajectory, p.IncludeContent); err != nil {
					return fmt.Errorf("export trajectory for %s: %w", trial.Create.TrialID, err)
				}
			}
			result, err := client.ExportScores(ctx, trial.Scores)
			if err != nil {
				return err
			}
			if result.AcceptedCount()+result.DuplicateCount() != len(trial.Scores) {
				return errors.New("Claude eval score export rejected items; experiment left running for retry")
			}
			if _, err := client.UpdateTrial(ctx, run.Create.RunID, trial.Create.TrialID, trial.Update); err != nil {
				return fmt.Errorf("update trial %s: %w", trial.Create.TrialID, err)
			}
			count += len(trial.Scores)
		}
		errorText := ""
		if run.Status == agento11y.ExperimentStatusFailed {
			errorText = "Source eval was partial or skipped paid graders; exclude from complete-run comparisons"
		}
		if _, err := client.Finalize(ctx, run.Create.RunID, run.Status, &count, errorText); err != nil {
			return err
		}
		fmt.Fprintf(output, "Imported %s: %d trials, %d scores — %s\n", run.Arm, len(run.Trials), count, client.ExperimentURL(run.Create.RunID))
	}
	return nil
}
