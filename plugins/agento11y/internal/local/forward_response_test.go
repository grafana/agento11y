package local

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/grafana/agento11y/go/proto/agento11y/wire"
	"github.com/stretchr/testify/require"
)

func TestGenerationForwardAcknowledgements(t *testing.T) {
	payload := []byte(`{"generations":[{"id":"one"},{"id":"two"}]}`)
	for _, tc := range []struct {
		name, response string
		ok             bool
	}{
		{name: "accepted", response: `{"results":[{"generation_id":"one","accepted":true},{"generation_id":"two","accepted":true}]}`, ok: true},
		{name: "camel case reversed", response: `{"results":[{"generationId":"two","accepted":true},{"generationId":"one","accepted":true}]}`, ok: true},
		{name: "empty"},
		{name: "malformed", response: `{"secret-token`},
		{name: "missing", response: `{"results":[]}`},
		{name: "rejected", response: `{"results":[{"generation_id":"one","accepted":true},{"generation_id":"two","error":"secret-token"}]}`},
		{name: "wrong identity", response: `{"results":[{"generation_id":"other","accepted":true},{"generation_id":"two","accepted":true}]}`},
		{name: "duplicate identity", response: `{"results":[{"generation_id":"one","accepted":true},{"generation_id":"one","accepted":true}]}`},
		{name: "oversize", response: strings.Repeat("secret-token", 1<<20)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkGenerationForwardResponse(strings.NewReader(tc.response), payload)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.NotContains(t, err.Error(), "secret-token")
			}
		})
	}
}

func TestTelemetryResponseIsolation(t *testing.T) {
	for _, tc := range []struct {
		name, response, contentType, signal string
		status                              int
		ok                                  bool
		transportErr                        error
		wantError                           string
	}{
		{name: "generations", signal: "generations", status: 200, response: `{"results":[{"generation_id":"one","accepted":true}]}`, ok: true},
		{name: "rejection", signal: "generations", status: 200, response: `{"results":[{"generation_id":"one","error":"secret-token"}]}`},
		{name: "hostile failure", signal: "generations", status: 401, response: "secret-token"},
		{name: "redirect", signal: "generations", status: 307},
		{name: "traces", signal: "traces", status: 200, ok: true},
		{name: "trace rejection", signal: "traces", status: 200, contentType: wire.ContentTypeJSON, response: `{"partialSuccess":{"rejectedSpans":"1","errorMessage":"secret-token"}}`},
		{name: "metric rejection", signal: "metrics", status: 200, contentType: wire.ContentTypeJSON, response: `{"partialSuccess":{"rejectedDataPoints":"2","errorMessage":"secret-token"}}`},
		{name: "malformed traces", signal: "traces", status: 200, contentType: wire.ContentTypeJSON, response: "secret-token"},
		{name: "bad descriptor", transportErr: &net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("connect: %w", syscall.EBADF)}, wantError: "telemetry transport failed: bad file descriptor"},
		{name: "connection refused", transportErr: syscall.ECONNREFUSED, wantError: "telemetry transport failed: " + syscall.ECONNREFUSED.Error()},
		{name: "DNS", transportErr: &net.DNSError{Name: "secret-token", Server: "secret-token", Err: "secret-token"}, wantError: "telemetry transport failed: DNS lookup failed"},
		{name: "TLS certificate", transportErr: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, wantError: "telemetry transport failed: TLS certificate verification failed"},
		{name: "TLS protocol", transportErr: tls.RecordHeaderError{Msg: "secret-token"}, wantError: "telemetry transport failed: TLS protocol error"},
		{name: "TLS alert", transportErr: tls.AlertError(40), wantError: "telemetry transport failed: TLS protocol error"},
		{name: "timeout", transportErr: &net.DNSError{IsTimeout: true, Err: "secret-token"}, wantError: "telemetry transport failed: connection timed out"},
		{name: "unknown transport error", transportErr: errors.New("secret-token"), wantError: "telemetry transport failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var redirected atomic.Int32
			sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redirected" {
					redirected.Add(1)
					return
				}
				require.Equal(t, "1", r.Header.Get(ForwardMarkerHeader))
				require.Equal(t, wire.ContentTypeJSON, r.Header.Get("Content-Type"))
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Location", "/redirected")
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.response))
			}))
			defer sink.Close()
			loader := newForwardLoader(filepath.Join(t.TempDir(), "config.env"), nil)
			loader.client = sink.Client()
			if tc.transportErr != nil {
				loader.client.Transport = dispatchRoundTripper(func(*http.Request) (*http.Response, error) {
					return nil, fmt.Errorf("https://secret-token@collector.test/?token=secret-token: %w", tc.transportErr)
				})
			}
			require.Nil(t, loader.client.CheckRedirect)
			err := loader.sendTelemetry(context.Background(), sink.URL, wire.ContentTypeJSON, "", map[string]string{ForwardMarkerHeader: "wrong", "Content-Type": "wrong"}, []byte(`{"generations":[{"id":"one"}]}`), tc.signal)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.NotContains(t, err.Error(), "secret-token")
				if tc.wantError != "" {
					require.EqualError(t, err, tc.wantError)
					require.ErrorIs(t, err, tc.transportErr)
					require.Equal(t, tc.wantError, forwardDispatchErrorCategory(err))
				}
			}
			require.Zero(t, redirected.Load())
			require.Nil(t, loader.client.CheckRedirect)
		})
	}
}

func writeAcceptedGenerationResponse(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Generations []struct {
			ID string `json:"id"`
		} `json:"generations"`
	}
	if json.NewDecoder(r.Body).Decode(&request) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	response := generationsResponse{}
	for _, generation := range request.Generations {
		response.Results = append(response.Results, generationResult{GenerationID: generation.ID, Accepted: true})
	}
	w.Header().Set("Content-Type", wire.ContentTypeJSON)
	_ = json.NewEncoder(w).Encode(response)
}
