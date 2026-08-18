// Package otelgenai instruments GenAI client calls with the OpenTelemetry
// GenAI semantic conventions. It emits one span, the client metric
// instruments, and optional operation-details log records. Message content is
// off unless an instrumentation asks for it with WithCaptureMode, or the
// environment asks for it and opts into the experimental GenAI signals; see
// EnvCaptureMessageContent and EnvSemconvStabilityOptIn.
//
// The package has no agento11y dependency and no vendor behavior of its own.
// An instrumentation that needs vendor attributes installs an EndHook, which
// sees the typed invocation before the span closes and may transform the
// invocation's content.
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
// The generated attributes come from semconv v1.41.0, the last core release
// with Go bindings for gen_ai.*. v1.42.0 dropped the namespace from the core
// registry, because the GenAI conventions moved to
// open-telemetry/semantic-conventions-genai, which generates no Go package.
// The names v1.41.0 defines are still current; attributes that registry added
// after the split are declared by hand in genai.go.
package otelgenai
