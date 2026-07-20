package agento11y

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCoreModuleHasNoAnthropicDependency(t *testing.T) {
	goModPath := filepath.Clean(filepath.Join("..", "go.mod"))
	content, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}

	if bytes.Contains(content, []byte("anthropic-sdk-go")) {
		t.Fatalf("core module must not depend on anthropic sdk")
	}
}
