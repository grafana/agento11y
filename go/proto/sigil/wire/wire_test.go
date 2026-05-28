package wire_test

import (
	"strings"
	"testing"

	sigilv1 "github.com/grafana/sigil-sdk/go/proto/sigil/v1"
	"github.com/grafana/sigil-sdk/go/proto/sigil/wire"
	"google.golang.org/protobuf/proto"
)

func TestNormalizeGenerationExportURL(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		insecure bool
		want     string
		wantErr  string
	}{
		{
			name:     "empty endpoint",
			endpoint: "",
			wantErr:  "endpoint is required",
		},
		{
			name:     "https host without path",
			endpoint: "https://sigil.example.com",
			want:     "https://sigil.example.com" + wire.GenerationExportHTTPPath,
		},
		{
			name:     "https host with trailing slash",
			endpoint: "https://sigil.example.com/",
			want:     "https://sigil.example.com" + wire.GenerationExportHTTPPath,
		},
		{
			name:     "preserves existing path",
			endpoint: "https://sigil.example.com/custom/path",
			want:     "https://sigil.example.com/custom/path",
		},
		{
			name:     "scheme-less defaults to https",
			endpoint: "sigil.example.com",
			want:     "https://sigil.example.com" + wire.GenerationExportHTTPPath,
		},
		{
			name:     "scheme-less with insecure picks http",
			endpoint: "sigil.example.com",
			insecure: true,
			want:     "http://sigil.example.com" + wire.GenerationExportHTTPPath,
		},
		{
			name:     "missing host",
			endpoint: "https://",
			wantErr:  "empty host",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := wire.NormalizeGenerationExportURL(tc.endpoint, tc.insecure)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("want %q, got %q", tc.want, got)
			}
		})
	}
}

func TestJSONRoundTripUsesProtoNames(t *testing.T) {
	req := &sigilv1.ExportGenerationsRequest{
		Generations: []*sigilv1.Generation{{
			Id:             "gen-1",
			ConversationId: "conv-1",
			AgentName:      "test-agent",
		}},
	}

	data, err := wire.MarshalExportGenerationsJSON(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	encoded := string(data)
	for _, name := range []string{"conversation_id", "agent_name"} {
		if !strings.Contains(encoded, name) {
			t.Errorf("expected proto field %q in JSON payload, got %s", name, encoded)
		}
	}
	if strings.Contains(encoded, "conversationId") || strings.Contains(encoded, "agentName") {
		t.Errorf("expected snake_case field names, got camelCase: %s", encoded)
	}

	decoded, err := wire.UnmarshalExportGenerationsJSON(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !proto.Equal(decoded, req) {
		t.Fatalf("round-trip mismatch: want %v, got %v", req, decoded)
	}
}

func TestJSONResponseRoundTrip(t *testing.T) {
	resp := &sigilv1.ExportGenerationsResponse{
		Results: []*sigilv1.ExportGenerationResult{
			{GenerationId: "gen-1", Accepted: true},
			{GenerationId: "gen-2", Accepted: false, Error: "bad payload"},
		},
	}
	data, err := wire.MarshalExportGenerationsResponseJSON(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), "generation_id") {
		t.Fatalf("expected proto field generation_id in payload, got %s", data)
	}
	decoded, err := wire.UnmarshalExportGenerationsResponseJSON(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !proto.Equal(decoded, resp) {
		t.Fatalf("round-trip mismatch: want %v, got %v", resp, decoded)
	}
}

func TestProtoRoundTrip(t *testing.T) {
	req := &sigilv1.ExportGenerationsRequest{
		Generations: []*sigilv1.Generation{{
			Id:             "gen-bin",
			ConversationId: "conv-bin",
		}},
	}
	data, err := wire.MarshalExportGenerationsProto(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded, err := wire.UnmarshalExportGenerationsProto(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !proto.Equal(decoded, req) {
		t.Fatalf("round-trip mismatch: want %v, got %v", req, decoded)
	}

	resp := &sigilv1.ExportGenerationsResponse{
		Results: []*sigilv1.ExportGenerationResult{{GenerationId: "gen-bin", Accepted: true}},
	}
	respData, err := wire.MarshalExportGenerationsResponseProto(resp)
	if err != nil {
		t.Fatalf("marshal resp: %v", err)
	}
	decodedResp, err := wire.UnmarshalExportGenerationsResponseProto(respData)
	if err != nil {
		t.Fatalf("unmarshal resp: %v", err)
	}
	if !proto.Equal(decodedResp, resp) {
		t.Fatalf("response round-trip mismatch: want %v, got %v", resp, decodedResp)
	}
}

func TestMarshalNilReturnsError(t *testing.T) {
	cases := []struct {
		name    string
		marshal func() error
		wantErr string
	}{
		{
			name: "request JSON marshal",
			marshal: func() error {
				_, err := wire.MarshalExportGenerationsJSON(nil)
				return err
			},
			wantErr: "nil ExportGenerationsRequest",
		},
		{
			name: "request proto marshal",
			marshal: func() error {
				_, err := wire.MarshalExportGenerationsProto(nil)
				return err
			},
			wantErr: "nil ExportGenerationsRequest",
		},
		{
			name: "response JSON marshal",
			marshal: func() error {
				_, err := wire.MarshalExportGenerationsResponseJSON(nil)
				return err
			},
			wantErr: "nil ExportGenerationsResponse",
		},
		{
			name: "response proto marshal",
			marshal: func() error {
				_, err := wire.MarshalExportGenerationsResponseProto(nil)
				return err
			},
			wantErr: "nil ExportGenerationsResponse",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.marshal()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestConstants(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{name: "tenant header", got: wire.TenantHeaderName, want: "X-Scope-OrgID"},
		{name: "authorization header", got: wire.AuthorizationHeaderName, want: "Authorization"},
		{name: "generation export path", got: wire.GenerationExportHTTPPath, want: "/api/v1/generations:export"},
		{name: "JSON content type", got: wire.ContentTypeJSON, want: "application/json"},
		{name: "protobuf content type", got: wire.ContentTypeProto, want: "application/x-protobuf"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
			}
		})
	}
}
