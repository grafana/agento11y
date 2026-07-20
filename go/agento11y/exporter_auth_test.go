package agento11y

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace/noop"
)

func base64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func TestResolveHeadersWithAuth(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		auth    AuthConfig
		want    map[string]string
	}{
		{
			name: "tenant mode adds X-Scope-OrgID",
			auth: AuthConfig{Mode: ExportAuthModeTenant, TenantID: "tenant-a"},
			want: map[string]string{tenantHeaderName: "tenant-a"},
		},
		{
			name: "bearer mode adds Authorization",
			auth: AuthConfig{Mode: ExportAuthModeBearer, BearerToken: "token-123"},
			want: map[string]string{authorizationHeaderName: "Bearer token-123"},
		},
		{
			name: "basic mode derives user from tenant_id",
			auth: AuthConfig{Mode: ExportAuthModeBasic, TenantID: "42", BasicPassword: "secret"},
			want: map[string]string{
				authorizationHeaderName: "Basic " + base64Encode("42:secret"),
				tenantHeaderName:        "42",
			},
		},
		{
			name: "basic mode uses explicit basic_user over tenant_id",
			auth: AuthConfig{Mode: ExportAuthModeBasic, TenantID: "42", BasicUser: "probe-user", BasicPassword: "secret"},
			want: map[string]string{
				authorizationHeaderName: "Basic " + base64Encode("probe-user:secret"),
				tenantHeaderName:        "42",
			},
		},
		{
			name: "tenant mode preserves explicit override header",
			headers: map[string]string{
				"x-scope-orgid": "tenant-override",
				"authorization": "Bearer override-token",
			},
			auth: AuthConfig{Mode: ExportAuthModeTenant, TenantID: "tenant-a"},
			want: map[string]string{
				"x-scope-orgid": "tenant-override",
				"authorization": "Bearer override-token",
			},
		},
		{
			name:    "bearer mode preserves explicit Authorization",
			headers: map[string]string{"authorization": "Bearer override-token"},
			auth:    AuthConfig{Mode: ExportAuthModeBearer, BearerToken: "token-123"},
			want:    map[string]string{"authorization": "Bearer override-token"},
		},
		{
			name: "basic mode preserves explicit headers",
			headers: map[string]string{
				"Authorization": "Basic override",
				"X-Scope-OrgID": "override-tenant",
			},
			auth: AuthConfig{Mode: ExportAuthModeBasic, TenantID: "42", BasicPassword: "secret"},
			want: map[string]string{
				"Authorization": "Basic override",
				"X-Scope-OrgID": "override-tenant",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveHeadersWithAuth(tc.headers, tc.auth)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("got[%q]=%q want %q", k, got[k], v)
				}
			}
		})
	}
}

// resolveHeadersWithAuth rejects only "mode requires X but X missing" cases.
// Mode-irrelevant fields (e.g. tenantId on a bearer-mode config) are silently
// ignored — env layering can populate any field independently of mode, and
// the strict cross-mode rejection only added cleanup work without preventing
// any real bug.
func TestResolveHeadersWithAuthRejectsMissingRequiredField(t *testing.T) {
	testCases := []AuthConfig{
		{Mode: ExportAuthModeTenant},                            // tenant requires tenant_id
		{Mode: ExportAuthModeBearer},                            // bearer requires bearer_token
		{Mode: ExportAuthModeBasic},                             // basic requires password
		{Mode: ExportAuthModeBasic, BasicPassword: "secret"},    // basic also requires user/tenant
		{Mode: ExportAuthMode("unknown"), TenantID: "tenant-a"}, // unknown mode
	}

	for _, testCase := range testCases {
		_, err := resolveHeadersWithAuth(nil, testCase)
		if err == nil {
			t.Fatalf("expected error for auth config: %+v", testCase)
		}
	}
}

// Mode-irrelevant fields are tolerated: callers can pass them without an
// error, the unused fields just have no effect on the resulting headers.
func TestResolveHeadersWithAuthTolerantOfIrrelevantFields(t *testing.T) {
	testCases := []AuthConfig{
		{Mode: ExportAuthModeNone, TenantID: "tenant-a"},
		{Mode: ExportAuthModeNone, BearerToken: "token"},
		{Mode: ExportAuthModeNone, BasicPassword: "secret"},
		{Mode: ExportAuthModeTenant, TenantID: "tenant-a", BearerToken: "token"},
		{Mode: ExportAuthModeBearer, TenantID: "tenant-a", BearerToken: "token"},
	}

	for _, testCase := range testCases {
		if _, err := resolveHeadersWithAuth(nil, testCase); err != nil {
			t.Errorf("unexpected error for %+v: %v", testCase, err)
		}
	}
}

func TestMergeAuthConfigBasicFields(t *testing.T) {
	base := AuthConfig{
		Mode:     ExportAuthModeBearer,
		TenantID: "base-tenant",
	}
	override := AuthConfig{
		Mode:          ExportAuthModeBasic,
		TenantID:      "override-tenant",
		BasicUser:     "probe-user",
		BasicPassword: "secret",
	}
	got := mergeAuthConfig(base, override)

	if got.Mode != ExportAuthModeBasic {
		t.Fatalf("Mode=%q, want %q", got.Mode, ExportAuthModeBasic)
	}
	if got.TenantID != "override-tenant" {
		t.Fatalf("TenantID=%q, want %q", got.TenantID, "override-tenant")
	}
	if got.BasicUser != "probe-user" {
		t.Fatalf("BasicUser=%q, want %q", got.BasicUser, "probe-user")
	}
	if got.BasicPassword != "secret" {
		t.Fatalf("BasicPassword=%q, want %q", got.BasicPassword, "secret")
	}
}

func TestMergeAuthConfigPreservesBaseBasicFields(t *testing.T) {
	base := AuthConfig{
		Mode:          ExportAuthModeBasic,
		BasicUser:     "base-user",
		BasicPassword: "base-secret",
	}
	override := AuthConfig{}
	got := mergeAuthConfig(base, override)

	if got.BasicUser != "base-user" {
		t.Fatalf("BasicUser=%q, want %q", got.BasicUser, "base-user")
	}
	if got.BasicPassword != "base-secret" {
		t.Fatalf("BasicPassword=%q, want %q", got.BasicPassword, "base-secret")
	}
}

// TestNewClientPanicsOnInvalidAuthConfig: a caller-supplied auth config with a
// mode but no required credential is a programming error. NewClient panics so
// the caller notices. Env-induced auth errors are handled separately by
// mode-clearing in mergeAuthConfig and don't reach this path.
func TestNewClientPanicsOnInvalidAuthConfig(t *testing.T) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatalf("expected panic for invalid auth config")
		}
		msg, _ := recovered.(string)
		if !strings.Contains(msg, "invalid generation auth config") {
			t.Fatalf("unexpected panic message: %v", recovered)
		}
	}()

	_ = NewClient(Config{
		Tracer: noop.NewTracerProvider().Tracer("test"),
		GenerationExport: GenerationExportConfig{
			Protocol:        GenerationExportProtocolHTTP,
			Endpoint:        "http://localhost:8080/api/v1/generations:export",
			Auth:            AuthConfig{Mode: ExportAuthModeTenant},
			BatchSize:       1,
			FlushInterval:   time.Second,
			QueueSize:       1,
			MaxRetries:      1,
			InitialBackoff:  time.Millisecond,
			MaxBackoff:      2 * time.Millisecond,
			PayloadMaxBytes: 1 << 20,
		},
		testGenerationExporter: &capturingGenerationExporter{},
		testDisableWorker:      true,
		Now:                    time.Now,
	})
}

func TestNewClientAppliesPerExportAuthToGenerationExporter(t *testing.T) {
	client := NewClient(Config{
		Tracer: noop.NewTracerProvider().Tracer("test"),
		GenerationExport: GenerationExportConfig{
			Protocol:        GenerationExportProtocolHTTP,
			Endpoint:        "http://localhost:8080/api/v1/generations:export",
			Auth:            AuthConfig{Mode: ExportAuthModeTenant, TenantID: "tenant-a"},
			BatchSize:       1,
			FlushInterval:   time.Second,
			QueueSize:       1,
			MaxRetries:      1,
			InitialBackoff:  time.Millisecond,
			MaxBackoff:      2 * time.Millisecond,
			PayloadMaxBytes: 1 << 20,
		},
		testGenerationExporter: &capturingGenerationExporter{},
		testDisableWorker:      true,
		Now:                    time.Now,
	})
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	if got := client.config.GenerationExport.Headers[tenantHeaderName]; got != "tenant-a" {
		t.Fatalf("expected generation tenant header tenant-a, got %q", got)
	}
}
