package guardeval

import (
	"context"
	"errors"
	"path"
	"reflect"
	"strings"
	"testing"
	"time"

	"mvdan.cc/sh/v3/syntax"
)

func TestShellAnalysis(t *testing.T) {
	tests := []struct {
		name       string
		source     string
		names      string
		incomplete bool
	}{
		{"empty", "", "", false},
		{"comment", "# rm -rf /", "", false},
		{"executable basename", "/bin/rm -rf /", "rm", false},
		{"executable quoted", `'/bin/rm' -rf /`, "rm", false},
		{"executable concatenated", `r""'m' -rf /`, "rm", false},
		{"executable escaped", `\r\m -rf /`, "rm", false},
		{"executable newline escape", "r\\\nm -rf /", "rm", false},
		{"literal echo", `echo 'rm -rf /'`, "echo", false},
		{"substitution echo", `echo "$(rm -rf /)"`, "rm,echo", false},
		{"backtick echo", "echo `rm -rf /`", "rm,echo", false},
		{"dynamic data", `echo "$anything"; printf '%s' "$value"; cat "$file"`, "echo,printf,cat", false},
		{"literal printf", `printf '%s' '$(rm -rf /)'`, "printf", false},
		{"assignment", `X=$(rm -rf /)`, "rm", false},
		{"assignment with call", `X=$(rm -rf /) echo ok`, "rm,echo", false},
		{"export substitution", `export X=$(rm -rf /)`, "rm", false},
		{"redirect substitution", `echo ok >"$(rm -rf /)"`, "rm,echo", false},
		{"process substitution", `cat <(rm -rf /) >(chmod 777 /)`, "rm,chmod,cat", false},
		{"parameter substitution", `echo "${x:-$(rm -rf /)}"`, "rm,echo", false},
		{"arithmetic substitution", `echo "$(( $(rm -rf /) + 1 ))"`, "rm,echo", true},
		{"arithmetic recursive", `(( value ))`, "", true},
		{"array index", `echo "${a[$x]}"`, "echo", true},
		{"indirect expansion", `ref='a[$(rm -rf ./src)]'; echo "${!ref}"`, "echo", true},
		{"indirect expansion unquoted", `echo ${!ref}`, "echo", true},
		{"indirect expansion transformation", `echo "${!ref@Q}"`, "echo", true},
		{"indirect expansion scalar", `ref=value; echo "${!ref}"`, "echo", true},
		{"indirect expansion retains calls", `rm -rf /; echo "${!ref}"; git status`, "rm,echo,git", true},
		{"indirect expansion literal data", `echo '${!ref}'`, "echo", false},
		{"name reference declaration", `declare -n ref='a[$(rm -rf ./src)]'; echo "$ref"`, "echo", true},
		{"name reference declaration combined flags", `declare -gn ref='a[$(rm -rf ./src)]'; [[ -v ref ]]`, "", true},
		{"name reference typeset", `typeset -n ref='a[$(rm -rf ./src)]'; echo "$ref"`, "echo", true},
		{"name reference local", `f() { local -n ref='a[$(rm -rf ./src)]'; [[ -v ref ]]; }`, "", true},
		{"name reference flag terminated", `declare -- '-n'`, "", false},
		{"export removal flag", `export -n ref='a[$(rm -rf ./src)]'; echo "$ref"`, "echo", false},
		{"parameter names star", `echo "${!prefix*}"`, "echo", false},
		{"parameter names at", `echo "${!prefix@}"`, "echo", false},
		{"prompt transformation", `x='$(rm -rf ./src)'; echo "${x@P}"`, "echo", true},
		{"prompt transformation unquoted", `echo ${x@P}`, "echo", true},
		{"prompt transformation retains calls", `rm -rf /; echo "${x@P}"; git push --force`, "rm,echo,git", true},
		{"prompt transformation literal data", `echo '${x@P}'`, "echo", false},
		{"parameter quote transformation", `x='$(rm -rf ./src)'; echo "${x@Q}"`, "echo", false},
		{"parameter case transformation", `echo "${x@U}"`, "echo", false},
		{"numeric test eq", `[[ 'a[$(rm -rf ./src)]' -eq 0 ]]`, "", true},
		{"numeric test ne", `[[ 0 -ne 'a[$(rm -rf ./src)]' ]]`, "", true},
		{"numeric test le", `[[ "$x" -le 0 ]]`, "", true},
		{"numeric test ge", `[[ 0 -ge "$x" ]]`, "", true},
		{"numeric test lt", `[[ x -lt 0 ]]`, "", true},
		{"numeric test gt", `[[ 0 -gt x ]]`, "", true},
		{"numeric test nested retains calls", `[[ x == y || ( ! 0 -eq "$(rm -rf /)" ) ]]; chmod 777 /`, "rm,chmod", true},
		{"numeric test literals", `[[ 1 -eq 1 ]]`, "", true},
		{"unary name test", `a=(); [[ -v 'a[$(rm -rf ./src)]' ]]`, "", true},
		{"unary name test escaped substitution", `[[ -v "a[\$(rm -rf ./src)]" ]]`, "", true},
		{"unary name test dynamic", `[[ -v "$ref" ]]`, "", true},
		{"unary name test literal index", `[[ -v 'a[0]' ]]`, "", true},
		{"unary reference test", `[[ -R 'a[$(rm -rf ./src)]' ]]`, "", true},
		{"unary reference test dynamic", `[[ -R "$ref" ]]`, "", true},
		{"unary name test substitution", `[[ -v "a[$(rm -rf /)]" ]]; git status`, "rm,git", true},
		{"unary name test nested", `[[ ! -v 'a[x]' || -n x ]]; chmod 777 /`, "chmod", true},
		{"unary name test scalar", `[[ -v value ]]`, "", false},
		{"unary name test quoted scalar", `[[ -v '_value2' ]]`, "", false},
		{"unary reference test scalar", `[[ -R ref ]]`, "", false},
		{"unary file test data", `[[ -e 'a[$(rm -rf ./src)]' ]]`, "", false},
		{"string test quoted data", `[[ 'a[$(rm -rf ./src)]' == x ]]`, "", false},
		{"string test ordering data", `[[ 'a[$(rm -rf ./src)]' < x ]]`, "", false},
		{"string test unary data", `[[ -n 'a[$(rm -rf ./src)]' ]]`, "", false},
		{"indexed assignment", `x='a[$(rm -rf ./src)]'; a[x]=0`, "", true},
		{"indexed append", `a[x]+=0; echo ok`, "echo", true},
		{"indexed compound assignment", `a=([x]=0)`, "", true},
		{"indexed assignment retains substitution", `a[$(rm -rf /)]=0; git status`, "rm,git", true},
		{"indexed assignment literals", `a[0]=data`, "", true},
		{"scalar assignment data", `x='a[$(rm -rf ./src)]'; echo "$x"`, "echo", false},
		{"array assignment data", `a=('a[$(rm -rf ./src)]')`, "", false},
		{"c style loop", `a=(); x='a[$(rm -rf ./src)]'; for (( i=x; i<0; i++ )); do :; done`, ":", true},
		{"c style loop condition", `for (( ; x; )); do echo ok; done`, "echo", true},
		{"c style loop retains calls", `for (( i=$(rm -rf /); i<0; i++ )); do git status; done; chmod 777 /`, "rm,git,chmod", true},
		{"c style empty loop", `for (( ;; )); do :; done`, ":", true},
		{"word loop data", `for x in 'a[$(rm -rf ./src)]'; do echo "$x"; done`, "echo", false},
		{"integer declaration", `declare -i x='a[$(rm -rf ./src)]'`, "", true},
		{"integer declaration combined flags", `declare -gi x='a[$(rm -rf ./src)]'`, "", true},
		{"integer declaration quoted flag", `declare '-i' x=x`, "", true},
		{"integer typeset", `typeset -i x=x`, "", true},
		{"integer local", `f() { local -i x=x; echo ok; }`, "echo", true},
		{"integer declaration dynamic flag", `declare "$flags" x=x`, "", true},
		{"integer declaration retains calls", `declare -i x=$(rm -rf /); git status`, "rm,git", true},
		{"integer attribute before assignment", `declare -i x; x='a[$(rm -rf ./src)]'`, "", true},
		{"quoted indexed declaration", `declare -a 'a[$(rm -rf ./src)]=0'`, "", true},
		{"quoted compound declaration", `declare -a 'a=($(rm -rf ./src))'`, "", true},
		{"quoted compound value", `declare -a a='($(rm -rf ./src))'`, "", true},
		{"quoted compound readonly", `readonly -a 'a=($(rm -rf ./src))'`, "", true},
		{"quoted compound after boundary", `declare -a -- 'a=($(rm -rf ./src))'`, "", true},
		{"quoted indexed declaration implicit", `declare 'a[$(rm -rf ./src)]=0'`, "", true},
		{"quoted indexed declaration flags", `declare -ga 'a[$(rm -rf ./src)]+=0'`, "", true},
		{"quoted indexed declaration terminated options", `declare -a -- 'a[$(rm -rf ./src)]=0'`, "", true},
		{"quoted indexed declaration after scalar", `declare x=y 'a[$(rm -rf ./src)]=0'`, "", true},
		{"quoted indexed declaration name only", `declare -a 'a[x]'`, "", true},
		{"quoted indexed declaration literal index", `declare -a 'a[0]=data'`, "", true},
		{"quoted indexed typeset", `typeset -a 'a[$(rm -rf ./src)]=0'`, "", true},
		{"quoted indexed local", `f() { local -a 'a[$(rm -rf ./src)]=0'; echo ok; }`, "echo", true},
		{"quoted indexed readonly", `readonly -a 'a[$(rm -rf ./src)]=0'`, "", true},
		{"quoted indexed export", `export 'a[$(rm -rf ./src)]=0'`, "", true},
		{"quoted indexed declaration retains calls", `declare -a "a[$(rm -rf /)]=0"; git status`, "rm,git", true},
		{"quoted scalar declaration data", `declare 'x=a[$(rm -rf ./src)]'`, "", false},
		{"quoted scalar declaration terminated options", `declare -- 'x=a[$(rm -rf ./src)]'`, "", false},
		{"declaration array data", `declare -a a=('a[$(rm -rf ./src)]')`, "", false},
		{"declaration scalar data", `declare x='a[$(rm -rf ./src)]'`, "", false},
		{"declaration flag in value", `declare x=-i`, "", false},
		{"declaration flag after terminator", `declare -- '-i'`, "", false},
		{"export scalar data", `export x='a[$(rm -rf ./src)]'`, "", false},
		{"printf indexed destination", `printf -v 'a[$(rm -rf ./src)]' %s x`, "printf", true},
		{"printf indexed destination attached", `printf '-va[$(rm -rf ./src)]' %s x`, "printf", true},
		{"printf dynamic destination", `printf -v "$dest" %s x`, "printf", true},
		{"printf dynamic option", `printf "$option" 'a[$(rm -rf ./src)]' %s x`, "printf", true},
		{"printf indexed destination wrapped", `command printf -v 'a[x]' %s x; rm -rf /`, "command,printf,rm", true},
		{"printf destination substitution", `printf -v "a[$(rm -rf /)]" %s x; chmod 777 /`, "rm,printf,chmod", true},
		{"printf indexed destination literal", `printf -v 'a[0]' %s x`, "printf", true},
		{"printf scalar destination", `printf -v result %s 'a[$(rm -rf ./src)]'`, "printf", false},
		{"printf scalar destination attached", `printf -v_result2 %s "$x"`, "printf", false},
		{"printf data flag", `printf %s -v 'a[$(rm -rf ./src)]'`, "printf", false},
		{"printf terminated options", `printf -- -v 'a[$(rm -rf ./src)]'`, "printf", false},
		{"printf format data", `printf 'a[$(rm -rf ./src)]'`, "printf", false},
		{"pipeline", `printf x | cat && rm -rf / || chmod 777 /`, "printf,cat,rm,chmod", false},
		{"compound", `(echo x); { rm -rf /; }; if true; then chmod 777 /; else chown root /; fi`, "echo,rm,true,chmod,chown", false},
		{"loops", `for x in "$(rm -rf /)"; do echo "$x"; done; while false; do chmod 777 /; done; until true; do chown root /; done`, "rm,echo,false,chmod,true,chown", false},
		{"case", `case "$(rm -rf /)" in x) chmod 777 /;; *) echo x;; esac`, "rm,chmod,echo", false},
		{"function definition", `f() { rm -rf /; }; f`, "rm,f", true},
		{"function body only", `f() { rm -rf /; }`, "rm", false},
		{"test substitution", `[[ "$(rm -rf /)" == x ]]`, "rm", false},
		{"coproc and time", `coproc cat; time rm -rf /`, "cat,rm", false},
		{"dynamic executable", `"$cmd" -rf /`, "?", true},
		{"substitution executable", `$(printf rm) -rf /`, "printf,?", true},
		{"glob executable", `r? -rf /`, "?", true},
		{"brace executable", `r{m,sync} -rf /`, "?", true},
		{"dynamic operand", `rm "$target"`, "rm", false},
		{"root glob operand", `rm -rf /*`, "rm", false},
		{"dynamic policy flags", `rm "$flags" /`, "rm", false},
		{"dynamic home operand", `chmod -R 777 "$HOME"`, "chmod", false},
		{"dynamic nondata command argument", `cp "$source" /tmp/dest`, "cp", true},
		{"known git command", `git status`, "git", false},
		{"known disk commands", `dd if=/dev/zero of=/tmp/data; mkfs.ext4 /tmp/image`, "dd,mkfs.ext4", false},
		{"unknown executor", `mystery 'rm -rf /'`, "mystery", true},
		{"interpreter", `python -c 'rm -rf /'`, "python", true},
		{"eval", `eval 'rm -rf /'`, "eval", true},
		{"source file", `. /tmp/script`, ".", true},
		{"script file executable", `/tmp/script`, "script", true},
		{"find executor", `find . -exec rm -rf / \;`, "find", true},
		{"data heredoc", "cat <<'EOF'\nrm -rf /\nEOF\n", "cat", false},
		{"data quoted substitution heredoc", "cat <<'EOF'\n$(rm -rf /)\nEOF\n", "cat", false},
		{"data expanding heredoc", "cat <<EOF\n$(rm -rf /)\nEOF\n", "rm,cat", false},
		{"data heredoc quotes do not suppress expansion", "cat <<EOF\n'$(rm -rf /)'\nEOF\n", "rm,cat", false},
		{"data heredoc escaped substitution", "cat <<EOF\n\\$(rm -rf /)\nEOF\n", "cat", false},
		{"shell literal heredoc", "sh <<'EOF'\nrm -rf /\nEOF\n", "sh,rm", false},
		{"shell unquoted literal heredoc", "sh <<EOF\nrm -rf /\nEOF\n", "sh,rm", false},
		{"shell quoted executable substitution heredoc", "sh <<'EOF'\n$(printf rm) -rf /\nEOF\n", "sh,printf,?", true},
		{"shell expanding executable substitution heredoc", "sh <<EOF\n$(printf rm) -rf /\nEOF\n", "printf,sh", true},
		{"shell substitution in quoted heredoc", "bash <<'EOF'\necho $(rm -rf /)\nEOF\n", "bash,rm,echo", false},
		{"shell expanding heredoc", "sh <<EOF\necho $script\nEOF\n", "sh", true},
		{"shell escaped heredoc delimiter", "sh <<\\EOF\nrm -rf /\nEOF\n", "sh,rm", false},
		{"shell tabs heredoc", "sh <<-'EOF'\n\trm -rf /\n\tEOF\n", "sh,rm", false},
		{"shell empty heredoc", "sh <<'EOF'\nEOF\n", "sh", false},
		{"shell unquoted heredoc backslash", "sh <<EOF\nr\\m -rf /\nEOF\n", "sh,rm", false},
		{"shell unquoted heredoc escaped substitution", "sh <<EOF\necho \\$(rm -rf /)\nEOF\n", "sh,rm,echo", false},
		{"shell quoted heredoc backslash", "sh <<'EOF'\nr\\m -rf /\nEOF\n", "sh,rm", false},
		{"here string data", `cat <<< 'rm -rf /'`, "cat", false},
		{"here string shell", `sh <<< 'rm -rf /'`, "sh,rm", false},
		{"dynamic here string shell", `sh <<< "$script"`, "sh", true},
		{"nonstdin heredoc", "sh 3<<'EOF'\nrm -rf /\nEOF\n", "sh", true},
		{"last stdin wins", "sh <<'EOF' </tmp/script\nrm -rf /\nEOF\n", "sh", true},
		{"explicit fd zero output replaces stdin", "sh <<'EOF' 0>/tmp/file\nrm -rf /\nEOF\n", "sh", true},
		{"explicit fd zero duplicate replaces stdin", "sh <<'EOF' 0>&3\nrm -rf /\nEOF\n", "sh", true},
		{"explicit fd zero heredoc", "sh 00<<'EOF'\nrm -rf /\nEOF\n", "sh,rm", false},
		{"multiple heredocs", "sh <<'A' <<'B'\necho ignored\nA\nrm -rf /\nB\n", "sh,rm", false},
		{"inherited shell input", "(sh) <<'EOF'\nrm -rf /\nEOF\n", "sh", true},
		{"piped shell input", `printf 'rm -rf /' | sh`, "printf,sh", true},
		{"malformed retains prior", `rm -rf /; echo 'unfinished`, "rm", true},
		{"malformed operator", `rm -rf /; |`, "rm", true},
		{"malformed heredoc", "cat <<EOF\nrm -rf /\n", "", true},
		{"malformed heredoc redirect", "sh <<EO&#0000000000000000000000\n", "", true},
		{"dollar quote", `echo $'rm\n-rf /'`, "echo", true},
		{"translated quote", `echo $"rm -rf /"`, "echo", true},
		{"dollar quote shell payload", `sh -c $'rm -rf /'`, "sh", true},
		{"no execution", `echo '$(touch /tmp/should-not-exist)'`, "echo", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := analyzeShell(context.Background(), tt.source, syntax.LangBash)
			checkShellResult(t, got, tt.names, tt.incomplete)
		})
	}
}

func TestShellWrappersAndPayloads(t *testing.T) {
	tests := []struct {
		source     string
		names      string
		incomplete bool
	}{
		{`env rm -rf /`, "env,rm", false},
		{`/usr/bin/env -i -u SECRET --chdir=/tmp X=y /bin/rm -rf /`, "env,rm", false},
		{`env -- X=y rm -rf /`, "env,rm", false},
		{`env -uSECRET -C/tmp -- rm -rf /`, "env,rm", false},
		{`env --unset SECRET --chdir /tmp rm -rf /`, "env,rm", false},
		{`env`, "env", false},
		{`env X=y -i rm -rf /`, "env,-i", true},
		{`env -S 'rm -rf /'`, "env", true},
		{`env "$opt" rm -rf /`, "env", true},
		{`env -u "$name" rm -rf /`, "env", true},
		{`env -u`, "env", true},
		{`env X="$value" rm -rf /`, "env", true},
		{`sudo -n -u root -- rm -rf /`, "sudo,rm", false},
		{`sudo -nEu root -g wheel --preserve-env=HOME X=y rm -rf /`, "sudo,rm", false},
		{`sudo --user=root --group wheel --chdir=/tmp rm -rf /`, "sudo,rm", false},
		{`sudo -uroot -groot rm -rf /`, "sudo,rm", false},
		{`sudo -s rm -rf /`, "sudo", true},
		{`sudo -i rm -rf /`, "sudo", true},
		{`sudo --unknown rm -rf /`, "sudo", true},
		{`sudo -u "$user" rm -rf /`, "sudo", true},
		{`sudo`, "sudo", true},
		{`command -p -- rm -rf /`, "command,rm", false},
		{`command -v rm`, "command", false},
		{`command -V rm`, "command", false},
		{`command -x rm -rf /`, "command", true},
		{`exec -cl -a custom /bin/rm -rf /`, "exec,rm", false},
		{`exec -aexample -- /bin/rm -rf /`, "exec,rm", false},
		{`exec -a`, "exec", true},
		{`exec -x rm -rf /`, "exec", true},
		{`exec`, "exec", false},
		{`env -i sudo -n command -p exec -- /bin/rm -rf /`, "env,sudo,command,exec,rm", false},
		{`env sh -c 'rm -rf /'`, "env,sh,rm", false},
		{`bash -lc 'rm -rf /'`, "bash,rm", false},
		{`bash -c -- 'rm -rf /'`, "bash,rm", false},
		{`bash -c -x 'rm -rf /'`, "bash,rm", false},
		{`sh -c ''`, "sh", false},
		{`sh -c "echo '\$(rm -rf /)'"`, "sh,echo", false},
		{`sh -c 'echo "$(rm -rf /)"'`, "sh,rm,echo", false},
		{`sh -B -c 'rm -rf /'`, "sh", true},
		{`/bin/dash -ec 'rm -rf /'`, "dash,rm", false},
		{`bash --noprofile --norc -o pipefail -c 'rm -rf /'`, "bash,rm", false},
		{`sh -c 'echo "$1"' ignored "$data"`, "sh,echo", false},
		{`sh -c '"$1" -rf /' ignored rm`, "sh,?", true},
		{`sh -c "$script"`, "sh", true},
		{`sh -c`, "sh", true},
		{`sh -c 'rm -rf /; echo "unfinished'`, "sh,rm", true},
		{`sh /tmp/script`, "sh", true},
		{`sh "$script"`, "sh", true},
		{`bash --rcfile /tmp/rc -c 'rm -rf /'`, "bash", true},
		{`sh -o unknown -c 'rm -rf /'`, "sh", true},
		{`bash -O extglob -c 'rm -rf /'`, "bash", true},
		{`sh -c '[[ x == x ]]; rm -rf /'`, "sh,?,rm", true},
		{`bash -c '[[ x == x ]]; rm -rf /'`, "bash,rm", false},
		{`bash -c 'echo "${x@P}"; rm -rf /'`, "bash,echo,rm", true},
		{`bash -c '[[ x -eq 0 ]]; git status'`, "bash,git", true},
		{`bash -c 'printf -v "a[x]" %s x; chmod 777 /'`, "bash,printf,chmod", true},
		{`bash -c 'sh -c "rm -rf /"'`, "bash,sh,rm", false},
		{"sh -s ignored <<'EOF'\nrm -rf /\nEOF\n", "sh,rm", false},
		{"sh -- <<'EOF'\nrm -rf /\nEOF\n", "sh,rm", false},
		{"env sh <<'EOF'\nrm -rf /\nEOF\n", "env,sh,rm", false},
		{"sh <<'EOF'\n[[ x == x ]]\nEOF\n", "sh,?", true},
		{"bash <<'EOF'\n[[ x == x ]]\nrm -rf /\nEOF\n", "bash,rm", false},
	}
	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			got := analyzeShell(context.Background(), tt.source, syntax.LangBash)
			checkShellResult(t, got, tt.names, tt.incomplete)
		})
	}
}

func TestShellGitExecutionConfig(t *testing.T) {
	tests := []struct {
		name       string
		source     string
		names      string
		incomplete bool
	}{
		{"ssh command", `git -c core.sshCommand='rm -rf ./src #' ls-remote ssh://example/repo`, "git", true},
		{"attached config", `git -ccore.sshCommand='rm -rf ./src #' ls-remote ssh://example/repo`, "git", true},
		{"quoted config", `git '-c' 'core.sshCommand=rm -rf ./src #' ls-remote ssh://example/repo`, "git", true},
		{"config key case", `git -c CORE.SSHCOMMAND='rm -rf ./src #' ls-remote ssh://example/repo`, "git", true},
		{"config env", `git --config-env=core.sshCommand=SSH_CMD ls-remote ssh://example/repo`, "git", true},
		{"config env separate", `git --config-env core.sshCommand=SSH_CMD ls-remote ssh://example/repo`, "git", true},
		{"multiple config", `git -c color.ui=false -c core.sshCommand='rm -rf ./src #' ls-remote ssh://example/repo`, "git", true},
		{"shell alias", `git -c 'alias.run=!rm -rf ./src' run`, "git", true},
		{"alias injecting config", `git -c 'alias.run=-c core.sshCommand="rm -rf ./src #" ls-remote' run ssh://example/repo`, "git", true},
		{"unused execution config", `git -c core.sshCommand=ssh status`, "git", true},
		{"global directory", `git -C /repo -c core.sshCommand='rm -rf ./src #' ls-remote ssh://example/repo`, "git", true},
		{"global attached directory", `git -C/repo --git-dir=/repo/.git --work-tree /repo -ccore.sshCommand=x status`, "git", true},
		{"wrapped git", `env -i command -- /usr/bin/git -c core.sshCommand='rm -rf ./src #' ls-remote ssh://example/repo`, "env,command,git", true},
		{"nested shell", `bash -c 'git -ccore.sshCommand="rm -rf ./src #" ls-remote ssh://example/repo'`, "bash,git", true},
		{"retains calls", `rm -rf /; git -c core.sshCommand='rm -rf ./src #' ls-remote ssh://example/repo; chmod 777 /`, "rm,git,chmod", true},
		{"retains substitution", `git -c core.sshCommand="$(rm -rf /)" ls-remote ssh://example/repo`, "rm,git", true},
		{"dynamic config", `git -c "$config" status`, "git", true},
		{"dynamic config env", `git --config-env="$config" status`, "git", true},
		{"dynamic global option", `git "$option" status`, "git", true},
		{"missing config", `git -c`, "git", true},
		{"missing config env", `git --config-env`, "git", true},
		{"ssh command environment", `GIT_SSH_COMMAND='rm -rf ./src #' git ls-remote ssh://example/repo`, "git", true},
		{"ssh executable environment", `GIT_SSH=./script git ls-remote ssh://example/repo`, "git", true},
		{"exported ssh command", `export GIT_SSH_COMMAND='rm -rf ./src #'; git ls-remote ssh://example/repo`, "git", true},
		{"quoted exported ssh command", `export 'GIT_SSH_COMMAND=rm -rf ./src #'; git ls-remote ssh://example/repo`, "git", true},
		{"dynamic ssh command", `GIT_SSH_COMMAND="$payload" git ls-remote ssh://example/repo`, "git", true},
		{"ssh command env wrapper", `env GIT_SSH_COMMAND='rm -rf ./src #' git ls-remote ssh://example/repo`, "env,git", true},
		{"ssh command sudo wrapper", `sudo -u root GIT_SSH_COMMAND='rm -rf ./src #' git ls-remote ssh://example/repo`, "sudo,git", true},
		{"ssh executable env wrapper", `env -- GIT_SSH=./script git ls-remote ssh://example/repo`, "env,git", true},
		{"unused explicit ssh command", `GIT_SSH_COMMAND=ssh; echo ok`, "echo", true},
		{"ordinary git", `git status; git diff; git ls-remote ssh://example/repo`, "git,git,git", false},
		{"ordinary config", `git -c color.ui=false -ccore.autocrlf=false status`, "git", true},
		{"ordinary config env", `git --config-env=color.ui=COLOR --config-env core.autocrlf=AUTOCRLF status`, "git", true},
		{"ordinary config value", `git -c user.name='rm -rf ./src #' status`, "git", true},
		{"unknown config key", `git -c unknown.key='rm -rf ./src #' status`, "git", true},
		{"browser command", `git -c web.browser=test -c 'browser.test.cmd=rm -rf ./src #' web--browse /tmp/example`, "git", true},
		{"explicit environment config", `GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.sshCommand GIT_CONFIG_VALUE_0='rm -rf ./src #' git ls-remote ssh://example/repo`, "git", true},
		{"wrapped environment config", `env GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.sshCommand GIT_CONFIG_VALUE_0='rm -rf ./src #' git status`, "env,git", true},
		{"quoted exported config", `export 'GIT_CONFIG_VALUE_0=rm -rf ./src #'; git status`, "git", true},
		{"config file override", `GIT_CONFIG_GLOBAL=./config git status`, "git", true},
		{"exec path query ignores remaining args", `git --exec-path -c 'core.sshCommand=rm -rf ./src #' status`, "git", false},
		{"version ignores remaining args", `git --version -c 'core.sshCommand=rm -rf ./src #' status`, "git", false},
		{"ordinary global options", `git --no-pager -C /repo --git-dir /repo/.git --work-tree=/repo status`, "git", false},
		{"global option value is data", `git -C '-ccore.sshCommand=rm -rf ./src #' status`, "git", false},
		{"global long option value is data", `git --work-tree '-ccore.sshCommand=rm -rf ./src #' status`, "git", false},
		{"subcommand option is data", `git commit -c 'core.sshCommand=rm -rf ./src #'`, "git", false},
		{"path after terminator is data", `git add -- '-ccore.sshCommand=rm -rf ./src #'`, "git", false},
		{"config query is data", `git config --get core.sshCommand`, "git", false},
		{"dynamic path remains data", `git status -- "$file"`, "git", false},
		{"ssh environment spelling is data", `echo 'GIT_SSH_COMMAND=rm -rf ./src #'`, "echo", false},
		{"unrelated environment", `env OTHER='GIT_SSH_COMMAND=rm -rf ./src #' git status`, "env,git", false},
		{"unset ssh command", `env -u GIT_SSH_COMMAND --unset=GIT_SSH git status`, "env,git", false},
		{"wrapper directory value is data", `env -C 'GIT_SSH_COMMAND=rm -rf ./src #' git status`, "env,git", false},
		{"ssh variant is data", `GIT_SSH_VARIANT=ssh git status`, "git", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := analyzeShell(context.Background(), tt.source, syntax.LangBash)
			checkShellResult(t, got, tt.names, tt.incomplete)
			if tt.incomplete && got.incomplete != "unsupported git execution configuration" && got.incomplete != "unsupported git invocation" {
				t.Fatalf("unexpected reason: %q", got.incomplete)
			}
		})
	}
}

func TestShellWords(t *testing.T) {
	tests := []struct {
		source string
		word   shellWord
	}{
		{`''`, shellWord{value: "", static: true}},
		{`""`, shellWord{value: "", static: true}},
		{`ab' cd'"ef"`, shellWord{value: "ab cdef", static: true}},
		{`a\ b`, shellWord{value: "a b", static: true}},
		{`'a\b'`, shellWord{value: `a\b`, static: true}},
		{`"a\qb"`, shellWord{value: `a\qb`, static: true}},
		{`"\$HOME"`, shellWord{value: "$HOME", static: true}},
		{`"\"\\\` + "`" + `"`, shellWord{value: "\"\\`", static: true}},
		{`\$HOME`, shellWord{value: "$HOME", static: true}},
		{`'$HOME'`, shellWord{value: "$HOME", static: true}},
		{`'${HOME}'`, shellWord{value: "${HOME}", static: true}},
		{`~`, shellWord{value: "~", rootOrHome: true}},
		{`~/`, shellWord{value: "~/", rootOrHome: true}},
		{`~/*`, shellWord{value: "~/*", rootOrHome: true}},
		{`~/file`, shellWord{value: "~/file"}},
		{`~"/"`, shellWord{value: "~/"}},
		{`~/''`, shellWord{value: "~/", rootOrHome: true}},
		{`~\/*`, shellWord{value: "~/*"}},
		{`~root`, shellWord{value: "~root"}},
		{`'~'`, shellWord{value: "~", static: true}},
		{`"~"`, shellWord{value: "~", static: true}},
		{`\~`, shellWord{value: "~", static: true}},
		{`''~`, shellWord{value: "~", static: true}},
		{`/`, shellWord{value: "/", static: true, rootOrHome: true}},
		{`'/'`, shellWord{value: "/", static: true, rootOrHome: true}},
		{`/*`, shellWord{value: "/*", rootOrHome: true}},
		{`'/*'`, shellWord{value: "/*", static: true}},
		{`/\*`, shellWord{value: "/*", static: true}},
		{`"/"*`, shellWord{value: "/*", rootOrHome: true}},
		{`/tmp`, shellWord{value: "/tmp", static: true}},
		{`$HOME`, shellWord{rootOrHome: true}},
		{`${HOME}`, shellWord{rootOrHome: true}},
		{`"$HOME"`, shellWord{rootOrHome: true}},
		{`${HOME}/`, shellWord{value: "/", rootOrHome: true}},
		{`"${HOME}/"`, shellWord{value: "/", rootOrHome: true}},
		{`"$HOME"/*`, shellWord{value: "/*", rootOrHome: true}},
		{`"${HOME}/*"`, shellWord{value: "/*"}},
		{`~/"*"`, shellWord{value: "~/*"}},
		{`"$HOME"/'*'`, shellWord{value: "/*"}},
		{`${HOME}/file`, shellWord{value: "/file"}},
		{`${HOME:-/}`, shellWord{}},
		{`${!HOME}`, shellWord{}},
		{`${HOME%/}`, shellWord{}},
		{`${HOME:0}`, shellWord{}},
		{`${HOME[0]}`, shellWord{}},
		{`$HOME$HOME`, shellWord{}},
		{`prefix$HOME`, shellWord{value: "prefix"}},
		{`$HOMES`, shellWord{}},
		{`"$other"`, shellWord{}},
		{`\*`, shellWord{value: "*", static: true}},
		{`"*"`, shellWord{value: "*", static: true}},
		{`a?`, shellWord{value: "a?"}},
		{`a[bc]`, shellWord{value: "a[bc]"}},
		{`a{b,c}`, shellWord{value: "a{b,c}"}},
	}
	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			got := analyzeShell(context.Background(), "echo "+tt.source, syntax.LangBash)
			if len(got.invocations) != 1 || len(got.invocations[0].args) != 2 {
				t.Fatalf("unexpected analysis: %#v", got)
			}
			if word := got.invocations[0].args[1]; word != tt.word {
				t.Fatalf("word = %#v, want %#v", word, tt.word)
			}
		})
	}
	got := analyzeShell(context.Background(), `echo '' "a b" c\ d ab'cd'"ef"`, syntax.LangBash)
	want := []shellWord{{value: "echo", static: true}, {value: "", static: true}, {value: "a b", static: true}, {value: "c d", static: true}, {value: "abcdef", static: true}}
	if len(got.invocations) != 1 || !reflect.DeepEqual(got.invocations[0].args, want) {
		t.Fatalf("argument boundaries lost: %#v", got)
	}
}

func TestShellLimitsAndControls(t *testing.T) {
	for _, source := range []string{"echo ok\r", "echo '\r'", "echo ok\x00", "# comment\x00", "cat <<'EOF'\nx\r\nEOF\n"} {
		got := analyzeShell(context.Background(), source, syntax.LangBash)
		checkShellResult(t, got, "", true)
		if got.incomplete != "unsupported shell control character" {
			t.Fatalf("unexpected control-character result: %#v", got)
		}
	}
	for _, source := range []string{`sh -c $'rm -rf /\r'`, `sh -c $'rm -rf /\x00'`} {
		checkShellResult(t, analyzeShell(context.Background(), source, syntax.LangBash), "sh", true)
	}
	t.Run("source boundary", func(t *testing.T) {
		source := "echo '" + strings.Repeat("x", shellSourceLimit-len("echo ''")) + "'"
		checkShellResult(t, analyzeShell(context.Background(), source, syntax.LangBash), "echo", false)
		checkShellResult(t, analyzeShell(context.Background(), source+" ", syntax.LangBash), "", true)
	})
	t.Run("cumulative source", func(t *testing.T) {
		payload := "echo '" + strings.Repeat("x", 33000) + "'"
		source := "rm -rf /; sh -c " + shellTestQuote(payload) + "; chmod 777 /"
		checkShellResult(t, analyzeShell(context.Background(), source, syntax.LangBash), "rm,sh,chmod", true)
	})
	t.Run("node boundary", func(t *testing.T) {
		checkShellResult(t, analyzeShell(context.Background(), "echo "+strings.Repeat("x ", 4093), syntax.LangBash), "echo", false)
		checkShellResult(t, analyzeShell(context.Background(), "echo "+strings.Repeat("x ", 4094), syntax.LangBash), "", true)
	})
	t.Run("node budget", func(t *testing.T) {
		source := "rm -rf /; " + strings.Repeat("echo x; ", 2000)
		got := analyzeShell(context.Background(), source, syntax.LangBash)
		if got.incomplete != "shell node limit exceeded" || len(got.invocations) == 0 || got.invocations[0].name != "rm" || len(got.invocations) >= 2001 {
			t.Fatalf("node budget or retained invocations incorrect: %d, %q", len(got.invocations), got.incomplete)
		}
	})
	t.Run("cumulative nodes", func(t *testing.T) {
		payload := strings.Repeat("echo x; ", 800)
		source := "sh -c " + shellTestQuote(payload) + "; sh -c " + shellTestQuote(payload)
		got := analyzeShell(context.Background(), source, syntax.LangBash)
		if got.incomplete != "shell node limit exceeded" {
			t.Fatalf("nested AST nodes not accumulated: %q", got.incomplete)
		}
	})
	t.Run("depth", func(t *testing.T) {
		for depth := 0; depth <= shellDepthLimit+1; depth++ {
			source := "rm -rf /"
			for range depth {
				source = "sh -c " + shellTestQuote(source)
			}
			got := analyzeShell(context.Background(), source, syntax.LangBash)
			want := strings.Repeat("sh,", depth) + "rm"
			if depth > shellDepthLimit {
				want = strings.TrimSuffix(strings.Repeat("sh,", depth), ",")
			}
			checkShellResult(t, got, want, depth > shellDepthLimit)
		}
	})
}

func TestShellContextAndDialect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	checkShellResult(t, analyzeShell(ctx, "rm -rf /", syntax.LangBash), "", true)
	ctx, cancel = context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	checkShellResult(t, analyzeShell(ctx, "rm -rf /", syntax.LangBash), "", true)
	counted := &shellTestContext{Context: context.Background(), remaining: 100}
	got := analyzeShell(counted, "rm -rf /; "+strings.Repeat("echo x; ", 1000), syntax.LangBash)
	if got.incomplete != "shell analysis canceled" || len(got.invocations) == 0 || got.invocations[0].name != "rm" || len(got.invocations) >= 1001 {
		t.Fatalf("cancellation during traversal: %d, %q", len(got.invocations), got.incomplete)
	}
	checkShellResult(t, analyzeShell(context.Background(), "rm -rf /", syntax.LangVariant(-1)), "", true)
	checkShellResult(t, analyzeShell(context.Background(), "rm -rf /", syntax.LangAuto), "", true)
	checkShellResult(t, analyzeShell(context.Background(), "rm -rf /", 0), "rm", false)
	checkShellResult(t, analyzeShell(context.Background(), "[[ x == x ]]", syntax.LangPOSIX), "?", true)
	checkShellResult(t, analyzeShell(context.Background(), "[[ x == x ]]", syntax.LangBash), "", false)
	got = analyzeShell(context.Background(), `echo a{b,c}`, syntax.LangPOSIX)
	if len(got.invocations) != 1 || !got.invocations[0].args[1].static {
		t.Fatal("POSIX literal braces were expanded")
	}
}

type shellTestContext struct {
	context.Context
	remaining int
}

func (c *shellTestContext) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		return errors.New("sensitive context error must not escape")
	}
	return nil
}

func checkShellResult(t *testing.T, got shellAnalysis, names string, incomplete bool) {
	t.Helper()
	var actual []string
	for _, invocation := range got.invocations {
		name := invocation.name
		if name == "" {
			name = "?"
		}
		actual = append(actual, name)
	}
	if strings.Join(actual, ",") != names || (got.incomplete != "") != incomplete {
		t.Fatalf("analysis = names %q, incomplete %q; want names %q, incomplete %v", strings.Join(actual, ","), got.incomplete, names, incomplete)
	}
	if !shellTestReason(got.incomplete) {
		t.Fatalf("unsanitized reason: %q", got.incomplete)
	}
}

func shellTestReason(reason string) bool {
	switch reason {
	case "", "shell analysis canceled", "unsupported shell control character", "shell source limit exceeded", "shell nesting limit exceeded", "unsupported shell dialect", "invalid shell syntax", "shell node limit exceeded", "unsupported shell quoting", "unsupported shell arithmetic", "unsupported shell prompt expansion", "unsupported shell indirect expansion", "unsupported shell printf destination", "unsupported git execution configuration", "unsupported git invocation", "dynamic shell executable", "unsupported shell wrapper", "dynamic shell argument", "unknown shell executor", "dynamic shell script", "unsupported shell option", "shell script file is not analyzed", "shell input is not literal":
		return true
	}
	return false
}

func shellTestQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func FuzzShellAnalysis(f *testing.F) {
	for _, source := range []string{
		"", "rm -rf /", `echo 'rm -rf /'`, `echo "$(rm -rf /)"`,
		`env -i sudo -u root command exec rm -rf /`, `sh -c 'rm -rf /'`,
		"cat <<'EOF'\nrm -rf /\nEOF\n", "sh <<EOF\n$(printf rm) -rf /\nEOF\n",
		`echo "${HOME:-$(rm -rf /)}"`, `echo $'\x00'`, "echo '\r'", "echo \x00", "echo '",
		"sh <<EO&#0000000000000000000000\n",
		`a=(); [[ -v 'a[$(rm -rf ./src)]' ]]`, `[[ -R "$ref" ]]`,
		`declare -a 'a[$(rm -rf ./src)]=0'`, `declare -- 'x=a[$(rm -rf ./src)]'`,
		`ref='a[$(rm -rf ./src)]'; echo "${!ref}"`, `echo "${!prefix@}"`,
		`declare -n ref='a[$(rm -rf ./src)]'; [[ -v ref ]]`,
		`git -c core.sshCommand='rm -rf ./src #' ls-remote ssh://example/repo`,
		`git -ccore.sshCommand='rm -rf ./src #' status`, `git --config-env=core.sshCommand=PAYLOAD status`,
		`env GIT_SSH_COMMAND='rm -rf ./src #' git status`, `git -c color.ui=false status`,
	} {
		f.Add(source)
	}
	f.Fuzz(func(t *testing.T, source string) {
		for _, dialect := range []syntax.LangVariant{syntax.LangBash, syntax.LangPOSIX} {
			got := analyzeShell(context.Background(), source, dialect)
			if !shellTestReason(got.incomplete) {
				t.Fatalf("unsanitized reason: %q", got.incomplete)
			}
			if !reflect.DeepEqual(got, analyzeShell(context.Background(), source, dialect)) {
				t.Fatal("analysis is not deterministic")
			}
			if len(got.invocations) > shellNodeLimit {
				t.Fatal("unbounded invocation count")
			}
			for _, invocation := range got.invocations {
				if len(invocation.args) == 0 {
					t.Fatal("invocation lost argv")
				}
				if invocation.name != "" && (!invocation.args[0].static || invocation.name != path.Base(invocation.args[0].value)) {
					t.Fatal("executable was not statically decoded")
				}
			}
			if len(source) > shellSourceLimit || strings.ContainsAny(source, "\r\x00") {
				if got.incomplete == "" || len(got.invocations) != 0 {
					t.Fatal("invalid source reached analysis")
				}
			}
		}
	})
}

func BenchmarkShellSimple(b *testing.B) {
	benchmarkShellSource(b, `env -i sudo -n command -- /bin/rm -rf /`)
}

func BenchmarkShellNested(b *testing.B) {
	benchmarkShellSource(b, `bash -lc 'echo "$(rm -rf /)"; sh -c "chmod 777 /"'`)
}

func BenchmarkShellLarge(b *testing.B) {
	benchmarkShellSource(b, strings.Repeat("echo x; ", 1500))
}

func BenchmarkShellData(b *testing.B) {
	benchmarkShellSource(b, "cat <<'EOF'\n"+strings.Repeat("rm -rf /\n", 4000)+"EOF\n")
}

func BenchmarkShellAdversarial(b *testing.B) {
	for _, tc := range []struct{ name, source string }{
		{"source-limit", "echo '" + strings.Repeat("x", shellSourceLimit-len("echo ''")) + "'"},
		{"deep-blocks", strings.Repeat("(", 4000) + "true" + strings.Repeat(")", 4000)},
		{"deep-substitutions", strings.Repeat("echo $(", 2000) + "true" + strings.Repeat(")", 2000)},
		{"unterminated-quote", "echo '" + strings.Repeat("x", shellSourceLimit-len("echo '"))},
	} {
		b.Run(tc.name, func(b *testing.B) { benchmarkShellSource(b, tc.source) })
	}
}

func benchmarkShellSource(b *testing.B, source string) {
	b.Helper()
	b.ReportAllocs()
	b.SetBytes(int64(len(source)))
	for b.Loop() {
		analyzeShell(context.Background(), source, syntax.LangBash)
	}
}
