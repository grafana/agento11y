package weavertest

import (
	"archive/tar"
	"bytes"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
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

func TestWriteInputsKeepsExistingAssets(t *testing.T) {
	registryRoot := t.TempDir()
	_, config, err := writeInputs(registryRoot)
	if err != nil {
		t.Fatalf("first writeInputs: %v", err)
	}
	openConfig, err := os.Open(config)
	if err != nil {
		t.Fatalf("open first config: %v", err)
	}
	defer openConfig.Close()
	original, err := openConfig.Stat()
	if err != nil {
		t.Fatalf("stat first config: %v", err)
	}

	_, nextConfig, err := writeInputs(registryRoot)
	if err != nil {
		t.Fatalf("second writeInputs: %v", err)
	}
	current, err := os.Stat(nextConfig)
	if err != nil {
		t.Fatalf("stat second config: %v", err)
	}
	if !os.SameFile(original, current) {
		t.Error("second writeInputs replaced the config returned by the first call")
	}
}

func TestWriteInputsConcurrentInstall(t *testing.T) {
	const calls = 32
	type result struct {
		policies string
		config   string
		err      error
	}

	registryRoot := t.TempDir()
	start := make(chan struct{})
	results := make(chan result, calls)
	for range calls {
		go func() {
			<-start
			policies, config, err := writeInputs(registryRoot)
			results <- result{policies: policies, config: config, err: err}
		}()
	}
	close(start)

	var first result
	for i := range calls {
		current := <-results
		if current.err != nil {
			t.Fatalf("writeInputs call %d: %v", i, current.err)
		}
		if i == 0 {
			first = current
			continue
		}
		if current.policies != first.policies || current.config != first.config {
			t.Errorf("writeInputs call %d returned %q and %q, want %q and %q", i, current.policies, current.config, first.policies, first.config)
		}
	}
	if _, err := os.Stat(first.config); err != nil {
		t.Errorf("stat config: %v", err)
	}
}

type tarEntry struct {
	name     string
	linkname string
	body     string
	typeflag byte
}

func buildTar(t *testing.T, entries []tarEntry) *bytes.Reader {
	t.Helper()
	buffer := &bytes.Buffer{}
	writer := tar.NewWriter(buffer)
	for _, entry := range entries {
		header := &tar.Header{
			Name:     entry.name,
			Linkname: entry.linkname,
			Typeflag: entry.typeflag,
			Mode:     0o644,
			Size:     int64(len(entry.body)),
		}
		if entry.typeflag != tar.TypeReg {
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatalf("write header %q: %v", entry.name, err)
		}
		if entry.typeflag == tar.TypeReg {
			if _, err := writer.Write([]byte(entry.body)); err != nil {
				t.Fatalf("write body %q: %v", entry.name, err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}
	return bytes.NewReader(buffer.Bytes())
}

func TestExtractTar(t *testing.T) {
	tests := []struct {
		name    string
		entries []tarEntry
		wantErr string
		// wantLink is the target root/link must resolve to after the
		// extraction.
		wantLink string
		// wantOutside is a path relative to the extraction root's parent
		// that must not exist after the extraction.
		wantOutside string
		// entryCap lowers maxEntryBytes for the case, so the cap can be
		// exercised without a 64 MiB fixture.
		entryCap int64
	}{
		{
			name: "regular file and symlink beside it",
			entries: []tarEntry{
				{name: "root/file", body: "data", typeflag: tar.TypeReg},
				{name: "root/link", linkname: "file", typeflag: tar.TypeSymlink},
			},
			wantLink: "file",
		},
		{
			name: "path escapes the root",
			entries: []tarEntry{
				{name: "../outside", body: "x", typeflag: tar.TypeReg},
			},
			wantErr: "escapes extraction root",
		},
		{
			name: "symlink target escapes the root",
			entries: []tarEntry{
				{name: "root/link", linkname: "../../outside", typeflag: tar.TypeSymlink},
			},
			wantErr: "symlink escapes extraction root",
		},
		{
			// Each hop is lexically inside the root, and the chain of
			// them still resolves above it.
			name: "write through a symlink chain written by the same archive",
			entries: []tarEntry{
				{name: "root/hop", linkname: "..", typeflag: tar.TypeSymlink},
				{name: "root/hop/hop2", linkname: "..", typeflag: tar.TypeSymlink},
				{name: "root/hop/hop2/planted", body: "owned", typeflag: tar.TypeReg},
			},
			wantErr:     "writes through symlink",
			wantOutside: "planted",
		},
		{
			name: "file larger than the entry cap",
			entries: []tarEntry{
				{name: "root/big", body: strings.Repeat("x", 16), typeflag: tar.TypeReg},
			},
			entryCap: 8,
			wantErr:  "larger than 8 bytes",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.entryCap > 0 {
				original := maxEntryBytes
				maxEntryBytes = test.entryCap
				defer func() { maxEntryBytes = original }()
			}

			parent := t.TempDir()
			root := filepath.Join(parent, "extract")
			if err := os.Mkdir(root, 0o755); err != nil {
				t.Fatalf("create extraction root: %v", err)
			}

			err := extractTar(buildTar(t, test.entries), root)
			switch {
			case test.wantErr == "" && err != nil:
				t.Fatalf("extractTar: %v", err)
			case test.wantErr != "" && err == nil:
				t.Fatalf("extractTar succeeded, want error containing %q", test.wantErr)
			case test.wantErr != "" && !strings.Contains(err.Error(), test.wantErr):
				t.Fatalf("extractTar error %q, want it to contain %q", err, test.wantErr)
			}

			if test.wantLink != "" {
				link, err := os.Readlink(filepath.Join(root, "root", "link"))
				if err != nil {
					t.Fatalf("Readlink: %v", err)
				}
				if link != test.wantLink {
					t.Errorf("link target = %q, want %q", link, test.wantLink)
				}
			}

			if test.wantOutside != "" {
				if _, err := os.Lstat(filepath.Join(parent, test.wantOutside)); err == nil {
					t.Fatalf("archive wrote %s outside the extraction root", test.wantOutside)
				}
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
