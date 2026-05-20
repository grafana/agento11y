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
