// Package anthropic maps Anthropic message payloads to agento11y.Generation.
//
// Use FromRequestResponse for non-streaming calls and FromStream for streaming
// calls. The resulting generation keeps request content in Input and model
// output in Output.
//
// This package currently supports Anthropic Messages APIs only. Call
// CheckEmbeddingsSupport before wiring embedding-specific flows; the official
// Anthropic SDK/API surface used by this module does not expose a native
// embeddings endpoint.
package anthropic
