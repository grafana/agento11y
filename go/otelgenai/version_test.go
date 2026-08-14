package otelgenai_test

import (
	"regexp"
	"testing"

	"github.com/grafana/agento11y/go/otelgenai"
)

func TestVersionIsSemanticVersion(t *testing.T) {
	semanticVersion := regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)
	if version := otelgenai.Version(); !semanticVersion.MatchString(version) {
		t.Fatalf("Version() = %q, want a semantic version", version)
	}
}
