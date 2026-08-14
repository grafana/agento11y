// Package otelhook is the agento11y extension of the otelgenai util. It
// implements otelgenai.CompletionHook, so a generation span produced by the
// util carries the agento11y.* attributes the Agent Observability backend
// decodes: the generation id that selects the strict wire-format mapper, the
// tags, and the fields the semantic conventions have no slot for.
//
// Without the hook, otelgenai emits a plain gen_ai span that any OTel backend
// can read; with the hook, the same span decodes into an agento11y Generation.
//
// Message content reaches the hook already sanitized: the SDK runs
// Config.GenerationSanitizer over the generation before the SDK builds the
// invocation, so the hook adds attributes rather than re-running redaction.
// The hook decides content capture on its own, and it emits the attributes
// below that carry provider or user text only when the invocation's capture
// mode puts content on the span.
package otelhook

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/grafana/agento11y/go/agento11y/contentcapture"
	"github.com/grafana/agento11y/go/otelgenai"
)

// Attribute keys. The agento11y.* namespace is the extension namespace the
// backend's strict decoder reads; gen_ai.token.semantics and user.id are
// shared keys the conventions do not define for GenAI.
const (
	AttrGenerationID        = "agento11y.generation.id"
	AttrParentGenerationIDs = "agento11y.generation.parent_generation_ids"
	AttrTags                = "agento11y.generation.tags"
	AttrMetadata            = "agento11y.generation.metadata"
	// AttrRawArtifacts resolves through contentcapture, which classifies the
	// key as content: the forwarder strips this key by that classification, and
	// a second spelling here would let this key and the forwarder's key drift
	// apart.
	AttrRawArtifacts     = contentcapture.RawArtifactsAttributeKey
	AttrEffectiveVersion = "agento11y.agent.effective_version"
	AttrToolChoice       = "agento11y.gen_ai.request.tool_choice"
	AttrThinkingEnabled  = "agento11y.gen_ai.request.thinking.enabled"
	AttrUsageTotalTokens = "agento11y.gen_ai.usage.total_tokens"
	AttrTokenSemantics   = "gen_ai.token.semantics"
	AttrUserID           = "user.id"
	// AttrTagPrefix is the per-tag dimension prefix the SDK already emits on
	// spans and metrics. The hook emits these dimensions next to the tags JSON
	// document, so existing trace filters keep working in otel mode.
	AttrTagPrefix = "agento11y.tag."

	// TokenSemanticsInclusive marks usage whose input_tokens already includes
	// both cache buckets, per the OTel GenAI contract.
	TokenSemanticsInclusive = "inclusive"
)

// contentMetadataKeys are the metadata keys the SDK mirrors content into. The
// hook exports the metadata document in every capture mode, because user
// metadata is not content by the SDK's policy. The mirrored values are
// content: two of these keys hold the conversation title, and the third holds
// the provider's raw error text.
var contentMetadataKeys = []string{
	contentcapture.ConversationTitleKey,
	contentcapture.LegacyConversationTitleKey,
	contentcapture.MetadataKeyCallError,
}

// Artifact is one raw provider payload attached to a generation. It matches
// the backend's raw-artifacts wire shape, which is where the base64 payload
// encoding comes from.
type Artifact struct {
	Kind        string
	Name        string
	ContentType string
	Payload     []byte
	RecordID    string
	URI         string
}

// Generation is the agento11y data an invocation carries for the hook. The
// SDK attaches it as otelgenai.Invocation.Vendor; a caller driving otelgenai
// directly can do the same.
//
// The fields are the ones the conventions cannot express. Everything the
// conventions do define (model, usage, messages, finish reasons) stays on the
// invocation itself.
type Generation struct {
	ID     string
	UserID string
	// ConversationTitle rides in the metadata document, which is the only
	// slot the wire format has for it. It carries user text, so it is
	// content.
	ConversationTitle   string
	Tags                map[string]string
	Metadata            map[string]any
	Artifacts           []Artifact
	ParentGenerationIDs []string
	// EffectiveVersion is the sha256:<hex> digest, not the raw version: the
	// backend stores the attribute value as it arrives, and the proprietary
	// transport sends the digest.
	EffectiveVersion string
	ToolChoice       *string
	ThinkingEnabled  *bool
	TotalTokens      int64
	// InclusiveTokenSemantics marks that Usage.InputTokens on the invocation
	// already includes both cache buckets.
	InclusiveTokenSemantics bool
}

// Hook implements otelgenai.CompletionHook.
type Hook struct{}

// New returns the agento11y completion hook.
func New() *Hook { return &Hook{} }

// OnCompletion returns the agento11y attributes for the invocation. An
// invocation whose Vendor is not a Generation gets no attributes, so a span
// from unrelated instrumentation carries no generation id.
//
// capture decides the content-bearing attributes: the raw artifacts, and the
// metadata keys that mirror the conversation title and the provider error.
func (h *Hook) OnCompletion(inv *otelgenai.Invocation, capture otelgenai.CaptureMode) []attribute.KeyValue {
	if inv == nil {
		return nil
	}
	generation, ok := vendorGeneration(inv.Vendor)
	if !ok {
		if inv.Vendor != nil {
			// An invocation from unrelated instrumentation carries no vendor
			// payload at all, so a nil Vendor reports nothing. A Vendor of
			// another type is a wiring mistake: staying silent would ship a
			// span the backend cannot decode as a generation.
			otel.Handle(fmt.Errorf("agento11y otelhook: invocation vendor is %T, want otelhook.Generation", inv.Vendor))
		}
		return nil
	}
	if generation.ID == "" {
		otel.Handle(errors.New("agento11y otelhook: generation has no id, the span cannot be decoded as a generation"))
	}
	withContent := capture.SpanContent()

	var attrs []attribute.KeyValue
	if generation.ID != "" {
		attrs = append(attrs, attribute.String(AttrGenerationID, generation.ID))
	}
	if generation.UserID != "" {
		attrs = append(attrs, attribute.String(AttrUserID, generation.UserID))
	}
	if generation.TotalTokens != 0 {
		attrs = append(attrs, attribute.Int64(AttrUsageTotalTokens, generation.TotalTokens))
	}
	if generation.InclusiveTokenSemantics {
		attrs = append(attrs, attribute.String(AttrTokenSemantics, TokenSemanticsInclusive))
	}
	if generation.ToolChoice != nil {
		attrs = append(attrs, attribute.String(AttrToolChoice, *generation.ToolChoice))
	}
	if generation.ThinkingEnabled != nil {
		attrs = append(attrs, attribute.Bool(AttrThinkingEnabled, *generation.ThinkingEnabled))
	}
	if len(generation.ParentGenerationIDs) > 0 {
		attrs = append(attrs, attribute.StringSlice(AttrParentGenerationIDs, generation.ParentGenerationIDs))
	}
	if generation.EffectiveVersion != "" {
		attrs = append(attrs, attribute.String(AttrEffectiveVersion, generation.EffectiveVersion))
	}
	if len(generation.Tags) > 0 {
		if payload, err := json.Marshal(generation.Tags); err == nil {
			attrs = append(attrs, attribute.String(AttrTags, string(payload)))
		}
		attrs = append(attrs, tagAttributes(generation.Tags)...)
	}
	if metadata := metadataDocument(generation, withContent); len(metadata) > 0 {
		// A value that does not marshal takes the whole document with it,
		// including the SDK's own capture-mode marker, and the proprietary
		// path fails the export outright on the same input.
		if payload, err := json.Marshal(metadata); err == nil {
			attrs = append(attrs, attribute.String(AttrMetadata, string(payload)))
		} else {
			otel.Handle(fmt.Errorf("agento11y otelhook: dropping generation metadata: %w", err))
		}
	}
	if withContent && len(generation.Artifacts) > 0 {
		if payload, err := json.Marshal(wireArtifacts(generation.Artifacts)); err == nil {
			attrs = append(attrs, attribute.String(AttrRawArtifacts, string(payload)))
		}
	}
	return attrs
}

// metadataDocument returns the metadata map to serialize. Without content
// capture it drops the mirrors of the conversation title and the call error,
// because the span destination is shared and those two mirrors carry user and
// provider text. With content capture it restores the title mirror, because
// the only reason the title would be missing is a sanitizer that rebuilt the
// metadata map.
func metadataDocument(generation Generation, withContent bool) map[string]any {
	metadata := maps.Clone(generation.Metadata)
	if !withContent {
		for _, key := range contentMetadataKeys {
			delete(metadata, key)
		}
		return metadata
	}
	if generation.ConversationTitle == "" {
		return metadata
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata[contentcapture.ConversationTitleKey] = generation.ConversationTitle
	return metadata
}

// wireArtifact is the backend's raw-artifacts encoding: a payload rides
// base64-encoded because a span attribute is a string.
type wireArtifact struct {
	Kind        string `json:"kind,omitempty"`
	Name        string `json:"name,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	PayloadB64  string `json:"payload_b64,omitempty"`
	RecordID    string `json:"record_id,omitempty"`
	URI         string `json:"uri,omitempty"`
}

func wireArtifacts(artifacts []Artifact) []wireArtifact {
	out := make([]wireArtifact, 0, len(artifacts))
	for _, artifact := range artifacts {
		wa := wireArtifact{
			Kind:        artifact.Kind,
			Name:        artifact.Name,
			ContentType: artifact.ContentType,
			RecordID:    artifact.RecordID,
			URI:         artifact.URI,
		}
		if len(artifact.Payload) > 0 {
			wa.PayloadB64 = base64.StdEncoding.EncodeToString(artifact.Payload)
		}
		out = append(out, wa)
	}
	return out
}

func vendorGeneration(vendor any) (Generation, bool) {
	switch value := vendor.(type) {
	case Generation:
		return value, true
	case *Generation:
		if value == nil {
			return Generation{}, false
		}
		return *value, true
	default:
		return Generation{}, false
	}
}

// tagAttributes renders tags as agento11y.tag.<key> dimensions in a stable
// order, so two spans with the same tags carry them identically.
func tagAttributes(tags map[string]string) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, len(tags))
	for _, key := range slices.Sorted(maps.Keys(tags)) {
		attrs = append(attrs, attribute.String(AttrTagPrefix+key, tags[key]))
	}
	return attrs
}
