package transcript

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestReadComplete_AppendedRecord(t *testing.T) {
	const prefix = `{"type":"user","message":{"content":"first"}}` + "\n"
	const record = `{"type":"assistant","message":{"content":[],"usage":{"output_tokens":1}}}`
	for _, tc := range []struct {
		name string
		cut  int
	}{
		{name: "split JSON", cut: len(record) / 2},
		{name: "valid JSON without newline", cut: len(record)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempFile(t, prefix+record[:tc.cut])
			lines, pos, err := ReadComplete(path, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(lines) != 1 || pos != int64(len(prefix)) {
				t.Fatalf("read incomplete tail: lines=%d offset=%d, want 1/%d", len(lines), pos, len(prefix))
			}
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString(record[tc.cut:] + "\n"); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			lines, next, err := ReadComplete(path, pos)
			if err != nil || len(lines) != 1 || lines[0].Type != "assistant" || next != int64(len(prefix)+len(record)+1) {
				t.Fatalf("read completed append: lines=%+v offset=%d error=%v", lines, next, err)
			}
			lines, final, err := ReadComplete(path, next)
			if err != nil || len(lines) != 0 || final != next {
				t.Fatalf("duplicate read: lines=%d offset=%d error=%v", len(lines), final, err)
			}
		})
	}
}

func TestReadComplete_MetadataTail(t *testing.T) {
	const prefix = `{"type":"custom-title","customTitle":"complete"}` + "\n"
	const tail = `{"type":"custom-title","customTitle":"pending"}`
	path := writeTempFile(t, prefix+tail)
	lines, pos, titles, err := ReadCompleteWithTitles(path, 0)
	if err != nil || len(lines) != 0 || pos != int64(len(prefix)) || titles.Custom != "complete" {
		t.Fatalf("lines=%d offset=%d titles=%+v error=%v", len(lines), pos, titles, err)
	}
	_, historyPos, historyTitles, err := ReadWithTitles(path, 0)
	if err != nil || historyPos != int64(len(prefix)+len(tail)) || historyTitles.Custom != "pending" {
		t.Fatalf("history offset=%d titles=%+v error=%v", historyPos, historyTitles, err)
	}
}

func TestRead_NativeToolUseResult(t *testing.T) {
	const result = `{"success":true,"commandName":"review","tool_use_id":"call-1"}`
	path := writeTempFile(t, `{"type":"user","toolUseResult":`+result+`,"message":{"content":[{"type":"tool_result","tool_use_id":"call-1","content":"ok"}]}}`+"\n")
	lines, _, err := Read(path, 0)
	if err != nil || len(lines) != 1 {
		t.Fatalf("lines=%d error=%v", len(lines), err)
	}
	encoded, err := json.Marshal(lines[0])
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if string(roundTrip["toolUseResult"]) != result {
		t.Fatalf("native structured result lost: %s", encoded)
	}
}

func TestReadComplete_OverlongTail(t *testing.T) {
	old := maxLineBytes
	maxLineBytes = 100
	t.Cleanup(func() { maxLineBytes = old })
	const record = `{"type":"user","message":{"content":"ok"}}` + "\n"
	for _, tc := range []struct {
		name    string
		content string
		wantPos int64
	}{
		{name: "unterminated oversized", content: record + strings.Repeat("x", 70*1024), wantPos: int64(len(record))},
		{name: "complete malformed and oversized", content: "broken\n" + strings.Repeat("x", 70*1024) + "\n" + record, wantPos: int64(7 + 70*1024 + 1 + len(record))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines, pos, err := ReadComplete(writeTempFile(t, tc.content), 0)
			if err != nil || len(lines) != 1 || pos != tc.wantPos {
				t.Fatalf("lines=%d offset=%d error=%v, want 1/%d/nil", len(lines), pos, err, tc.wantPos)
			}
		})
	}
}
