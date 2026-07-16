#!/bin/sh
# install.sh - Download and install the latest agento11y binary.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/grafana/sigil-sdk/main/plugins/sigil/scripts/install.sh | sh
#
# Environment variables:
#   INSTALL_DIR    Directory to install into (default: $HOME/.local/bin)
#   VERSION        Specific version to install, without v prefix (default: latest)
#   GITHUB_TOKEN   GitHub token for API requests (avoids rate limits)

set -eu

GITHUB_REPO="grafana/sigil-sdk"
BINARY_NAME="agento11y"
# Old command name, installed as a symlink to agento11y so existing
# setups keep working.
LEGACY_BINARY_NAME="sigil"
# Binary releases are tagged plugins/sigil/v<ver> in the monorepo.
TAG_PREFIX="plugins/sigil/v"
DEFAULT_INSTALL_DIR="${HOME}/.local/bin"

info() {
    printf '  %s\n' "$@"
}

warn() {
    printf '  WARNING: %s\n' "$@" >&2
}

err() {
    printf '  ERROR: %s\n' "$@" >&2
    exit 1
}

need_cmd() {
    if ! command -v "$1" >/dev/null 2>&1; then
        err "Required command '$1' not found. Please install it and try again."
    fi
}

detect_os() {
    os="$(uname -s)"
    case "$os" in
        Linux)  echo "linux" ;;
        Darwin) echo "darwin" ;;
        *)      err "Unsupported OS: $os. This installer supports Linux and macOS." \
                    "On Windows, download the zip from https://github.com/${GITHUB_REPO}/releases" ;;
    esac
}

detect_arch() {
    arch="$(uname -m)"
    case "$arch" in
        x86_64|amd64)   echo "amd64" ;;
        aarch64|arm64)  echo "arm64" ;;
        *)              err "Unsupported architecture: $arch" ;;
    esac
}

detect_user_shell() {
    if [ -n "${SHELL:-}" ]; then
        printf '%s\n' "${SHELL##*/}"
    else
        printf '%s\n' "sh"
    fi
}

print_path_instructions() {
    install_dir="$1"
    shell_name=$(detect_user_shell)

    echo ""
    case "$shell_name" in
        bash)
            info "${install_dir} is not in your PATH. Add it with:"
            echo ""
            info "  echo 'export PATH=\"${install_dir}:\$PATH\"' >> ~/.bashrc"
            info "  . ~/.bashrc"
            ;;
        zsh)
            info "${install_dir} is not in your PATH. Add it with:"
            echo ""
            info "  echo 'export PATH=\"${install_dir}:\$PATH\"' >> ~/.zshrc"
            info "  source ~/.zshrc"
            ;;
        fish)
            info "${install_dir} is not in your PATH. Add it with:"
            echo ""
            info "  mkdir -p ~/.config/fish"
            info "  echo 'fish_add_path ${install_dir}' >> ~/.config/fish/config.fish"
            info "  source ~/.config/fish/config.fish"
            ;;
        *)
            info "${install_dir} is not in your PATH. Add it to your shell startup file:"
            echo ""
            info "  export PATH=\"${install_dir}:\$PATH\""
            ;;
    esac
}

get_latest_version() {
    # The monorepo hosts releases for more than the sigil binary, so
    # releases/latest is not guaranteed to be one of ours, and the newest
    # plugins/sigil/v* release can fall past the first page once other
    # components publish enough releases. Page through (newest first) and
    # take the first plugins/sigil/v* tag we hit.
    auth_header=""
    if [ -n "${GITHUB_TOKEN:-}" ]; then
        auth_header="Authorization: Bearer ${GITHUB_TOKEN}"
    fi

    page=1
    while [ "$page" -le 10 ]; do
        url="https://api.github.com/repos/${GITHUB_REPO}/releases?per_page=100&page=${page}"
        if [ -n "$auth_header" ]; then
            response=$(curl -fsSL -H "$auth_header" "$url") || err "Failed to fetch releases from GitHub API."
        else
            response=$(curl -fsSL "$url") || err "Failed to fetch releases from GitHub API. If rate-limited, set GITHUB_TOKEN or VERSION."
        fi

        tag=$(printf '%s' "$response" |
            grep -o "\"tag_name\": *\"${TAG_PREFIX}[^\"]*\"" |
            sed 's/.*: *"//;s/"$//' |
            head -1)
        if [ -n "$tag" ]; then
            # Strip the tag prefix; archive filenames use bare version numbers.
            printf '%s' "${tag#"${TAG_PREFIX}"}"
            return 0
        fi

        # Stop once a page comes back with no releases at all.
        if ! printf '%s' "$response" | grep -q '"tag_name"'; then
            break
        fi

        page=$((page + 1))
    done

    err "Could not find a ${TAG_PREFIX}* release."
}

verify_checksum() {
    archive_path="$1"
    expected="$2"

    if command -v sha256sum >/dev/null 2>&1; then
        actual=$(sha256sum "$archive_path" | cut -d' ' -f1)
    elif command -v shasum >/dev/null 2>&1; then
        actual=$(shasum -a 256 "$archive_path" | cut -d' ' -f1)
    else
        warn "Neither sha256sum nor shasum found. Skipping checksum verification."
        return 0
    fi

    if [ "$actual" != "$expected" ]; then
        err "Checksum mismatch! Expected: ${expected}, got: ${actual}"
    fi
    info "Checksum verified."
}

main() {
    need_cmd curl
    need_cmd tar

    os=$(detect_os)
    arch=$(detect_arch)

    if [ -n "${VERSION:-}" ]; then
        version="${VERSION#v}"
    else
        info "Fetching latest release..."
        version=$(get_latest_version)
    fi

    install_dir="${INSTALL_DIR:-$DEFAULT_INSTALL_DIR}"
    base_url="https://github.com/${GITHUB_REPO}/releases/download/${TAG_PREFIX}${version}"

    info "Installing ${BINARY_NAME} ${version} (${os}/${arch})"

    tmpdir=$(mktemp -d)
    trap 'rm -rf "$tmpdir"' EXIT

    # Download the archive. Releases older than the agento11y rename only
    # ship sigil_* assets, so on a 404 retry with the old asset name. Any
    # other failure is fatal.
    asset_prefix="${BINARY_NAME}"
    archive="${asset_prefix}_${version}_${os}_${arch}.tar.gz"
    info "Downloading ${archive}..."
    status=$(curl -sSL -o "${tmpdir}/${archive}" -w '%{http_code}' "${base_url}/${archive}") ||
        err "Failed to download ${base_url}/${archive}"
    if [ "$status" = "404" ]; then
        asset_prefix="${LEGACY_BINARY_NAME}"
        archive="${asset_prefix}_${version}_${os}_${arch}.tar.gz"
        info "Not found; this release predates the agento11y rename. Downloading ${archive}..."
        curl -fsSL "${base_url}/${archive}" -o "${tmpdir}/${archive}" ||
            err "Failed to download ${base_url}/${archive}"
    elif [ "$status" != "200" ]; then
        err "Failed to download ${base_url}/${archive} (HTTP ${status})"
    fi

    checksums_file="${asset_prefix}_${version}_checksums.txt"
    curl -fsSL "${base_url}/${checksums_file}" -o "${tmpdir}/${checksums_file}" ||
        err "Failed to download checksums file."

    # Verify checksum.
    expected=$(grep "${archive}" "${tmpdir}/${checksums_file}" | cut -d' ' -f1)
    if [ -z "$expected" ]; then
        err "Archive ${archive} not found in checksums file."
    fi
    verify_checksum "${tmpdir}/${archive}" "$expected"

    # Extract the binary. The executable inside the archive is named after
    # the asset prefix (agento11y, or sigil in pre-rename releases).
    tar xzf "${tmpdir}/${archive}" -C "${tmpdir}" "${asset_prefix}" ||
        err "Failed to extract ${asset_prefix} from archive."

    # Install the binary as agento11y even if it came from an old sigil
    # archive, and add a sigil symlink so the old name keeps working.
    mkdir -p "$install_dir"
    mv "${tmpdir}/${asset_prefix}" "${install_dir}/${BINARY_NAME}"
    chmod +x "${install_dir}/${BINARY_NAME}"
    ln -sf "${BINARY_NAME}" "${install_dir}/${LEGACY_BINARY_NAME}"

    # Remove macOS quarantine attribute if present.
    if [ "$os" = "darwin" ] && command -v xattr >/dev/null 2>&1; then
        xattr -d com.apple.quarantine "${install_dir}/${BINARY_NAME}" 2>/dev/null || true
    fi

    # Verify installation.
    if "${install_dir}/${BINARY_NAME}" --version >/dev/null 2>&1; then
        installed_version=$("${install_dir}/${BINARY_NAME}" --version 2>&1 | head -1)
        info "Installed: ${installed_version}"
    else
        info "Installed ${BINARY_NAME} to ${install_dir}/${BINARY_NAME}"
    fi

    # Check if install dir is in PATH.
    case ":${PATH}:" in
        *":${install_dir}:"*) ;;
        *)
            print_path_instructions "$install_dir"
            ;;
    esac

    echo ""
    info "To uninstall: rm ${install_dir}/${BINARY_NAME} ${install_dir}/${LEGACY_BINARY_NAME}"
}

main "$@"
