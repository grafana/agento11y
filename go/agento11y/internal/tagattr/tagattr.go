// Package tagattr renders a tag map as agento11y.tag.<key> attributes.
package tagattr

import (
	"slices"
	"strings"

	"go.opentelemetry.io/otel/attribute"
)

// Attributes renders tags as prefix+key attributes, sorted by key. Keys and
// values are trimmed, and a key that trims to nothing is dropped.
func Attributes(prefix string, tags map[string]string) []attribute.KeyValue {
	if len(tags) == 0 {
		return nil
	}
	attrs := make([]attribute.KeyValue, 0, len(tags))
	for key, value := range tags {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" {
			continue
		}
		attrs = append(attrs, attribute.String(prefix+trimmed, strings.TrimSpace(value)))
	}
	if len(attrs) == 0 {
		return nil
	}
	slices.SortFunc(attrs, func(a, b attribute.KeyValue) int { return strings.Compare(string(a.Key), string(b.Key)) })
	return attrs
}
