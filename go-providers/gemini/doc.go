// Package gemini maps Google Gemini payloads to agento11y.Generation.
//
// Use FromRequestResponse for non-streaming calls and FromStream for streaming calls.
// The resulting generation keeps request content in Input and model output in Output.
package gemini
