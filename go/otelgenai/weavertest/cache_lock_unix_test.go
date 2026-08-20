//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package weavertest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStartWeaverWaitsForExitBeforeReadingDiagnostics(t *testing.T) {
	script := filepath.Join(t.TempDir(), "fake-weaver")
	contents := "#!/bin/sh\necho diagnostic >&2\nexec sleep 10\n"
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := startWeaver(ctx, script, Assets{})
	if err == nil || !strings.Contains(err.Error(), "diagnostic") {
		t.Fatalf("startWeaver error = %v, want captured diagnostics", err)
	}
}

func TestCacheLockSerializesCallers(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".provision.lock")
	first, err := acquireCacheLock(context.Background(), path)
	if err != nil {
		t.Fatalf("acquire first lock: %v", err)
	}
	defer first.release()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := acquireCacheLock(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock error = %v, want context deadline exceeded", err)
	}

	first.release()
	second, err := acquireCacheLock(context.Background(), path)
	if err != nil {
		t.Fatalf("acquire lock after release: %v", err)
	}
	second.release()
}
