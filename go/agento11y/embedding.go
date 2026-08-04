package agento11y

import (
	"strings"
	"time"
)

const defaultEmbeddingOperationName = "embeddings"

// EmbeddingCaptureConfig controls optional embedding input text capture on spans.
type EmbeddingCaptureConfig struct {
	CaptureInput  bool
	MaxInputItems int
	MaxTextLength int
}

// EmbeddingStart seeds embedding span fields before the provider call executes.
//
// EmbeddingStart has no per-call ContentCapture field; embedding input text
// capture is gated by EmbeddingCaptureConfig.CaptureInput and the effective
// client mode. See [ContentCaptureModeFullWithMetadataSpans].
type EmbeddingStart struct {
	Model          ModelRef
	AgentName      string
	AgentVersion   string
	Dimensions     *int64
	EncodingFormat string
	Tags           map[string]string
	Metadata       map[string]any
	StartedAt      time.Time
}

// EmbeddingResult captures final embedding call fields set after the provider call.
type EmbeddingResult struct {
	InputCount    int
	InputTokens   int64
	InputTexts    []string
	ResponseModel string
	Dimensions    *int64
}

func cloneEmbeddingStart(in EmbeddingStart) EmbeddingStart {
	return EmbeddingStart{
		Model:          cloneModelRef(in.Model),
		AgentName:      strings.Clone(in.AgentName),
		AgentVersion:   strings.Clone(in.AgentVersion),
		Dimensions:     cloneInt64Ptr(in.Dimensions),
		EncodingFormat: strings.Clone(in.EncodingFormat),
		Tags:           cloneTags(in.Tags),
		Metadata:       cloneMetadata(in.Metadata),
		StartedAt:      in.StartedAt,
	}
}

func cloneEmbeddingResult(in EmbeddingResult) EmbeddingResult {
	return EmbeddingResult{
		InputCount:    in.InputCount,
		InputTokens:   in.InputTokens,
		InputTexts:    cloneStringSlice(in.InputTexts),
		ResponseModel: strings.Clone(in.ResponseModel),
		Dimensions:    cloneInt64Ptr(in.Dimensions),
	}
}

func cloneStringSlice(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i := range in {
		out[i] = strings.Clone(in[i])
	}
	return out
}
