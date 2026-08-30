package local

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

const (
	ToolSpansDir = "tool-spans"

	spanAttrOperationName  = "gen_ai.operation.name"
	spanAttrConversationID = "gen_ai.conversation.id"
	spanAttrToolName       = "gen_ai.tool.name"
	spanAttrToolCallID     = "gen_ai.tool.call.id"
	spanAttrErrorType      = "error.type"
	spanAttrErrorCategory  = "error.category"
	spanAttrSkillName      = "agento11y.skill.name"
)

// toolSpanRecord is the metadata-only projection retained from an execute_tool
// span. It deliberately has no field capable of holding tool content or error
// text.
type toolSpanRecord struct {
	TraceID        string    `json:"trace_id"`
	SpanID         string    `json:"span_id"`
	ParentSpanID   string    `json:"parent_span_id,omitempty"`
	ConversationID string    `json:"conversation_id"`
	ToolCallID     string    `json:"tool_call_id,omitempty"`
	ToolName       string    `json:"tool_name"`
	StartedAt      time.Time `json:"started_at"`
	CompletedAt    time.Time `json:"completed_at"`
	Failed         bool      `json:"failed,omitempty"`
	ErrorType      string    `json:"error_type,omitempty"`
	ErrorCategory  string    `json:"error_category,omitempty"`
	SkillName      string    `json:"skill_name,omitempty"`
	DeliveryOrder  uint64    `json:"delivery_order,omitempty"`
}

type toolSpanFile struct {
	id      string
	path    string
	size    int64
	modTime time.Time
}

type toolSpanCacheEntry struct {
	size    int64
	modTime time.Time
	spans   []toolSpanRecord
	skipped int
}

type toolSpanCache struct {
	mu      sync.Mutex
	entries map[string]*toolSpanCacheEntry
}

func (c *toolSpanCache) invalidate(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, path)
}

func (c *toolSpanCache) get(file toolSpanFile) (*toolSpanCacheEntry, error) {
	c.mu.Lock()
	if cached := c.entries[file.path]; cached != nil && cached.size == file.size && cached.modTime.Equal(file.modTime) {
		c.mu.Unlock()
		return cached, nil
	}
	c.mu.Unlock()

	entry, err := decodeToolSpanFile(file)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]*toolSpanCacheEntry{}
	}
	c.entries[file.path] = entry
	c.mu.Unlock()
	return entry, nil
}

func decodeToolSpanFile(file toolSpanFile) (*toolSpanCacheEntry, error) {
	entry := &toolSpanCacheEntry{size: file.size, modTime: file.modTime}
	var persisted []toolSpanRecord
	skipped, err := scanJSONL(file.path, func(line []byte) (toolSpanRecord, bool) {
		var span toolSpanRecord
		if err := json.Unmarshal(line, &span); err != nil {
			return toolSpanRecord{}, false
		}
		return span, span.TraceID != "" && span.SpanID != "" && validConversationID(span.ConversationID)
	}, func(span toolSpanRecord) {
		persisted = append(persisted, span)
	})
	entry.spans = dedupeToolSpans(persisted)
	entry.skipped = skipped
	return entry, err
}

// dedupeToolSpans keeps the final occurrence of each trace/span identity and
// leaves winners in their persisted order. Keeping the winner's position (not
// the first occurrence's position) makes the ordering stable when the cache is
// rebuilt after a restart.
func dedupeToolSpans(records []toolSpanRecord) []toolSpanRecord {
	winner := make(map[string]int, len(records))
	for index, record := range records {
		key := record.TraceID + "/" + record.SpanID
		previous, exists := winner[key]
		if !exists || record.DeliveryOrder >= records[previous].DeliveryOrder {
			winner[key] = index
		}
	}
	out := make([]toolSpanRecord, 0, len(winner))
	for index, record := range records {
		if winner[record.TraceID+"/"+record.SpanID] == index {
			out = append(out, record)
		}
	}
	return out
}

func (s *Storage) appendToolSpans(records []toolSpanRecord) ([]string, error) {
	if len(records) == 0 {
		return nil, nil
	}

	// Serialise the global persisted order as well as each sidecar's writes.
	s.toolSpansAppendMu.Lock()
	defer s.toolSpansAppendMu.Unlock()

	spanFiles, err := s.toolSpanFiles()
	if err != nil {
		return nil, fmt.Errorf("local storage: read tool span order: %w", err)
	}
	var lastDelivery uint64
	for _, file := range spanFiles {
		entry, err := s.toolSpans.get(file)
		if err != nil {
			return nil, fmt.Errorf("local storage: read tool span order: %w", err)
		}
		for _, span := range entry.spans {
			if span.DeliveryOrder > lastDelivery {
				lastDelivery = span.DeliveryOrder
			}
		}
	}

	// Global order prevents an unrelated append to the old conversation from
	// making a stale identity win after restart.
	unordered := make([]toolSpanRecord, len(records))
	copy(unordered, records)
	for index := range unordered {
		unordered[index].DeliveryOrder = 0
	}
	records = dedupeToolSpans(unordered)
	for index := range records {
		if lastDelivery < ^uint64(0) {
			lastDelivery++
		}
		records[index].DeliveryOrder = lastDelivery
	}
	groups := map[string][]toolSpanRecord{}
	order := make([]string, 0)
	for _, record := range records {
		if !validConversationID(record.ConversationID) {
			s.logf("local: skip tool span with unsafe conversation id %q", record.ConversationID)
			continue
		}
		if _, ok := groups[record.ConversationID]; !ok {
			order = append(order, record.ConversationID)
		}
		groups[record.ConversationID] = append(groups[record.ConversationID], record)
	}
	changed := make([]string, 0, len(order))
	var appendErrors []error
	for _, conversationID := range order {
		name := filepath.Join(ToolSpansDir, conversationID+".jsonl")
		path := s.Path(name)
		written, err := appendJSONL(s, name, groups[conversationID], nil)
		if written > 0 || err != nil {
			s.toolSpans.invalidate(path)
		}
		if written > 0 {
			changed = append(changed, conversationID)
		}
		if err != nil {
			appendErrors = append(appendErrors, err)
		}
	}
	return changed, errors.Join(appendErrors...)
}

func (s *Storage) toolSpanFiles() ([]toolSpanFile, error) {
	dir := filepath.Join(s.dir, ToolSpansDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]toolSpanFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".jsonl")
		if !validConversationID(id) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		out = append(out, toolSpanFile{
			id: id, path: filepath.Join(dir, entry.Name()), size: info.Size(), modTime: info.ModTime(),
		})
	}
	// DeliveryOrder decides modern records. Sorting files by id supplies a
	// deterministic persisted order for legacy records that predate it.
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out, nil
}

func projectToolSpans(body []byte, contentType, contentEncoding string) ([]toolSpanRecord, error) {
	decoded := body
	if isGzipEncoding(contentEncoding) {
		var err error
		decoded, err = gunzipLimited(body, maxOTLPBodyBytes)
		if err != nil {
			return nil, err
		}
	}
	request, err := decodeTracePayload(decoded, contentType)
	if err != nil {
		return nil, err
	}
	return toolSpanRecords(request), nil
}

func gunzipLimited(body []byte, limit int64) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	out, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(out)) > limit {
		return nil, fmt.Errorf("decompressed trace payload exceeds %d bytes", limit)
	}
	return out, nil
}

func toolSpanRecords(request *coltracepb.ExportTraceServiceRequest) []toolSpanRecord {
	var out []toolSpanRecord
	for _, resource := range request.GetResourceSpans() {
		for _, scope := range resource.GetScopeSpans() {
			for _, span := range scope.GetSpans() {
				if record, ok := toolSpanRecordFromProto(span); ok {
					out = append(out, record)
				}
			}
		}
	}
	return out
}

func toolSpanRecordFromProto(span *tracepb.Span) (toolSpanRecord, bool) {
	if span == nil || len(span.GetTraceId()) != 16 || len(span.GetSpanId()) != 8 {
		return toolSpanRecord{}, false
	}
	attrs := spanStringAttributes(span.GetAttributes())
	if strings.TrimSpace(attrs[spanAttrOperationName]) != "execute_tool" {
		return toolSpanRecord{}, false
	}
	conversationID := strings.TrimSpace(attrs[spanAttrConversationID])
	toolName := strings.TrimSpace(attrs[spanAttrToolName])
	if !validConversationID(conversationID) || toolName == "" {
		return toolSpanRecord{}, false
	}
	errorType := strings.TrimSpace(attrs[spanAttrErrorType])
	errorCategory := strings.TrimSpace(attrs[spanAttrErrorCategory])
	record := toolSpanRecord{
		TraceID:        hex.EncodeToString(span.GetTraceId()),
		SpanID:         hex.EncodeToString(span.GetSpanId()),
		ConversationID: conversationID,
		ToolCallID:     strings.TrimSpace(attrs[spanAttrToolCallID]),
		ToolName:       toolName,
		StartedAt:      unixNanoTime(span.GetStartTimeUnixNano()),
		CompletedAt:    unixNanoTime(span.GetEndTimeUnixNano()),
		Failed:         span.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR || errorType != "" || errorCategory != "",
		ErrorType:      errorType,
		ErrorCategory:  errorCategory,
		SkillName:      strings.TrimSpace(attrs[spanAttrSkillName]),
	}
	if len(span.GetParentSpanId()) == 8 {
		record.ParentSpanID = hex.EncodeToString(span.GetParentSpanId())
	}
	return record, true
}

func spanStringAttributes(attributes []*commonpb.KeyValue) map[string]string {
	out := make(map[string]string, len(attributes))
	for _, attribute := range attributes {
		if attribute == nil || attribute.GetValue().GetValue() == nil {
			continue
		}
		if _, ok := attribute.GetValue().GetValue().(*commonpb.AnyValue_StringValue); ok {
			out[attribute.GetKey()] = attribute.GetValue().GetStringValue()
		}
	}
	return out
}

func unixNanoTime(value uint64) time.Time {
	if value == 0 || value > ^uint64(0)>>1 {
		return time.Time{}
	}
	return time.Unix(0, int64(value)).UTC()
}
