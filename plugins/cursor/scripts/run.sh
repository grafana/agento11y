#!/bin/sh
# Cursor hook entrypoint for macOS/Linux. Locates the agento11y binary
# (or its legacy sigil name) and execs `agento11y cursor hook`.
#
# Cursor inherits its parent process's PATH, which on macOS GUI launches is
# launchd's default (/usr/bin:/bin:/usr/sbin:/sbin) and on Linux desktop
# launches is whatever the .desktop launcher set — neither contains the
# usual `go install` target ($HOME/go/bin) or Homebrew's bin. We probe the
# common locations directly, preferring the canonical agento11y name over
# the legacy sigil one. Set AGENTO11Y_BIN (or the legacy SIGIL_BIN) to
# override.
set -u

# Save the original stdin (the JSON payload from Cursor) before the heredoc
# below replaces fd 0 with the candidate-path list.
exec 3<&0

# Iterate over a newline-delimited list so paths with spaces (e.g. macOS
# user homes) survive word-splitting.
while IFS= read -r b; do
  [ -n "$b" ] && [ -x "$b" ] && exec "$b" cursor hook "$@" <&3
done <<EOF
${AGENTO11Y_BIN:-}
${SIGIL_BIN:-}
${HOME:-}/go/bin/agento11y
/opt/homebrew/bin/agento11y
/usr/local/bin/agento11y
${HOME:-}/.local/bin/agento11y
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
