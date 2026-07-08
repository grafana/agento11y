package sigil

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	sigilv1 "github.com/grafana/sigil-sdk/go/proto/sigil/v1"
	"github.com/grafana/sigil-sdk/go/proto/sigil/wire"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

const (
	tenantHeaderName        = wire.TenantHeaderName
	authorizationHeaderName = wire.AuthorizationHeaderName
)

type queuedGeneration struct {
	generation *sigilv1.Generation
}

type queuedWorkflowStep struct {
	workflowStep *sigilv1.WorkflowStep
}

type generationExporter interface {
	Export(ctx context.Context, request *sigilv1.ExportGenerationsRequest) (*sigilv1.ExportGenerationsResponse, error)
	ExportWorkflowSteps(ctx context.Context, request *sigilv1.ExportWorkflowStepsRequest) (*sigilv1.ExportWorkflowStepsResponse, error)
	Shutdown(ctx context.Context) error
}

type noopGenerationExporter struct {
	err error
}

func newNoopGenerationExporter(err error) generationExporter {
	return &noopGenerationExporter{err: err}
}

func (e *noopGenerationExporter) Export(_ context.Context, request *sigilv1.ExportGenerationsRequest) (*sigilv1.ExportGenerationsResponse, error) {
	if e.err != nil {
		return nil, e.err
	}
	if request == nil {
		return &sigilv1.ExportGenerationsResponse{}, nil
	}
	response := &sigilv1.ExportGenerationsResponse{
		Results: make([]*sigilv1.ExportGenerationResult, 0, len(request.GetGenerations())),
	}
	for _, generation := range request.GetGenerations() {
		response.Results = append(response.Results, &sigilv1.ExportGenerationResult{
			GenerationId: generation.GetId(),
			Accepted:     true,
		})
	}
	return response, nil
}

func (e *noopGenerationExporter) ExportWorkflowSteps(_ context.Context, request *sigilv1.ExportWorkflowStepsRequest) (*sigilv1.ExportWorkflowStepsResponse, error) {
	if e.err != nil {
		return nil, e.err
	}
	if request == nil {
		return &sigilv1.ExportWorkflowStepsResponse{}, nil
	}
	response := &sigilv1.ExportWorkflowStepsResponse{
		Results: make([]*sigilv1.ExportWorkflowStepResult, 0, len(request.GetWorkflowSteps())),
	}
	for _, step := range request.GetWorkflowSteps() {
		response.Results = append(response.Results, &sigilv1.ExportWorkflowStepResult{
			StepId:   step.GetId(),
			Accepted: true,
		})
	}
	return response, nil
}

func (e *noopGenerationExporter) Shutdown(_ context.Context) error {
	return nil
}

type grpcGenerationExporter struct {
	client             sigilv1.GenerationIngestServiceClient
	workflowStepClient sigilv1.WorkflowStepIngestServiceClient
	conn               *grpc.ClientConn
	headers            map[string]string
}

func newGRPCGenerationExporter(cfg GenerationExportConfig) (generationExporter, error) {
	endpoint, _, insecureEndpoint, err := splitEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	maxSendMessageBytes := cfg.GRPCMaxSendMessageBytes
	if maxSendMessageBytes <= 0 {
		maxSendMessageBytes = defaultGRPCMaxSendMessageBytes
	}
	maxReceiveMessageBytes := cfg.GRPCMaxReceiveMessageBytes
	if maxReceiveMessageBytes <= 0 {
		maxReceiveMessageBytes = defaultGRPCMaxReceiveMessageBytes
	}

	transportCreds := credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2"},
	})
	if insecureValue(cfg.Insecure) || insecureEndpoint {
		transportCreds = insecure.NewCredentials()
	}

	// gRPC reserves the user-agent metadata key, so the User-Agent must travel
	// via the dial option rather than outgoing metadata. grpc-go appends its own
	// token after this value.
	userAgent, headers := splitUserAgent(cfg.Headers)

	conn, err := grpc.NewClient(
		endpoint,
		grpc.WithTransportCredentials(transportCreds),
		grpc.WithUserAgent(userAgent),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallSendMsgSize(maxSendMessageBytes),
			grpc.MaxCallRecvMsgSize(maxReceiveMessageBytes),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("dial generation ingest grpc endpoint %q: %w", endpoint, err)
	}

	return &grpcGenerationExporter{
		client:             sigilv1.NewGenerationIngestServiceClient(conn),
		workflowStepClient: sigilv1.NewWorkflowStepIngestServiceClient(conn),
		conn:               conn,
		headers:            headers,
	}, nil
}

// splitUserAgent returns the User-Agent to set as the gRPC dial option and the
// remaining headers with any User-Agent entry removed. When the caller did not
// supply one, the SDK default (UserAgent) is used.
func splitUserAgent(headers map[string]string) (userAgent string, rest map[string]string) {
	userAgent = UserAgent()
	rest = cloneTags(headers)
	for key, value := range rest {
		if strings.EqualFold(key, "User-Agent") {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				userAgent = value
			}
			delete(rest, key)
		}
	}
	return userAgent, rest
}

func (e *grpcGenerationExporter) Export(ctx context.Context, request *sigilv1.ExportGenerationsRequest) (*sigilv1.ExportGenerationsResponse, error) {
	if len(e.headers) > 0 {
		ctx = metadata.NewOutgoingContext(ctx, metadata.New(e.headers))
	}
	return e.client.ExportGenerations(ctx, request)
}

func (e *grpcGenerationExporter) ExportWorkflowSteps(ctx context.Context, request *sigilv1.ExportWorkflowStepsRequest) (*sigilv1.ExportWorkflowStepsResponse, error) {
	if len(e.headers) > 0 {
		ctx = metadata.NewOutgoingContext(ctx, metadata.New(e.headers))
	}
	return e.workflowStepClient.ExportWorkflowSteps(ctx, request)
}

func (e *grpcGenerationExporter) Shutdown(_ context.Context) error {
	if e.conn != nil {
		return e.conn.Close()
	}
	return nil
}

type httpGenerationExporter struct {
	endpoint             string
	workflowStepEndpoint string
	userAgent            string
	headers              map[string]string
	client               *http.Client
}

func newHTTPGenerationExporter(cfg GenerationExportConfig) (generationExporter, error) {
	urlString, err := wire.NormalizeGenerationExportURL(cfg.Endpoint, insecureValue(cfg.Insecure))
	if err != nil {
		return nil, err
	}
	workflowStepURLString, err := wire.NormalizeWorkflowStepExportURL(cfg.Endpoint, insecureValue(cfg.Insecure))
	if err != nil {
		return nil, err
	}

	// Resolve the User-Agent the same way the gRPC exporter does: a non-blank
	// caller override wins, otherwise the SDK default. headers has any
	// User-Agent entry removed so it can't blank out the resolved value below.
	userAgent, headers := splitUserAgent(cfg.Headers)
	return &httpGenerationExporter{
		endpoint:             urlString,
		workflowStepEndpoint: workflowStepURLString,
		userAgent:            userAgent,
		headers:              headers,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}, nil
}

func (e *httpGenerationExporter) Export(ctx context.Context, request *sigilv1.ExportGenerationsRequest) (*sigilv1.ExportGenerationsResponse, error) {
	payload, err := wire.MarshalExportGenerationsJSON(request)
	if err != nil {
		return nil, fmt.Errorf("marshal generation request: %w", err)
	}

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return nil, fmt.Errorf("build generation request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", wire.ContentTypeJSON)
	httpRequest.Header.Set("User-Agent", e.userAgent)
	for key, value := range e.headers {
		httpRequest.Header.Set(key, value)
	}

	response, err := e.client.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("http generation export failed: %w", err)
	}
	defer func() {
		_ = response.Body.Close()
	}()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read generation response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("http generation export status %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}

	exportResponse, err := wire.UnmarshalExportGenerationsResponseJSON(body)
	if err != nil {
		return nil, fmt.Errorf("unmarshal generation response: %w", err)
	}

	return exportResponse, nil
}

func (e *httpGenerationExporter) ExportWorkflowSteps(ctx context.Context, request *sigilv1.ExportWorkflowStepsRequest) (*sigilv1.ExportWorkflowStepsResponse, error) {
	payload, err := wire.MarshalExportWorkflowStepsJSON(request)
	if err != nil {
		return nil, fmt.Errorf("marshal workflow step request: %w", err)
	}

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, e.workflowStepEndpoint, strings.NewReader(string(payload)))
	if err != nil {
		return nil, fmt.Errorf("build workflow step request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", wire.ContentTypeJSON)
	httpRequest.Header.Set("User-Agent", e.userAgent)
	for key, value := range e.headers {
		httpRequest.Header.Set(key, value)
	}

	response, err := e.client.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("http workflow step export failed: %w", err)
	}
	defer func() {
		_ = response.Body.Close()
	}()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read workflow step response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("http workflow step export status %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}

	exportResponse, err := wire.UnmarshalExportWorkflowStepsResponseJSON(body)
	if err != nil {
		return nil, fmt.Errorf("unmarshal workflow step response: %w", err)
	}

	return exportResponse, nil
}

func (e *httpGenerationExporter) Shutdown(_ context.Context) error {
	return nil
}

func newGenerationExporter(cfg GenerationExportConfig) (generationExporter, error) {
	switch cfg.Protocol {
	case GenerationExportProtocolGRPC:
		return newGRPCGenerationExporter(cfg)
	case GenerationExportProtocolHTTP:
		return newHTTPGenerationExporter(cfg)
	case GenerationExportProtocolNone:
		return newNoopGenerationExporter(nil), nil
	default:
		return nil, fmt.Errorf("unsupported generation export protocol %q", cfg.Protocol)
	}
}

// insecureValue dereferences a *bool used for the optional Insecure flag,
// treating nil as false (TLS expected).
func insecureValue(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

func mergeGenerationExportConfig(base, override GenerationExportConfig) GenerationExportConfig {
	out := base
	if override.Protocol != "" {
		out.Protocol = override.Protocol
	}
	if override.Endpoint != "" {
		out.Endpoint = override.Endpoint
	}
	if override.Headers != nil {
		out.Headers = cloneTags(override.Headers)
	}
	out.Auth = mergeAuthConfig(out.Auth, override.Auth)
	if override.Insecure != nil {
		out.Insecure = override.Insecure
	}
	if override.GRPCMaxSendMessageBytes > 0 {
		out.GRPCMaxSendMessageBytes = override.GRPCMaxSendMessageBytes
	}
	if override.GRPCMaxReceiveMessageBytes > 0 {
		out.GRPCMaxReceiveMessageBytes = override.GRPCMaxReceiveMessageBytes
	}
	if override.BatchSize > 0 {
		out.BatchSize = override.BatchSize
	}
	if override.FlushInterval > 0 {
		out.FlushInterval = override.FlushInterval
	}
	if override.QueueSize > 0 {
		out.QueueSize = override.QueueSize
	}
	if override.MaxRetries > 0 {
		out.MaxRetries = override.MaxRetries
	}
	if override.InitialBackoff > 0 {
		out.InitialBackoff = override.InitialBackoff
	}
	if override.MaxBackoff > 0 {
		out.MaxBackoff = override.MaxBackoff
	}
	if override.PayloadMaxBytes > 0 {
		out.PayloadMaxBytes = override.PayloadMaxBytes
	}
	return out
}

func mergeAPIConfig(base, override APIConfig) APIConfig {
	out := base
	if override.Endpoint != "" {
		out.Endpoint = override.Endpoint
	}
	return out
}

func mergeEmbeddingCaptureConfig(base, override EmbeddingCaptureConfig) EmbeddingCaptureConfig {
	out := base
	out.CaptureInput = override.CaptureInput
	if override.MaxInputItems > 0 {
		out.MaxInputItems = override.MaxInputItems
	}
	if override.MaxTextLength > 0 {
		out.MaxTextLength = override.MaxTextLength
	}
	return out
}

func mergeAuthConfig(base, override AuthConfig) AuthConfig {
	out := base
	if override.Mode != "" {
		out.Mode = override.Mode
	}
	if override.TenantID != "" {
		out.TenantID = override.TenantID
	}
	if override.BearerToken != "" {
		out.BearerToken = override.BearerToken
	}
	if override.BasicUser != "" {
		out.BasicUser = override.BasicUser
	}
	if override.BasicPassword != "" {
		out.BasicPassword = override.BasicPassword
	}
	return out
}

// resolveHeadersWithAuth builds the auth headers for the given mode.
// Mode-irrelevant fields (e.g. TenantID when mode=bearer) are silently ignored.
func resolveHeadersWithAuth(headers map[string]string, auth AuthConfig) (map[string]string, error) {
	mode := auth.Mode
	if mode == "" {
		mode = ExportAuthModeNone
	}

	tenantID := strings.TrimSpace(auth.TenantID)
	bearerToken := strings.TrimSpace(auth.BearerToken)

	switch mode {
	case ExportAuthModeNone:
		return cloneTags(headers), nil
	case ExportAuthModeTenant:
		if tenantID == "" {
			return nil, errors.New("auth mode tenant requires tenant_id")
		}
		out := cloneTags(headers)
		if hasHeaderKey(out, tenantHeaderName) {
			return out, nil
		}
		if out == nil {
			out = make(map[string]string, 1)
		}
		out[tenantHeaderName] = tenantID
		return out, nil
	case ExportAuthModeBearer:
		if bearerToken == "" {
			return nil, errors.New("auth mode bearer requires bearer_token")
		}
		out := cloneTags(headers)
		if hasHeaderKey(out, authorizationHeaderName) {
			return out, nil
		}
		if out == nil {
			out = make(map[string]string, 1)
		}
		out[authorizationHeaderName] = formatBearerTokenValue(bearerToken)
		return out, nil
	case ExportAuthModeBasic:
		password := strings.TrimSpace(auth.BasicPassword)
		if password == "" {
			return nil, errors.New("auth mode basic requires basic_password")
		}
		user := strings.TrimSpace(auth.BasicUser)
		if user == "" {
			user = tenantID
		}
		if user == "" {
			return nil, errors.New("auth mode basic requires basic_user or tenant_id")
		}
		out := cloneTags(headers)
		if out == nil {
			out = make(map[string]string, 2)
		}
		if !hasHeaderKey(out, authorizationHeaderName) {
			out[authorizationHeaderName] = "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
		}
		if tenantID != "" && !hasHeaderKey(out, tenantHeaderName) {
			out[tenantHeaderName] = tenantID
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported auth mode %q", mode)
	}
}

func hasHeaderKey(headers map[string]string, key string) bool {
	for existing := range headers {
		if strings.EqualFold(existing, key) {
			return true
		}
	}
	return false
}

func formatBearerTokenValue(token string) string {
	trimmed := strings.TrimSpace(token)
	if strings.HasPrefix(strings.ToLower(trimmed), "bearer ") {
		trimmed = strings.TrimSpace(trimmed[len("bearer "):])
	}
	return "Bearer " + trimmed
}

func splitEndpoint(endpoint string) (host string, path string, insecure bool, err error) {
	trimmed := strings.TrimSpace(endpoint)
	if trimmed == "" {
		return "", "", false, errors.New("endpoint is required")
	}

	if strings.Contains(trimmed, "://") {
		parsed, parseErr := url.Parse(trimmed)
		if parseErr != nil {
			return "", "", false, fmt.Errorf("parse endpoint %q: %w", endpoint, parseErr)
		}
		if parsed.Host == "" {
			return "", "", false, fmt.Errorf("endpoint %q has empty host", endpoint)
		}
		return parsed.Host, parsed.Path, parsed.Scheme == "http", nil
	}

	return trimmed, "", false, nil
}

func (c *Client) startWorker() {
	c.workerOnce.Do(func() {
		go c.runExportWorker()
	})
}

func (c *Client) runExportWorker() {
	defer close(c.workerDone)

	generationBatch := make([]*sigilv1.Generation, 0, c.config.GenerationExport.BatchSize)
	workflowStepBatch := make([]*sigilv1.WorkflowStep, 0, c.config.GenerationExport.BatchSize)
	flushInterval := c.config.GenerationExport.FlushInterval
	timer := time.NewTimer(flushInterval)
	defer timer.Stop()

	flushGenerations := func() error {
		if len(generationBatch) == 0 {
			return nil
		}

		request := &sigilv1.ExportGenerationsRequest{Generations: generationBatch}
		err := c.exportWithRetry(request)
		generationBatch = generationBatch[:0]
		return err
	}
	flushWorkflowSteps := func() error {
		if len(workflowStepBatch) == 0 {
			return nil
		}

		request := &sigilv1.ExportWorkflowStepsRequest{WorkflowSteps: workflowStepBatch}
		err := c.exportWorkflowStepsWithRetry(request)
		workflowStepBatch = workflowStepBatch[:0]
		return err
	}
	flushAll := func() error {
		return errors.Join(flushGenerations(), flushWorkflowSteps())
	}

	// pendingErr captures failures from async flushes (timer or batch-size
	// triggered) since the last explicit Flush call. It's returned to and
	// cleared by the next flushReq handler so callers who use Flush as a
	// durability checkpoint can detect silent data loss instead of treating an
	// empty post-failure batch as success. Errors are joined rather than
	// keeping only the first: a single timer tick flushes the generation and
	// workflow-step queues separately, so both can fail in the same cycle and
	// neither failure should be dropped.
	var pendingErr error
	recordAsyncErr := func(err error) {
		if err == nil {
			return
		}
		pendingErr = errors.Join(pendingErr, err)
	}

	generationQueue := c.queue
	workflowStepQueue := c.workflowStepQueue
	for {
		if generationQueue == nil && workflowStepQueue == nil {
			if err := flushAll(); err != nil {
				c.logf("sigil export flush on shutdown failed: %v", err)
			}
			return
		}

		select {
		case queued, ok := <-generationQueue:
			if !ok {
				generationQueue = nil
				continue
			}
			generationBatch = append(generationBatch, queued.generation)
			if len(generationBatch) >= c.config.GenerationExport.BatchSize {
				if err := flushGenerations(); err != nil {
					c.logf("sigil generation export failed: %v", err)
					recordAsyncErr(err)
				}
				resetTimer(timer, flushInterval)
			}
		case queued, ok := <-workflowStepQueue:
			if !ok {
				workflowStepQueue = nil
				continue
			}
			workflowStepBatch = append(workflowStepBatch, queued.workflowStep)
			if len(workflowStepBatch) >= c.config.GenerationExport.BatchSize {
				if err := flushWorkflowSteps(); err != nil {
					c.logf("sigil workflow step export failed: %v", err)
					recordAsyncErr(err)
				}
				resetTimer(timer, flushInterval)
			}
		case ack := <-c.flushReq:
			// Drain anything the worker hadn't yet pulled off its queues so
			// that an explicit Flush sees every export enqueued before
			// the call. Without this, the worker's select can service
			// flushReq before an already-queued item, returning nil while
			// the item still lingers on the channel.
			//
			// Respect BatchSize during the drain: queue depth can far
			// exceed BatchSize, so flush mid-drain whenever the batch
			// reaches the configured size and join any errors with the
			// final flush result.
			var flushErr error
			joinFlush := func(err error) {
				if err == nil {
					return
				}
				if flushErr == nil {
					flushErr = err
					return
				}
				flushErr = errors.Join(flushErr, err)
			}
			generationClosed := false
			workflowStepClosed := false
		draining:
			for {
				select {
				case queued, ok := <-generationQueue:
					if !ok {
						generationQueue = nil
						generationClosed = true
						if workflowStepQueue == nil {
							break draining
						}
						continue
					}
					generationBatch = append(generationBatch, queued.generation)
					if len(generationBatch) >= c.config.GenerationExport.BatchSize {
						joinFlush(flushGenerations())
					}
				case queued, ok := <-workflowStepQueue:
					if !ok {
						workflowStepQueue = nil
						workflowStepClosed = true
						if generationQueue == nil {
							break draining
						}
						continue
					}
					workflowStepBatch = append(workflowStepBatch, queued.workflowStep)
					if len(workflowStepBatch) >= c.config.GenerationExport.BatchSize {
						joinFlush(flushWorkflowSteps())
					}
				default:
					break draining
				}
			}
			joinFlush(flushAll())
			if pendingErr != nil {
				joinFlush(pendingErr)
				pendingErr = nil
			}
			ack <- flushErr
			if generationClosed && workflowStepClosed {
				// Shutdown raced with Flush — caller has been acked, exit
				// the worker like the regular shutdown path.
				return
			}
			resetTimer(timer, flushInterval)
		case <-timer.C:
			if err := flushGenerations(); err != nil {
				c.logf("sigil generation export failed: %v", err)
				recordAsyncErr(err)
			}
			if err := flushWorkflowSteps(); err != nil {
				c.logf("sigil workflow step export failed: %v", err)
				recordAsyncErr(err)
			}
			resetTimer(timer, flushInterval)
		}
	}
}

func resetTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}

func (c *Client) exportWithRetry(request *sigilv1.ExportGenerationsRequest) error {
	attempts := c.config.GenerationExport.MaxRetries + 1
	backoff := c.config.GenerationExport.InitialBackoff
	if backoff <= 0 {
		backoff = 100 * time.Millisecond
	}

	var lastErr error
	for attempt := range attempts {
		timeoutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		response, err := c.exporter.Export(timeoutCtx, request)
		cancel()
		if err == nil {
			resultsCount := 0
			if response != nil {
				resultsCount = len(response.GetResults())
			}
			c.logf(
				"sigil generation export response requested=%d results=%d",
				len(request.GetGenerations()),
				resultsCount,
			)
			logRejectedExportResults(c, response)
			validateErr := validateExportResponse(request, response)
			if validateErr == nil {
				return nil
			}
			lastErr = validateErr
			if !isRetryableExportValidationError(validateErr) {
				return validateErr
			}
		} else {
			lastErr = err
		}
		if attempt == attempts-1 {
			break
		}
		time.Sleep(backoff)
		if backoff < c.config.GenerationExport.MaxBackoff {
			backoff *= 2
			if backoff > c.config.GenerationExport.MaxBackoff {
				backoff = c.config.GenerationExport.MaxBackoff
			}
		}
	}

	return lastErr
}

func (c *Client) exportWorkflowStepsWithRetry(request *sigilv1.ExportWorkflowStepsRequest) error {
	attempts := c.config.GenerationExport.MaxRetries + 1
	backoff := c.config.GenerationExport.InitialBackoff
	if backoff <= 0 {
		backoff = 100 * time.Millisecond
	}

	var lastErr error
	for attempt := range attempts {
		timeoutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		response, err := c.exporter.ExportWorkflowSteps(timeoutCtx, request)
		cancel()
		if err == nil {
			resultsCount := 0
			if response != nil {
				resultsCount = len(response.GetResults())
			}
			c.logf(
				"sigil workflow step export response requested=%d results=%d",
				len(request.GetWorkflowSteps()),
				resultsCount,
			)
			logRejectedWorkflowStepExportResults(c, response)
			validateErr := validateWorkflowStepExportResponse(request, response)
			if validateErr == nil {
				return nil
			}
			lastErr = validateErr
			if !isRetryableExportValidationError(validateErr) {
				return validateErr
			}
		} else {
			lastErr = err
		}
		if attempt == attempts-1 {
			break
		}
		time.Sleep(backoff)
		if backoff < c.config.GenerationExport.MaxBackoff {
			backoff *= 2
			if backoff > c.config.GenerationExport.MaxBackoff {
				backoff = c.config.GenerationExport.MaxBackoff
			}
		}
	}

	return lastErr
}

type exportValidationError struct {
	err       error
	retryable bool
}

func (e *exportValidationError) Error() string {
	return e.err.Error()
}

func (e *exportValidationError) Unwrap() error {
	return e.err
}

func isRetryableExportValidationError(err error) bool {
	var validationErr *exportValidationError
	return errors.As(err, &validationErr) && validationErr.retryable
}

func logRejectedExportResults(c *Client, response *sigilv1.ExportGenerationsResponse) {
	for _, result := range response.GetResults() {
		if result == nil || result.Accepted {
			continue
		}
		c.logf("sigil generation rejected id=%s error=%s", result.GenerationId, result.Error)
	}
}

func logRejectedWorkflowStepExportResults(c *Client, response *sigilv1.ExportWorkflowStepsResponse) {
	for _, result := range response.GetResults() {
		if result == nil || result.Accepted {
			continue
		}
		c.logf("sigil workflow step rejected id=%s error=%s", result.StepId, result.Error)
	}
}

func validateExportResponse(request *sigilv1.ExportGenerationsRequest, response *sigilv1.ExportGenerationsResponse) error {
	if response == nil {
		return &exportValidationError{err: errors.New("nil generation export response"), retryable: true}
	}
	requested := len(request.GetGenerations())
	results := response.GetResults()
	if len(results) != requested {
		return &exportValidationError{err: fmt.Errorf("generation export result count mismatch: requested=%d results=%d", requested, len(results)), retryable: true}
	}
	malformed := make([]string, 0, len(results))
	rejected := make([]string, 0, len(results))
	for _, result := range results {
		if result == nil {
			malformed = append(malformed, "<nil result>")
			continue
		}
		if result.Accepted {
			continue
		}
		msg := strings.TrimSpace(result.Error)
		if msg == "" {
			msg = "rejected without error"
		}
		rejected = append(rejected, fmt.Sprintf("%s: %s", result.GenerationId, msg))
	}
	if len(rejected) > 0 {
		return &exportValidationError{err: fmt.Errorf("generation export rejected: %s", strings.Join(rejected, "; "))}
	}
	if len(malformed) > 0 {
		return &exportValidationError{err: fmt.Errorf("generation export malformed response: %s", strings.Join(malformed, "; ")), retryable: true}
	}
	return nil
}

func validateWorkflowStepExportResponse(request *sigilv1.ExportWorkflowStepsRequest, response *sigilv1.ExportWorkflowStepsResponse) error {
	if response == nil {
		return &exportValidationError{err: errors.New("nil workflow step export response"), retryable: true}
	}
	requested := len(request.GetWorkflowSteps())
	results := response.GetResults()
	if len(results) != requested {
		return &exportValidationError{err: fmt.Errorf("workflow step export result count mismatch: requested=%d results=%d", requested, len(results)), retryable: true}
	}
	malformed := make([]string, 0, len(results))
	rejected := make([]string, 0, len(results))
	for _, result := range results {
		if result == nil {
			malformed = append(malformed, "<nil result>")
			continue
		}
		if result.Accepted {
			continue
		}
		msg := strings.TrimSpace(result.Error)
		if msg == "" {
			msg = "rejected without error"
		}
		rejected = append(rejected, fmt.Sprintf("%s: %s", result.StepId, msg))
	}
	if len(rejected) > 0 {
		return &exportValidationError{err: fmt.Errorf("workflow step export rejected: %s", strings.Join(rejected, "; "))}
	}
	if len(malformed) > 0 {
		return &exportValidationError{err: fmt.Errorf("workflow step export malformed response: %s", strings.Join(malformed, "; ")), retryable: true}
	}
	return nil
}

func (c *Client) enqueueGeneration(generation Generation) error {
	protoGeneration, err := generationToProto(generation)
	if err != nil {
		return err
	}

	if maxPayload := c.config.GenerationExport.PayloadMaxBytes; maxPayload > 0 {
		if payloadSize := proto.Size(protoGeneration); payloadSize > maxPayload {
			return fmt.Errorf("generation payload exceeds max bytes (%d > %d)", payloadSize, maxPayload)
		}
	}

	c.queueMu.RLock()
	defer c.queueMu.RUnlock()

	if c.shutdown {
		return ErrClientShutdown
	}

	select {
	case c.queue <- queuedGeneration{generation: protoGeneration}:
		return nil
	default:
		return ErrQueueFull
	}
}

func (c *Client) enqueueWorkflowStep(step WorkflowStep) error {
	protoStep, err := workflowStepToProto(step)
	if err != nil {
		return err
	}

	if maxPayload := c.config.GenerationExport.PayloadMaxBytes; maxPayload > 0 {
		if payloadSize := proto.Size(protoStep); payloadSize > maxPayload {
			return fmt.Errorf("workflow step payload exceeds max bytes (%d > %d)", payloadSize, maxPayload)
		}
	}

	c.queueMu.RLock()
	defer c.queueMu.RUnlock()

	if c.shutdown {
		return ErrClientShutdown
	}

	select {
	case c.workflowStepQueue <- queuedWorkflowStep{workflowStep: protoStep}:
		return nil
	default:
		return ErrWorkflowStepQueueFull
	}
}

func (c *Client) Flush(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.queueMu.RLock()
	shuttingDown := c.shutdown
	c.queueMu.RUnlock()
	if shuttingDown {
		return ErrClientShutdown
	}

	ack := make(chan error, 1)
	select {
	case c.flushReq <- ack:
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case err := <-ack:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) Shutdown(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	var shutdownErr error
	c.shutdownOnce.Do(func() {
		c.queueMu.Lock()
		c.shutdown = true
		close(c.queue)
		close(c.workflowStepQueue)
		c.queueMu.Unlock()

		select {
		case <-c.workerDone:
		case <-ctx.Done():
			shutdownErr = errors.Join(shutdownErr, ctx.Err())
			return
		}

		if err := c.exporter.Shutdown(ctx); err != nil {
			shutdownErr = errors.Join(shutdownErr, err)
		}
	})

	return shutdownErr
}

func (c *Client) logf(format string, args ...any) {
	if c == nil || c.config.Logger == nil {
		return
	}
	c.config.Logger.Printf(format, args...)
}
