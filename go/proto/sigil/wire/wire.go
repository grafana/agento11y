// Package wire exposes the shared HTTP transport constants and encoding
// helpers that the Sigil SDK exporter and other generation producers use to
// talk to a Sigil generation ingest endpoint.
//
// The helpers here intentionally have no dependency on the SDK client. They
// only operate on the generated protobuf types from
// github.com/grafana/sigil-sdk/go/proto/sigil/v1.
package wire

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	sigilv1 "github.com/grafana/sigil-sdk/go/proto/sigil/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	// TenantHeaderName carries the Sigil tenant ID on HTTP and gRPC requests.
	TenantHeaderName = "X-Scope-OrgID"
	// AuthorizationHeaderName carries the bearer or basic auth credential.
	AuthorizationHeaderName = "Authorization"
	// GenerationExportHTTPPath is the HTTP path for ExportGenerations.
	GenerationExportHTTPPath = "/api/v1/generations:export"
	// ContentTypeJSON identifies the protojson body shape.
	ContentTypeJSON = "application/json"
	// ContentTypeProto identifies the binary protobuf body shape.
	ContentTypeProto = "application/x-protobuf"
)

// NormalizeGenerationExportURL returns endpoint with the Sigil generation
// export HTTP path applied when no path is present, mirroring the SDK
// exporter behavior. If endpoint has no scheme, https is assumed unless
// insecure is true (in which case http is used).
func NormalizeGenerationExportURL(endpoint string, insecure bool) (string, error) {
	trimmed := strings.TrimSpace(endpoint)
	if trimmed == "" {
		return "", errors.New("endpoint is required")
	}

	lower := strings.ToLower(trimmed)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		scheme := "https://"
		if insecure {
			scheme = "http://"
		}
		trimmed = scheme + trimmed
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("parse generation export endpoint %q: %w", endpoint, err)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("endpoint %q has empty host", endpoint)
	}
	if parsed.Path == "" || parsed.Path == "/" {
		parsed.Path = GenerationExportHTTPPath
	}
	return parsed.String(), nil
}

// MarshalExportGenerationsJSON marshals an ExportGenerationsRequest with
// proto field names, matching the SDK's HTTP wire format.
func MarshalExportGenerationsJSON(req *sigilv1.ExportGenerationsRequest) ([]byte, error) {
	if req == nil {
		return nil, errors.New("nil ExportGenerationsRequest")
	}
	return protojson.MarshalOptions{UseProtoNames: true}.Marshal(req)
}

// UnmarshalExportGenerationsJSON decodes a protojson-encoded
// ExportGenerationsRequest produced by MarshalExportGenerationsJSON or the
// SDK exporter.
func UnmarshalExportGenerationsJSON(data []byte) (*sigilv1.ExportGenerationsRequest, error) {
	var req sigilv1.ExportGenerationsRequest
	if err := protojson.Unmarshal(data, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

// MarshalExportGenerationsResponseJSON marshals an ExportGenerationsResponse
// with proto field names.
func MarshalExportGenerationsResponseJSON(resp *sigilv1.ExportGenerationsResponse) ([]byte, error) {
	if resp == nil {
		return nil, errors.New("nil ExportGenerationsResponse")
	}
	return protojson.MarshalOptions{UseProtoNames: true}.Marshal(resp)
}

// UnmarshalExportGenerationsResponseJSON decodes an HTTP response body into
// an ExportGenerationsResponse.
func UnmarshalExportGenerationsResponseJSON(data []byte) (*sigilv1.ExportGenerationsResponse, error) {
	var resp sigilv1.ExportGenerationsResponse
	if err := protojson.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// MarshalExportGenerationsProto marshals an ExportGenerationsRequest using
// the binary protobuf encoding for application/x-protobuf clients.
func MarshalExportGenerationsProto(req *sigilv1.ExportGenerationsRequest) ([]byte, error) {
	if req == nil {
		return nil, errors.New("nil ExportGenerationsRequest")
	}
	return proto.Marshal(req)
}

// UnmarshalExportGenerationsProto decodes a binary protobuf payload into an
// ExportGenerationsRequest.
func UnmarshalExportGenerationsProto(data []byte) (*sigilv1.ExportGenerationsRequest, error) {
	var req sigilv1.ExportGenerationsRequest
	if err := proto.Unmarshal(data, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

// MarshalExportGenerationsResponseProto marshals an ExportGenerationsResponse
// with the binary protobuf encoding.
func MarshalExportGenerationsResponseProto(resp *sigilv1.ExportGenerationsResponse) ([]byte, error) {
	if resp == nil {
		return nil, errors.New("nil ExportGenerationsResponse")
	}
	return proto.Marshal(resp)
}

// UnmarshalExportGenerationsResponseProto decodes a binary protobuf response.
func UnmarshalExportGenerationsResponseProto(data []byte) (*sigilv1.ExportGenerationsResponse, error) {
	var resp sigilv1.ExportGenerationsResponse
	if err := proto.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
