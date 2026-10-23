package guardeval

import "strings"

type ShellPolicyProfile string

const (
	ShellPolicyGit         ShellPolicyProfile = "git"
	ShellPolicyDestructive ShellPolicyProfile = "destructive"
	ShellPolicyPermissions ShellPolicyProfile = "permissions"
	ShellPolicyDisk        ShellPolicyProfile = "disk"
)

func checkShellPolicy(profile ShellPolicyProfile, analysis shellAnalysis) (denied bool, incomplete string) {
	switch profile {
	case ShellPolicyGit, ShellPolicyDestructive, ShellPolicyPermissions, ShellPolicyDisk:
	default:
		return false, ""
	}
	incomplete = analysis.incomplete
	for _, invocation := range analysis.invocations {
		if len(invocation.args) == 0 {
			continue
		}
		command := invocation.args[0]
		if !command.static {
			if incomplete == "" {
				incomplete = "unresolved shell command"
			}
			continue
		}
		// Do not fold case or clean paths: neither operation models shell lookup.
		name := command.value[strings.LastIndexByte(command.value, '/')+1:]
		args := invocation.args[1:]
		var deny, unknown bool
		switch profile {
		case ShellPolicyGit:
			if name == "git" {
				deny, unknown = shellPolicyGit(args)
			}
		case ShellPolicyDestructive:
			if name == "rm" {
				opts := shellPolicyOptions(args, shellPolicyOptionSpec{
					short: "dfiIrRvPWx", long: "force recursive dir interactive? one-file-system no-preserve-root preserve-root? verbose help version",
				})
				deny = (opts.has("r") || opts.has("R") || opts.has("recursive")) && (opts.has("f") || opts.has("force"))
				unknown = opts.unknown
			}
		case ShellPolicyPermissions:
			if name == "chmod" || name == "chown" {
				deny, unknown = shellPolicyPermissions(name, args)
			}
		case ShellPolicyDisk:
			deny, unknown = shellPolicyDisk(name, args)
		}
		denied = denied || deny
		if unknown && incomplete == "" {
			incomplete = "unresolved " + name + " policy arguments"
		}
	}
	return denied, incomplete
}

type shellPolicyOptionSpec struct {
	// ':' consumes an attached or following value; '?' consumes attached values only.
	short        string
	long         string
	negatable    map[string]string
	negativeMode bool
}

type shellPolicyParsedOptions struct {
	flags    map[string]bool
	operands []shellWord
	boundary bool
	unknown  bool
}

func (o shellPolicyParsedOptions) has(name string) bool { return o.flags[name] }

func shellPolicyOptions(args []shellWord, spec shellPolicyOptionSpec) shellPolicyParsedOptions {
	out := shellPolicyParsedOptions{flags: make(map[string]bool)}
	long := make(map[string]byte)
	for option := range strings.FieldsSeq(spec.long) {
		kind := byte(0)
		if last := option[len(option)-1]; last == ':' || last == '?' {
			kind, option = last, option[:len(option)-1]
		}
		long[option] = kind
	}
	for name := range spec.negatable {
		long["no-"+name] = 0
	}
	record := func(name string) {
		for positive, alias := range spec.negatable {
			if name == positive || name == alias || name == "no-"+positive {
				out.flags[positive] = name != "no-"+positive
				out.flags[alias] = out.flags[positive]
				return
			}
		}
		out.flags[name] = true
	}
	short := make(map[byte]byte)
	for i := 0; i < len(spec.short); i++ {
		name, kind := spec.short[i], byte(0)
		if i+1 < len(spec.short) && (spec.short[i+1] == ':' || spec.short[i+1] == '?') {
			i++
			kind = spec.short[i]
		}
		short[name] = kind
	}
	for i := 0; i < len(args); i++ {
		word := args[i]
		if out.boundary {
			out.operands = append(out.operands, word)
			continue
		}
		if !word.static {
			// Before "--", even a home expansion can supply options.
			out.unknown = true
			out.operands = append(out.operands, word)
			continue
		}
		value := word.value
		if value == "--" {
			out.boundary = true
			continue
		}
		if spec.negativeMode && strings.HasPrefix(value, "-") && strings.Trim(value[1:], "rwxXstugo,+-=") == "" {
			out.operands = append(out.operands, word)
			continue
		}
		if strings.HasPrefix(value, "--") {
			name, _, attached := strings.Cut(value[2:], "=")
			kind, known := long[name]
			if !known || (attached && kind == 0) {
				out.unknown = true
				continue
			}
			record(name)
			if kind == ':' && !attached {
				if i+1 < len(args) {
					i++
					out.unknown = out.unknown || !args[i].static
				} else {
					out.unknown = true
				}
			}
			continue
		}
		if len(value) < 2 || value[0] != '-' {
			out.operands = append(out.operands, word)
			continue
		}
		// Do not read option values or invalid suffixes as flags.
		var flags []string
		valid, consume := true, false
		for j := 1; j < len(value); j++ {
			kind, known := short[value[j]]
			if !known {
				valid = false
				break
			}
			flags = append(flags, value[j:j+1])
			if kind != 0 {
				consume = kind == ':' && j+1 == len(value)
				break
			}
		}
		if !valid {
			out.unknown = true
		} else {
			for _, flag := range flags {
				record(flag)
			}
			if consume {
				if i+1 < len(args) {
					i++
					out.unknown = out.unknown || !args[i].static
				} else {
					out.unknown = true
				}
			}
		}
	}
	return out
}

func shellPolicyGit(args []shellWord) (bool, bool) {
	command, rest, unknown := shellPolicyGitCommand(args)
	if command == "" {
		return false, unknown
	}
	var spec shellPolicyOptionSpec
	switch command {
	case "reset":
		spec = shellPolicyOptionSpec{short: "qNp", long: "soft mixed hard merge keep quiet patch intent-to-add recurse-submodules? no-recurse-submodules pathspec-from-file: pathspec-file-nul"}
	case "push":
		spec = shellPolicyOptionSpec{short: "vfqnudo:46", long: "force verbose quiet dry-run porcelain delete tags all mirror prune atomic no-verify follow-tags set-upstream ipv4 ipv6 thin no-thin repo: receive-pack: exec: push-option: signed? no-signed force-with-lease? force-if-includes recurse-submodules? progress"}
	case "clean":
		spec = shellPolicyOptionSpec{
			short: "dfinqxe:X", long: "force dry-run quiet interactive exclude: directory ignored only-ignored",
			negatable: map[string]string{"dry-run": "n"},
		}
	case "checkout":
		spec = shellPolicyOptionSpec{short: "qb:B:lfmpt", long: "quiet branch: force-branch: force merge patch track? no-track orphan: conflict: pathspec-from-file: pathspec-file-nul detach overlay no-overlay ignore-other-worktrees recurse-submodules?"}
	case "branch":
		spec = shellPolicyOptionSpec{short: "dDfmMcCrlavqit?u:", long: "delete force move copy list all remotes verbose quiet show-current track? no-track set-upstream-to: unset-upstream color? no-color contains? no-contains? merged? no-merged? points-at: format: sort: column? no-column create-reflog edit-description"}
	case "stash":
		spec = shellPolicyOptionSpec{short: "q", long: "quiet"}
	default:
		return false, unknown
	}
	opts := shellPolicyOptions(rest, spec)
	opts.unknown = opts.unknown || unknown
	denied := false
	switch command {
	case "reset":
		denied = opts.has("hard")
	case "push":
		denied = opts.has("f") || opts.has("force")
	case "clean":
		denied = (opts.has("f") || opts.has("force")) && !opts.has("dry-run")
	case "checkout":
		denied = opts.boundary
	case "branch":
		denied = opts.has("D")
	case "stash":
		if len(opts.operands) > 0 {
			subcommand := opts.operands[0]
			if !subcommand.static {
				return false, true
			}
			denied = subcommand.value == "drop" || subcommand.value == "clear"
			if !denied {
				return false, unknown
			}
		}
	}
	return denied, opts.unknown
}

func shellPolicyGitCommand(args []shellWord) (string, []shellWord, bool) {
	unknown := false
	for i := 0; i < len(args); i++ {
		if !args[i].static {
			return "", nil, true
		}
		value := args[i].value
		if !strings.HasPrefix(value, "-") {
			return value, args[i+1:], unknown
		}
		if strings.HasPrefix(value, "-C") || strings.HasPrefix(value, "-c") {
			// Git configuration can select programs to execute.
			unknown = unknown || strings.HasPrefix(value, "-c")
			if len(value) == 2 {
				i++
				if i == len(args) {
					return "", nil, true
				}
				unknown = unknown || !args[i].static
			}
			continue
		}
		name, _, attached := strings.Cut(value, "=")
		switch name {
		case "--git-dir", "--work-tree", "--namespace", "--super-prefix", "--config-env", "--attr-source":
			unknown = unknown || name == "--config-env"
			if !attached {
				i++
				if i == len(args) {
					return "", nil, true
				}
				unknown = unknown || !args[i].static
			}
		case "--exec-path":
			if !attached {
				return "", nil, unknown
			}
		case "-h", "-v", "--help", "--version":
			return "", nil, unknown || attached
		case "-p", "-P", "--paginate", "--no-pager", "--no-replace-objects", "--bare", "--literal-pathspecs", "--glob-pathspecs", "--noglob-pathspecs", "--icase-pathspecs", "--no-optional-locks", "--no-lazy-fetch", "--no-advice":
			if attached {
				return "", nil, true
			}
		default:
			return "", nil, true
		}
	}
	return "", nil, unknown
}

func shellPolicyPermissions(command string, args []shellWord) (bool, bool) {
	spec := shellPolicyOptionSpec{
		short: "RcfvHLP", long: "recursive changes silent quiet verbose reference: preserve-root no-preserve-root help version",
		negativeMode: command == "chmod",
	}
	if command == "chown" {
		spec.short += "h"
		spec.long += " dereference no-dereference from:"
	}
	opts := shellPolicyOptions(args, spec)
	recursive := opts.has("R") || opts.has("recursive")
	targets := opts.operands
	mode777 := false
	if !opts.has("reference") {
		if len(targets) == 0 {
			return false, opts.unknown
		}
		mode777 = command == "chmod" && targets[0].static && targets[0].value == "777"
		// An unquoted mode or owner can expand into additional targets, even after "--".
		opts.unknown = opts.unknown || !targets[0].static
		targets = targets[1:]
	}
	denied, unknown := false, opts.unknown
	for _, target := range targets {
		if target.rootOrHome {
			denied = denied || recursive || mode777
		} else if !target.static && (recursive || mode777) {
			unknown = true
		}
	}
	return denied, unknown
}

func shellPolicyDisk(command string, args []shellWord) (bool, bool) {
	switch {
	case command == "mkfs", strings.HasPrefix(command, "mkfs.") && len(command) > len("mkfs."), command == "wipefs", command == "parted":
		return true, false
	case command == "dd":
		denied, unknown := false, false
		for _, arg := range args {
			if !arg.static {
				unknown = true
			} else if strings.HasPrefix(arg.value, "of=/dev/") {
				denied = true
			}
		}
		return denied, unknown
	case command == "fdisk":
		opts := shellPolicyOptions(args, shellPolicyOptionSpec{
			short: "lxFb:c?u?o:t:w:W:L?n",
			long:  "list list-details list-free sector-size: compatibility? units? output: type: wipe: wipe-partitions: color? noauto lock?",
		})
		if opts.has("l") || opts.has("list") || opts.has("x") || opts.has("list-details") || opts.has("F") || opts.has("list-free") {
			return false, opts.unknown
		}
		return !opts.unknown, opts.unknown
	}
	return false, false
}
