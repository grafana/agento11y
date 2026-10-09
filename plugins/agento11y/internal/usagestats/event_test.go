package usagestats

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// fullyPopulatedEvent sets every field non-zero so none can slip past the
// inventory test by marshalling away.
func fullyPopulatedEvent() Event {
	return Event{
		Service:            ServiceName,
		Version:            "1.2.3",
		OS:                 "darwin",
		Arch:               "arm64",
		InstallID:          "6c1f0b6e-0d2a-4f1e-9c3a-6b5d4e3f2a10",
		InstallIDPersisted: true,
		Command:            "skills show",
		Surface:            SurfaceCommand,
		Agent:              "claude",
		Skill:              "setup-coding-agent",
		Flags:              "json,local",
		Outcome:            OutcomeOK,
		ExitCode:           1,
		ErrorKind:          ErrorKindRuntime,
		DurationMS:         42,
		IsTTY:              true,
		IsCI:               true,
		CIProvider:         "github_actions",
	}
}

// wantEventFields is the wire contract. Hand-written on purpose: deriving it
// from the struct would make the test tautological. A change here should
// prompt the matching change to the receiver's columns, the docs page, and the
// first-run notice.
var wantEventFields = []string{
	"service",
	"version",
	"os",
	"arch",
	"install_id",
	"install_id_persisted",
	"command",
	"surface",
	"agent",
	"skill",
	"flags",
	"outcome",
	"exit_code",
	"error_kind",
	"duration_ms",
	"is_tty",
	"is_ci",
	"ci_provider",
}

func TestEventFieldInventory(t *testing.T) {
	body, err := json.Marshal(fullyPopulatedEvent())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	got := make([]string, 0, len(decoded))
	for key := range decoded {
		got = append(got, key)
	}
	slices.Sort(got)

	want := slices.Clone(wantEventFields)
	slices.Sort(want)

	if !slices.Equal(got, want) {
		t.Errorf("event fields on the wire = %v\nwant %v\n\nThe wire shape changed. The receiver's BigQuery columns, the published usage-statistics page, and the first-run notice all describe this set; new collection also needs the notice revision bumped.", got, want)
	}
}

// TestEventHasNoCredentialShapedFields cannot prove a field is safe, but it
// catches the obvious mistakes by name.
func TestEventHasNoCredentialShapedFields(t *testing.T) {
	forbidden := []string{
		"token", "secret", "password", "credential", "tenant", "endpoint",
		"url", "host", "user", "email", "account", "repo", "branch", "tag",
		"path", "dir", "cwd", "file", "message", "prompt", "content",
	}

	typ := reflect.TypeFor[Event]()
	for i := range typ.NumField() {
		field := typ.Field(i)
		tag := strings.Split(field.Tag.Get("json"), ",")[0]
		for _, bad := range forbidden {
			if strings.Contains(strings.ToLower(field.Name), bad) || strings.Contains(strings.ToLower(tag), bad) {
				t.Errorf("Event.%s (json %q) contains the forbidden substring %q; usage statistics carry no identifiers, locations, or free text", field.Name, tag, bad)
			}
		}
	}
}

// TestClosedVocabulariesAreNonEmpty: a deleted constant would make its field
// silently free-form.
func TestClosedVocabulariesAreNonEmpty(t *testing.T) {
	for name, value := range map[string]string{
		"OutcomeOK":         OutcomeOK,
		"OutcomeLaunched":   OutcomeLaunched,
		"OutcomeHelp":       OutcomeHelp,
		"OutcomeUsageError": OutcomeUsageError,
		"OutcomeError":      OutcomeError,
		"OutcomePanic":      OutcomePanic,
		"SurfaceCommand":    SurfaceCommand,
		"SurfaceLauncher":   SurfaceLauncher,
		"SurfaceHelp":       SurfaceHelp,
		"ErrorKindUsage":    ErrorKindUsage,
		"ErrorKindRuntime":  ErrorKindRuntime,
		"CommandUnknown":    CommandUnknown,
		"ServiceName":       ServiceName,
	} {
		if strings.TrimSpace(value) == "" {
			t.Errorf("%s is empty", name)
		}
	}
}
