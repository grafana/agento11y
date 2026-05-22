package config

import (
	"bytes"
	"log"
	"testing"

	"github.com/grafana/sigil-sdk/go/sigil"
)

func TestLoad_DefaultsContentCaptureToMetadataOnly(t *testing.T) {
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "")
	cfg := Load(log.New(&bytes.Buffer{}, "", 0))
	if cfg.ContentCapture != sigil.ContentCaptureModeMetadataOnly {
		t.Fatalf("ContentCapture = %v, want metadata_only", cfg.ContentCapture)
	}
}

func TestLoad_InvalidContentCaptureFailsClosed(t *testing.T) {
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "surprise")
	cfg := Load(log.New(&bytes.Buffer{}, "", 0))
	if cfg.ContentCapture != sigil.ContentCaptureModeMetadataOnly {
		t.Fatalf("ContentCapture = %v, want metadata_only", cfg.ContentCapture)
	}
}

func TestLoad_Guards(t *testing.T) {
	tests := []struct {
		name          string
		env           map[string]string
		wantEnabled   bool
		wantFailOpen  bool
		wantTimeoutMs int
	}{
		{
			name:          "defaults are disabled fail-open 1500ms",
			env:           map[string]string{},
			wantEnabled:   false,
			wantFailOpen:  true,
			wantTimeoutMs: 1500,
		},
		{
			name: "enabled via env",
			env: map[string]string{
				"SIGIL_GUARDS_ENABLED": "true",
			},
			wantEnabled:   true,
			wantFailOpen:  true,
			wantTimeoutMs: 1500,
		},
		{
			name: "fail-closed via env",
			env: map[string]string{
				"SIGIL_GUARDS_ENABLED":   "true",
				"SIGIL_GUARDS_FAIL_OPEN": "false",
			},
			wantEnabled:   true,
			wantFailOpen:  false,
			wantTimeoutMs: 1500,
		},
		{
			name: "timeout override",
			env: map[string]string{
				"SIGIL_GUARDS_ENABLED":    "true",
				"SIGIL_GUARDS_TIMEOUT_MS": "750",
			},
			wantEnabled:   true,
			wantFailOpen:  true,
			wantTimeoutMs: 750,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SIGIL_GUARDS_ENABLED", "")
			t.Setenv("SIGIL_GUARDS_FAIL_OPEN", "")
			t.Setenv("SIGIL_GUARDS_TIMEOUT_MS", "")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			cfg := Load(log.New(&bytes.Buffer{}, "", 0))
			if cfg.Guards.Enabled != tt.wantEnabled {
				t.Errorf("Guards.Enabled = %t, want %t", cfg.Guards.Enabled, tt.wantEnabled)
			}
			if cfg.Guards.FailOpen != tt.wantFailOpen {
				t.Errorf("Guards.FailOpen = %t, want %t", cfg.Guards.FailOpen, tt.wantFailOpen)
			}
			if cfg.Guards.TimeoutMs != tt.wantTimeoutMs {
				t.Errorf("Guards.TimeoutMs = %d, want %d", cfg.Guards.TimeoutMs, tt.wantTimeoutMs)
			}
		})
	}
}
