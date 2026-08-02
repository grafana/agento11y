package redact

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// stringFixtures mirrors redaction/fixtures/strings.json, the shared case file
// every redaction engine loads. The plugin module is built with GOWORK=off and
// pins a released SDK, so it reads the fixture file directly rather than
// importing a helper from the SDK.
type stringFixtures struct {
	Cases []stringCase `json:"cases"`
}

type stringCase struct {
	ID       string `json:"id"`
	Mode     string `json:"mode"`
	Emails   bool   `json:"emails"`
	Input    string `json:"input"`
	Expected string `json:"expected"`
}

func TestConformanceRedactionStrings(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "redaction", "fixtures", "strings.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var fixtures stringFixtures
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(fixtures.Cases) == 0 {
		t.Fatalf("%s has no cases", path)
	}

	for _, tc := range fixtures.Cases {
		t.Run(tc.ID, func(t *testing.T) {
			r := NewWithOptions(Options{RedactEmailAddresses: tc.Emails})
			var got string
			switch tc.Mode {
			case "full":
				got = r.Redact(tc.Input)
			case "light":
				got = r.RedactLightweight(tc.Input)
			default:
				t.Fatalf("unknown mode %q", tc.Mode)
			}
			if got != tc.Expected {
				t.Errorf("mode=%s emails=%v\n input: %q\n   got: %q\n  want: %q", tc.Mode, tc.Emails, tc.Input, got, tc.Expected)
			}
		})
	}
}
