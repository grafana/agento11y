package otel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestForceFlushReportsAnEarlierAsyncTraceFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/traces") {
			http.Error(w, "denied", http.StatusForbidden)
		}
	}))
	defer server.Close()
	p, err := SetupWithOptions(context.Background(), "test", Options{Endpoint: server.URL, Headers: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()
	_, span := p.Tracer("test").Start(context.Background(), "test")
	span.End()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.traceExport.mu.Lock()
		failed := p.traceExport.err != nil
		p.traceExport.mu.Unlock()
		if failed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background batch did not run")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := p.ForceFlush(); err == nil {
		t.Fatal("background export failed, but ForceFlush claimed success")
	}
	if err := p.ForceFlush(); err != nil {
		t.Fatal("already-reported failure was not cleared")
	}
}
