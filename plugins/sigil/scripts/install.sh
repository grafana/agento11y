#!/bin/sh
# Forwarding stub for the pre-rename installer URL.
#
# The plugin moved from plugins/sigil/ to plugins/agento11y/ as part of
# the sigil -> agento11y rename. This stub keeps the documented
# one-liner working:
#
#   curl -fsSL https://raw.githubusercontent.com/grafana/sigil-sdk/main/plugins/sigil/scripts/install.sh | sh
#
# It downloads and runs the real installer from its new location.

set -eu

INSTALL_URL="https://raw.githubusercontent.com/grafana/sigil-sdk/main/plugins/agento11y/scripts/install.sh"

echo "  This installer moved to ${INSTALL_URL}"
echo "  Fetching and running it..."

# Fetch first, then run: piping curl straight into sh would hide a
# failed download (POSIX sh has no pipefail), making the one-liner
# report success while installing nothing.
script=$(curl -fsSL "$INSTALL_URL")
printf '%s\n' "$script" | sh
