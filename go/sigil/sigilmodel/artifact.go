package sigilmodel

type ArtifactKind string

const (
	ArtifactKindRequest       ArtifactKind = "request"
	ArtifactKindResponse      ArtifactKind = "response"
	ArtifactKindTools         ArtifactKind = "tools"
	ArtifactKindProviderEvent ArtifactKind = "provider_event"
)

type Artifact struct {
	Kind        ArtifactKind `json:"kind"`
	Name        string       `json:"name,omitempty"`
	ContentType string       `json:"content_type,omitempty"`
	Payload     []byte       `json:"payload,omitempty"`
	RecordID    string       `json:"record_id,omitempty"`
	URI         string       `json:"uri,omitempty"`
}

type ArtifactRef struct {
	Kind        ArtifactKind `json:"kind"`
	Name        string       `json:"name,omitempty"`
	ContentType string       `json:"content_type,omitempty"`
	RecordID    string       `json:"record_id"`
	URI         string       `json:"uri"`
}
