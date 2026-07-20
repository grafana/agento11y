package emit

import (
	"testing"
	"time"
)

func TestExportEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		want     string
	}{
		{"no trailing slash", "http://localhost:8080", "http://localhost:8080/api/v1/generations:export"},
		{"trailing slash trimmed", "http://localhost:8080/", "http://localhost:8080/api/v1/generations:export"},
		{"multiple trailing slashes trimmed", "http://localhost:8080///", "http://localhost:8080/api/v1/generations:export"},
		{"empty endpoint", "", "/api/v1/generations:export"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SIGIL_ENDPOINT", tc.endpoint)
			if got := ExportEndpoint(); got != tc.want {
				t.Fatalf("ExportEndpoint() = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestExportConfig(t *testing.T) {
	t.Setenv("SIGIL_ENDPOINT", "http://localhost:8080")

	t.Run("empty user agent leaves headers unset", func(t *testing.T) {
		if got := exportConfig("").Headers; got != nil {
			t.Fatalf("Headers = %v; want nil", got)
		}
	})

	t.Run("user agent sets header", func(t *testing.T) {
		got := exportConfig("agento11y-plugin-codex/1.2.3").Headers["User-Agent"]
		if got != "agento11y-plugin-codex/1.2.3" {
			t.Fatalf("User-Agent = %q; want %q", got, "agento11y-plugin-codex/1.2.3")
		}
	})
}

func TestNewClientUsesSDKEnvResolution(t *testing.T) {
	t.Setenv("SIGIL_ENDPOINT", "http://localhost:8080")
	// The adapters leave basic-auth credentials to the SDK's env resolution; the
	// SDK validates that basic_password is present, so provide them.
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")
	client := NewClient(ClientOptions{InstrumentationName: "sigil.test"})
	if client == nil {
		t.Fatal("NewClient returned nil")
	}
	_ = client.Shutdown(t.Context())
}

func TestToolSpanWindow(t *testing.T) {
	genEnd := time.Date(2026, 4, 28, 12, 0, 30, 0, time.UTC)
	dur := func(ms float64) *float64 { return &ms }

	cases := []struct {
		name          string
		completedAt   string
		duration      *float64
		wantStarted   time.Time
		wantCompleted time.Time
	}{
		{
			name:          "completedAt minus duration",
			completedAt:   "2026-04-28T12:00:10.500Z",
			duration:      dur(2500),
			wantStarted:   time.Date(2026, 4, 28, 12, 0, 8, 0, time.UTC),
			wantCompleted: time.Date(2026, 4, 28, 12, 0, 10, 500_000_000, time.UTC),
		},
		{
			name:          "no duration → started equals completed",
			completedAt:   "2026-04-28T12:00:10Z",
			duration:      nil,
			wantStarted:   time.Date(2026, 4, 28, 12, 0, 10, 0, time.UTC),
			wantCompleted: time.Date(2026, 4, 28, 12, 0, 10, 0, time.UTC),
		},
		{
			name:          "missing completedAt falls back to genCompletedAt",
			completedAt:   "",
			duration:      dur(1000),
			wantStarted:   genEnd.Add(-1000 * time.Millisecond),
			wantCompleted: genEnd,
		},
		{
			name:          "unparseable completedAt falls back to genCompletedAt",
			completedAt:   "not-a-timestamp",
			duration:      dur(500),
			wantStarted:   genEnd.Add(-500 * time.Millisecond),
			wantCompleted: genEnd,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotStart, gotEnd := ToolSpanWindow(tc.completedAt, tc.duration, genEnd)
			if !gotStart.Equal(tc.wantStarted) {
				t.Errorf("startedAt = %s; want %s", gotStart, tc.wantStarted)
			}
			if !gotEnd.Equal(tc.wantCompleted) {
				t.Errorf("completedAt = %s; want %s", gotEnd, tc.wantCompleted)
			}
		})
	}
}

func TestToolError(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want string
	}{
		{"empty message → sentinel", "", "tool returned error"},
		{"non-empty message preserved", "boom", "boom"},
		{"whitespace message preserved unchanged", "   ", "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ToolError(tc.msg).Error(); got != tc.want {
				t.Errorf("ToolError(%q) = %q; want %q", tc.msg, got, tc.want)
			}
		})
	}
}
