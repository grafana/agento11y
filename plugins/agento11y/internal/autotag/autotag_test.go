package autotag

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// stubUsername replaces the OS account lookup for one test so the last user
// fallback is testable on any machine.
func stubUsername(t *testing.T, name string) {
	t.Helper()
	prev := osUsername
	osUsername = func() string { return name }
	t.Cleanup(func() { osUsername = prev })
}

// checkout writes a git checkout fixture with an origin remote and a branch,
// the same file-only shape internal/gitbranch tests use.
func checkout(t *testing.T, name, remote, branch string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	writeFile(t, filepath.Join(root, ".git/config"), "[remote \"origin\"]\n\turl = "+remote+"\n")
	writeFile(t, filepath.Join(root, ".git/HEAD"), "ref: refs/heads/"+branch+"\n")
	return root
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// lookupFrom builds an envconfig.Lookup over a suffix->value map, the same
// shape doctor passes when it resolves from its pre-merge snapshot.
func lookupFrom(env map[string]string) envconfig.Lookup {
	return func(suffix string) (string, string, bool) {
		v := strings.TrimSpace(env[suffix])
		if v == "" {
			return "", "", false
		}
		return v, envconfig.PreferredKey(suffix), true
	}
}

func TestResolve(t *testing.T) {
	repoRemote := "git@github.com:grafana/agento11y.git"

	cases := []struct {
		name     string
		enabled  map[envconfig.AutoTag]bool
		env      map[string]string
		agentID  string
		osUser   string
		cwd      func(t *testing.T) string
		want     map[string]string
		shadowed []envconfig.AutoTag
	}{
		{
			name:   "no names enabled resolves nothing",
			osUser: "alex",
			env:    map[string]string{"USER_ID": "alice@example.com"},
			want:   nil,
		},
		{
			name:    "configured user id wins",
			enabled: map[envconfig.AutoTag]bool{envconfig.AutoTagUser: true},
			env:     map[string]string{"USER_ID": "alice@example.com"},
			agentID: "bob@example.com",
			osUser:  "alex",
			want:    map[string]string{"user": "alice@example.com"},
		},
		{
			name:    "agent user id is the second choice",
			enabled: map[envconfig.AutoTag]bool{envconfig.AutoTagUser: true},
			agentID: "bob@example.com",
			osUser:  "alex",
			want:    map[string]string{"user": "bob@example.com"},
		},
		{
			name:    "os account name is the last choice",
			enabled: map[envconfig.AutoTag]bool{envconfig.AutoTagUser: true},
			osUser:  "alex",
			want:    map[string]string{"user": "alex"},
		},
		{
			name:    "unresolved user leaves the key off",
			enabled: map[envconfig.AutoTag]bool{envconfig.AutoTagUser: true},
			osUser:  "",
			want:    nil,
		},
		{
			name:    "repo and branch come from the checkout",
			enabled: map[envconfig.AutoTag]bool{envconfig.AutoTagRepo: true, envconfig.AutoTagBranch: true},
			cwd: func(t *testing.T) string {
				return checkout(t, "agento11y", repoRemote, "feature/auto-tags")
			},
			want: map[string]string{"repo": "grafana/agento11y", "git.branch": "feature/auto-tags"},
		},
		{
			name:    "outside a checkout repo and branch are omitted",
			enabled: map[envconfig.AutoTag]bool{envconfig.AutoTagRepo: true, envconfig.AutoTagBranch: true},
			cwd:     func(t *testing.T) string { return t.TempDir() },
			want:    nil,
		},
		{
			name:    "all three names",
			enabled: map[envconfig.AutoTag]bool{envconfig.AutoTagUser: true, envconfig.AutoTagRepo: true, envconfig.AutoTagBranch: true},
			env:     map[string]string{"USER_ID": "alice@example.com"},
			cwd: func(t *testing.T) string {
				return checkout(t, "agento11y", repoRemote, "main")
			},
			want: map[string]string{
				"user":       "alice@example.com",
				"repo":       "grafana/agento11y",
				"git.branch": "main",
			},
		},
		{
			name:    "value is trimmed and capped",
			enabled: map[envconfig.AutoTag]bool{envconfig.AutoTagUser: true},
			agentID: "  " + strings.Repeat("a", 130) + "  ",
			want:    map[string]string{"user": strings.Repeat("a", 128)},
		},
		{
			name:    "explicit repo tag wins",
			enabled: map[envconfig.AutoTag]bool{envconfig.AutoTagRepo: true, envconfig.AutoTagBranch: true},
			env:     map[string]string{"TAGS": "repo=monorepo"},
			cwd: func(t *testing.T) string {
				return checkout(t, "agento11y", repoRemote, "main")
			},
			want:     map[string]string{"git.branch": "main"},
			shadowed: []envconfig.AutoTag{envconfig.AutoTagRepo},
		},
		{
			name:    "explicit branch tag wins",
			enabled: map[envconfig.AutoTag]bool{envconfig.AutoTagBranch: true},
			env:     map[string]string{"TAGS": "git.branch=release"},
			cwd: func(t *testing.T) string {
				return checkout(t, "agento11y", repoRemote, "main")
			},
			want:     nil,
			shadowed: []envconfig.AutoTag{envconfig.AutoTagBranch},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubUsername(t, tc.osUser)
			cwd := ""
			if tc.cwd != nil {
				cwd = tc.cwd(t)
			}
			in := Inputs{Cwd: cwd, UserID: tc.agentID, Lookup: lookupFrom(tc.env)}

			if got := Resolve(tc.enabled, in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Resolve() = %v, want %v", got, tc.want)
			}
			if got := Describe(tc.enabled, in).Shadowed; !reflect.DeepEqual(got, tc.shadowed) {
				t.Errorf("Describe().Shadowed = %v, want %v", got, tc.shadowed)
			}
		})
	}
}

// TestResolveReadsProcessEnvByDefault covers the nil-Lookup path every hook
// uses: without an injected lookup the resolver reads the branded families
// from the process environment, either spelling.
func TestResolveReadsProcessEnvByDefault(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	stubUsername(t, "alex")
	t.Setenv("SIGIL_USER_ID", "alice@example.com")

	got := Resolve(map[envconfig.AutoTag]bool{envconfig.AutoTagUser: true}, Inputs{})
	want := map[string]string{"user": "alice@example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve() = %v, want %v", got, want)
	}
}

// TestDescribeValuesReportUnsuppressedResolution pins what doctor prints: the
// values row shows what each name resolved to even when an explicit tag stops
// it from reaching the client.
func TestDescribeValuesReportUnsuppressedResolution(t *testing.T) {
	stubUsername(t, "alex")
	root := checkout(t, "agento11y", "git@github.com:grafana/agento11y.git", "main")

	res := Describe(
		map[envconfig.AutoTag]bool{envconfig.AutoTagRepo: true},
		Inputs{Cwd: root, Lookup: lookupFrom(map[string]string{"TAGS": "repo=monorepo"})},
	)
	if got := res.Values[envconfig.AutoTagRepo]; got != "grafana/agento11y" {
		t.Errorf("Values[repo] = %q, want %q", got, "grafana/agento11y")
	}
	if res.Tags != nil {
		t.Errorf("Tags = %v, want nil", res.Tags)
	}
}

// TestSelect pins the two variables against each other: the switch decides
// whether anything resolves, and the allowlist only narrows what the switch
// turned on. Every misconfiguration has to leave the mechanism off and say so
// in the log rather than stopping the hook.
func TestSelect(t *testing.T) {
	all := envconfig.AllAutoTags()

	cases := []struct {
		name         string
		env          map[string]string
		wantOn       bool
		wantEnabled  map[envconfig.AutoTag]bool
		wantUnknown  []string
		wantNamesSet bool
		wantLogged   string
		wantNoLog    bool
	}{
		{name: "switch unset resolves nothing", wantNoLog: true},
		{
			name:      "switch off resolves nothing",
			env:       map[string]string{envconfig.AutoTagsSuffix: "false"},
			wantNoLog: true,
		},
		{
			name:        "switch on takes every name",
			env:         map[string]string{envconfig.AutoTagsSuffix: "true"},
			wantOn:      true,
			wantEnabled: all,
			wantNoLog:   true,
		},
		{
			name: "the allowlist narrows the switch",
			env: map[string]string{
				envconfig.AutoTagsSuffix:     "1",
				envconfig.AutoTagNamesSuffix: " User , repo ",
			},
			wantOn:       true,
			wantEnabled:  map[envconfig.AutoTag]bool{envconfig.AutoTagUser: true, envconfig.AutoTagRepo: true},
			wantNamesSet: true,
			wantNoLog:    true,
		},
		{
			name: "an allowlist of all is the same as no allowlist",
			env: map[string]string{
				envconfig.AutoTagsSuffix:     "yes",
				envconfig.AutoTagNamesSuffix: "all",
			},
			wantOn:       true,
			wantEnabled:  all,
			wantNamesSet: true,
			wantNoLog:    true,
		},
		{
			name: "an unsupported name is logged and the rest still apply",
			env: map[string]string{
				envconfig.AutoTagsSuffix:     "true",
				envconfig.AutoTagNamesSuffix: "user,team",
			},
			wantOn:       true,
			wantEnabled:  map[envconfig.AutoTag]bool{envconfig.AutoTagUser: true},
			wantUnknown:  []string{"team"},
			wantNamesSet: true,
			wantLogged:   "unsupported names team",
		},
		{
			name: "an allowlist of only unsupported names turns nothing on",
			env: map[string]string{
				envconfig.AutoTagsSuffix:     "true",
				envconfig.AutoTagNamesSuffix: "team",
			},
			wantOn:       true,
			wantUnknown:  []string{"team"},
			wantNamesSet: true,
			wantLogged:   "names no supported value",
		},
		{
			name:         "an allowlist without the switch resolves nothing",
			env:          map[string]string{envconfig.AutoTagNamesSuffix: "user"},
			wantNamesSet: true,
			wantLogged:   "is off",
		},
		{
			name: "a list in the switch is not a boolean and is redirected",
			env:  map[string]string{envconfig.AutoTagsSuffix: "user,repo"},
			// The value names what the user wanted, so the message has to point at
			// the variable that takes names.
			wantLogged: envconfig.PreferredKey(envconfig.AutoTagNamesSuffix),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			sel := Select(lookupFrom(tc.env), log.New(&logged, "", 0))

			if sel.On != tc.wantOn {
				t.Errorf("On = %v, want %v", sel.On, tc.wantOn)
			}
			if !reflect.DeepEqual(sel.Enabled, tc.wantEnabled) {
				t.Errorf("Enabled = %v, want %v", sel.Enabled, tc.wantEnabled)
			}
			if !reflect.DeepEqual(sel.Unknown, tc.wantUnknown) {
				t.Errorf("Unknown = %v, want %v", sel.Unknown, tc.wantUnknown)
			}
			if sel.NamesSet != tc.wantNamesSet {
				t.Errorf("NamesSet = %v, want %v", sel.NamesSet, tc.wantNamesSet)
			}
			switch {
			case tc.wantNoLog && logged.Len() > 0:
				t.Errorf("logged %q, want nothing", logged.String())
			case tc.wantLogged != "" && !strings.Contains(logged.String(), tc.wantLogged):
				t.Errorf("logged %q, want it to name %q", logged.String(), tc.wantLogged)
			}
		})
	}
}

// TestFromEnv drives the call every client-construction path makes, over the
// process environment rather than an injected lookup. The switch is off by
// default, so the cases that leave it unset or blank pin that a session which
// did not opt in keeps exactly the tags it had before.
func TestFromEnv(t *testing.T) {
	cases := []struct {
		name        string
		autoTags    string
		names       string
		userID      string
		want        map[string]string
		wantLogged  string
		wantNoLogln bool
	}{
		{
			name:        "switch unset resolves nothing",
			userID:      "bob@example.com",
			wantNoLogln: true,
		},
		{
			name:        "switch blank resolves nothing",
			autoTags:    "   ",
			userID:      "bob@example.com",
			wantNoLogln: true,
		},
		{
			name:        "switch off resolves nothing",
			autoTags:    "off",
			userID:      "bob@example.com",
			wantNoLogln: true,
		},
		{
			// The working directory is not a checkout, so `user` is the only name
			// with a value; that is what makes the resolved map comparable here.
			name:        "switch on takes every name",
			autoTags:    "true",
			userID:      "bob@example.com",
			want:        map[string]string{"user": "bob@example.com"},
			wantNoLogln: true,
		},
		{
			name:        "the allowlist narrows the switch",
			autoTags:    "true",
			names:       "user",
			userID:      "bob@example.com",
			want:        map[string]string{"user": "bob@example.com"},
			wantNoLogln: true,
		},
		{
			name:       "only unknown names resolve nothing but are logged",
			autoTags:   "true",
			names:      "team",
			userID:     "bob@example.com",
			wantLogged: "team",
		},
		{
			name:       "recognized name resolves while the unknown one is logged",
			autoTags:   "true",
			names:      "user,team",
			userID:     "bob@example.com",
			want:       map[string]string{"user": "bob@example.com"},
			wantLogged: "team",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			stubUsername(t, "alex")
			// Outside a checkout, so `repo` and `branch` resolve to nothing however
			// the machine running the test is laid out.
			t.Chdir(t.TempDir())
			if tc.autoTags != "" {
				t.Setenv(envconfig.PreferredKey(envconfig.AutoTagsSuffix), tc.autoTags)
			}
			if tc.names != "" {
				t.Setenv(envconfig.PreferredKey(envconfig.AutoTagNamesSuffix), tc.names)
			}

			var logged bytes.Buffer
			got := FromEnv(Inputs{UserID: tc.userID}, log.New(&logged, "", 0))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("FromEnv() = %v, want %v", got, tc.want)
			}
			switch {
			case tc.wantNoLogln && logged.Len() > 0:
				t.Errorf("logged %q, want nothing", logged.String())
			case tc.wantLogged != "" && !strings.Contains(logged.String(), tc.wantLogged):
				t.Errorf("logged %q, want it to name %q", logged.String(), tc.wantLogged)
			}
		})
	}
}

// TestFromEnvFallsBackToWorkingDirectory covers the callers that have no
// workspace root on their payload (guard): with an empty Inputs.Cwd the
// repository and branch come from the directory the host agent started the
// hook in.
func TestFromEnvFallsBackToWorkingDirectory(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	t.Setenv(envconfig.PreferredKey(envconfig.AutoTagsSuffix), "true")
	t.Setenv(envconfig.PreferredKey(envconfig.AutoTagNamesSuffix), "repo,branch")
	root := checkout(t, "agento11y", "git@github.com:grafana/agento11y.git", "main")
	t.Chdir(root)

	got := FromEnv(Inputs{}, nil)
	want := map[string]string{"repo": "grafana/agento11y", "git.branch": "main"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FromEnv() = %v, want %v", got, want)
	}
}

// TestTagKey pins the key each name is written under; git.branch in
// particular is shared with the per-generation built-in.
func TestTagKey(t *testing.T) {
	for name, want := range map[envconfig.AutoTag]string{
		envconfig.AutoTagUser:     "user",
		envconfig.AutoTagRepo:     "repo",
		envconfig.AutoTagBranch:   "git.branch",
		envconfig.AutoTag("nope"): "",
	} {
		if got := TagKey(name); got != want {
			t.Errorf("TagKey(%q) = %q, want %q", name, got, want)
		}
	}
}
