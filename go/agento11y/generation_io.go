package agento11y

import (
	"crypto/sha256"
	"fmt"
	"hash"
	"strconv"
)

func generationIOFingerprint(g Generation) string {
	usage := g.Usage.Normalize()
	h := sha256.New()
	writeHashField(h, g.Model.Provider)
	writeHashField(h, g.Model.Name)
	writeHashField(h, g.AgentName)
	writeHashField(h, firstTextContent(g.Input))
	writeHashField(h, firstTextContent(g.Output))
	writeHashField(h, strconv.FormatInt(usage.InputTokens, 10))
	writeHashField(h, strconv.FormatInt(usage.OutputTokens, 10))
	return fmt.Sprintf("%x", h.Sum(nil))
}

func writeHashField(h hash.Hash, value string) {
	_, _ = h.Write([]byte(strconv.Itoa(len(value))))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(value))
	_, _ = h.Write([]byte{0})
}

func firstTextContent(messages []Message) string {
	for i := range messages {
		for j := range messages[i].Parts {
			if messages[i].Parts[j].Kind == PartKindText {
				return messages[i].Parts[j].Text
			}
		}
	}
	return ""
}
