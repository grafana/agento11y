#!/bin/sh
# Cursor hook entrypoint for macOS/Linux. Locates the sigil binary and
# execs `sigil cursor hook`.
#
# Cursor inherits its parent process's PATH, which on macOS GUI launches is
# launchd's default (/usr/bin:/bin:/usr/sbin:/sbin) and on Linux desktop
# launches is whatever the .desktop launcher set — neither contains the
# usual `go install` target ($HOME/go/bin) or Homebrew's bin. We probe the
# common locations directly. Set SIGIL_BIN to override.
set -u

# Iterate over a newline-delimited list so paths with spaces (e.g. macOS
# user homes) survive word-splitting.
while IFS= read -r b; do
  [ -n "$b" ] && [ -x "$b" ] && exec "$b" cursor hook "$@"
done <<EOF
${SIGIL_BIN:-}
${HOME:-}/go/bin/sigil
/opt/homebrew/bin/sigil
/usr/local/bin/sigil
${HOME:-}/.local/bin/sigil
EOF

# Binary not installed yet. Write a permissive response so Cursor doesn't
# block beforeSubmitPrompt; exit 0 so the hook indicator stays green.
# Telemetry resumes once the user runs `go install`.
echo '{"continue":true,"permission":"allow"}'
exit 0
