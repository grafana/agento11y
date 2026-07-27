package agento11y

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

func StableID(prefix string, parts ...any) string {
	values := make([]string, len(parts))
	for i, part := range parts {
		if part != nil {
			values[i] = fmt.Sprint(part)
		}
	}
	digest := sha1.Sum([]byte(strings.Join(values, "\x1f")))
	return prefix + "-" + hex.EncodeToString(digest[:])[:16]
}

func acceptedOrError(response *ExportScoresResponse) (int, error) {
	if response == nil {
		return 0, fmt.Errorf("%w: empty response", ErrScoreExportFailed)
	}
	rejected := response.Rejected()
	rejectedCount := max(len(rejected), response.RejectedCount)
	if rejectedCount == 0 {
		return response.AcceptedCount(), nil
	}
	if len(rejected) == 0 {
		return 0, fmt.Errorf("%w: rejected %d score(s)", ErrScoreExportFailed, rejectedCount)
	}
	parts := make([]string, len(rejected))
	for i, result := range rejected {
		detail := result.Error
		if detail == "" {
			detail = "rejected"
		}
		if result.ScoreID == "" {
			parts[i] = detail
			continue
		}
		parts[i] = result.ScoreID + ": " + detail
	}
	return 0, fmt.Errorf("%w: rejected %d score(s): %s", ErrScoreExportFailed, rejectedCount, strings.Join(parts, "; "))
}

func cloneTestSuite(in *TestSuite) *TestSuite {
	if in == nil {
		return nil
	}
	out := *in
	out.Tags = append([]string(nil), in.Tags...)
	out.TestCases = cloneTestCases(in.TestCases)
	return &out
}

func cloneTestCases(in []TestCase) []TestCase {
	if len(in) == 0 {
		return nil
	}
	out := make([]TestCase, len(in))
	for i := range in {
		out[i] = cloneTestCase(in[i])
	}
	return out
}

func cloneTestCase(in TestCase) TestCase {
	in.Tags = append([]string(nil), in.Tags...)
	in.Metadata = cloneMetadata(in.Metadata)
	in.ArtifactRefs = append([]ExperimentArtifactRef(nil), in.ArtifactRefs...)
	return in
}

func cloneCandidate(in *Candidate) *Candidate {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func experimentRandomHex(n int) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstString(values ...any) string {
	for _, value := range values {
		switch typed := value.(type) {
		case string:
			if typed != "" {
				return typed
			}
		case fmt.Stringer:
			if typed.String() != "" {
				return typed.String()
			}
		}
	}
	return ""
}

func textMessages(role Role, text string) []Message {
	if text == "" {
		return nil
	}
	if role == RoleAssistant {
		return []Message{AssistantTextMessage(text)}
	}
	return []Message{UserTextMessage(text)}
}

func candidateModelProvider(c *Candidate) string {
	if c == nil {
		return ""
	}
	return c.ModelProvider
}

func candidateModelName(c *Candidate) string {
	if c == nil {
		return ""
	}
	return c.ModelName
}

func candidateAgentName(c *Candidate) string {
	if c == nil {
		return ""
	}
	return c.AgentName
}
