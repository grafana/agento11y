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

	sigilv1 "github.com/grafana/sigil-sdk/go/sigil/internal/gen/sigil/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	tenantHeaderName         = "X-Scope-OrgID"
	authorizationHeaderName  = "Authorization"
	httpGenerationExportPath = "/api/v1/generations:export"
)

type queuedGeneration struct {
	generation *sigilv1.Generation
}

type generationExporter interface {
	Export(ctx context.Context, request *sigilv1.ExportGenerationsRequest) (*sigilv1.ExportGenerationsResponse, error)
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

func (e *noopGenerationExporter) Shutdown(_ context.Context) error {
	return nil
}

type grpcGenerationExporter struct {
	client  sigilv1.GenerationIngestServiceClient
	conn    *grpc.ClientConn
	headers map[string]string
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

	conn, err := grpc.NewClient(
		endpoint,
		grpc.WithTransportCredentials(transportCreds),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallSendMsgSize(maxSendMessageBytes),
			grpc.MaxCallRecvMsgSize(maxReceiveMessageBytes),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("dial generation ingest grpc endpoint %q: %w", endpoint, err)
	}

	return &grpcGenerationExporter{
		client:  sigilv1.NewGenerationIngestServiceClient(conn),
		conn:    conn,
		headers: cloneTags(cfg.Headers),
	}, nil
}

func (e *grpcGenerationExporter) Export(ctx context.Context, request *sigilv1.ExportGenerationsRequest) (*sigilv1.ExportGenerationsResponse, error) {
	if len(e.headers) > 0 {
		ctx = metadata.NewOutgoingContext(ctx, metadata.New(e.headers))
	}
	return e.client.ExportGenerations(ctx, request)
}

func (e *grpcGenerationExporter) Shutdown(_ context.Context) error {
	if e.conn != nil {
		return e.conn.Close()
	}
	return nil
}

type httpGenerationExporter struct {
	endpoint string
	headers  map[string]string
	client   *http.Client
}

func newHTTPGenerationExporter(cfg GenerationExportConfig) (generationExporter, error) {
	trimmed := strings.TrimSpace(cfg.Endpoint)
	if trimmed == "" {
		return nil, errors.New("endpoint is required")
	}

	urlString := trimmed
	lowerPrefix := strings.ToLower(trimmed)
	if !strings.HasPrefix(lowerPrefix, "http://") && !strings.HasPrefix(lowerPrefix, "https://") {
		scheme := "https://"
		if insecureValue(cfg.Insecure) {
			scheme = "http://"
		}
		urlString = scheme + trimmed
	}
	parsed, err := url.Parse(urlString)
	if err != nil {
		return nil, fmt.Errorf("parse generation export endpoint %q: %w", cfg.Endpoint, err)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("endpoint %q has empty host", cfg.Endpoint)
	}
	if parsed.Path == "" || parsed.Path == "/" {
		parsed.Path = httpGenerationExportPath
	}
	urlString = parsed.String()

	return &httpGenerationExporter{
		endpoint: urlString,
		headers:  cloneTags(cfg.Headers),
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}, nil
}

func (e *httpGenerationExporter) Export(ctx context.Context, request *sigilv1.ExportGenerationsRequest) (*sigilv1.ExportGenerationsResponse, error) {
	payload, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("marshal generation request: %w", err)
	}

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return nil, fmt.Errorf("build generation request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
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

	var exportResponse sigilv1.ExportGenerationsResponse
	if err := protojson.Unmarshal(body, &exportResponse); err != nil {
		return nil, fmt.Errorf("unmarshal generation response: %w", err)
	}

	return &exportResponse, nil
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

	batch := make([]*sigilv1.Generation, 0, c.config.GenerationExport.BatchSize)
	flushInterval := c.config.GenerationExport.FlushInterval
	timer := time.NewTimer(flushInterval)
	defer timer.Stop()

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}

		request := &sigilv1.ExportGenerationsRequest{Generations: batch}
		err := c.exportWithRetry(request)
		batch = batch[:0]
		return err
	}

	// pendingErr captures the first failure from an async flush (timer or
	// batch-size triggered) since the last explicit Flush call. It's
	// returned to and cleared by the next flushReq handler so callers who
	// use Flush as a durability checkpoint can detect silent data loss
	// instead of treating an empty post-failure batch as success.
	var pendingErr error
	recordAsyncErr := func(err error) {
		if err == nil || pendingErr != nil {
			return
		}
		pendingErr = err
	}

	for {
		select {
		case queued, ok := <-c.queue:
			if !ok {
				if err := flush(); err != nil {
					c.logf("sigil generation export flush on shutdown failed: %v", err)
				}
				return
			}
			batch = append(batch, queued.generation)
			if len(batch) >= c.config.GenerationExport.BatchSize {
				if err := flush(); err != nil {
					c.logf("sigil generation export failed: %v", err)
					recordAsyncErr(err)
				}
				resetTimer(timer, flushInterval)
			}
		case ack := <-c.flushReq:
			// Drain anything the worker hadn't yet pulled off c.queue so
			// that an explicit Flush sees every generation enqueued before
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
			queueClosed := false
		draining:
			for {
				select {
				case queued, ok := <-c.queue:
					if !ok {
						queueClosed = true
						break draining
					}
					batch = append(batch, queued.generation)
					if len(batch) >= c.config.GenerationExport.BatchSize {
						joinFlush(flush())
					}
				default:
					break draining
				}
			}
			joinFlush(flush())
			if pendingErr != nil {
				joinFlush(pendingErr)
				pendingErr = nil
			}
			ack <- flushErr
			if queueClosed {
				// Shutdown raced with Flush — caller has been acked, exit
				// the worker like the regular shutdown path.
				return
			}
			resetTimer(timer, flushInterval)
		case <-timer.C:
			if err := flush(); err != nil {
				c.logf("sigil generation export failed: %v", err)
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
	for attempt := 0; attempt < attempts; attempt++ {
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

func validateExportResponse(request *sigilv1.ExportGenerationsRequest, response *sigilv1.ExportGenerationsResponse) error {
	if response == nil {
		return &exportValidationError{err: errors.New("nil generation export response"), retryable: true}
	}
	requested := len(request.GetGenerations())
	results := response.GetResults()
	if len(results) != requested {
		return &exportValidationError{err: fmt.Errorf("generation export result count mismatch: requested=%d results=%d", requested, len(results)), retryable: true}
	}
	var malformed []string
	var rejected []string
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
