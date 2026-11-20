#!/bin/sh
# The release workflow fills in the tag and the public key before publishing.

fail() {
    printf 'transcoop install: %s\n' "$*" >&2
    exit 1
}

existing_install() {
    printf 'Transcoop is already installed at %s\n' "$1"
    printf '%s\n' 'For releases with the updater, run transcoop update using the executable above.'
    printf '%s\n' 'For v0.1.0-alpha.1, download the new archive and replace the executable at that path manually.'
}

supports_raw_signatures() {
    "$1" pkeyutl -help 2>&1 | grep -q -e '-rawin'
}

find_openssl() {
    if command -v openssl >/dev/null 2>&1 && supports_raw_signatures openssl; then
        command -v openssl
        return
    fi
    if command -v brew >/dev/null 2>&1 && prefix=$(brew --prefix openssl@3 2>/dev/null) &&
        supports_raw_signatures "$prefix/bin/openssl"; then
        printf '%s\n' "$prefix/bin/openssl"
        return
    fi
    return 1
}

download() {
    curl --disable --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
        --connect-timeout 15 --max-time 300 --max-filesize "$2" \
        --output "$stage/$1" "$release_url/$1" || fail "Could not download $1 from $release_url."
}

install_transcoop() {
    set -eu
    version='@TRANSCOOP_VERSION@'
    public_key='@TRANSCOOP_UPDATE_PUBLIC_KEY@'
    case "$version" in
        v*) ;;
        *) fail 'Download install.sh from a tagged Transcoop release.' ;;
    esac
    case ${HOME:-} in
        /*) ;;
        *) fail 'Set HOME to the absolute path of your home directory.' ;;
    esac
    install_dir="$HOME/.local/bin"
    target="$install_dir/transcoop"
    if [ -f "$target" ] && [ -x "$target" ]; then
        existing_install "$target"
        return
    fi
    if [ -e "$target" ] || [ -L "$target" ]; then
        fail "The install path is occupied: $target"
    fi
    if installed=$(command -v transcoop); then
        existing_install "$installed"
        return
    fi

    os=$(uname -s)
    case "$os" in
        Linux) os=linux ;;
        Darwin) os=darwin ;;
        *) fail "Unsupported operating system: $os. This installer supports Linux and macOS." ;;
    esac
    arch=$(uname -m)
    case "$arch" in
        x86_64|amd64) arch=amd64 ;;
        arm64|aarch64) arch=arm64 ;;
        *) fail "Unsupported architecture: $arch. Choose an x64 or ARM64 release." ;;
    esac
    openssl=$(find_openssl) || fail 'Install OpenSSL 3 to verify release signatures. On macOS with Homebrew, run brew install openssl@3.'
    command -v curl >/dev/null 2>&1 || fail 'Install curl to download the release.'
    command -v tar >/dev/null 2>&1 || fail 'Install tar to unpack the release.'

    release_url="https://github.com/friesencr/transcoop/releases/download/$version"
    stem="transcoop_${version}_${os}_${arch}"
    archive="$stem.tar.gz"
    umask 077
    mkdir -p "$install_dir" || fail "Cannot create $install_dir."
    stage=$(mktemp -d "$install_dir/.transcoop-install.XXXXXXXX") || fail "Cannot stage the install in $install_dir."
    trap 'rm -rf "$stage"' 0
    trap 'exit 1' HUP INT TERM

    download SHA256SUMS 1048576
    download SHA256SUMS.sig 64
    printf '%s\n' '-----BEGIN PUBLIC KEY-----' "$public_key" '-----END PUBLIC KEY-----' > "$stage/key.pem"
    "$openssl" pkeyutl -verify -rawin -pubin -inkey "$stage/key.pem" \
        -in "$stage/SHA256SUMS" -sigfile "$stage/SHA256SUMS.sig" >/dev/null ||
        fail 'The release checksum signature could not be verified.'
    expected=$(LC_ALL=C awk -v name="$archive" '
        $2 == name && NF == 2 { hash = $1; count++ }
        END {
            if (count != 1 || length(hash) != 64 || hash ~ /[^0-9a-fA-F]/) exit 1
            print tolower(hash)
        }' "$stage/SHA256SUMS") || fail "Expected one signed SHA-256 checksum for $archive."

    download "$archive" 134217728
    digest=$("$openssl" dgst -sha256 -r < "$stage/$archive") || fail "Could not hash $archive."
    [ "${digest%% *}" = "$expected" ] || fail "SHA-256 checksum mismatch for $archive."

    tar -xzOf "$stage/$archive" "$stem/transcoop" > "$stage/transcoop" || fail "Cannot extract $stem/transcoop."
    chmod 755 "$stage/transcoop"
    reported=$("$stage/transcoop" version) || fail 'The downloaded executable failed its version check.'
    [ "$reported" = "transcoop $version" ] || fail "The downloaded executable did not report $version."

    # A same-filesystem hard link publishes the complete file only if the name is free.
    if ! ln "$stage/transcoop" "$install_dir/" 2>/dev/null; then
        if [ -f "$target" ] && [ -x "$target" ]; then
            existing_install "$target"
            return
        fi
        fail "Cannot install at $target. Check permissions and hard-link support or install the archive manually."
    fi
    printf 'Installed transcoop %s at %s\n' "$version" "$target"
    printf '%s\n' 'Run guided setup with: "$HOME/.local/bin/transcoop" setup'
}

# Keep the call last so a shell reading a partial download cannot start installation.
install_transcoop
