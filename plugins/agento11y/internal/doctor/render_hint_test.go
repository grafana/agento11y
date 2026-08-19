package doctor

import (
	"bytes"
	"strings"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/skills"
)

// TestRenderHuman_SetupHint pins which of the two footers each report ends
// with. The golden files pin the bytes, but a blind `UPDATE_GOLDENS=1` reseed
// accepts whatever the renderer produced, so this test is what fails when the
// footer is dropped or the wrong branch fires.
//
// The minimal case is the one that matters most: a machine that has configured
// nothing reports "no problems detected", and still needs the paste block. See
// needsSetup.
func TestRenderHuman_SetupHint(t *testing.T) {
	cases := []struct {
		name      string
		report    *Report
		wantBlock bool
	}{
		{name: "healthy and configured", report: goldenHealthyReport()},
		{name: "local capture configured", report: localCaptureReport()},
		{name: "nothing configured", report: goldenMinimalReport(), wantBlock: true},
		{name: "config in error", report: brokenConfigReport(), wantBlock: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			renderHuman(&buf, tc.report, false)
			out := buf.String()

			block := skills.SetupCodingAgentHintIntro + "\n" + skills.SetupCodingAgentPasteLine
			switch {
			case tc.wantBlock:
				if !strings.Contains(out, block) {
					t.Errorf("report does not end with the paste block:\n%s", out)
				}
				if strings.Contains(out, skills.SetupCodingAgentOneLiner) {
					t.Errorf("report printed the quiet one-liner too:\n%s", out)
				}
			default:
				if !strings.Contains(out, skills.SetupCodingAgentOneLiner) {
					t.Errorf("report does not end with the quiet one-liner:\n%s", out)
				}
				if strings.Contains(out, block) {
					t.Errorf("report printed the multi-line paste block:\n%s", out)
				}
			}

			// The footer is the last thing printed, after the summary.
			trimmed := strings.TrimRight(out, "\n")
			lastLine := trimmed[strings.LastIndex(trimmed, "\n")+1:]
			if !strings.Contains(lastLine, skills.SetupCodingAgentCommand) {
				t.Errorf("the footer is not the last line: %q", lastLine)
			}
		})
	}
}

func localCaptureReport() *Report {
	r := goldenMinimalReport()
	r.Conversations.Health = HealthOK
	r.Conversations.Messages = []string{"local capture is enabled; Grafana Cloud credentials are not required"}
	r.Analytics.Health = HealthOK
	r.Analytics.Messages = []string{"local capture is enabled; a Cloud OTLP endpoint is not required"}
	r.localCaptureConfigured = true
	return r
}

// brokenConfigReport is a report whose only failing section is config, so the
// footer is driven by something other than the two pipelines. It is the fifth
// golden fixture; see TestRenderHumanGolden.
func brokenConfigReport() *Report {
	r := goldenHealthyReport()
	r.Config.Health = HealthError
	r.Config.Messages = []string{"config.env is not readable"}
	return r
}
