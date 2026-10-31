package local

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"

	agento11yv1 "github.com/grafana/agento11y/go/proto/agento11y/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

func (l *forwardLoader) sendTelemetry(ctx context.Context, target, contentType, contentEncoding string, headers map[string]string, body []byte, signal string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid telemetry request")
	}
	for key, value := range headers {
		if value != "" {
			req.Header.Set(key, value)
		}
	}
	req.Header.Set(ForwardMarkerHeader, "1")
	req.Header.Set("Content-Type", contentType)
	req.Header.Del("Content-Encoding")
	if contentEncoding != "" {
		req.Header.Set("Content-Encoding", contentEncoding)
	}
	client := *l.client
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return errors.New("telemetry request canceled or timed out")
		}
		return &telemetryTransportError{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("telemetry HTTP status %d", resp.StatusCode)
	}
	if signal == forwardLabelGenerations {
		return checkGenerationForwardResponse(resp.Body, body)
	}
	responseType := resp.Header.Get("Content-Type")
	if responseType == "" {
		responseType = contentType
	}
	return checkOTLPForwardResponse(resp.Body, responseType, signal)
}

type telemetryTransportError struct {
	err error
}

func (e *telemetryTransportError) Unwrap() error { return e.err }

func (e *telemetryTransportError) Error() string {
	for cause := e.err; cause != nil; cause = errors.Unwrap(cause) {
		if networkError, ok := cause.(net.Error); ok && networkError.Timeout() {
			return "telemetry transport failed: connection timed out"
		}
	}
	var dnsError *net.DNSError
	var certificateError *tls.CertificateVerificationError
	var recordError tls.RecordHeaderError
	var alert tls.AlertError
	var errno syscall.Errno
	var reason string
	switch {
	case errors.As(e.err, &dnsError):
		reason = "DNS lookup failed"
	case errors.As(e.err, &certificateError):
		reason = "TLS certificate verification failed"
	case errors.As(e.err, &recordError), errors.As(e.err, &alert):
		reason = "TLS protocol error"
	case errors.As(e.err, &errno):
		reason = errno.Error()
	default:
		return "telemetry transport failed"
	}
	return "telemetry transport failed: " + reason
}

func checkGenerationForwardResponse(body io.Reader, payload []byte) error {
	const limit = 1 << 20
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil || len(data) > limit {
		return errors.New("generation acknowledgement could not be read or exceeds 1 MiB")
	}
	var response agento11yv1.ExportGenerationsResponse
	if err := protojson.Unmarshal(data, &response); err != nil {
		return errors.New("invalid generation acknowledgement")
	}
	var request struct {
		Generations []struct {
			ID string `json:"id"`
		} `json:"generations"`
	}
	if err := json.Unmarshal(payload, &request); err != nil {
		return errors.New("invalid generation payload")
	}
	if len(response.GetResults()) != len(request.Generations) {
		return errors.New("generation acknowledgement result count mismatch")
	}
	pending := make(map[string]int, len(request.Generations))
	for _, generation := range request.Generations {
		pending[generation.ID]++
	}
	for _, result := range response.GetResults() {
		if pending[result.GetGenerationId()] == 0 {
			return errors.New("generation acknowledgement identity mismatch")
		}
		pending[result.GetGenerationId()]--
		if !result.GetAccepted() || result.GetError() != "" {
			return errors.New("generation acknowledgement rejected records")
		}
	}
	return nil
}
