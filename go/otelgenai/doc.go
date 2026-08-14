// Package otelgenai instruments GenAI client calls with the OpenTelemetry
// GenAI semantic conventions. It emits one span, the client metric
// instruments, and optional operation-details log records. The conventions'
// capture setting gates message content.
//
// The package has no agento11y dependency and no vendor behavior of its own.
// An instrumentation that needs vendor attributes installs a CompletionHook,
// which sees the typed invocation before the span closes and may transform
// the invocation's content.
//
//	handler := otelgenai.NewHandler()
//	inv := &otelgenai.Invocation{
//		Provider:     "openai",
//		RequestModel: "gpt-5",
//		StartedAt:    time.Now(),
//	}
//	ctx = handler.Start(ctx, inv)
//	// ... call the provider, fill inv's response fields ...
//	inv.CompletedAt = time.Now()
//	handler.End(ctx, inv)
//
// The conventions are pinned at v1.41.0, the last version before the core
// semconv module deprecated the gen_ai.* namespace.
package otelgenai
