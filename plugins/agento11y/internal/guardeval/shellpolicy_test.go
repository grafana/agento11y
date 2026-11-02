package guardeval

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"mvdan.cc/sh/v3/syntax"
)

func shellPolicyTestWords(values ...string) []shellWord {
	words := make([]shellWord, len(values))
	for i, value := range values {
		words[i] = shellWord{value: value, static: true, rootOrHome: value == "/"}
	}
	return words
}

func TestShellPolicyTyped(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile ShellPolicyProfile
		args    []string
		deny    bool
	}{
		{"no profile", "", []string{"rm", "-rf"}, false},
		{"unrelated", ShellPolicyGit, []string{"echo", "git", "push", "--force"}, false},
		{"git basename", ShellPolicyGit, []string{"/usr/bin/git", "push", "--force"}, true},
		{"git case", ShellPolicyGit, []string{"GIT", "push", "--force"}, false},
		{"git subcommand case", ShellPolicyGit, []string{"git", "Push", "--force"}, false},
		{"git trailing slash", ShellPolicyGit, []string{"/usr/bin/git/", "push", "--force"}, false},
		{"git backslash", ShellPolicyGit, []string{`x\git`, "push", "--force"}, false},
		{"reset hard", ShellPolicyGit, []string{"git", "reset", "HEAD", "--hard"}, true},
		{"reset soft", ShellPolicyGit, []string{"git", "reset", "HEAD", "--soft"}, false},
		{"reset boundary", ShellPolicyGit, []string{"git", "reset", "--", "--hard"}, false},
		{"reset option value", ShellPolicyGit, []string{"git", "reset", "--pathspec-from-file", "--hard"}, false},
		{"reset attached value", ShellPolicyGit, []string{"git", "reset", "--pathspec-from-file=--hard"}, false},
		{"push force", ShellPolicyGit, []string{"git", "push", "--force"}, true},
		{"push short", ShellPolicyGit, []string{"git", "push", "-f"}, true},
		{"push grouped", ShellPolicyGit, []string{"git", "push", "-uf", "origin"}, true},
		{"push group order", ShellPolicyGit, []string{"git", "push", "-fu", "origin"}, true},
		{"push force lease", ShellPolicyGit, []string{"git", "push", "--force-with-lease"}, false},
		{"push lease value", ShellPolicyGit, []string{"git", "push", "--force-with-lease=main:abc"}, false},
		{"push includes", ShellPolicyGit, []string{"git", "push", "--force-if-includes"}, false},
		{"push boundary", ShellPolicyGit, []string{"git", "push", "--", "--force"}, false},
		{"push repo value", ShellPolicyGit, []string{"git", "push", "--repo", "--force"}, false},
		{"push repo attached", ShellPolicyGit, []string{"git", "push", "--repo=origin --force"}, false},
		{"push exec value", ShellPolicyGit, []string{"git", "push", "--exec", "-f"}, false},
		{"push receive pack", ShellPolicyGit, []string{"git", "push", "--receive-pack", "--force"}, false},
		{"push option value", ShellPolicyGit, []string{"git", "push", "-o", "-f"}, false},
		{"push option attached", ShellPolicyGit, []string{"git", "push", "-of"}, false},
		{"push option group", ShellPolicyGit, []string{"git", "push", "-uof"}, false},
		{"push force before value", ShellPolicyGit, []string{"git", "push", "-fof"}, true},
		{"push value boundary", ShellPolicyGit, []string{"git", "push", "--repo", "--", "-f"}, true},
		{"git global C", ShellPolicyGit, []string{"git", "-C", "/repo", "push", "-f"}, true},
		{"git global dir", ShellPolicyGit, []string{"git", "--git-dir=/repo/.git", "push", "-f"}, true},
		{"git global tree", ShellPolicyGit, []string{"git", "--work-tree", "push", "status", "-f"}, false},
		{"git global namespace", ShellPolicyGit, []string{"git", "--namespace", "push", "status", "-f"}, false},
		{"git global super prefix", ShellPolicyGit, []string{"git", "--super-prefix", "push", "status", "-f"}, false},
		{"git global command as value", ShellPolicyGit, []string{"git", "--work-tree", "push", "push", "-f"}, true},
		{"git global attr source", ShellPolicyGit, []string{"git", "--attr-source", "push", "status", "-f"}, false},
		{"git global pager", ShellPolicyGit, []string{"git", "--no-pager", "--literal-pathspecs", "push", "-f"}, true},
		{"git exec path query", ShellPolicyGit, []string{"git", "--exec-path", "push", "-f"}, false},
		{"git exec path set", ShellPolicyGit, []string{"git", "--exec-path=/bin", "push", "-f"}, true},
		{"git help", ShellPolicyGit, []string{"git", "--help", "push", "-f"}, false},
		{"git version", ShellPolicyGit, []string{"git", "--version"}, false},
		{"clean force", ShellPolicyGit, []string{"git", "clean", "-f"}, true},
		{"clean dry run", ShellPolicyGit, []string{"git", "clean", "-nf"}, false},
		{"clean long dry run", ShellPolicyGit, []string{"git", "clean", "--force", "--dry-run"}, false},
		{"clean short preview cancelled", ShellPolicyGit, []string{"git", "clean", "-n", "-f", "--no-dry-run"}, true},
		{"clean long preview cancelled", ShellPolicyGit, []string{"git", "clean", "--dry-run", "--force", "--no-dry-run"}, true},
		{"clean short preview restored", ShellPolicyGit, []string{"git", "clean", "--dry-run", "--no-dry-run", "-nf"}, false},
		{"clean long preview restored", ShellPolicyGit, []string{"git", "clean", "-nf", "--no-dry-run", "--dry-run"}, false},
		{"clean negation without preview", ShellPolicyGit, []string{"git", "clean", "--no-dry-run", "-f"}, true},
		{"clean negation without force", ShellPolicyGit, []string{"git", "clean", "-n", "--no-dry-run"}, false},
		{"clean negation as value", ShellPolicyGit, []string{"git", "clean", "-nf", "-e", "--no-dry-run"}, false},
		{"clean negation after boundary", ShellPolicyGit, []string{"git", "clean", "-nf", "--", "--no-dry-run"}, false},
		{"clean exclude dry run", ShellPolicyGit, []string{"git", "clean", "-e", "-n", "-f"}, true},
		{"clean long exclude dry run", ShellPolicyGit, []string{"git", "clean", "--exclude", "--dry-run", "--force"}, true},
		{"clean exclude force", ShellPolicyGit, []string{"git", "clean", "-e", "-fd"}, false},
		{"clean attached force value", ShellPolicyGit, []string{"git", "clean", "-qef"}, false},
		{"clean attached dry value", ShellPolicyGit, []string{"git", "clean", "-en", "-f"}, true},
		{"clean group force then exclude", ShellPolicyGit, []string{"git", "clean", "-dfevalue"}, true},
		{"clean group exclude then force value", ShellPolicyGit, []string{"git", "clean", "-def"}, false},
		{"clean exclude boundary", ShellPolicyGit, []string{"git", "clean", "-qe", "--", "-fd"}, true},
		{"clean repeated exclude", ShellPolicyGit, []string{"git", "clean", "-e", "-e", "--", "-fd"}, false},
		{"clean empty exclude", ShellPolicyGit, []string{"git", "clean", "--exclude=", "-fd"}, true},
		{"clean attached boundary", ShellPolicyGit, []string{"git", "clean", "-e--", "-fd"}, true},
		{"clean boundary", ShellPolicyGit, []string{"git", "clean", "--", "-fd"}, false},
		{"clean actual dry after value", ShellPolicyGit, []string{"git", "clean", "-e", "--", "-n", "-f"}, false},
		{"checkout boundary", ShellPolicyGit, []string{"git", "checkout", "--", "file"}, true},
		{"checkout normal", ShellPolicyGit, []string{"git", "checkout", "main"}, false},
		{"checkout boundary value", ShellPolicyGit, []string{"git", "checkout", "-b", "--"}, false},
		{"checkout long value", ShellPolicyGit, []string{"git", "checkout", "--orphan", "--"}, false},
		{"stash drop", ShellPolicyGit, []string{"git", "stash", "drop"}, true},
		{"stash clear", ShellPolicyGit, []string{"git", "stash", "clear"}, true},
		{"stash show", ShellPolicyGit, []string{"git", "stash", "show", "drop"}, false},
		{"branch D", ShellPolicyGit, []string{"git", "branch", "-vD", "old"}, true},
		{"branch long force delete", ShellPolicyGit, []string{"git", "branch", "--delete", "--force", "old"}, true},
		{"branch short force delete", ShellPolicyGit, []string{"git", "branch", "-d", "-f", "old"}, true},
		{"branch grouped force delete", ShellPolicyGit, []string{"git", "branch", "-df", "old"}, true},
		{"branch reversed force delete", ShellPolicyGit, []string{"git", "branch", "-fd", "old"}, true},
		{"branch mixed force delete", ShellPolicyGit, []string{"git", "branch", "--force", "-d", "old"}, true},
		{"branch mixed delete force", ShellPolicyGit, []string{"git", "branch", "--delete", "-f", "old"}, true},
		{"branch force only", ShellPolicyGit, []string{"git", "branch", "--force", "old"}, false},
		{"branch force value", ShellPolicyGit, []string{"git", "branch", "--delete", "--format", "--force"}, false},
		{"branch delete value", ShellPolicyGit, []string{"git", "branch", "--force", "--format", "--delete"}, false},
		{"branch force after boundary", ShellPolicyGit, []string{"git", "branch", "--delete", "--", "--force"}, false},
		{"branch d", ShellPolicyGit, []string{"git", "branch", "-d", "old"}, false},
		{"branch delete", ShellPolicyGit, []string{"git", "branch", "--delete", "old"}, false},
		{"branch boundary", ShellPolicyGit, []string{"git", "branch", "--", "-D"}, false},
		{"branch format value", ShellPolicyGit, []string{"git", "branch", "--format", "-D"}, false},
		{"branch upstream value", ShellPolicyGit, []string{"git", "branch", "-uD"}, false},
		{"rm recursive force", ShellPolicyDestructive, []string{"rm", "-rf", "/tmp"}, true},
		{"rm reverse", ShellPolicyDestructive, []string{"rm", "-fr"}, true},
		{"rm capital recursive", ShellPolicyDestructive, []string{"rm", "-fR"}, true},
		{"rm split", ShellPolicyDestructive, []string{"rm", "-r", "/tmp", "-f"}, true},
		{"rm long", ShellPolicyDestructive, []string{"rm", "--recursive", "--force"}, true},
		{"rm mixed", ShellPolicyDestructive, []string{"rm", "--recursive", "-f"}, true},
		{"rm recursive only", ShellPolicyDestructive, []string{"rm", "-r"}, false},
		{"rm force only", ShellPolicyDestructive, []string{"rm", "-f"}, false},
		{"rm boundary", ShellPolicyDestructive, []string{"rm", "-r", "--", "-f"}, false},
		{"rm initial boundary", ShellPolicyDestructive, []string{"rm", "--", "-rf"}, false},
		{"rm flags before boundary", ShellPolicyDestructive, []string{"rm", "-rf", "--", "x"}, true},
		{"rm optional value", ShellPolicyDestructive, []string{"rm", "--preserve-root=all -rf"}, false},
		{"rm basename", ShellPolicyDestructive, []string{"/bin/rm", "-rf"}, true},
		{"rm case", ShellPolicyDestructive, []string{"RM", "-rf"}, false},
		{"chmod recursive", ShellPolicyPermissions, []string{"chmod", "-R", "755", "/"}, true},
		{"chmod mac H cluster", ShellPolicyPermissions, []string{"chmod", "-RH", "755", "/"}, true},
		{"chmod mac L cluster", ShellPolicyPermissions, []string{"chmod", "-LR", "755", "/"}, true},
		{"chmod mac P cluster", ShellPolicyPermissions, []string{"chmod", "-RP", "755", "/"}, true},
		{"chmod mac split flags", ShellPolicyPermissions, []string{"chmod", "-H", "-R", "755", "/"}, true},
		{"chmod mac nonrecursive", ShellPolicyPermissions, []string{"chmod", "-HLP", "755", "/"}, false},
		{"chmod mac project", ShellPolicyPermissions, []string{"chmod", "-RH", "755", "./src"}, false},
		{"chmod mac boundary", ShellPolicyPermissions, []string{"chmod", "--", "755", "-RH", "/"}, false},
		{"chmod negative mode", ShellPolicyPermissions, []string{"chmod", "-R", "-w", "/"}, true},
		{"chmod grouped negative mode", ShellPolicyPermissions, []string{"chmod", "-R", "-rwX", "/"}, true},
		{"chmod compound negative mode", ShellPolicyPermissions, []string{"chmod", "--recursive", "-w,u+x", "/"}, true},
		{"chmod negative copy mode", ShellPolicyPermissions, []string{"chmod", "-R", "-u", "/"}, true},
		{"chmod negative mode project", ShellPolicyPermissions, []string{"chmod", "-R", "-w", "/tmp/project"}, false},
		{"chmod negative mode nonrecursive", ShellPolicyPermissions, []string{"chmod", "-w", "/"}, false},
		{"chmod negative mode boundary", ShellPolicyPermissions, []string{"chmod", "-R", "--", "-w", "/"}, true},
		{"chmod mode", ShellPolicyPermissions, []string{"chmod", "777", "/"}, true},
		{"chmod octal mode", ShellPolicyPermissions, []string{"chmod", "0777", "/"}, true},
		{"chmod padded octal mode", ShellPolicyPermissions, []string{"chmod", "000777", "/"}, true},
		{"chmod octal boundary", ShellPolicyPermissions, []string{"chmod", "--", "0777", "/"}, true},
		{"chmod octal nonrecursive", ShellPolicyPermissions, []string{"chmod", "0755", "/"}, false},
		{"chmod octal project", ShellPolicyPermissions, []string{"chmod", "0777", "/tmp/project"}, false},
		{"chmod octal reference value", ShellPolicyPermissions, []string{"chmod", "--reference", "0777", "/"}, false},
		{"chmod nonrecursive", ShellPolicyPermissions, []string{"chmod", "755", "/"}, false},
		{"chmod project", ShellPolicyPermissions, []string{"chmod", "-R", "777", "/tmp"}, false},
		{"chmod lowercase r", ShellPolicyPermissions, []string{"chmod", "-r", "755", "/"}, false},
		{"chmod case", ShellPolicyPermissions, []string{"/bin/CHMOD", "777", "/"}, false},
		{"chmod reference recursive", ShellPolicyPermissions, []string{"chmod", "--reference", "file", "-R", "/"}, true},
		{"chmod reference no mode", ShellPolicyPermissions, []string{"chmod", "--reference=file", "777", "/"}, false},
		{"chmod reference value", ShellPolicyPermissions, []string{"chmod", "--reference", "-R", "/"}, false},
		{"chmod reference root value", ShellPolicyPermissions, []string{"chmod", "-R", "--reference", "/", "/tmp"}, false},
		{"chmod mode not target", ShellPolicyPermissions, []string{"chmod", "-R", "/", "/tmp"}, false},
		{"chmod boundary", ShellPolicyPermissions, []string{"chmod", "--", "777", "/"}, true},
		{"chmod flag after boundary", ShellPolicyPermissions, []string{"chmod", "--", "755", "-R", "/"}, false},
		{"chown recursive", ShellPolicyPermissions, []string{"chown", "--recursive", "root", "/"}, true},
		{"chown mode not chmod", ShellPolicyPermissions, []string{"chown", "777", "/"}, false},
		{"chown from value", ShellPolicyPermissions, []string{"chown", "--from", "-R", "root", "/"}, false},
		{"chown reference", ShellPolicyPermissions, []string{"chown", "-R", "--reference=file", "/"}, true},
		{"chown from root value", ShellPolicyPermissions, []string{"chown", "--from", "/", "-R", "root", "/tmp"}, false},
		{"dd output device", ShellPolicyDisk, []string{"dd", "if=/dev/zero", "of=/dev/sda"}, true},
		{"dd input device", ShellPolicyDisk, []string{"dd", "if=/dev/sda", "of=/tmp/image"}, false},
		{"dd value embedded", ShellPolicyDisk, []string{"dd", "if=/tmp/of=/dev/sda"}, false},
		{"dd case", ShellPolicyDisk, []string{"DD", "of=/dev/sda"}, false},
		{"mkfs", ShellPolicyDisk, []string{"mkfs"}, true},
		{"mkfs variant", ShellPolicyDisk, []string{"/sbin/mkfs.ext4", "/dev/sda"}, true},
		{"mkfs suffix boundary", ShellPolicyDisk, []string{"mkfsbackup"}, false},
		{"mkfs empty suffix", ShellPolicyDisk, []string{"mkfs."}, false},
		{"wipefs", ShellPolicyDisk, []string{"wipefs", "/dev/sda"}, true},
		{"parted", ShellPolicyDisk, []string{"parted", "print"}, true},
		{"fdisk", ShellPolicyDisk, []string{"fdisk", "/dev/sda"}, true},
		{"fdisk list", ShellPolicyDisk, []string{"fdisk", "-l", "/dev/sda"}, false},
		{"fdisk long list", ShellPolicyDisk, []string{"fdisk", "--list", "/dev/sda"}, false},
		{"fdisk list details", ShellPolicyDisk, []string{"fdisk", "--list-details"}, false},
		{"fdisk list free", ShellPolicyDisk, []string{"fdisk", "-F"}, false},
		{"fdisk grouped", ShellPolicyDisk, []string{"fdisk", "-lb512", "/dev/sda"}, false},
		{"fdisk optional value", ShellPolicyDisk, []string{"fdisk", "-lu=sectors", "/dev/sda"}, false},
		{"fdisk list option value", ShellPolicyDisk, []string{"fdisk", "-o", "-l", "/dev/sda"}, true},
		{"fdisk long value", ShellPolicyDisk, []string{"fdisk", "--output=--list", "/dev/sda"}, true},
		{"fdisk list boundary", ShellPolicyDisk, []string{"fdisk", "--", "-l"}, true},
		{"disk no extra commands", ShellPolicyDisk, []string{"sfdisk", "/dev/sda"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			analysis := shellAnalysis{invocations: []shellInvocation{{args: shellPolicyTestWords(tc.args...)}}}
			denied, incomplete := checkShellPolicy(tc.profile, analysis)
			if denied != tc.deny || incomplete != "" {
				t.Fatalf("checkShellPolicy() = (%v, %q), want (%v, empty)", denied, incomplete, tc.deny)
			}
		})
	}
}

func TestShellPolicyDynamic(t *testing.T) {
	words := shellPolicyTestWords
	dynamic := shellWord{value: "$unknown"}
	home := shellWord{value: "$HOME", rootOrHome: true}
	for _, tc := range []struct {
		name       string
		profile    ShellPolicyProfile
		args       []shellWord
		deny       bool
		incomplete bool
	}{
		{"dynamic command", ShellPolicyGit, []shellWord{dynamic}, false, true},
		{"no profile", "", []shellWord{dynamic}, false, false},
		{"unrelated args", ShellPolicyGit, append(words("echo"), dynamic), false, false},
		{"git subcommand", ShellPolicyGit, append(words("git"), dynamic), false, true},
		{"git global value", ShellPolicyGit, append(append(words("git", "-C"), dynamic), words("push", "-f")...), true, true},
		{"git global config value", ShellPolicyGit, append(append(words("git", "-c"), dynamic), words("push", "-f")...), true, true},
		{"git static attached config", ShellPolicyGit, words("git", "-C/repo", "-ccore.pager=cat", "push", "-f"), true, true},
		{"git static config value", ShellPolicyGit, words("git", "-C", "push", "-c", "--force", "status"), false, true},
		{"git static config env", ShellPolicyGit, words("git", "--config-env", "push", "status", "-f"), false, true},
		{"git global long value", ShellPolicyGit, append(append(words("git", "--git-dir"), dynamic), words("push", "-f")...), true, true},
		{"git global work tree", ShellPolicyGit, append(append(words("git", "--work-tree"), dynamic), words("clean", "-nf")...), false, true},
		{"git global namespace", ShellPolicyGit, append(append(words("git", "--namespace"), dynamic), words("push", "-f")...), true, true},
		{"git global super prefix", ShellPolicyGit, append(append(words("git", "--super-prefix"), dynamic), words("push", "-f")...), true, true},
		{"git global config env", ShellPolicyGit, append(append(words("git", "--config-env"), dynamic), words("push", "-f")...), true, true},
		{"git global attr source", ShellPolicyGit, append(append(words("git", "--attr-source"), dynamic), words("push", "-f")...), true, true},
		{"git global home value", ShellPolicyGit, append(append(words("git", "-C"), home), words("push", "-f")...), true, true},
		{"git global irrelevant command", ShellPolicyGit, append(append(words("git", "-C"), dynamic), words("status")...), false, true},
		{"git global stash show", ShellPolicyGit, append(append(words("git", "-C"), dynamic), words("stash", "show")...), false, true},
		{"git unknown flags", ShellPolicyGit, append(words("git", "push"), dynamic), false, true},
		{"reset suffix", ShellPolicyGit, words("git", "reset", "--hard suffix"), false, true},
		{"push flag equals", ShellPolicyGit, words("git", "push", "--force=anything"), false, true},
		{"push literal suffix", ShellPolicyGit, words("git", "push", "-f suffix"), false, true},
		{"push uppercase force", ShellPolicyGit, words("git", "push", "-F"), false, true},
		{"git global boundary", ShellPolicyGit, words("git", "--", "push", "-f"), false, true},
		{"git global unknown long option", ShellPolicyGit, words("git", "--unknown", "push", "-f"), false, true},
		{"git global invalid cluster", ShellPolicyGit, words("git", "-pZ", "push", "-f"), false, true},
		{"git global invalid attached value", ShellPolicyGit, words("git", "--no-pager=yes", "push", "-f"), false, true},
		{"git home argument", ShellPolicyGit, append(words("git", "push"), home), false, true},
		{"git typed denial", ShellPolicyGit, append(append(words("git", "push"), dynamic), words("--force")...), true, true},
		{"git option value", ShellPolicyGit, append(words("git", "push", "--repo"), dynamic), false, true},
		{"git short option value", ShellPolicyGit, append(append(words("git", "push", "-o"), dynamic), words("-f")...), true, true},
		{"git boundary", ShellPolicyGit, append(words("git", "push", "--"), dynamic), false, true},
		{"git irrelevant command", ShellPolicyGit, append(words("git", "status"), dynamic), false, false},
		{"clean unknown", ShellPolicyGit, append(words("git", "clean"), dynamic), false, true},
		{"clean abbreviated negation", ShellPolicyGit, words("git", "clean", "-nf", "--no-dry-r"), false, true},
		{"clean negation invalid value", ShellPolicyGit, words("git", "clean", "-nf", "--no-dry-run=yes"), false, true},
		{"clean invalid cluster", ShellPolicyGit, words("git", "clean", "-nfZ"), false, true},
		{"clean unsupported option typed denial", ShellPolicyGit, words("git", "clean", "-f", "--unknown"), true, true},
		{"clean home argument", ShellPolicyGit, append(words("git", "clean", "-nf"), home), false, true},
		{"clean home before preview", ShellPolicyGit, append(append(words("git", "clean"), home), words("-nf")...), false, true},
		{"clean home after boundary", ShellPolicyGit, append(words("git", "clean", "-nf", "--"), home), false, false},
		{"clean known dry run with unknown arguments", ShellPolicyGit, append(words("git", "clean", "-nf"), dynamic), false, true},
		{"clean dynamic value can consume preview flag", ShellPolicyGit, append(append(words("git", "clean"), dynamic), words("-n", "-f")...), false, true},
		{"clean exclude value", ShellPolicyGit, append(append(words("git", "clean", "-e"), dynamic), words("-f")...), true, true},
		{"clean exclude dynamic preview", ShellPolicyGit, append(append(words("git", "clean", "-e"), dynamic), words("-n", "-f")...), false, true},
		{"clean long exclude dynamic preview", ShellPolicyGit, append(append(words("git", "clean", "--exclude"), dynamic), words("--dry-run", "--force")...), false, true},
		{"clean exclude home value", ShellPolicyGit, append(append(words("git", "clean", "-e"), home), words("-nf")...), false, true},
		{"stash unknown", ShellPolicyGit, append(words("git", "stash"), dynamic), false, true},
		{"stash irrelevant argument", ShellPolicyGit, append(words("git", "stash", "show"), dynamic), false, false},
		{"rm unknown flag", ShellPolicyDestructive, append(words("rm", "-r"), dynamic), false, true},
		{"rm literal suffix", ShellPolicyDestructive, words("rm", "-rf suffix"), false, true},
		{"rm invalid group", ShellPolicyDestructive, words("rm", "-rfj"), false, true},
		{"rm not flag suffix", ShellPolicyDestructive, words("rm", "--recursive-file", "-f"), false, true},
		{"rm home argument", ShellPolicyDestructive, append(words("rm", "-r"), home), false, true},
		{"rm home after boundary", ShellPolicyDestructive, append(words("rm", "-r", "--"), home), false, false},
		{"rm typed denial", ShellPolicyDestructive, append(words("rm", "-rf"), dynamic), true, true},
		{"rm boundary", ShellPolicyDestructive, append(words("rm", "--"), dynamic), false, false},
		{"chmod invalid recursive cluster", ShellPolicyPermissions, words("chmod", "-RHZ", "755", "/"), false, true},
		{"chmod unsupported option typed denial", ShellPolicyPermissions, words("chmod", "-R", "--unknown", "755", "/"), true, true},
		{"chmod home recursive", ShellPolicyPermissions, append(words("chmod", "-R", "755"), home), true, true},
		{"chmod home mac recursive", ShellPolicyPermissions, append(words("chmod", "-RH", "755"), home), true, true},
		{"chmod home negative mode", ShellPolicyPermissions, append(words("chmod", "-R", "-x"), home), true, true},
		{"chmod unknown negative mode target", ShellPolicyPermissions, append(words("chmod", "-R", "-x"), dynamic), false, true},
		{"chmod home mode", ShellPolicyPermissions, append(words("chmod", "777"), home), true, true},
		{"chmod home safe mode", ShellPolicyPermissions, append(words("chmod", "755"), home), false, true},
		{"chmod root glob", ShellPolicyPermissions, append(words("chmod", "-R", "755"), shellWord{value: "/*", rootOrHome: true}), true, true},
		{"chmod home glob", ShellPolicyPermissions, append(words("chmod", "777"), shellWord{value: "~/*", rootOrHome: true}), true, true},
		{"chown home", ShellPolicyPermissions, append(words("chown", "-R", "root"), home), true, true},
		{"chmod home as mode", ShellPolicyPermissions, append(append(words("chmod"), home), words("./src")...), false, true},
		{"chown home as owner", ShellPolicyPermissions, append(append(words("chown", "-R"), home), words("./src")...), false, true},
		{"chmod home as option value", ShellPolicyPermissions, append(append(words("chmod", "-R", "--reference"), home), words("./src")...), false, true},
		{"chmod home recursive after boundary", ShellPolicyPermissions, append(words("chmod", "-RH", "--", "755"), home), true, false},
		{"chmod home mode after boundary", ShellPolicyPermissions, append(words("chmod", "--", "777"), home), true, false},
		{"chmod home safe mode after boundary", ShellPolicyPermissions, append(words("chmod", "--", "755"), home), false, false},
		{"chown home after boundary", ShellPolicyPermissions, append(words("chown", "-R", "--", "root"), home), true, false},
		{"chmod home as mode after boundary", ShellPolicyPermissions, append(append(words("chmod", "--"), home), words("./src")...), false, true},
		{"chown home as owner after boundary", ShellPolicyPermissions, append(append(words("chown", "-R", "--"), home), words("./src")...), false, true},
		{"chmod unknown target", ShellPolicyPermissions, append(words("chmod", "-R", "755"), dynamic), false, true},
		{"chmod unknown mode", ShellPolicyPermissions, append(append(words("chmod"), dynamic), home), false, true},
		{"chmod recursive unknown mode", ShellPolicyPermissions, append(append(words("chmod", "-R"), dynamic), home), true, true},
		{"chmod reference value", ShellPolicyPermissions, append(append(words("chmod", "-R", "--reference"), dynamic), home), true, true},
		{"chmod boundary safe mode", ShellPolicyPermissions, append(words("chmod", "--", "755"), dynamic), false, false},
		{"chmod literal home", ShellPolicyPermissions, words("chmod", "-R", "755", "$HOME"), false, false},
		{"chmod literal tilde", ShellPolicyPermissions, words("chmod", "-R", "755", "~/"), false, false},
		{"dd unknown output", ShellPolicyDisk, append(words("dd"), dynamic), false, true},
		{"dd known output", ShellPolicyDisk, append(append(words("dd"), dynamic), words("of=/dev/sda")...), true, true},
		{"dd unknown after output", ShellPolicyDisk, append(words("dd", "of=/dev/sda"), dynamic), true, true},
		{"mkfs known command", ShellPolicyDisk, append(words("mkfs.ext4"), dynamic), true, false},
		{"fdisk unknown list flag", ShellPolicyDisk, append(words("fdisk"), dynamic), false, true},
		{"fdisk invalid list equals", ShellPolicyDisk, words("fdisk", "--list=no"), false, true},
		{"fdisk invalid list cluster", ShellPolicyDisk, words("fdisk", "-lZ"), false, true},
		{"fdisk home argument", ShellPolicyDisk, append(words("fdisk", "-l"), home), false, true},
		{"fdisk home after boundary", ShellPolicyDisk, append(words("fdisk", "-l", "--"), home), false, false},
		{"fdisk list with unknown arguments", ShellPolicyDisk, append(words("fdisk", "-l"), dynamic), false, true},
		{"fdisk dynamic value can consume list flag", ShellPolicyDisk, append(append(words("fdisk"), dynamic), words("-l")...), false, true},
		{"fdisk output value", ShellPolicyDisk, append(words("fdisk", "-o"), dynamic), false, true},
		{"missing option value", ShellPolicyGit, words("git", "clean", "-e"), false, true},
		{"missing global value", ShellPolicyGit, words("git", "-C"), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			denied, incomplete := checkShellPolicy(tc.profile, shellAnalysis{invocations: []shellInvocation{{args: tc.args}}})
			if denied != tc.deny || (incomplete != "") != tc.incomplete {
				t.Fatalf("checkShellPolicy() = (%v, %q), want (%v, incomplete=%v)", denied, incomplete, tc.deny, tc.incomplete)
			}
		})
	}
}

func TestShellPolicyReviewRegressions(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		profile      ShellPolicyProfile
		deny         bool
		incomplete   bool
	}{
		{"restore path", `git restore file.go`, ShellPolicyGit, true, false},
		{"restore no paths", `git restore`, ShellPolicyGit, true, false},
		{"restore staged", `git restore --staged file.go`, ShellPolicyGit, false, false},
		{"restore short staged", `git restore -S file.go`, ShellPolicyGit, false, false},
		{"restore staged source", `git restore -s HEAD~1 --staged .`, ShellPolicyGit, false, false},
		{"restore both modes", `git restore --staged --worktree file.go`, ShellPolicyGit, true, false},
		{"restore reversed modes", `git restore -W -S file.go`, ShellPolicyGit, true, false},
		{"restore grouped modes", `git restore -SW file.go`, ShellPolicyGit, true, false},
		{"restore reversed group", `git restore -qWS file.go`, ShellPolicyGit, true, false},
		{"restore source value", `git restore -S --source -W file.go`, ShellPolicyGit, false, false},
		{"restore attached source", `git restore -SsW file.go`, ShellPolicyGit, false, false},
		{"restore source hides staged", `git restore --source --staged file.go`, ShellPolicyGit, true, false},
		{"restore worktree before source", `git restore -SWsHEAD file.go`, ShellPolicyGit, true, false},
		{"restore file input", `git restore --pathspec-from-file paths`, ShellPolicyGit, true, false},
		{"restore stdin input", `git restore --pathspec-from-file=-`, ShellPolicyGit, true, false},
		{"restore staged file input", `git restore -S --pathspec-from-file paths --pathspec-file-nul`, ShellPolicyGit, false, false},
		{"restore file input value", `git restore --pathspec-from-file --staged`, ShellPolicyGit, true, false},
		{"restore file input not flag", `git restore -S --pathspec-from-file=-W`, ShellPolicyGit, false, false},
		{"restore boundary", `git restore -- --staged`, ShellPolicyGit, true, false},
		{"restore staged boundary", `git restore -S -- -W`, ShellPolicyGit, false, false},
		{"restore value boundary", `git restore --source -- -S file.go`, ShellPolicyGit, false, false},
		{"restore help", `git restore --help`, ShellPolicyGit, false, false},
		{"restore short help", `git restore -h`, ShellPolicyGit, false, false},
		{"restore unknown option", `git restore --unknown file.go`, ShellPolicyGit, false, true},
		{"restore abbreviated staged", `git restore --st .`, ShellPolicyGit, false, true},
		{"restore abbreviated worktree", `git restore --staged --worktr .`, ShellPolicyGit, false, true},
		{"restore dynamic option", `git restore "$flags" file.go`, ShellPolicyGit, false, true},
		{"restore dynamic source", `git restore -s "$source" file.go`, ShellPolicyGit, false, true},
		{"restore dynamic file input", `git restore --pathspec-from-file="$paths"`, ShellPolicyGit, false, true},
		{"restore dynamic path after boundary", `git restore -- "$path"`, ShellPolicyGit, true, false},
		{"restore staged dynamic path after boundary", `git restore -S -- "$path"`, ShellPolicyGit, false, false},
		{"restore explicit denial incomplete", `git restore -W "$path"`, ShellPolicyGit, true, true},
		{"switch short force", `git switch -f feature`, ShellPolicyGit, true, false},
		{"switch long force", `git switch --force feature`, ShellPolicyGit, true, false},
		{"switch discard", `git switch --discard-changes feature`, ShellPolicyGit, true, false},
		{"switch grouped force", `git switch -qf feature`, ShellPolicyGit, true, false},
		{"switch create", `git switch -c feature`, ShellPolicyGit, false, false},
		{"switch value", `git switch -c -f`, ShellPolicyGit, false, false},
		{"switch attached value", `git switch -cf`, ShellPolicyGit, false, false},
		{"switch boundary", `git switch -- --force`, ShellPolicyGit, false, false},
		{"switch conflict value", `git switch --conflict --discard-changes feature`, ShellPolicyGit, false, false},
		{"switch abbreviation", `git switch --disc feature`, ShellPolicyGit, false, true},
		{"checkout dot", `git checkout .`, ShellPolicyGit, true, false},
		{"checkout dot slash", `git checkout ./`, ShellPolicyGit, true, false},
		{"checkout top", `git checkout :/`, ShellPolicyGit, true, false},
		{"checkout top dot", `git checkout :/.`, ShellPolicyGit, true, false},
		{"checkout top wildcard", `git checkout ':/*'`, ShellPolicyGit, true, false},
		{"checkout whole tree after revision", `git checkout HEAD .`, ShellPolicyGit, true, false},
		{"checkout wildcard", `git checkout '*'`, ShellPolicyGit, true, false},
		{"checkout relative wildcard", `git checkout './*'`, ShellPolicyGit, true, false},
		{"checkout shell wildcard", `git checkout *`, ShellPolicyGit, false, true},
		{"checkout shell relative wildcard", `git checkout ./*`, ShellPolicyGit, false, true},
		{"checkout force", `git checkout -f`, ShellPolicyGit, true, false},
		{"checkout long force", `git checkout --force feature`, ShellPolicyGit, true, false},
		{"checkout grouped force", `git checkout -qf feature`, ShellPolicyGit, true, false},
		{"checkout branch value", `git checkout -b .`, ShellPolicyGit, false, false},
		{"checkout attached branch value", `git checkout -bf`, ShellPolicyGit, false, false},
		{"checkout force value", `git checkout --orphan -f`, ShellPolicyGit, false, false},
		{"checkout pathspec file", `git checkout --pathspec-from-file paths`, ShellPolicyGit, false, true},
		{"checkout pathspec stdin", `git checkout --pathspec-from-file=-`, ShellPolicyGit, false, true},
		{"checkout file input as path", `git checkout -- --pathspec-from-file=paths`, ShellPolicyGit, true, false},
		{"checkout ordinary path", `git checkout file.go`, ShellPolicyGit, false, false},
		{"push force refspec", `git push origin +HEAD:main`, ShellPolicyGit, true, false},
		{"push force refspec quoted", `git push origin '+HEAD:main'`, ShellPolicyGit, true, false},
		{"push force refspec boundary", `git push -- origin +HEAD:main`, ShellPolicyGit, true, false},
		{"push repo option overridden", `git push --repo=origin +remote HEAD:main`, ShellPolicyGit, false, false},
		{"push repo option refspec", `git push --repo=origin other +HEAD:main`, ShellPolicyGit, true, false},
		{"push multiple refspecs", `git push origin HEAD:main +other:other`, ShellPolicyGit, true, false},
		{"push option refspec value", `git push -o +HEAD:main origin HEAD:main`, ShellPolicyGit, false, false},
		{"push repo refspec value", `git push --repo +HEAD:main HEAD:main`, ShellPolicyGit, false, false},
		{"push remote is not refspec", `git push +remote HEAD:main`, ShellPolicyGit, false, false},
		{"push plus alone", `git push origin +`, ShellPolicyGit, false, false},
		{"push nonprefix plus", `git push origin HEAD:main+`, ShellPolicyGit, false, false},
		{"push dynamic refspec boundary", `git push -- origin "$refspec"`, ShellPolicyGit, false, true},
		{"root dot", `chmod -R 755 /.`, ShellPolicyPermissions, true, false},
		{"root dot quoted", `chmod 777 '/.'`, ShellPolicyPermissions, true, false},
		{"home dot", `chown -R me $HOME/.`, ShellPolicyPermissions, true, true},
		{"home dot boundary", `chmod -R -- 755 "${HOME}/."`, ShellPolicyPermissions, true, false},
		{"tilde dot", `chmod -R 755 ~/.`, ShellPolicyPermissions, true, true},
		{"literal home dot", `chmod -R 755 '$HOME/.'`, ShellPolicyPermissions, false, false},
		{"root dot reference value", `chmod -R --reference=/. ./src`, ShellPolicyPermissions, false, false},
		{"root dot child", `chmod -R 755 /.cache`, ShellPolicyPermissions, false, false},
		{"parted list", `parted -l`, ShellPolicyDisk, false, false},
		{"parted long list", `parted --list`, ShellPolicyDisk, false, false},
		{"parted list disk", `parted -l /dev/sda`, ShellPolicyDisk, false, false},
		{"parted print", `parted /dev/sda print`, ShellPolicyDisk, false, false},
		{"parted print free", `parted -sm /dev/sda print free`, ShellPolicyDisk, false, false},
		{"parted units", `parted --script /dev/sda unit s print`, ShellPolicyDisk, false, false},
		{"parted interactive disk", `parted /dev/sda`, ShellPolicyDisk, true, false},
		{"parted no args", `parted`, ShellPolicyDisk, false, false},
		{"parted list boundary", `parted -- -l`, ShellPolicyDisk, true, false},
		{"parted mutation", `parted /dev/sda mklabel gpt`, ShellPolicyDisk, true, false},
		{"parted mutation after print", `parted /dev/sda print mkpart primary 0% 100%`, ShellPolicyDisk, true, false},
		{"parted list and mutation", `parted -l /dev/sda rm 1`, ShellPolicyDisk, true, false},
		{"parted command value", `parted /dev/sda unit rm print`, ShellPolicyDisk, false, false},
		{"parted option value", `parted --align rm /dev/sda print`, ShellPolicyDisk, false, false},
		{"parted unknown command", `parted /dev/sda mystery`, ShellPolicyDisk, false, true},
		{"parted quoted script", `parted /dev/sda 'mklabel gpt'`, ShellPolicyDisk, false, true},
		{"parted unknown option", `parted --unknown /dev/sda print`, ShellPolicyDisk, false, true},
		{"parted known mutation dynamic value", `parted /dev/sda mklabel "$label"`, ShellPolicyDisk, true, true},
		{"parted known mutation unknown option", `parted --unknown /dev/sda rm 1`, ShellPolicyDisk, true, true},
		{"parted dynamic device", `parted "$device" print`, ShellPolicyDisk, false, true},
		{"parted missing align value", `parted --align`, ShellPolicyDisk, false, true},
		{"parted missing unit value", `parted /dev/sda unit`, ShellPolicyDisk, false, true},
		{"parted dynamic command", `parted /dev/sda "$command"`, ShellPolicyDisk, false, true},
		{"parted dynamic command boundary", `parted -- /dev/sda "$command"`, ShellPolicyDisk, false, true},
		{"parted partial command", `parted /dev/sda "${HOME}rm" 1`, ShellPolicyDisk, false, true},
		{"dd null", `dd if=/dev/zero of=/dev/null`, ShellPolicyDisk, false, false},
		{"dd zero", `dd of=/dev/zero`, ShellPolicyDisk, false, false},
		{"dd stdout", `dd of=/dev/stdout`, ShellPolicyDisk, false, false},
		{"dd fd", `dd of=/dev/fd/3`, ShellPolicyDisk, false, false},
		{"dd tty", `dd of=/dev/tty`, ShellPolicyDisk, false, false},
		{"dd random", `dd of=/dev/urandom`, ShellPolicyDisk, false, false},
		{"dd disk", `dd of=/dev/disk2`, ShellPolicyDisk, true, false},
		{"dd raw disk", `dd of=/dev/rdisk2`, ShellPolicyDisk, true, false},
		{"dd nvme", `dd of=/dev/nvme0n1`, ShellPolicyDisk, true, false},
		{"dd mapper", `dd of=/dev/mapper/root`, ShellPolicyDisk, true, false},
		{"dd hd", `dd of=/dev/hda`, ShellPolicyDisk, true, false},
		{"dd vd", `dd of=/dev/vda`, ShellPolicyDisk, true, false},
		{"dd xvd", `dd of=/dev/xvda`, ShellPolicyDisk, true, false},
		{"dd mmc", `dd of=/dev/mmcblk0`, ShellPolicyDisk, true, false},
		{"dd md", `dd of=/dev/md0`, ShellPolicyDisk, true, false},
		{"dd dm", `dd of=/dev/dm-0`, ShellPolicyDisk, true, false},
		{"dd loop", `dd of=/dev/loop0`, ShellPolicyDisk, true, false},
		{"dd mtd", `dd of=/dev/mtdblock0`, ShellPolicyDisk, true, false},
		{"dd nbd", `dd of=/dev/nbd0`, ShellPolicyDisk, true, false},
		{"dd zd", `dd of=/dev/zd0`, ShellPolicyDisk, true, false},
		{"dd sr", `dd of=/dev/sr0`, ShellPolicyDisk, true, false},
		{"dd ram", `dd of=/dev/ram0`, ShellPolicyDisk, true, false},
		{"dd device prefix only", `dd of=/dev/sd`, ShellPolicyDisk, true, false},
		{"dd device not output", `dd if=/dev/sda of=/tmp/image`, ShellPolicyDisk, false, false},
		{"dd dynamic output", `dd of="$device"`, ShellPolicyDisk, false, true},
		{"policy mention data", `printf '%s' 'git restore file.go; parted /dev/sda rm 1'`, ShellPolicyGit, false, false},
		{"short preview cancelled", `git clean -n -f --no-dry-run`, ShellPolicyGit, true, false},
		{"long preview cancelled", `git clean --dry-run --force --no-dry-run`, ShellPolicyGit, true, false},
		{"abbreviated preview negation", `git clean -nf --no-dry-r`, ShellPolicyGit, false, true},
		{"unknown long option", `git clean -nf --unknown`, ShellPolicyGit, false, true},
		{"invalid cluster", `git clean -nfZ`, ShellPolicyGit, false, true},
		{"invalid attached value", `git clean -nf --no-dry-run=yes`, ShellPolicyGit, false, true},
		{"unknown option typed denial", `git clean -f --unknown`, ShellPolicyGit, true, true},
		{"unknown global option", `git --unknown clean -nf`, ShellPolicyGit, false, true},
		{"invalid global cluster", `git -pZ clean -nf`, ShellPolicyGit, false, true},
		{"home cancels preview", `HOME=--no-dry-run; git clean -nf "$HOME"`, ShellPolicyGit, false, true},
		{"home consumes preview", `HOME=-e; git clean "$HOME" -n -f`, ShellPolicyGit, false, true},
		{"home option value", `git clean -e "$HOME" -nf`, ShellPolicyGit, false, true},
		{"home forces removal", `HOME=-f; rm -r "$HOME" ./src`, ShellPolicyDestructive, false, true},
		{"home consumes disk preview", `HOME=-o; fdisk "$HOME" -l /dev/sda`, ShellPolicyDisk, false, true},
		{"invalid removal cluster", `rm -rfZ ./src`, ShellPolicyDestructive, false, true},
		{"invalid disk preview cluster", `fdisk -lZ /dev/sda`, ShellPolicyDisk, false, true},
		{"mac chmod H", `chmod -RH 755 "$HOME"`, ShellPolicyPermissions, true, true},
		{"mac chmod L", `chmod -LR 755 "$HOME"`, ShellPolicyPermissions, true, true},
		{"mac chmod P", `chmod -RP 755 "$HOME"`, ShellPolicyPermissions, true, true},
		{"invalid chmod cluster", `chmod -RHZ 755 /`, ShellPolicyPermissions, false, true},
		{"unknown chmod option typed denial", `chmod -R --unknown 755 /`, ShellPolicyPermissions, true, true},
		{"home changes chmod recursion", `HOME=-R; chmod 755 "$HOME" /`, ShellPolicyPermissions, false, true},
		{"home expands mode and target", `HOME='777 /'; chmod $HOME ./src`, ShellPolicyPermissions, false, true},
		{"home expands mode after boundary", `HOME='777 /'; chmod -- $HOME ./src`, ShellPolicyPermissions, false, true},
		{"home expands owner after boundary", `HOME='root /'; chown -R -- $HOME ./src`, ShellPolicyPermissions, false, true},
		{"home expands reference value", `chmod -R --reference $HOME ./src`, ShellPolicyPermissions, false, true},
		{"home expands reference option", `chmod --reference=$HOME ./src`, ShellPolicyPermissions, false, true},
		{"short exclude expansion", `pattern=; git clean -e $pattern -n -f`, ShellPolicyGit, false, true},
		{"long exclude expansion", `pattern=; git clean --exclude $pattern --dry-run --force`, ShellPolicyGit, false, true},
		{"quoted exclude expansion", `git clean -e "$pattern" -nf`, ShellPolicyGit, false, true},
		{"typed denial with dynamic value", `git clean -e $pattern -f`, ShellPolicyGit, true, true},
		{"global value expansion", `dir='. reset --hard'; git -C $dir HEAD`, ShellPolicyGit, false, true},
		{"global value typed denial", `git -c $config push -f`, ShellPolicyGit, true, true},
		{"global value preview", `git --git-dir $dir clean -nf`, ShellPolicyGit, false, true},
		{"negative home mode", `chmod -R -x "$HOME"`, ShellPolicyPermissions, true, true},
		{"negative root mode", `chmod -R -w /`, ShellPolicyPermissions, true, false},
		{"negative mode incomplete target", `chmod -R -x "$HOME" "$target"`, ShellPolicyPermissions, true, true},
		{"safe short preview", `git clean -n -f`, ShellPolicyGit, false, false},
		{"safe long preview", `git clean --dry-run --force`, ShellPolicyGit, false, false},
		{"safe static exclude preview", `git clean -e '' -nf`, ShellPolicyGit, false, false},
		{"safe restored preview", `git clean -nf --no-dry-run --dry-run`, ShellPolicyGit, false, false},
		{"safe negative mode target", `chmod -R -w ./src`, ShellPolicyPermissions, false, false},
		{"nonrecursive home option ambiguity", `chmod -x "$HOME"`, ShellPolicyPermissions, false, true},
		{"safe home preview after boundary", `HOME=--no-dry-run; git clean -nf -- "$HOME"`, ShellPolicyGit, false, false},
		{"safe unsupported option after boundary", `git clean -nf -- --no-dry-r -Z`, ShellPolicyGit, false, false},
		{"safe unsupported option as value", `git clean -nf -e --no-dry-r`, ShellPolicyGit, false, false},
		{"safe invalid cluster as value", `git clean -nf -e -Z`, ShellPolicyGit, false, false},
		{"safe literal home", `git clean -nf '$HOME'`, ShellPolicyGit, false, false},
		{"safe removal home after boundary", `HOME=-f; rm -r -- "$HOME" ./src`, ShellPolicyDestructive, false, false},
		{"safe disk home after boundary", `HOME=-o; fdisk -l -- "$HOME"`, ShellPolicyDisk, false, false},
		{"safe chmod home after boundary", `chmod -- 755 "$HOME"`, ShellPolicyPermissions, false, false},
		{"safe chmod mac project", `chmod -RH 755 ./src`, ShellPolicyPermissions, false, false},
		{"chmod mac root", `chmod -RH 755 /`, ShellPolicyPermissions, true, false},
		{"chmod home target after boundary", `chmod -RH -- 755 "$HOME"`, ShellPolicyPermissions, true, false},
		{"chmod home mode after boundary", `chmod -- 777 "$HOME"`, ShellPolicyPermissions, true, false},
		{"chmod home glob after boundary", `chmod -R -- 755 "$HOME"/*`, ShellPolicyPermissions, true, false},
		{"chown home target after boundary", `chown -R -- root "$HOME"`, ShellPolicyPermissions, true, false},
		{"safe git help", `git --help`, ShellPolicyGit, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			analysis := analyzeShell(context.Background(), tc.source, syntax.LangBash)
			denied, incomplete := checkShellPolicy(tc.profile, analysis)
			if denied != tc.deny || (incomplete != "") != tc.incomplete {
				t.Errorf("checkShellPolicy() = (%v, %q), want (%v, incomplete=%v)", denied, incomplete, tc.deny, tc.incomplete)
			}
			input, err := json.Marshal(map[string]string{"command": tc.source})
			if err != nil {
				t.Fatal(err)
			}
			in := agento11y.HookInput{Output: []agento11y.Message{{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{
				agento11y.ToolCallPart(agento11y.ToolCall{Name: "Bash", InputJSON: input}),
			}}}}
			for _, legacyMatch := range []bool{false, true} {
				pattern := `a^`
				if legacyMatch {
					pattern = `(?s).`
				}
				ce, err := compileEvaluator(EvaluatorSpec{Kind: "regex", Config: map[string]any{
					"target": "shell_command", "pattern": pattern, "reject": true,
				}})
				if err != nil {
					t.Fatal(err)
				}
				evaluator := &shellPolicyEvaluator{profile: tc.profile, fallback: ce.(*regexEvaluator)}
				cache := newShellRequestCache(context.Background(), []ShellMetadata{{Dialect: "bash"}})
				ran, passed, explanation := evaluator.evaluate(in, cache)
				wantPassed := !(tc.deny || (tc.incomplete && legacyMatch))
				if !ran || passed != wantPassed || strings.Contains(explanation, "used legacy regex matching") != tc.incomplete {
					t.Errorf("legacyMatch=%v: evaluator = (%v, %v, %q), want passed=%v, fallback=%v", legacyMatch, ran, passed, explanation, wantPassed, tc.incomplete)
				}
			}
		})
	}
}

func TestShellPolicyInvocationIsolation(t *testing.T) {
	analysis := shellAnalysis{invocations: []shellInvocation{
		{args: shellPolicyTestWords("rm", "-r")},
		{args: shellPolicyTestWords("echo", "-f")},
		{args: shellPolicyTestWords("rm", "-f")},
	}}
	if denied, incomplete := checkShellPolicy(ShellPolicyDestructive, analysis); denied || incomplete != "" {
		t.Fatalf("flags crossed invocations: (%v, %q)", denied, incomplete)
	}
	analysis.invocations = append(analysis.invocations, shellInvocation{args: shellPolicyTestWords("rm", "-rf")})
	analysis.incomplete = "unsupported shell syntax"
	denied, incomplete := checkShellPolicy(ShellPolicyDestructive, analysis)
	if !denied || incomplete != analysis.incomplete {
		t.Fatalf("lost denial or analyzer diagnostic: (%v, %q)", denied, incomplete)
	}
	if denied, incomplete := checkShellPolicy("", analysis); denied || incomplete != "" {
		t.Fatalf("empty profile evaluated analysis: (%v, %q)", denied, incomplete)
	}
}
