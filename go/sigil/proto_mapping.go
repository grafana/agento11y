package sigil

import (
	sigilv1 "github.com/grafana/sigil-sdk/go/proto/sigil/v1"
	"github.com/grafana/sigil-sdk/go/sigil/sigilcodec"
)

// generationToProto translates the SDK's Generation value into the wire-level
// protobuf message. It delegates to sigilcodec.ToProto so callers that use
// sigilcodec directly get the same field mapping and effective_version
// hashing.
func generationToProto(g Generation) (*sigilv1.Generation, error) {
	return sigilcodec.ToProto(g)
}
