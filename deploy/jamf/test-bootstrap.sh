#!/bin/sh
# Portable contract test for the Jamf bootstrap's user-context path.
#
# This deliberately uses a fake agento11y binary, so it can run on Linux CI.
# The macOS-specific root-to-console-user handoff still needs the pilot in
# README.md.
set -eu

root="$(mktemp -d)"
trap 'rm -rf "$root"' EXIT HUP INT TERM

home="$root/home"
receipt_dir="$root/receipt"
fake_bin="$root/agento11y"
calls="$root/calls"
bootstrap_dir="$(CDPATH= cd "$(dirname "$0")" && pwd)"
bootstrap="$bootstrap_dir/bootstrap.sh"

cat > "$fake_bin" <<'EOF'
#!/bin/sh
set -eu
if [ "$*" != 'agents reconcile --agents claude,cursor --json' ]; then
  echo "unexpected arguments: $*" >&2
  exit 1
fi
printf '%s\n' "$AGENTO11Y_TEST_CALLS" >> "$AGENTO11Y_TEST_CALLS"
printf '%s\n' '{"schema_version":1,"status":"converged","agento11y":{"version":"test"},"config":{"revision":"1"},"agents":[{"name":"claude","status":"installed"},{"name":"cursor","status":"already_installed"}]}'
EOF
chmod 755 "$fake_bin"

run_bootstrap() {
  HOME="$home" \
    AGENTO11Y_RECEIPT_DIR="$receipt_dir" \
    AGENTO11Y_BIN="$fake_bin" \
    AGENTO11Y_TEST_CALLS="$calls" \
    "$bootstrap"
}

missing_config='{"schema_version":1,"status":"deferred_missing_config","agents":[]}'
output="$(run_bootstrap)"
test "$output" = "$missing_config"
test "$(cat "$receipt_dir/jamf-reconcile.json")" = "$missing_config"

mkdir -p "$home/.config/agento11y"
printf '%s\n' 'AGENTO11Y_MANAGED_CONFIG_REVISION=1' > "$home/.config/agento11y/config.env"
expected='{"schema_version":1,"status":"converged","agento11y":{"version":"test"},"config":{"revision":"1"},"agents":[{"name":"claude","status":"installed"},{"name":"cursor","status":"already_installed"}]}'

output="$(run_bootstrap)"
test "$output" = "$expected"
test "$(cat "$receipt_dir/jamf-reconcile.json")" = "$expected"

output="$(run_bootstrap)"
test "$output" = "$expected"
test "$(wc -l < "$calls" | tr -d ' ' )" = 2
