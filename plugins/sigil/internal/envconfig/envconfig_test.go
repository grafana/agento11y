package envconfig

import (
	"reflect"
	"testing"
)

func TestParseBool(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"true", true},
		{"TRUE", true},
		{"1", true},
		{"yes", true},
		{"on", true},
		{" true ", true},
		{"false", false},
		{"0", false},
		{"", false},
		{"random", false},
	}
	for _, tc := range cases {
		if got := ParseBool(tc.in); got != tc.want {
			t.Errorf("ParseBool(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("SIGIL_TEST_PRESENT", "present")
	t.Setenv("SIGIL_TEST_EMPTY", "")
	if got := EnvOr("SIGIL_TEST_PRESENT", "fallback"); got != "present" {
		t.Errorf("EnvOr(present) = %q, want %q", got, "present")
	}
	if got := EnvOr("SIGIL_TEST_EMPTY", "fallback"); got != "fallback" {
		t.Errorf("EnvOr(empty) = %q, want %q", got, "fallback")
	}
	if got := EnvOr("SIGIL_TEST_MISSING", "fallback"); got != "fallback" {
		t.Errorf("EnvOr(missing) = %q, want %q", got, "fallback")
	}
}

func TestMissingEnvVars(t *testing.T) {
	order := []string{"A", "B", "C"}
	vars := map[string]string{"A": "x", "B": "", "C": "y"}
	got := MissingEnvVars(order, vars)
	want := []string{"B"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MissingEnvVars = %v, want %v", got, want)
	}
}

func TestParseExtraTags(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]string
	}{
		{"", nil},
		{"  ", nil},
		{"a=1", map[string]string{"a": "1"}},
		{"a=1,b=2", map[string]string{"a": "1", "b": "2"}},
		{"a=1, b=2 ", map[string]string{"a": "1", "b": "2"}},
		{"a=,b=2", map[string]string{"b": "2"}},
		{"=1,b=2", map[string]string{"b": "2"}},
		{"justakey", nil},
	}
	for _, tc := range cases {
		got := ParseExtraTags(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseExtraTags(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
