// Package otelhook is the agento11y extension of the otelgenai util. It
// implements otelgenai.EndHook, so a generation span produced by the util
// carries the agento11y.* attributes for the generation fields the Gen AI
// semantic conventions do not define.
//
// Message content reaches the hook already sanitized: the SDK runs
// Config.GenerationSanitizer over the generation before the SDK builds the
// invocation, so the hook adds attributes rather than re-running redaction.
// The handler cannot inspect what a hook returns, so the hook checks the
// resolved capture mode itself: it emits the attributes below that carry
// provider or user text only when that mode puts content on the span.
package otelhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/grafana/agento11y/go/agento11y/codec"
	"github.com/grafana/agento11y/go/agento11y/contentcapture"
	"github.com/grafana/agento11y/go/agento11y/internal/tagattr"
	"github.com/grafana/agento11y/go/agento11y/model"
	"github.com/grafana/agento11y/go/otelgenai"
)

const (
	AttrGenerationID        = "agento11y.generation.id"
	AttrParentGenerationIDs = "agento11y.generation.parent_generation_ids"
	AttrTags                = "agento11y.generation.tags"
	AttrMetadata            = "agento11y.generation.metadata"
	// These two alias the contentcapture keys, which classify them as content.
	// A second spelling here would let the two keys drift apart.
	AttrRawArtifacts      = contentcapture.RawArtifactsAttributeKey
	AttrConversationTitle = contentcapture.ConversationTitleKey
	AttrEffectiveVersion  = "agento11y.agent.effective_version"
	AttrToolChoice        = "agento11y.gen_ai.request.tool_choice"
	AttrThinkingEnabled   = "agento11y.gen_ai.request.thinking.enabled"
	AttrUsageTotalTokens  = "agento11y.gen_ai.usage.total_tokens"
	AttrTokenSemantics    = "gen_ai.token.semantics"
	AttrUserID            = "user.id"
	// AttrTagPrefix is the per-tag dimension prefix the SDK already emits on
	// spans and metrics. The hook emits these dimensions next to the tags JSON
	// document, so existing trace filters keep working in otel mode.
	AttrTagPrefix = "agento11y.tag."

	// TokenSemanticsInclusive marks usage whose input_tokens already includes
	// both cache buckets, per the OTel GenAI contract.
	TokenSemanticsInclusive = "inclusive"
)

// contentMetadataKeys are the metadata keys the SDK mirrors content into. The
// hook drops them in every capture mode: a reducing forwarder keeps the
// metadata document and deletes by key, so content nested in it would survive.
var contentMetadataKeys = []string{
	contentcapture.ConversationTitleKey,
	contentcapture.LegacyConversationTitleKey,
	contentcapture.MetadataKeyCallError,
}

// Artifact is one raw provider payload attached to a generation. The fields
// mirror agento11y.v1.Artifact in proto/agento11y/v1/generation_ingest.proto.
type Artifact struct {
	Kind        model.ArtifactKind
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
	// ConversationTitle carries user text, so it goes under
	// AttrConversationTitle and only under content capture.
	ConversationTitle   string
	Tags                map[string]string
	Metadata            map[string]any
	Artifacts           []Artifact
	ParentGenerationIDs []string
	// EffectiveVersion is the raw version string. The hook emits the digest,
	// through codec.EffectiveVersionDigest.
	EffectiveVersion string
	ToolChoice       *string
	ThinkingEnabled  *bool
	TotalTokens      int64
	// InclusiveTokenSemantics marks that Usage.InputTokens on the invocation
	// already includes both cache buckets.
	InclusiveTokenSemantics bool
}

// Hook implements otelgenai.EndHook.
type Hook struct{}

// New returns the agento11y end hook.
func New() *Hook { return &Hook{} }

// OnEnd returns the agento11y attributes for the invocation. An
// invocation whose Vendor is not a Generation gets no attributes, so a span
// from unrelated instrumentation carries no generation id.
//
// capture gates the content-bearing attributes: the conversation title and the
// raw artifacts.
func (h *Hook) OnEnd(_ context.Context, inv *otelgenai.Invocation, capture otelgenai.CaptureMode) []attribute.KeyValue {
	if inv == nil {
		return nil
	}
	generation, ok := vendorGeneration(inv.Vendor)
	if !ok {
		// An invocation from unrelated instrumentation carries no vendor payload
		// at all, so a nil Vendor reports nothing. Anything else is a wiring
		// mistake: staying silent would ship a span with none of the agento11y
		// attributes on it.
		switch value := inv.Vendor.(type) {
		case nil:
		case *Generation:
			// Only a nil pointer reaches this case.
			otel.Handle(errors.New("agento11y otelhook: invocation vendor is a nil *otelhook.Generation, so the span carries no agento11y attributes"))
		default:
			otel.Handle(fmt.Errorf("agento11y otelhook: invocation vendor is %T, want otelhook.Generation, so the span carries no agento11y attributes", value))
		}
		return nil
	}
	if generation.ID == "" {
		otel.Handle(errors.New("agento11y otelhook: generation has no id, so the span carries no " + AttrGenerationID))
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
	if digest := codec.EffectiveVersionDigest(generation.EffectiveVersion); digest != "" {
		attrs = append(attrs, attribute.String(AttrEffectiveVersion, digest))
	}
	if len(generation.Tags) > 0 {
		// The document keeps the map as given, matching the proto tags field.
		// The dimensions trim it.
		if payload, err := json.Marshal(generation.Tags); err == nil {
			attrs = append(attrs, attribute.String(AttrTags, string(payload)))
		}
		attrs = append(attrs, tagAttributes(generation.Tags)...)
	}
	if withContent {
		if title := conversationTitle(generation); title != "" {
			attrs = append(attrs, attribute.String(AttrConversationTitle, title))
		}
	}
	if metadata := metadataDocument(generation.Metadata); len(metadata) > 0 {
		// A value that does not marshal takes the whole document with it,
		// including the SDK's own capture-mode marker, so report the drop.
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

// metadataDocument returns the metadata map to serialize, without the mirrors
// listed in contentMetadataKeys.
func metadataDocument(metadata map[string]any) map[string]any {
	out := maps.Clone(metadata)
	for _, key := range contentMetadataKeys {
		delete(out, key)
	}
	return out
}

// conversationTitle falls back to the metadata mirrors, where a generation
// filled from the proto shape carries the title.
func conversationTitle(generation Generation) string {
	if generation.ConversationTitle != "" {
		return generation.ConversationTitle
	}
	for _, key := range []string{contentcapture.ConversationTitleKey, contentcapture.LegacyConversationTitleKey} {
		if title, ok := generation.Metadata[key].(string); ok && title != "" {
			return title
		}
	}
	return ""
}

// wireArtifact is the span encoding of an Artifact: the same JSON as
// model.Artifact, which spells the agento11y.v1.Artifact field names and
// base64-encodes the payload.
type wireArtifact struct {
	Kind        string `json:"kind,omitempty"`
	Name        string `json:"name,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Payload     []byte `json:"payload,omitempty"`
	RecordID    string `json:"record_id,omitempty"`
	URI         string `json:"uri,omitempty"`
}

func wireArtifacts(artifacts []Artifact) []wireArtifact {
	out := make([]wireArtifact, 0, len(artifacts))
	for _, artifact := range artifacts {
		out = append(out, wireArtifact{
			Kind:        string(artifact.Kind),
			Name:        artifact.Name,
			ContentType: artifact.ContentType,
			Payload:     artifact.Payload,
			RecordID:    artifact.RecordID,
			URI:         artifact.URI,
		})
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

func tagAttributes(tags map[string]string) []attribute.KeyValue {
	return tagattr.Attributes(AttrTagPrefix, tags)
}
