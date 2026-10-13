package codec

import (
	"testing"

	"github.com/grafana/agento11y/go/agento11y/model"
)

func TestModalityPresence(t *testing.T) {
	u := usageToProto(model.TokenUsage{InputTokens: 3, InputByModality: &model.ModalityTokenCounts{Tokens: map[string]int64{"image": 3}, Complete: true}})
	if u.InputByModality == nil || u.OutputByModality != nil || !u.InputByModality.Complete || u.InputByModality.Tokens["image"] != 3 {
		t.Fatalf("lost modality usage: %+v", u)
	}
}
