package otelgenai_test

import (
	"os"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
)

type otelErrorSink struct {
	mu     sync.Mutex
	errors []error
}

func (s *otelErrorSink) Handle(err error) {
	s.mu.Lock()
	s.errors = append(s.errors, err)
	s.mu.Unlock()
}

func (s *otelErrorSink) reset() {
	s.mu.Lock()
	s.errors = nil
	s.mu.Unlock()
}

func (s *otelErrorSink) read() []error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]error(nil), s.errors...)
}

var testOTelErrors otelErrorSink

func TestMain(m *testing.M) {
	otel.SetErrorHandler(&testOTelErrors)
	os.Exit(m.Run())
}
