package sigil

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	ExperimentRunIDTag         = "experiment.run_id"
	ExperimentRunIDMetadataKey = "experiment_run_id"
)

type UploadMode string

const (
	UploadModeContinuous UploadMode = "continuous"
	UploadModeBulk       UploadMode = "bulk"
	UploadModeManual     UploadMode = "manual"
)

type DatasetItem struct {
	ID       string
	Input    any
	Expected any
	Metadata map[string]any
}

type TargetResult struct {
	Output         any
	GenerationIDs  []string
	ConversationID string
	Metadata       map[string]any
}

type ScoreOutput struct {
	EvaluatorID      string
	EvaluatorVersion string
	ScoreKey         string
	Value            ScoreValue
	GenerationID     string
	Passed           *bool
	Explanation      string
	Metadata         map[string]any
}

type ExperimentResult struct {
	RunID          string
	AcceptedScores int
	URL            string
	Report         *ExperimentReport
}

type DatasetTarget func(ctx context.Context, item DatasetItem) (TargetResult, error)
type DatasetScorer func(ctx context.Context, item DatasetItem, result TargetResult) ([]ScoreOutput, error)

type ExperimentOptions struct {
	Client        *Client
	RunID         string
	Name          string
	Description   string
	Tags          []string
	Metadata      map[string]any
	Dataset       map[string]any
	Candidate     map[string]any
	Upload        UploadMode
	PrintURL      bool
	AgentName     string
	AgentVersion  string
	ExtraTags     map[string]string
	ExtraMetadata map[string]any
}

type ExperimentRun struct {
	client *Client

	RunID string
	Name  string

	dataset       map[string]any
	candidate     map[string]any
	upload        UploadMode
	agentName     string
	agentVersion  string
	extraTags     map[string]string
	extraMetadata map[string]any

	mu                   sync.Mutex
	buffer               []ScoreItem
	accepted             int
	finalized            bool
	recorders            []*GenerationRecorder
	trackedIDs           []string
	activeConversationID string
}

type AddScoresOptions struct {
	Item           *DatasetItem
	GenerationIDs  []string
	ConversationID string
	TrialID        string
}

type ExperimentRunner struct {
	Client        *Client
	RunID         string
	Name          string
	Description   string
	Tags          []string
	Metadata      map[string]any
	Dataset       map[string]any
	Candidate     map[string]any
	Upload        UploadMode
	PrintURL      bool
	FetchReport   bool
	AgentName     string
	AgentVersion  string
	ExtraTags     map[string]string
	ExtraMetadata map[string]any
}

func StableID(prefix string, parts ...any) string {
	values := make([]string, len(parts))
	for i, part := range parts {
		if part != nil {
			values[i] = fmt.Sprint(part)
		}
	}
	digest := sha1.Sum([]byte(strings.Join(values, "\x1f")))
	return prefix + "-" + hex.EncodeToString(digest[:])[:16]
}

func NewExperimentRun(opts ExperimentOptions) *ExperimentRun {
	upload := opts.Upload
	if upload == "" {
		upload = UploadModeContinuous
	}
	return &ExperimentRun{
		client:        opts.Client,
		RunID:         opts.RunID,
		Name:          opts.Name,
		dataset:       cloneMetadata(opts.Dataset),
		candidate:     cloneMetadata(opts.Candidate),
		upload:        upload,
		agentName:     opts.AgentName,
		agentVersion:  opts.AgentVersion,
		extraTags:     cloneTags(opts.ExtraTags),
		extraMetadata: cloneMetadata(opts.ExtraMetadata),
	}
}

func WithExperiment(ctx context.Context, opts ExperimentOptions, fn func(context.Context, *ExperimentRun) error) (*ExperimentRun, error) {
	if opts.Client == nil {
		return nil, ErrNilClient
	}
	if opts.Upload == "" {
		opts.Upload = UploadModeContinuous
	}
	if opts.RunID = strings.TrimSpace(opts.RunID); opts.RunID == "" {
		return nil, fmt.Errorf("%w: run_id is required", ErrExperimentValidationFailed)
	}
	if opts.Name = strings.TrimSpace(opts.Name); opts.Name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrExperimentValidationFailed)
	}
	if _, err := opts.Client.CreateExperiment(ctx, CreateExperimentRequest{
		RunID:       opts.RunID,
		Name:        opts.Name,
		Source:      ExperimentSourceExternal,
		Description: opts.Description,
		Tags:        append([]string(nil), opts.Tags...),
		Metadata:    runMetadata(opts.Metadata, opts.Dataset, opts.Candidate),
	}); err != nil {
		return nil, err
	}

	run := NewExperimentRun(opts)
	err := fn(ctx, run)
	if err != nil {
		if errorsIsContextCanceled(err) {
			_, _ = opts.Client.CancelExperiment(ctx, opts.RunID)
			return run, err
		}
		_ = run.Finalize(ctx, ExperimentStatusFailed, err.Error())
		return run, err
	}

	if opts.Upload == UploadModeManual {
		if opts.PrintURL {
			opts.Client.config.Logger.Printf("[sigil] experiment %q left open (manual mode): %d score(s) buffered. Call run.Publish() then run.Finalize() to upload.", opts.RunID, run.BufferedScoreCount())
		}
		return run, nil
	}
	if _, err := run.Publish(ctx); err != nil {
		_ = run.Finalize(ctx, ExperimentStatusFailed, err.Error())
		return run, err
	}
	if err := run.Finalize(ctx, ExperimentStatusSucceeded, ""); err != nil {
		return run, err
	}
	if opts.PrintURL {
		opts.Client.config.Logger.Printf("[sigil] experiment %q finished (%d scores): %s", opts.RunID, run.AcceptedScores(), run.URL())
	}
	return run, nil
}

func (r *ExperimentRun) Context(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if r == nil {
		return ctx
	}
	return withExperimentRun(ctx, r)
}

func (r *ExperimentRun) TrackGenerationID(generationID string) {
	if r == nil {
		return
	}
	generationID = strings.TrimSpace(generationID)
	if generationID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !containsString(r.trackedIDs, generationID) {
		r.trackedIDs = append(r.trackedIDs, generationID)
	}
}

func (r *ExperimentRun) ResetCapture(conversationID string) string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recorders = nil
	r.trackedIDs = nil
	r.activeConversationID = strings.TrimSpace(conversationID)
	return r.activeConversationID
}

func (r *ExperimentRun) ProducedGenerationIDs() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for _, recorder := range r.recorders {
		if recorder == nil {
			continue
		}
		recorder.mu.Lock()
		id := recorder.lastGeneration.ID
		recorder.mu.Unlock()
		if id != "" && !containsString(ids, id) {
			ids = append(ids, id)
		}
	}
	for _, id := range r.trackedIDs {
		if id != "" && !containsString(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

func (r *ExperimentRun) ActiveConversationID() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activeConversationID
}

func (r *ExperimentRun) captureRecorder(recorder *GenerationRecorder) {
	if r == nil || recorder == nil {
		return
	}
	r.mu.Lock()
	r.recorders = append(r.recorders, recorder)
	r.mu.Unlock()
}

func (r *ExperimentRun) AddScores(ctx context.Context, scores []ScoreOutput, opts AddScoresOptions) (int, error) {
	if r == nil || r.client == nil {
		return 0, ErrNilClient
	}
	if len(scores) == 0 {
		return 0, nil
	}
	generationIDs := append([]string(nil), opts.GenerationIDs...)
	if len(generationIDs) == 0 {
		generationIDs = r.ProducedGenerationIDs()
	}
	conversationID := strings.TrimSpace(opts.ConversationID)
	if conversationID == "" {
		conversationID = r.ActiveConversationID()
	}

	items := make([]ScoreItem, 0, len(scores))
	for _, score := range scores {
		item, err := r.buildScoreItem(score, opts.Item, generationIDs, conversationID, opts.TrialID)
		if err != nil {
			return 0, err
		}
		items = append(items, item)
	}

	r.mu.Lock()
	upload := r.upload
	if upload != UploadModeContinuous {
		r.buffer = append(r.buffer, items...)
		r.mu.Unlock()
		return len(items), nil
	}
	r.mu.Unlock()

	if err := r.client.Flush(ctx); err != nil {
		return 0, err
	}
	response, err := r.client.ExportScores(ctx, items)
	if err != nil {
		return 0, err
	}
	accepted, err := acceptedOrError(response)
	if err != nil {
		return 0, err
	}
	r.mu.Lock()
	r.accepted += accepted
	r.mu.Unlock()
	return accepted, nil
}

func (r *ExperimentRun) Publish(ctx context.Context) (int, error) {
	if r == nil || r.client == nil {
		return 0, ErrNilClient
	}
	r.mu.Lock()
	if len(r.buffer) == 0 {
		r.mu.Unlock()
		return 0, nil
	}
	items := append([]ScoreItem(nil), r.buffer...)
	r.mu.Unlock()

	if err := r.client.Flush(ctx); err != nil {
		return 0, err
	}
	response, err := r.client.ExportScores(ctx, items)
	if err != nil {
		return 0, err
	}
	accepted, err := acceptedOrError(response)
	if err != nil {
		return 0, err
	}

	r.mu.Lock()
	r.accepted += accepted
	r.buffer = nil
	r.mu.Unlock()
	return accepted, nil
}

func (r *ExperimentRun) AcceptedScores() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.accepted
}

func (r *ExperimentRun) BufferedScoreCount() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.buffer)
}

func (r *ExperimentRun) URL() string {
	if r == nil || r.client == nil {
		return ""
	}
	return r.client.ExperimentURL(r.RunID)
}

func (r *ExperimentRun) Report(ctx context.Context) (*ExperimentReport, error) {
	if r == nil || r.client == nil {
		return nil, ErrNilClient
	}
	return r.client.GetExperimentReport(ctx, r.RunID)
}

func (r *ExperimentRun) Finalize(ctx context.Context, status ExperimentStatus, errorText string) error {
	if r == nil || r.client == nil {
		return ErrNilClient
	}
	r.mu.Lock()
	if r.finalized {
		r.mu.Unlock()
		return nil
	}
	scoreCount := r.accepted
	r.mu.Unlock()

	_, err := r.client.CompleteExperiment(ctx, r.RunID, status, CompleteExperimentOptions{
		ScoreCount: &scoreCount,
		Error:      errorText,
	})
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.finalized = true
	r.mu.Unlock()
	return nil
}

func (r *ExperimentRunner) Run(ctx context.Context, items []DatasetItem, target DatasetTarget, scorers []DatasetScorer) (ExperimentResult, error) {
	if r == nil {
		return ExperimentResult{}, ErrNilClient
	}
	fetchReport := r.FetchReport
	opts := ExperimentOptions{
		Client:        r.Client,
		RunID:         r.RunID,
		Name:          r.Name,
		Description:   r.Description,
		Tags:          append([]string(nil), r.Tags...),
		Metadata:      cloneMetadata(r.Metadata),
		Dataset:       cloneMetadata(r.Dataset),
		Candidate:     cloneMetadata(r.Candidate),
		Upload:        r.Upload,
		PrintURL:      r.PrintURL,
		AgentName:     r.AgentName,
		AgentVersion:  r.AgentVersion,
		ExtraTags:     cloneTags(r.ExtraTags),
		ExtraMetadata: cloneMetadata(r.ExtraMetadata),
	}
	var completedRun *ExperimentRun
	run, err := WithExperiment(ctx, opts, func(ctx context.Context, run *ExperimentRun) error {
		completedRun = run
		for _, item := range items {
			run.ResetCapture(StableID("conv", run.RunID, item.ID))
			itemCtx := run.Context(ctx)
			result, err := target(itemCtx, item)
			if err != nil {
				return err
			}
			generationIDs := append([]string(nil), result.GenerationIDs...)
			if len(generationIDs) == 0 {
				generationIDs = run.ProducedGenerationIDs()
			}
			var outputs []ScoreOutput
			for _, scorer := range scorers {
				produced, err := scorer(ctx, item, result)
				if err != nil {
					return err
				}
				outputs = append(outputs, produced...)
			}
			if _, err := run.AddScores(ctx, outputs, AddScoresOptions{
				Item:           &item,
				GenerationIDs:  generationIDs,
				ConversationID: firstNonBlank(result.ConversationID, run.ActiveConversationID()),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if run != nil {
		completedRun = run
	}
	if err != nil {
		if completedRun == nil {
			return ExperimentResult{}, err
		}
		return ExperimentResult{
			RunID:          completedRun.RunID,
			AcceptedScores: completedRun.AcceptedScores(),
			URL:            completedRun.URL(),
		}, err
	}
	var report *ExperimentReport
	if fetchReport && completedRun != nil {
		report, _ = completedRun.Report(ctx)
	}
	if completedRun == nil {
		return ExperimentResult{}, nil
	}
	return ExperimentResult{
		RunID:          completedRun.RunID,
		AcceptedScores: completedRun.AcceptedScores(),
		URL:            completedRun.URL(),
		Report:         report,
	}, nil
}

func (r *ExperimentRun) prepareGeneration(start GenerationStart) GenerationStart {
	seed := cloneGenerationStart(start)

	r.mu.Lock()
	conversationID := strings.TrimSpace(seed.ConversationID)
	if conversationID == "" {
		conversationID = strings.TrimSpace(r.activeConversationID)
	}
	if conversationID == "" {
		conversationID = StableID("conv", r.RunID, experimentRandomHex(8))
	}
	r.activeConversationID = conversationID
	r.mu.Unlock()
	seed.ConversationID = conversationID

	tags := cloneTags(r.extraTags)
	if tags == nil {
		tags = map[string]string{}
	}
	maps.Copy(tags, seed.Tags)
	tags[ExperimentRunIDTag] = r.RunID
	seed.Tags = tags

	metadata := cloneMetadata(r.extraMetadata)
	if metadata == nil {
		metadata = map[string]any{}
	}
	maps.Copy(metadata, seed.Metadata)
	metadata[ExperimentRunIDMetadataKey] = r.RunID
	seed.Metadata = metadata

	if seed.AgentName == "" && r.agentName != "" {
		seed.AgentName = r.agentName
	}
	if seed.AgentVersion == "" && r.agentVersion != "" {
		seed.AgentVersion = r.agentVersion
	}
	return seed
}

func (r *ExperimentRun) buildScoreItem(score ScoreOutput, item *DatasetItem, generationIDs []string, conversationID string, trialID string) (ScoreItem, error) {
	generationID := strings.TrimSpace(score.GenerationID)
	if generationID == "" {
		if len(generationIDs) == 1 {
			generationID = generationIDs[0]
		} else if len(generationIDs) > 1 {
			return ScoreItem{}, fmt.Errorf("%w: target produced multiple generations; scorer %q must set ScoreOutput.GenerationID explicitly", ErrScoreValidationFailed, score.EvaluatorID)
		}
	}
	scoreItemID := ""
	if item != nil {
		scoreItemID = item.ID
	}
	scoreID := StableID("score", r.RunID, scoreItemID, generationID, score.EvaluatorID, score.EvaluatorVersion, score.ScoreKey, trialID)
	return ScoreItem{
		ScoreID:          scoreID,
		GenerationID:     generationID,
		ConversationID:   strings.TrimSpace(conversationID),
		RunID:            r.RunID,
		EvaluatorID:      score.EvaluatorID,
		EvaluatorVersion: score.EvaluatorVersion,
		ScoreKey:         score.ScoreKey,
		Value:            score.Value,
		Passed:           score.Passed,
		Explanation:      score.Explanation,
		Metadata:         r.scoreMetadata(score, item, trialID),
		Source:           &ScoreSource{Kind: "experiment", ID: r.RunID},
	}, nil
}

func (r *ExperimentRun) scoreMetadata(score ScoreOutput, item *DatasetItem, trialID string) map[string]any {
	metadata := map[string]any{}
	if id, ok := stringMetadataValue(r.dataset, "id"); ok {
		metadata["dataset_id"] = id
	}
	if version, ok := stringMetadataValue(r.dataset, "version"); ok {
		metadata["dataset_version"] = version
	}
	if len(r.candidate) > 0 {
		metadata["candidate"] = cloneMetadata(r.candidate)
	}
	if item != nil {
		metadata["item_id"] = item.ID
		maps.Copy(metadata, item.Metadata)
	}
	if strings.TrimSpace(trialID) != "" {
		metadata["trial_id"] = strings.TrimSpace(trialID)
	}
	maps.Copy(metadata, score.Metadata)
	return metadata
}

func runMetadata(metadata, dataset, candidate map[string]any) map[string]any {
	out := cloneMetadata(metadata)
	if out == nil {
		out = map[string]any{}
	}
	if id, ok := stringMetadataValue(dataset, "id"); ok {
		if _, exists := out["dataset_id"]; !exists {
			out["dataset_id"] = id
		}
	}
	if version, ok := stringMetadataValue(dataset, "version"); ok {
		if _, exists := out["dataset_version"]; !exists {
			out["dataset_version"] = version
		}
	}
	if uri, ok := stringMetadataValue(dataset, "uri"); ok {
		if _, exists := out["dataset_uri"]; !exists {
			out["dataset_uri"] = uri
		}
	}
	if len(candidate) > 0 {
		if _, exists := out["candidate"]; !exists {
			out["candidate"] = cloneMetadata(candidate)
		}
	}
	if _, exists := out["created_at"]; !exists {
		out["created_at"] = time.Now().UTC().Format(time.RFC3339)
	}
	return out
}

func acceptedOrError(response *ExportScoresResponse) (int, error) {
	if response == nil {
		return 0, fmt.Errorf("%w: empty response", ErrScoreExportFailed)
	}
	rejected := response.Rejected()
	if len(rejected) == 0 {
		return response.AcceptedCount(), nil
	}
	parts := make([]string, len(rejected))
	for i, result := range rejected {
		detail := result.Error
		if detail == "" {
			detail = "rejected"
		}
		parts[i] = result.ScoreID + ": " + detail
	}
	return 0, fmt.Errorf("%w: rejected %d score(s): %s", ErrScoreExportFailed, len(rejected), strings.Join(parts, "; "))
}

func stringMetadataValue(metadata map[string]any, key string) (string, bool) {
	if len(metadata) == 0 {
		return "", false
	}
	value, ok := metadata[key]
	if !ok {
		return "", false
	}
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	text = strings.TrimSpace(text)
	return text, text != ""
}

func experimentRandomHex(n int) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func containsString(values []string, target string) bool {
	return slices.Contains(values, target)
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func errorsIsContextCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
