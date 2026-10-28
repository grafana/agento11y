package skillidentity

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	for _, tt := range []struct{ name, want string }{
		{"review", "review"},
		{" My.Plugin:Review_2 ", "My.Plugin:Review_2"},
		{"org:plugin:review", "org:plugin:review"},
		{"团队:审查", "团队:审查"},
		{strings.Repeat("a", 256), strings.Repeat("a", 256)},
		{strings.Repeat("é", 128), strings.Repeat("é", 128)},
		{"\u2003review\u2003", "review"},
		{"", ""}, {"   ", ""}, {"code review", ""},
		{strings.Repeat("a", 257), ""}, {strings.Repeat("é", 129), ""},
		{"/private/review", ""}, {`C:\private\review`, ""},
		{`\\host\review`, ""}, {"skills/review", ""}, {"../review", ""},
		{"https://example.test/review", ""}, {"file:review", ""},
		{"HTTPS:review", ""}, {"git:review", ""}, {"github:review", ""},
		{"file", "file"}, {"review?token=private", ""},
		{":review", ""}, {"plugin::review", ""}, {"plugin:", ""},
		{"plugin:..", ""}, {"plugin:_review", ""},
		{"review\n", ""}, {"\treview", ""}, {"re\x00view", ""},
		{"review\x7f", ""}, {"review\u0085", ""},
		{"review\u202e", ""}, {"re\u200bview", ""},
		{"re\xffview", ""}, {"re\ufffdview", ""},
		{"glc_" + strings.Repeat("a", 24), ""},
		{"plugin:glc_" + strings.Repeat("a", 24), ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Validate(tt.name)
			if got != tt.want || ok != (tt.want != "") {
				t.Fatalf("Validate() = (%q, %v), want %q", got, ok, tt.want)
			}
		})
	}
}
