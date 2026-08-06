// Package autotag resolves the client tags a session opts into with
// AGENTO11Y_AUTO_CODING_AGENT_TAGS. The launcher already knows who is running
// it, which repository the session is in, and which branch is checked out;
// this package turns the enabled names into client tags so those values reach
// OTel metrics.
//
// Two variables drive it. AGENTO11Y_AUTO_CODING_AGENT_TAGS is the on/off
// switch and is off by default. AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES is an
// optional allowlist that narrows the switch to some of the supported names;
// with the switch on and no list, every name applies.
//
// Client tags are the only tag mechanism that becomes a metric label
// (docs/concepts/tags-and-metadata.md), which is why the resolved values are
// attached here rather than as per-generation tags. Nothing resolves until the
// switch is on: with the switch off, Resolve reads no files, runs no lookups,
// and returns nil.
package autotag

import (
	"log"
	"os"
	"os/user"
	"strings"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/gitbranch"
)

// Tag keys the enabled names are written under. The branch reuses the
// per-generation built-in key `git.branch` so the export carries one branch
// key: the built-in wins in the generation export, and this client tag
// supplies the metric label.
const (
	KeyUser   = "user"
	KeyRepo   = "repo"
	KeyBranch = "git.branch"
)

// MaxValueLen caps a resolved value at 128 Unicode code points. Every value
// becomes a Prometheus label, so an accidentally long branch name or email
// stays bounded. Truncation keeps the start of the value.
const MaxValueLen = 128

// osUsername returns the login name of the account running the process.
// Indirected through a variable so tests can drive the last user fallback
// without depending on the machine they run on.
var osUsername = func() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.Username
}

// Inputs carry what the caller already knows. Everything else is resolved
// here.
type Inputs struct {
	// Cwd is the workspace root used for repository and branch resolution.
	// Empty falls back to the process working directory, which is the
	// directory the host agent started the hook in.
	Cwd string
	// UserID is the identity the agent adapter already resolved, if any. It
	// is the fallback below the configured USER_ID and above the OS account
	// name.
	UserID string
	// Lookup reads branded variables. nil uses the process environment;
	// doctor passes a lookup backed by its pre-merge snapshot so it reports
	// the values the hooks resolve.
	Lookup envconfig.Lookup
}

// Result is the full picture of one resolution, for callers that report on it
// rather than just consuming the tags.
type Result struct {
	// Tags are the client tags to pass to the SDK: resolved values, minus any
	// key an explicit AGENTO11Y_TAGS entry already defines. nil when empty.
	Tags map[string]string
	// Values is the resolved value per enabled name before explicit tags are
	// applied. A name that resolved to nothing is absent.
	Values map[envconfig.AutoTag]string
	// Shadowed lists enabled names whose key AGENTO11Y_TAGS already defines,
	// in AutoTagOrder. The explicit value wins, so these are not in Tags.
	Shadowed []envconfig.AutoTag
}

// TagKey returns the client tag key an auto-tag name is written under.
func TagKey(t envconfig.AutoTag) string {
	switch t {
	case envconfig.AutoTagUser:
		return KeyUser
	case envconfig.AutoTagRepo:
		return KeyRepo
	case envconfig.AutoTagBranch:
		return KeyBranch
	default:
		return ""
	}
}

// Selection is what the two AUTO_CODING_AGENT_TAGS variables asked for.
type Selection struct {
	// On reports that the switch holds a true value. It can be true with an
	// empty Enabled set, when the allowlist named nothing supported.
	On bool
	// Enabled is the set of names to resolve: every supported name, unless the
	// allowlist narrows it.
	Enabled map[envconfig.AutoTag]bool
	// Unknown lists allowlist entries that name no supported value, lowercased.
	Unknown []string
	// NamesSet reports that the allowlist variable held a value. It separates a
	// selection narrowed to nothing from one that was never narrowed.
	NamesSet bool
}

// Select reads the switch and the allowlist and reports which names to
// resolve. look reads branded variables; nil uses the process environment.
//
// Every problem is logged and none stops the caller: a switch value that is
// not a boolean leaves the mechanism off, an unsupported name is skipped, and
// an allowlist set while the switch is off attaches nothing. Hooks must not
// write to stderr, so pass the adapter logger (nil stays silent).
func Select(look envconfig.Lookup, logger *log.Logger) Selection {
	if look == nil {
		look = envconfig.LookupEnv
	}
	namesRaw, namesKey, namesSet := look(envconfig.AutoTagNamesSuffix)
	sel := Selection{NamesSet: namesSet}

	raw, key, ok := look(envconfig.AutoTagsSuffix)
	if ok {
		on, valid := envconfig.ParseBoolValue(raw)
		if !valid && logger != nil {
			logger.Printf("config: invalid %s=%q; expected a boolean, and the names go in %s",
				key, raw, envconfig.PreferredKey(envconfig.AutoTagNamesSuffix))
		}
		sel.On = on
	}
	if !sel.On {
		if namesSet && logger != nil {
			logger.Printf("config: %s is set but %s is off, so no automatic tags are attached",
				namesKey, envconfig.PreferredKey(envconfig.AutoTagsSuffix))
		}
		return sel
	}
	if !namesSet {
		sel.Enabled = envconfig.AllAutoTags()
		return sel
	}
	sel.Enabled, sel.Unknown = envconfig.ParseAutoTags(namesRaw)
	if logger != nil {
		if len(sel.Unknown) > 0 {
			logger.Printf("config: %s has unsupported names %s; supported: %s",
				namesKey, strings.Join(sel.Unknown, ", "), SupportedNames())
		}
		if len(sel.Enabled) == 0 {
			logger.Printf("config: %s names no supported value, so no automatic tags are attached", namesKey)
		}
	}
	return sel
}

// SupportedNames lists the values the allowlist accepts, for the message that
// reports a rejected one.
func SupportedNames() string {
	names := make([]string, 0, len(envconfig.AutoTagOrder)+1)
	for _, name := range envconfig.AutoTagOrder {
		names = append(names, string(name))
	}
	return strings.Join(append(names, envconfig.AutoTagAll), ", ")
}

// FromEnv is the one call a client-construction path makes: it reads the
// AUTO_CODING_AGENT_TAGS family, resolves whatever it enables, and returns the
// client tags to hand to the SDK. It returns nil when the switch is off, so a
// session that has not opted in carries exactly the tags it carried before.
//
// Hooks must not write to stderr, so pass the adapter logger (nil stays
// silent); Select documents what gets logged.
//
// Replayed sessions must not take these values: the importer runs on a
// different day, possibly in a different checkout, so the current user,
// repository and branch do not describe the session it is importing. That is
// why internal/history/export.go does not call this.
func FromEnv(in Inputs, logger *log.Logger) map[string]string {
	if in.Lookup == nil {
		in.Lookup = envconfig.LookupEnv
	}
	sel := Select(in.Lookup, logger)
	if len(sel.Enabled) == 0 {
		return nil
	}
	return Resolve(sel.Enabled, in)
}

// Resolve returns the client tags for the enabled names. Values are trimmed
// and capped at MaxValueLen; a name that resolves to nothing leaves its key
// off. A key already set in AGENTO11Y_TAGS is left off too, because the SDK
// merges caller tags over environment tags and an explicit tag must win.
// Returns nil when nothing resolves.
func Resolve(enabled map[envconfig.AutoTag]bool, in Inputs) map[string]string {
	return Describe(enabled, in).Tags
}

// Describe resolves the enabled names and reports what each one produced,
// including the names an explicit tag shadows. `agento11y doctor` uses it to
// show the exact strings before they leave the machine; hooks use Resolve.
func Describe(enabled map[envconfig.AutoTag]bool, in Inputs) Result {
	if len(enabled) == 0 {
		return Result{}
	}
	look := in.Lookup
	if look == nil {
		look = envconfig.LookupEnv
	}
	if in.Cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			in.Cwd = wd
		}
	}

	res := Result{Values: make(map[envconfig.AutoTag]string, len(enabled))}
	for _, name := range envconfig.AutoTagOrder {
		if !enabled[name] {
			continue
		}
		if v := clean(resolveOne(name, in, look)); v != "" {
			res.Values[name] = v
		}
	}
	if len(res.Values) == 0 {
		return Result{}
	}

	tagsValue, _, _ := look("TAGS")
	explicit := envconfig.ParseExtraTags(tagsValue)
	tags := make(map[string]string, len(res.Values))
	for _, name := range envconfig.AutoTagOrder {
		value, ok := res.Values[name]
		if !ok {
			continue
		}
		key := TagKey(name)
		if _, taken := explicit[key]; taken {
			res.Shadowed = append(res.Shadowed, name)
			continue
		}
		tags[key] = value
	}
	if len(tags) > 0 {
		res.Tags = tags
	}
	return res
}

func resolveOne(name envconfig.AutoTag, in Inputs, look envconfig.Lookup) string {
	switch name {
	case envconfig.AutoTagUser:
		return resolveUser(in, look)
	case envconfig.AutoTagRepo:
		return gitbranch.Repo(in.Cwd)
	case envconfig.AutoTagBranch:
		return gitbranch.Resolve(in.Cwd)
	default:
		return ""
	}
}

// resolveUser prefers the identity the user configured, then the one the agent
// adapter resolved from its own session data, then the OS account name.
func resolveUser(in Inputs, look envconfig.Lookup) string {
	if v, _, ok := look("USER_ID"); ok {
		return v
	}
	if v := strings.TrimSpace(in.UserID); v != "" {
		return v
	}
	return osUsername()
}

func clean(v string) string {
	v = strings.TrimSpace(v)
	runes := []rune(v)
	if len(runes) > MaxValueLen {
		return string(runes[:MaxValueLen])
	}
	return v
}
