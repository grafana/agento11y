package entry

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agentinstall"
	"github.com/grafana/agento11y/plugins/agento11y/internal/history"
)

func TestPublicHelpPages(t *testing.T) {
	paths := []string{"", "help", "login", "doctor", "guards", "guards test", "agents", "agents reconcile", "cursor", "cursor install", "cursor uninstall", "local", "local start", "local open", "local status", "local stop", "local restart", "history", "history import", "skills", "skills list", "skills show", "skills get", "claude eval", "claude eval import", "claude install", "copilot install", "opencode install", "pi install"}
	for name := range launchers {
		paths = append(paths, name)
	}
	for _, spec := range history.Specs() {
		for _, name := range append([]string{string(spec.ID)}, spec.Aliases...) {
			paths = append(paths, "history import "+name)
		}
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			page, ok := helpPage(path)
			if !ok || page.Summary == "" || len(page.Usage) == 0 {
				t.Fatalf("missing help page for %q", path)
			}
			var out bytes.Buffer
			printHelp(path, &out)
			if !strings.Contains(out.String(), page.Command) || !strings.Contains(out.String(), page.Summary) || !strings.Contains(out.String(), "Usage:") {
				t.Fatalf("incomplete help: %s", out.String())
			}
			if path == "login" {
				for _, want := range []string{
					"access-policy token with the sigil:write scope",
					"requires --endpoint and --tenant. Cannot be combined with --token.",
					"Prompt for missing values. Verify credentials before saving unless --no-verify is set.",
				} {
					if !strings.Contains(out.String(), want) {
						t.Errorf("missing login guidance %q in %q", want, out.String())
					}
				}
			}
		})
	}
	for _, path := range []string{"local serve", "codex install", "vibe install", "history search", "history show", "cursor hook"} {
		if _, ok := helpPage(path); ok {
			t.Errorf("unexpected public page for %q", path)
		}
	}
}

type unreadableHelpInput struct{}

func (unreadableHelpInput) Read([]byte) (int, error) { panic("help consumed stdin") }

func TestHelpRouting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("AGENTO11Y_DEBUG", "true")
	t.Setenv("AGENTO11Y_TAGS", "original=value")
	configDir := filepath.Join(home, "agento11y")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.env"), []byte("AGENTO11Y_AUTH_TOKEN=help-must-not-load-this\nAGENTO11Y_LOCAL=true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before := os.Environ()
	for path := range publicHelpPages() {
		for _, form := range []string{"--help", "-h", "topic"} {
			t.Run(path+"/"+form, func(t *testing.T) {
				args := strings.Fields(path)
				if form == "topic" {
					args = append([]string{"help"}, args...)
				} else {
					args = append(args, form)
				}
				var out, errOut bytes.Buffer
				code := withExit(t, func() { run(args, unreadableHelpInput{}, &out, &errOut) })
				// guards owns its parser and exits with the command result, including
				// for help. Other command parsers return after routeHelp handles help.
				wantExit := form != "topic" && (path == "guards" || path == "guards test")
				if (code != nil) != wantExit || code != nil && *code != 0 || errOut.Len() != 0 || !strings.Contains(out.String(), "Usage:") {
					t.Fatalf("exit=%v stdout=%q stderr=%q", code, out.String(), errOut.String())
				}
			})
		}
	}
	if !reflect.DeepEqual(before, os.Environ()) {
		t.Fatal("help changed environment")
	}
	if _, err := os.Stat(filepath.Join(home, "state")); !os.IsNotExist(err) {
		t.Fatalf("help created state: %v", err)
	}
}

func TestHelpArgumentOwnership(t *testing.T) {
	for _, tt := range []struct {
		name    string
		args    []string
		handled bool
		want    string
	}{
		{"login token", []string{"login", "--token", "--help"}, false, ""},
		{"history value", []string{"history", "import", "claude-code", "--workspace", "--help"}, false, ""},
		{"eval value", []string{"claude", "eval", "import", "missing.json", "--name", "--help"}, false, ""},
		{"eval file help", []string{"claude", "eval", "import", "missing.json", "--help"}, true, "Usage:"},
		{"launcher forwarded", []string{"pi", "--", "--help"}, false, ""},
		{"launcher tag value", []string{"pi", "--tag", "--help"}, true, "invalid --tag"},
		{"launcher help", []string{"pi", "--local", "--help"}, true, "Usage:"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			var handled bool
			code := withExit(t, func() { handled = routeHelp(tt.args, &out, &errOut) })
			if code != nil {
				handled = true
			}
			if handled != tt.handled || !strings.Contains(out.String()+errOut.String(), tt.want) {
				t.Fatalf("handled=%v stdout=%q stderr=%q", handled, out.String(), errOut.String())
			}
		})
	}
}

func TestUsageAndEmptyGroups(t *testing.T) {
	for _, path := range []string{"", "local", "history", "skills", "agents", "cursor", "claude eval"} {
		t.Run("empty/"+path, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := withExit(t, func() { run(strings.Fields(path), unreadableHelpInput{}, &out, &errOut) })
			if code != nil || errOut.Len() != 0 || !strings.Contains(out.String(), "Usage:") {
				t.Fatalf("exit=%v stdout=%q stderr=%q", code, out.String(), errOut.String())
			}
		})
	}
	for _, tt := range []struct {
		args []string
		path string
	}{
		{[]string{"skills", "show"}, "skills show"},
		{[]string{"skills", "list", "extra"}, "skills list"},
		{[]string{"skills", "list", "--bogus"}, "skills list"},
		{[]string{"history", "import"}, "history import"},
		{[]string{"claude", "eval", "import"}, "claude eval import"},
		{[]string{"agents", "reconcile"}, "agents reconcile"},
		{[]string{"local", "start", "extra"}, "local start"},
		{[]string{"local", "open", "extra"}, "local open"},
		{[]string{"local", "stop", "extra"}, "local stop"},
		{[]string{"local", "restart", "extra"}, "local restart"},
		{[]string{"cursor", "install", "extra"}, "cursor install"},
		{[]string{"cursor", "uninstall", "extra"}, "cursor uninstall"},
		{[]string{"help", "local", "missing"}, "local"},
		{[]string{"help", "history", "import", "missing"}, "history import"},
		{[]string{"help", "nonsense"}, ""},
		{[]string{"help", "hook"}, ""},
		{[]string{"help", "hook", "--help"}, ""},
		{[]string{"login", "--token-stdin"}, "login"},
	} {
		t.Run(strings.Join(tt.args, "/"), func(t *testing.T) {
			home := isolateDotenvHome(t)
			var out, errOut bytes.Buffer
			code := withExit(t, func() { run(tt.args, unreadableHelpInput{}, &out, &errOut) })
			command := strings.TrimSpace("agento11y " + tt.path)
			if code == nil || *code != 2 || out.Len() != 0 || !strings.Contains(errOut.String(), "usage: "+command) || !strings.Contains(errOut.String(), "`"+command+" --help`") {
				t.Fatalf("exit=%v stdout=%q stderr=%q", code, out.String(), errOut.String())
			}
			if tt.args[0] == "help" && !strings.Contains(errOut.String(), "unknown help topic") {
				t.Fatalf("missing topic diagnostic: %q", errOut.String())
			}
			if strings.Count(errOut.String(), "\n") != 3 {
				t.Fatalf("duplicate diagnostics: %q", errOut.String())
			}
			if _, err := os.Stat(filepath.Join(home, "state")); !os.IsNotExist(err) {
				t.Fatalf("misuse created state: %v", err)
			}
		})
	}
}

func TestHelpRegistryListings(t *testing.T) {
	if os.Getenv("AGENTO11Y_HELP_REGISTRY_TEST") == "1" {
		history.Register(history.AgentSpec{ID: "help-fixture", DisplayName: "Help Fixture", Aliases: []string{"help-alias"}}, func() history.Importer { t.Fatal("help created an importer"); return nil })
		var out bytes.Buffer
		printHelp("history import", &out)
		for _, name := range []string{"help-fixture", "help-alias"} {
			if !strings.Contains(out.String(), name) {
				t.Fatalf("missing registered name: %s", out.String())
			}
			if _, ok := helpPage("history import " + name); !ok {
				t.Fatalf("missing page for %s", name)
			}
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestHelpRegistryListings$")
	cmd.Env = append(os.Environ(), "AGENTO11Y_HELP_REGISTRY_TEST=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("registry subprocess: %v\n%s", err, out)
	}
	old := registeredInstallers
	registeredInstallers = func() []agentinstall.Spec { return []agentinstall.Spec{{Name: "test-installer"}} }
	t.Cleanup(func() { registeredInstallers = old })
	var out bytes.Buffer
	printHelp("agents reconcile", &out)
	if !strings.Contains(out.String(), "test-installer") {
		t.Fatal(out.String())
	}
	out.Reset()
	printHelp("history import", &out)
	for _, spec := range history.Specs() {
		for _, name := range append([]string{string(spec.ID)}, spec.Aliases...) {
			if !strings.Contains(out.String(), name) {
				t.Errorf("missing importer %q", name)
			}
		}
	}
}
