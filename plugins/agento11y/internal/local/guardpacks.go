package local

import (
	"fmt"
	"maps"
	"strings"

	"github.com/grafana/agento11y/plugins/agento11y/internal/guardeval"
	"github.com/grafana/agento11y/plugins/agento11y/internal/redact"
)

// Guard packs are named local rulesets the Settings → Local page toggles.
// Each pack is one rule with id pack.<id>, so turning it off removes that
// rule and leaves hand-written siblings alone.

const packRulePrefix = "pack."

const (
	packSecrets     = "secrets"
	packFiles       = "files"
	packGit         = "git"
	packDestructive = "destructive"
	packPermissions = "permissions"
	packDisk        = "disk"
	packKindRedact  = "redact"
	packKindDeny    = "deny"
)

// guardPack is one toggle on the Local tab.
type guardPack struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Kind        string   `json:"kind"`
	Detail      string   `json:"detail"`
	Preview     []string `json:"preview"`
	Enabled     bool     `json:"enabled"`
}

func packRuleID(id string) string {
	return packRulePrefix + id
}

func packIDFromRule(ruleID string) (string, bool) {
	id := strings.TrimSpace(ruleID)
	if !strings.HasPrefix(id, packRulePrefix) {
		return "", false
	}
	rest := strings.TrimPrefix(id, packRulePrefix)
	if rest == "" || strings.Contains(rest, ".") {
		return "", false
	}
	return rest, true
}

func catalogPacks() []guardPack {
	secretIDs := make([]string, 0, len(redact.Tier1Specs()))
	for _, spec := range redact.Tier1Specs() {
		secretIDs = append(secretIDs, spec.ID)
	}
	return []guardPack{
		{
			ID:          packSecrets,
			Title:       "Secret redaction",
			Description: "Redact API keys, tokens, and private keys in captured tool calls.",
			Kind:        packKindRedact,
			Detail:      fmt.Sprintf("%d common secret formats", len(secretIDs)),
			Preview:     secretIDs,
		},
		{
			ID:          packFiles,
			Title:       "File safety",
			Description: "Block reading and writing .env files across file and shell tools.",
			Kind:        packKindDeny,
			Detail:      ".env and .env.*",
			Preview:     filesPreview,
		},
		{
			ID:          packGit,
			Title:       "Git safety",
			Description: "Block git commands that throw away uncommitted work or rewrite history.",
			Kind:        packKindDeny,
			Detail:      fmt.Sprintf("%d git commands", len(gitPreview)),
			Preview:     gitPreview,
		},
		{
			ID:          packDestructive,
			Title:       "Destructive commands",
			Description: "Block recursive deletes such as rm -rf across shell tools.",
			Kind:        packKindDeny,
			Detail:      "rm -rf and rm -fr",
			Preview:     []string{"rm -rf", "rm -fr"},
		},
		{
			ID:          packPermissions,
			Title:       "Permissions",
			Description: "Block recursive chmod/chown and chmod 777 on / or $HOME.",
			Kind:        packKindDeny,
			Detail:      "chmod/chown -R and chmod 777 on / or $HOME",
			Preview:     permissionsPreview,
		},
		{
			ID:          packDisk,
			Title:       "Disk wipe",
			Description: "Block irreversible disk operations: dd to devices, mkfs, wipefs, fdisk, and parted.",
			Kind:        packKindDeny,
			Detail:      "dd of=/dev, mkfs, wipefs, fdisk, parted",
			Preview:     diskPreview,
		},
	}
}

var gitPreview = []string{
	"git reset --hard",
	"git push --force",
	"git push -f",
	"git clean -f",
	"git checkout --",
	"git stash drop",
	"git stash clear",
	"git branch -D",
}

var gitPatterns = []string{
	gitCommandPrefix + `reset` + shellOptionGap + shellQuoted(`--hard`) + shellArgumentEnd,
	gitCommandPrefix + `push` + shellOptionGap + shellQuoted(`(?:--force|-[a-z]*f[a-z]*)`) + shellArgumentEnd,
	gitCommandPrefix + `clean` + gitCleanOptionGap + shellQuoted(`(?:--force|-[diqnxX]*f[a-z]*)`) + shellArgumentEnd,
	gitCommandPrefix + `checkout` + shellOptionGap + shellQuoted(`--`) + shellArgumentEnd,
	gitCommandPrefix + `stash[ \t]+(?:drop|clear)` + shellArgumentEnd,
	gitCommandPrefix + `branch` + shellOptionGap + shellQuoted(`(?-i:-[a-zA-Z]*D[a-zA-Z]*)`) + shellArgumentEnd,
}

var destructivePatterns = []string{
	`(?i)\brm` + shellOptionGap + shellQuoted(`-[a-z]*(?:r[a-z]*f|f[a-z]*r)[a-z]*`) + shellArgumentEnd,
	`(?i)\brm` + shellOptionGap + shellQuoted(rmRecursiveFlag) + shellOptionGap + shellQuoted(rmForceFlag) + shellArgumentEnd,
	`(?i)\brm` + shellOptionGap + shellQuoted(rmForceFlag) + shellOptionGap + shellQuoted(rmRecursiveFlag) + shellArgumentEnd,
}

var permissionsPreview = []string{
	"chmod -R … /",
	"chmod -R … ~",
	"chmod -R … $HOME",
	"chown -R … /",
	"chown -R … ~",
	"chown -R … $HOME",
	"chmod 777 /",
	"chmod 777 ~",
	"chmod 777 $HOME",
}

// pathRootOrHome matches /, ~, or $HOME, including home paths ending in / or /*
// and the root glob /*. It does not match /tmp or a project under $HOME.
// Quoted tildes and single-quoted variables do not expand to the home directory.
const pathRootOrHome = `(?:/\*?|(?:~|\$HOME|\$\{HOME\})(?:/\*?)?|'/'|"(?:/|\$(?:HOME|\{HOME\})/?)")` + shellArgumentEnd

var permissionsPatterns = []string{
	`(?i:\bchmod)` + shellOptionGap + shellQuoted(`(?:--recursive|-[a-zA-Z]*R[a-zA-Z]*)`) + shellArgumentGap + pathRootOrHome,
	`(?i:\bchown)` + shellOptionGap + shellQuoted(`(?:--recursive|-[a-zA-Z]*R[a-zA-Z]*)`) + shellArgumentGap + pathRootOrHome,
	`(?i:\bchmod)` + shellOptionGap + shellQuoted(`777`) + shellArgumentGap + pathRootOrHome,
}

var diskPreview = []string{
	"dd of=/dev/…",
	"mkfs",
	"mkfs.ext4 and other mkfs.*",
	"wipefs",
	"fdisk",
	"parted",
}

var diskPatterns = []string{
	`(?i)(?:\bdd|'dd'|"dd")` + shellArgumentGap + `(?:of=["']?/dev/|["']of=/dev/)`,
	`(?i)\bmkfs(\.[A-Za-z0-9]+)?\b`,
	`(?i)\bwipefs\b`,
	`(?i)\bfdisk\b`,
	`(?i)\bparted\b`,
}

var filesPreview = []string{
	".env",
	".env.local",
	".env.*",
}

// envFileToolNames are the host spellings File safety matches with a qualified
// glob, so a .env path is blocked whether the call is a Read, a Write, or a
// shell cat. Matching is case-insensitive at compile time.
var envFileToolNames = []string{
	"Bash", "shell", "run_terminal_cmd", "execute_command", "run_command",
	"Write", "Edit", "MultiEdit", "str_replace_editor", "apply_patch", "create_file",
	"Read", "read_file", "view_file",
}

func envFileBlockedNames() []string {
	out := make([]string, 0, len(envFileToolNames)+1)
	for _, name := range envFileToolNames {
		out = append(out, name+`(*[/"' <>|;&()].env[."' <>|;&()]*)`)
		if name == "apply_patch" {
			// Patch header delimiters are JSON-escaped; file tools can use them inside basenames.
			out = append(out, name+`(*[/"' <>|;&()].env\\[nrt]*)`)
		}
	}
	return out
}

func packByID() map[string]guardPack {
	out := make(map[string]guardPack, 6)
	for _, p := range catalogPacks() {
		out[p.ID] = p
	}
	return out
}

func packRule(id string) (guardeval.Rule, error) {
	switch id {
	case packSecrets:
		return secretsPackRule(), nil
	case packFiles:
		return filesPackRule(), nil
	case packGit:
		return denyShellRule(packGit, 20, gitPatterns), nil
	case packDestructive:
		return denyShellRule(packDestructive, 30, destructivePatterns), nil
	case packPermissions:
		return denyShellRule(packPermissions, 40, permissionsPatterns), nil
	case packDisk:
		return denyShellRule(packDisk, 50, diskPatterns), nil
	default:
		return guardeval.Rule{}, fmt.Errorf("unknown pack %q", id)
	}
}

func secretsPackRule() guardeval.Rule {
	specs := redact.Tier1Specs()
	patterns := make([]guardeval.TransformPattern, 0, len(specs))
	for _, spec := range specs {
		patterns = append(patterns, guardeval.TransformPattern{ID: spec.ID, Regex: spec.Regex})
	}
	return guardeval.Rule{
		RuleID:   packRuleID(packSecrets),
		Phase:    "postflight",
		Priority: 10,
		Transform: &guardeval.TransformConfig{
			Patterns: patterns,
			JSONMode: "strings",
		},
	}
}

func filesPackRule() guardeval.Rule {
	return guardeval.Rule{
		RuleID:       packRuleID(packFiles),
		Phase:        "postflight",
		Priority:     15,
		ActionOnFail: "deny",
		ToolFilter: &guardeval.ToolFilterConfig{
			BlockedNames: envFileBlockedNames(),
		},
		Evaluators: []guardeval.EvaluatorSpec{{
			Kind: "regex",
			Config: map[string]any{
				"target": "shell_command",
				"reject": true,
				"patterns": []any{
					`(?i)(^|[/'"[:space:]<>;|&()])\.env($|['"[:space:].<>;|&()])`,
				},
			},
		}},
	}
}

func denyShellRule(id string, priority int, patterns []string) guardeval.Rule {
	list := make([]any, 0, len(patterns))
	for _, p := range patterns {
		for _, pattern := range shellWrappedPatterns(p) {
			list = append(list, pattern)
		}
	}
	return guardeval.Rule{
		RuleID:       packRuleID(id),
		Phase:        "postflight",
		Priority:     priority,
		ActionOnFail: "deny",
		Evaluators: []guardeval.EvaluatorSpec{{
			Kind: "regex",
			Config: map[string]any{
				"target":   "shell_command",
				"reject":   true,
				"patterns": list,
			},
		}},
	}
}

func packsFromRules(rules []guardeval.Rule) []guardPack {
	on := map[string]bool{}
	for _, r := range rules {
		if id, ok := packIDFromRule(r.RuleID); ok {
			on[id] = on[id] || r.Enabled == nil || *r.Enabled
		}
	}
	out := catalogPacks()
	for i := range out {
		out[i].Enabled = on[out[i].ID]
	}
	return out
}

// applyPackUpdates turns packs on or off against an existing ruleset. Keys
// not in updates keep their current on/off state, including enabled=false.
// Only unchanged shipped packs are refreshed from the current catalog after a
// binary upgrade. Custom rules are never removed, including pack.* ids this
// binary does not know (a newer pack, or a hand-named rule).
func applyPackUpdates(existing []guardeval.Rule, updates map[string]bool) ([]guardeval.Rule, error) {
	known := packByID()
	for id := range updates {
		if _, ok := known[id]; !ok {
			return nil, fmt.Errorf("unknown pack %q", id)
		}
	}
	enabled := map[string]bool{}
	stored := map[string]guardeval.Rule{}
	custom := make([]guardeval.Rule, 0, len(existing))
	for _, rule := range existing {
		id, isPack := packIDFromRule(rule.RuleID)
		if !isPack {
			custom = append(custom, rule)
			continue
		}
		if _, ok := known[id]; !ok {
			custom = append(custom, rule)
			continue
		}
		enabled[id] = true
		previous, exists := stored[id]
		// Disabled duplicates do not replace the definition the engine enforces.
		if !exists || rule.Enabled == nil || *rule.Enabled || (previous.Enabled != nil && !*previous.Enabled) {
			stored[id] = rule
		}
	}
	maps.Copy(enabled, updates)
	out := custom
	for _, pack := range catalogPacks() {
		if !enabled[pack.ID] {
			continue
		}
		rule, exists := stored[pack.ID]
		var err error
		if exists {
			rule, err = refreshStoredPackRule(rule)
		} else {
			rule, err = packRule(pack.ID)
		}
		if err != nil {
			return nil, err
		}
		if _, toggled := updates[pack.ID]; toggled {
			rule.Enabled = nil
		}
		out = append(out, rule)
	}
	return out, nil
}
