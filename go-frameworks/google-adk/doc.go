// Package googleadk maps Google ADK lifecycle events to agento11y generation/tool recorders.
//
// The adapter is conversation-first and keeps run/thread/event IDs as optional
// metadata for tracing and generation analysis.
//
// NewCallbacks provides one-time function-based lifecycle wiring for runner setup.
// Embedding support is currently exposed as an explicit unsupported capability
// gate because the Google ADK lifecycle surface used here does not provide a
// dedicated embeddings callback.
package googleadk
