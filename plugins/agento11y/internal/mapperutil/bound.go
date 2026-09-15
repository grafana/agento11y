package mapperutil

import (
	"encoding/json"
	"unicode/utf8"
)

// MaxToolInputBytes bounds a tool call's argument JSON on export. It is the
// cap the Claude Code mapper has applied to tool inputs since its first
// release; the other mappers now share it.
const MaxToolInputBytes = 4096

// MaxToolResultBytes bounds a tool result on export. The SDK refuses a
// generation whose encoded payload exceeds GenerationExportConfig.PayloadMaxBytes
// (16MB by default) before it leaves the machine, so one large tool result, a
// file read or a subagent's retrieval, would otherwise fail the whole turn's
// export. A stop-hook retry cannot succeed, because the size is deterministic.
// 1MB keeps a turn with several results well inside the cap while preserving
// far more of a result than an evaluator or a reviewer reads.
const MaxToolResultBytes = 1 << 20

// BoundJSON returns raw unchanged when it is at most limit bytes. A longer
// payload is cut on a UTF-8 boundary and returned as a JSON string ending in
// " [truncated]", so the field stays valid JSON. The type changes from
// object or array to string when that happens, which is what the Claude Code
// mapper has always done for tool inputs.
func BoundJSON(raw json.RawMessage, limit int) json.RawMessage {
	if len(raw) <= limit {
		return raw
	}
	quoted, _ := json.Marshal(cutUTF8(string(raw), limit) + " [truncated]")
	return json.RawMessage(quoted)
}

// BoundText is BoundJSON for a tool result held as text: unchanged when at
// most limit bytes, otherwise cut on a UTF-8 boundary with " [truncated]"
// appended.
func BoundText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return cutUTF8(s, limit) + " [truncated]"
}

func cutUTF8(s string, limit int) string {
	if limit < 0 {
		limit = 0
	}
	cut := s[:limit]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}
