package weavertest

import (
	"archive/tar"
	"bytes"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"
)

func TestReportHelpers(t *testing.T) {
	finding := map[string]any{
		"level":       "violation",
		"id":          "missing_attribute",
		"message":     "Attribute `vendor.key` does not exist in the registry.",
		"context":     map[string]any{"attribute_key": "vendor.key"},
		"signal_name": "chat model",
		"signal_type": "span",
	}
	report := Report{Raw: map[string]any{
		"samples": []any{
			map[string]any{"span": map[string]any{
				"attributes": []any{
					map[string]any{"name": "gen_ai.operation.name", "value": "chat"},
					map[string]any{
						"name":              "vendor.key",
						"live_check_result": map[string]any{"all_advice": []any{finding}},
					},
				},
			}},
			map[string]any{
				"span": map[string]any{
					"attributes": []any{
						map[string]any{"name": "gen_ai.operation.name", "value": "chat"},
					},
				},
				"nested": map[string]any{
					"live_check_result": map[string]any{"all_advice": []any{finding}},
				},
			},
		},
		"statistics": map[string]any{
			"seen_registry_metrics": map[string]any{
				"gen_ai.client.operation.duration": float64(1),
				"unused":                           float64(0),
			},
		},
	}}

	violations := report.Violations()
	if len(violations) != 1 {
		t.Fatalf("len(Violations) = %d, want 1", len(violations))
	}
	if violations[0].Count != 2 {
		t.Errorf("violation count = %d, want 2", violations[0].Count)
	}
	if got, want := report.SpanOperationCounts(), map[string]int{"chat": 2}; !maps.Equal(got, want) {
		t.Errorf("SpanOperationCounts = %v, want %v", got, want)
	}
	if _, ok := report.SeenMetricNames()["gen_ai.client.operation.duration"]; !ok {
		t.Error("seen metric was omitted")
	}
	if _, ok := report.SeenMetricNames()["unused"]; ok {
		t.Error("zero-count metric was included")
	}
}

func TestFreePortsReturnsDistinctPorts(t *testing.T) {
	ports, err := freePorts(2)
	if err != nil {
		t.Fatalf("freePorts: %v", err)
	}
	if len(ports) != 2 || ports[0] == ports[1] {
		t.Fatalf("freePorts(2) = %v, want two distinct ports", ports)
	}
}

func TestIsAddressInUse(t *testing.T) {
	if !isAddressInUse(errors.New("bind failed: Address already in use (os error 48)")) {
		t.Error("address-in-use error was not recognized")
	}
	if isAddressInUse(errors.New("registry failed to load")) {
		t.Error("unrelated error was recognized as an address collision")
	}
}

func TestExtractTarPreservesSafeSymlink(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Name: "root/file", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{Name: "root/link", Typeflag: tar.TypeSymlink, Linkname: "file"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	target := t.TempDir()
	if err := extractTar(&archive, target); err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	link, err := os.Readlink(filepath.Join(target, "root", "link"))
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if link != "file" {
		t.Errorf("link target = %q, want file", link)
	}
}

func TestExtractTarRejectsEscapes(t *testing.T) {
	tests := []struct {
		name   string
		header tar.Header
	}{
		{
			name:   "entry path",
			header: tar.Header{Name: "../outside", Typeflag: tar.TypeReg, Mode: 0o644},
		},
		{
			name:   "symlink target",
			header: tar.Header{Name: "root/link", Typeflag: tar.TypeSymlink, Linkname: "../../outside"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var archive bytes.Buffer
			writer := tar.NewWriter(&archive)
			if err := writer.WriteHeader(&test.header); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := extractTar(&archive, t.TempDir()); err == nil {
				t.Fatal("extractTar accepted a path outside the extraction root")
			}
		})
	}
}

func TestStripGroupBlock(t *testing.T) {
	input := "groups:\n  - id: keep.before\n    type: attribute_group\n  - id: registry.aws.bedrock\n    type: attribute_group\n    brief: remove me\n  - id: keep.after\n    type: attribute_group\n"
	want := "groups:\n  - id: keep.before\n    type: attribute_group\n  - id: keep.after\n    type: attribute_group\n"
	if got := stripGroupBlock(input, "registry.aws.bedrock"); got != want {
		t.Errorf("stripGroupBlock =\n%s\nwant\n%s", got, want)
	}
}
