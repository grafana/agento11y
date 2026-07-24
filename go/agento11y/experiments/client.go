package experiments

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"strings"
	"time"

	agento11y "github.com/grafana/agento11y/go/agento11y"
)

const (
	defaultIngestActor = "ingest:sdk/go"
	ingestActorHeader  = "X-Sigil-Ingest-Actor"
)

// ClientOptions configures experiment ingest. Empty connection fields are read
// from their canonical AGENTO11Y_* environment variables.
type ClientOptions struct {
	Endpoint            string
	TenantID            string
	IngestToken         string
	Actor               string
	Trusted             bool
	GrafanaURL          string
	RetryTimeout        time.Duration
	GenerationEndpoint  string
	Insecure            *bool
	UseExperimentalOTel *bool
	RedactSecrets       *bool
}

// Client owns the shared core transport used for experiment, generation,
// score, and artifact writes.
type Client struct {
	core                *agento11y.Client
	grafanaURL          string
	redactSecrets       bool
	useExperimentalOTel bool
}

func NewClient(opts ClientOptions) (*Client, error) {
	endpoint := firstNonBlank(opts.Endpoint, os.Getenv("AGENTO11Y_ENDPOINT"))
	if endpoint == "" {
		return nil, errors.New("Agent Observability endpoint is required: pass Endpoint or set AGENTO11Y_ENDPOINT")
	}
	token := firstNonBlank(opts.IngestToken, os.Getenv("AGENTO11Y_AUTH_TOKEN"))
	if token == "" {
		return nil, errors.New("ingest token is required: pass IngestToken or set AGENTO11Y_AUTH_TOKEN")
	}
	tenantID := firstNonBlank(opts.TenantID, os.Getenv("AGENTO11Y_AUTH_TENANT_ID"))
	actor := firstNonBlank(opts.Actor, os.Getenv("AGENTO11Y_INGEST_ACTOR"), defaultIngestActor)
	generationEndpoint := firstNonBlank(opts.GenerationEndpoint, endpoint)
	insecure := opts.Insecure
	if insecure == nil {
		value := strings.HasPrefix(strings.ToLower(endpoint), "http://")
		insecure = &value
	}
	redact := true
	if opts.RedactSecrets != nil {
		redact = *opts.RedactSecrets
	}
	useOTel := envBool("AGENTO11Y_USE_EXPERIMENTAL_OTEL")
	if opts.UseExperimentalOTel != nil {
		useOTel = *opts.UseExperimentalOTel
	}
	auth := agento11y.AuthConfig{Mode: agento11y.ExportAuthModeBearer, BearerToken: token}
	if tenantID != "" {
		auth = agento11y.AuthConfig{
			Mode: agento11y.ExportAuthModeBasic, TenantID: tenantID,
			BasicUser: tenantID, BasicPassword: token,
		}
	}
	cfg := agento11y.Config{
		API: agento11y.APIConfig{Endpoint: endpoint},
		GenerationExport: agento11y.GenerationExportConfig{
			Protocol:    agento11y.GenerationExportProtocolHTTP,
			Endpoint:    generationEndpoint,
			Auth:        auth,
			Headers:     map[string]string{ingestActorHeader: actor},
			Insecure:    insecure,
			HTTPTimeout: opts.RetryTimeout,
		},
		ExperimentRetryTimeout:  opts.RetryTimeout,
		RedactExperimentSecrets: redact,
	}
	if redact {
		cfg.GenerationSanitizer = agento11y.NewSecretRedactionSanitizer(agento11y.SecretRedactionOptions{
			RedactInputMessages: agento11y.BoolPtr(true),
		})
	}
	return &Client{
		core:                agento11y.NewClient(cfg),
		grafanaURL:          strings.TrimRight(firstNonBlank(opts.GrafanaURL, os.Getenv("AGENTO11Y_GRAFANA_URL")), "/"),
		redactSecrets:       redact,
		useExperimentalOTel: useOTel,
	}, nil
}

// NewClientFromEnv constructs a client entirely from AGENTO11Y_* variables.
func NewClientFromEnv() (*Client, error) { return NewClient(ClientOptions{}) }

func (c *Client) Core() *agento11y.Client {
	if c == nil {
		return nil
	}
	return c.core
}

func (c *Client) UpsertExperiment(ctx context.Context, req agento11y.CreateExperimentRequest) (*agento11y.Experiment, error) {
	if c == nil || c.core == nil {
		return nil, agento11y.ErrNilClient
	}
	return c.core.CreateExperiment(ctx, req)
}

func (c *Client) Finalize(ctx context.Context, experimentID string, status ExperimentStatus, scoreCount *int, errorText string) (*agento11y.Experiment, error) {
	if c == nil || c.core == nil {
		return nil, agento11y.ErrNilClient
	}
	return c.core.FinalizeExperiment(ctx, experimentID, status, agento11y.CompleteExperimentOptions{
		ScoreCount: scoreCount, Error: c.redactText(errorText),
	})
}

func (c *Client) UpsertTrial(ctx context.Context, experimentID string, req agento11y.UpsertTrialRequest) (*agento11y.TestCaseTrial, error) {
	if c == nil || c.core == nil {
		return nil, agento11y.ErrNilClient
	}
	if c.redactSecrets {
		req.Metadata = redactMap(req.Metadata)
		req.TestCase = redactSnapshot(req.TestCase)
	}
	return c.core.UpsertTrial(ctx, experimentID, req)
}

func (c *Client) UpdateTrial(ctx context.Context, experimentID, trialID string, req agento11y.UpdateTrialRequest) (*agento11y.TestCaseTrial, error) {
	if c == nil || c.core == nil {
		return nil, agento11y.ErrNilClient
	}
	req.Error = c.redactText(req.Error)
	return c.core.UpdateTrial(ctx, experimentID, trialID, req)
}

func (c *Client) ExportScores(ctx context.Context, scores []ScoreItem) (*agento11y.ExportScoresResponse, error) {
	if c == nil || c.core == nil {
		return nil, agento11y.ErrNilClient
	}
	if c.redactSecrets {
		scores = redactScores(scores)
	}
	return c.core.ExportScores(ctx, scores)
}

func (c *Client) RecordGeneration(ctx context.Context, generationID string, opts GenerationOptions) error {
	if c == nil || c.core == nil {
		return agento11y.ErrNilClient
	}
	start := agento11y.GenerationStart{
		ID: generationID, ConversationID: opts.ConversationID,
		Model:     agento11y.ModelRef{Provider: firstNonBlank(opts.ModelProvider, "eval"), Name: firstNonBlank(opts.ModelName, "experiment")},
		AgentName: opts.AgentName, AgentVersion: opts.AgentVersion,
		OperationName: firstNonBlank(opts.OperationName, "invoke_agent"),
		Tags:          cloneStrings(opts.Tags), Metadata: cloneMap(opts.Metadata),
	}
	_, recorder := c.core.StartGeneration(ctx, start)
	generation := agento11y.Generation{
		ID: generationID, ConversationID: opts.ConversationID, Model: start.Model,
		AgentName: opts.AgentName, AgentVersion: opts.AgentVersion,
		Input:  textMessages(agento11y.RoleUser, opts.InputText),
		Output: textMessages(agento11y.RoleAssistant, opts.OutputText),
		Usage:  opts.Usage.Normalize(),
	}
	recorder.SetResult(generation, nil)
	recorder.End()
	if err := recorder.Err(); err != nil {
		return err
	}
	return c.core.Flush(ctx)
}

type GenerationOptions struct {
	ConversationID string
	InputText      string
	OutputText     string
	ModelProvider  string
	ModelName      string
	AgentName      string
	AgentVersion   string
	OperationName  string
	Usage          TokenUsage
	Tags           map[string]string
	Metadata       map[string]any
}

func (c *Client) UploadArtifact(ctx context.Context, experimentID, trialID string, upload agento11y.TrialArtifactUpload) (*TrialArtifact, error) {
	if c == nil || c.core == nil {
		return nil, agento11y.ErrNilClient
	}
	if c.redactSecrets && textLikeArtifact(upload.Kind, upload.MIME) {
		upload.Content = []byte(agento11y.RedactSecretText(string(upload.Content)))
	}
	return c.core.UploadTrialArtifact(ctx, experimentID, trialID, upload)
}

func (c *Client) GetReport(ctx context.Context, experimentID string) (*ExperimentReport, error) {
	if c == nil || c.core == nil {
		return nil, agento11y.ErrNilClient
	}
	report, err := c.core.GetExperimentReport(ctx, experimentID)
	if err != nil {
		return nil, err
	}
	return &ExperimentReport{
		Run: report.Run, Rows: report.Rows,
		Summary: ExperimentReportSummary{
			TestCaseCount: report.Summary.TestCaseCount, TrialCount: report.Summary.TrialCount,
			CompletedCount: report.Summary.CompletedCount, FailedCount: report.Summary.FailedCount,
			CanceledCount: report.Summary.CanceledCount, PassRate: report.Summary.PassRateValue,
			PassAtK: report.Summary.PassAtK, PassPowerK: report.Summary.PassPowerK,
			FinalScoreAvg: report.Summary.FinalScoreAvgValue, TotalCost: report.Summary.TotalCostValue,
			TotalTokens: report.Summary.TotalTokensValue, PassCount: report.Summary.PassCount,
			PassDenominator: report.Summary.PassDenominator, FinalScoreSum: report.Summary.FinalScoreSum,
			FinalScoreCount: report.Summary.FinalScoreCount, TokenCoverage: report.Summary.TokenCoverage,
			CostCoverage: report.Summary.CostCoverage,
		},
	}, nil
}

func (c *Client) ExperimentURL(experimentID string) string {
	if c == nil {
		return ""
	}
	if c.grafanaURL != "" {
		return c.grafanaURL + "/a/grafana-sigil-app/offline-experiments/experiments/" + url.PathEscape(experimentID)
	}
	return c.core.ExperimentURL(experimentID)
}

func (c *Client) Flush(ctx context.Context) error {
	if c == nil || c.core == nil {
		return agento11y.ErrNilClient
	}
	return c.core.Flush(ctx)
}

func (c *Client) Shutdown(ctx context.Context) error {
	if c == nil || c.core == nil {
		return nil
	}
	return c.core.Shutdown(ctx)
}

func (c *Client) redactText(value string) string {
	if c != nil && c.redactSecrets {
		return agento11y.RedactSecretText(value)
	}
	return value
}

func redactScores(in []ScoreItem) []ScoreItem {
	out := make([]ScoreItem, len(in))
	for i := range in {
		out[i] = in[i]
		if in[i].Value.String != nil {
			value := agento11y.RedactSecretText(*in[i].Value.String)
			out[i].Value.String = &value
		}
		out[i].Explanation = agento11y.RedactSecretText(in[i].Explanation)
		out[i].Metadata = redactMap(in[i].Metadata)
	}
	return out
}

func redactMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	value, _ := agento11y.RedactSecretValue(in).(map[string]any)
	return value
}

func redactSnapshot(in *agento11y.TestCaseSnapshot) *agento11y.TestCaseSnapshot {
	if in == nil {
		return nil
	}
	out := *in
	out.Name = agento11y.RedactSecretText(out.Name)
	out.Description = agento11y.RedactSecretText(out.Description)
	out.Input = agento11y.RedactSecretValue(out.Input)
	out.Expected = agento11y.RedactSecretValue(out.Expected)
	out.Metadata = redactMap(out.Metadata)
	return &out
}

func textMessages(role agento11y.Role, text string) []agento11y.Message {
	if text == "" {
		return nil
	}
	if role == agento11y.RoleAssistant {
		return []agento11y.Message{agento11y.AssistantTextMessage(text)}
	}
	return []agento11y.Message{agento11y.UserTextMessage(text)}
}

func cloneStrings(in map[string]string) map[string]string {
	return maps.Clone(in)
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func textLikeArtifact(kind, mime string) bool {
	kind = strings.ToLower(strings.TrimSpace(kind))
	mime = strings.ToLower(strings.TrimSpace(mime))
	return kind == "json" || kind == "markdown" || kind == "text" || kind == "csv" ||
		strings.HasPrefix(mime, "text/") || mime == "application/json"
}

func acceptedCount(response *agento11y.ExportScoresResponse) (int, error) {
	if response == nil {
		return 0, errors.New("empty score export response")
	}
	if rejected := response.Rejected(); len(rejected) > 0 || response.RejectedCount > 0 {
		return 0, fmt.Errorf("score export rejected %d score(s)", max(len(rejected), response.RejectedCount))
	}
	return response.AcceptedCount() + response.DuplicateCount(), nil
}
