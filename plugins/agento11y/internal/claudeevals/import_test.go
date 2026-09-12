package claudeevals

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/go/agento11y/experiments"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/claude-2.1.269.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRealClaudeResult(t *testing.T) {
	data := fixture(t)
	plan, err := Parse(bytes.NewReader(data), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Runs) != 2 || len(plan.Runs[0].Trials) != 1 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	if *plan.Runs[0].Create.PlannedTrialCount != 1 {
		t.Fatal("--runs override was replaced by the source case's default of three")
	}
	with, without := plan.Runs[0].Trials[0], plan.Runs[1].Trials[0]
	if *with.Scores[0].Value.Number != 1 || *without.Scores[0].Value.Number != .5 {
		t.Fatal("changed Claude's scores")
	}
	if !*with.Scores[0].Passed || *without.Scores[0].Passed {
		t.Fatal("changed Claude's verdicts")
	}
	for _, run := range plan.Runs {
		if len(run.Create.RunID) > 128 {
			t.Fatal("experiment id too long")
		}
		for _, trial := range run.Trials {
			if trial.Update.Status != "completed" {
				t.Fatal("a failed verdict is not an execution failure")
			}
			for i, score := range trial.Scores {
				if (i == 0) != (score.ScoreKey == "final") {
					t.Fatalf("only the aggregate score may be final: %q", score.ScoreKey)
				}
			}
		}
	}
	indicator := with.Scores[3]
	if indicator.Metadata["scored"] != false || !strings.HasPrefix(indicator.ScoreKey, "grader.") {
		t.Fatal("skill indicator contributes to headline")
	}
	encoded, _ := json.Marshal(plan)
	for _, private := range []string{"promptMarkdown", "getUser", "graderMarkdown", "tracePath", "judge votes:"} {
		if bytes.Contains(encoded, []byte(private)) {
			t.Fatalf("content leaked by default: %s", private)
		}
	}
	full, err := Parse(bytes.NewReader(data), Options{IncludeContent: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := full.Runs[0].Trials[0].Create.TestCase.Expected.(map[string]any); !ok {
		t.Fatal("backend requires test_case.expected to be an object")
	}
	if full.Runs[0].Trials[0].Create.TestCase.Input == nil || full.Runs[0].Trials[0].Scores[1].Explanation == "" {
		t.Fatal("opted-in content missing")
	}
	var reformatted any
	_ = json.Unmarshal(data, &reformatted)
	compact, _ := json.Marshal(reformatted)
	retry, err := Parse(bytes.NewReader(compact), Options{})
	if err != nil || retry.ComparisonID != plan.ComparisonID {
		t.Fatal("formatting changed retry identity", err)
	}
}

func TestPartialValidationAndPrivacy(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    func(*document)
		wantError bool
	}{
		{"future schema", func(d *document) { d.SchemaVersion = 2 }, true},
		{"missing score", func(d *document) { d.Cases[0].Arms["with"][0].Score = nil }, true},
		{"missing baseline", func(d *document) { delete(d.Cases[0].Arms, "without") }, true},
		{"duplicate case", func(d *document) { d.Cases = append(d.Cases, d.Cases[0]) }, true},
		{"invalid score", func(d *document) { x := 2.; d.Cases[0].Arms["with"][0].Score = &x }, true},
		{"single arm", func(d *document) { d.Suite.Ablation = "none"; delete(d.Cases[0].Arms, "without") }, false},
		{"partial", func(d *document) {
			d.Partial = true
			d.PartialReason = "cost_ceiling"
			delete(d.Cases[0].Arms, "without")
		}, false},
		{"skipped judge", func(d *document) { d.Cases[0].Arms["with"][0].SkippedPaidGraders = true }, false},
		{"execution error with nonzero score", func(d *document) { d.Cases[0].Arms["with"][0].Error = "timeout" }, false},
		{"aborted", func(d *document) {
			d.Cases[0].Arms["with"][0].Aborted = json.RawMessage(`{"server":"example","reason":"abort"}`)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var d document
			if err := json.Unmarshal(fixture(t), &d); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&d)
			data, _ := json.Marshal(d)
			p, err := Parse(bytes.NewReader(data), Options{})
			if tc.wantError {
				if err == nil {
					t.Fatal("invalid source accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "partial" && p.Runs[0].Status != agento11y.ExperimentStatusFailed {
				t.Fatal("partial finalized as success")
			}
			if tc.name == "skipped judge" && (p.Runs[0].Trials[0].Scores[0].ScoreKey == "final" || p.Runs[0].Status != agento11y.ExperimentStatusFailed) {
				t.Fatal("skipped judge entered comparable results")
			}
			if (tc.name == "aborted" || tc.name == "execution error with nonzero score") && p.Runs[0].Trials[0].Update.Status != "failed" {
				t.Fatal("execution error lost")
			}
		})
	}
	var d map[string]any
	_ = json.Unmarshal(fixture(t), &d)
	d["unknownFutureField"] = "do not export"
	d["suite"].(map[string]any)["env"] = map[string]string{"private": "do not export"}
	data, _ := json.Marshal(d)
	p, err := Parse(bytes.NewReader(data), Options{IncludeContent: true})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(p)
	if bytes.Contains(out, []byte("do not export")) {
		t.Fatal("unknown source configuration leaked")
	}
	// Synthetic credential, never a real token.
	fake := "ghp_" + strings.Repeat("a", 36)
	data = bytes.ReplaceAll(fixture(t), []byte("getUser"), []byte(fake))
	p, err = Parse(bytes.NewReader(data), Options{IncludeContent: true})
	if err != nil {
		t.Fatal(err)
	}
	out, _ = json.Marshal(p)
	if bytes.Contains(out, []byte(fake)) || !bytes.Contains(out, []byte("REDACTED")) {
		t.Fatal("content was not redacted")
	}
}

func TestExportLifecycleAndRetry(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprintf("reject=%v", reject), func(t *testing.T) {
			p, err := Parse(bytes.NewReader(fixture(t)), Options{})
			if err != nil {
				t.Fatal(err)
			}
			terminal := map[string]string{}
			created := map[string]bool{}
			scoreRequests, finals := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Header.Get("X-Sigil-Ingest-Actor") != "ingest:claude-plugin-eval" {
					t.Error("missing shared ingest actor")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				switch {
				case strings.HasSuffix(r.URL.Path, ":upsert"):
					id := body["experiment_id"].(string)
					status := terminal[id]
					if status == "" {
						status = "running"
					}
					fmt.Fprintf(w, `{"run":{"experiment_id":%q,"status":%q}}`, id, status)
				case strings.HasSuffix(r.URL.Path, "/trials"):
					created[body["trial_id"].(string)] = true
					fmt.Fprint(w, `{}`)
				case strings.HasSuffix(r.URL.Path, "/scores:export"):
					scoreRequests++
					scores := body["scores"].([]any)
					for _, v := range scores {
						score := v.(map[string]any)
						if !created[score["trial_id"].(string)] {
							t.Error("scores exported before typed trial")
						}
						if _, exists := score["report_role"]; exists {
							t.Error("unreleased report_role field sent to production")
						}
						if _, exists := score["generation_id"]; exists {
							t.Error("synthetic generation fabricated")
						}
					}
					if reject {
						fmt.Fprint(w, `{"accepted":0,"rejected":1}`)
					} else {
						fmt.Fprintf(w, `{"accepted":%d}`, len(scores))
					}
				case strings.HasSuffix(r.URL.Path, ":finalize"):
					finals++
					id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/experiment-runs/"), ":finalize")
					terminal[id] = body["status"].(string)
					fmt.Fprintf(w, `{"run":{"experiment_id":%q,"status":%q}}`, id, terminal[id])
				case r.Method == http.MethodPatch:
					fmt.Fprint(w, `{}`)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer server.Close()
			client, err := experiments.NewClient(experiments.ClientOptions{Endpoint: server.URL, IngestToken: "test-only", TenantID: "test", Actor: "ingest:claude-plugin-eval"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Shutdown(context.Background()) }()
			var output bytes.Buffer
			err = p.Export(context.Background(), client, nil, &output)
			if reject {
				if err == nil || finals != 0 {
					t.Fatal("rejected scores finalized successfully")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Export(context.Background(), client, nil, &output); err != nil {
				t.Fatal(err)
			}
			if finals != 2 || scoreRequests != 2 || !strings.Contains(output.String(), "Already imported") {
				t.Fatal("replay duplicated remote writes")
			}
		})
	}
}
