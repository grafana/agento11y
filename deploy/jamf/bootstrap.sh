#!/bin/sh
# Bootstrap Agent Observability for coding agents on one managed macOS user.
#
# Run this script after MDM has installed the agento11y binary and written the
# current user's ~/.config/agento11y/config.env. Jamf runs scripts as root, so
# this script resolves the console user and re-executes its installation step in
# that user's launchd context. It never receives or logs credentials: the
# config file is the only input that carries them. See README.md in this
# directory for the Jamf policy sequence.
set -eu

agento11y_bin="${AGENTO11Y_BIN:-/usr/local/bin/agento11y}"
agents="${AGENTO11Y_AGENTS:-claude,cursor}"

emit_deferred_receipt() {
  printf '{"schema_version":1,"status":"%s","agento11y":{"version":null},"config":{"revision":null},"agents":[]}\n' "$1"
}

if [ "$(id -u)" -eq 0 ]; then
  console_user="$(/usr/bin/stat -f%Su /dev/console)"
  case "$console_user" in
    ""|root|loginwindow)
      emit_deferred_receipt deferred_no_user
      exit 0
      ;;
  esac
  console_uid="$(/usr/bin/id -u "$console_user")"
  console_home="$(/usr/bin/dscl . -read "/Users/$console_user" NFSHomeDirectory | /usr/bin/awk '{print $2}')"
  if [ -z "$console_home" ]; then
    echo "could not resolve home directory for $console_user" >&2
    exit 1
  fi
  exec /bin/launchctl asuser "$console_uid" /usr/bin/sudo -u "$console_user" /usr/bin/env \
    HOME="$console_home" USER="$console_user" LOGNAME="$console_user" \
    PATH="$console_home/.local/bin:$console_home/.npm-global/bin:$console_home/.bun/bin:$console_home/.volta/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin" \
    AGENTO11Y_BIN="$agento11y_bin" AGENTO11Y_AGENTS="$agents" \
    "$0" --user-context
fi

if [ "${1:-}" = "--user-context" ]; then
  shift
fi

# LaunchAgents inherit launchd's minimal PATH. Keep the user-local locations
# that commonly contain Claude Code even when this script did not start as
# root and therefore skipped the launchctl asuser branch above.
managed_path="$HOME/.local/bin:$HOME/.npm-global/bin:$HOME/.bun/bin:$HOME/.volta/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
PATH="$managed_path:${PATH:-/usr/bin:/bin:/usr/sbin:/sbin}"
export PATH

if [ ! -x "$agento11y_bin" ]; then
  echo "agento11y binary is not executable: $agento11y_bin" >&2
  exit 1
fi

config_file="$HOME/.config/agento11y/config.env"
receipt_dir="${AGENTO11Y_RECEIPT_DIR:-$HOME/Library/Application Support/agento11y}"
receipt_file="$receipt_dir/jamf-reconcile.json"
umask 077
mkdir -p "$receipt_dir"

write_receipt() {
  tmp_receipt="$receipt_file.tmp"
  printf '%s\n' "$1" > "$tmp_receipt"
  mv "$tmp_receipt" "$receipt_file"
}

if [ ! -r "$config_file" ]; then
  receipt="$(emit_deferred_receipt deferred_missing_config)"
  printf '%s\n' "$receipt"
  write_receipt "$receipt"
  exit 0
fi

set +e
receipt="$("$agento11y_bin" agents reconcile --agents "$agents" --json)"
status=$?
set -e
printf '%s\n' "$receipt"
write_receipt "$receipt"
exit "$status"
