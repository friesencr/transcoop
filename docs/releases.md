# Publishing a Transcoop release

A `v`-prefixed semantic version tag triggers the release workflow. It builds
the six Linux, macOS and Windows archives and renders a POSIX shell installer
with that tag and the release public key. It signs a checksum manifest covering
all seven files and publishes the assets and generated notes on GitHub. A prerelease tag such as
`v0.1.0-alpha.2` creates a GitHub prerelease.

## Set up the release key once

The signing key authenticates the archives used by the installer and updater.
From a private directory outside the checkout, use OpenSSL 3 and an
authenticated GitHub CLI account with access to the repository settings.

```sh
umask 077
openssl genpkey -algorithm ED25519 -out transcoop-update-key.pem
gh secret set TRANSCOOP_UPDATE_SIGNING_KEY --repo friesencr/transcoop < transcoop-update-key.pem
openssl pkey -in transcoop-update-key.pem -pubout -outform DER | openssl base64 -A | gh variable set TRANSCOOP_UPDATE_PUBLIC_KEY --repo friesencr/transcoop
```

Export a public copy for independent verification with
`openssl pkey -in transcoop-update-key.pem -pubout -out release-public.pem`.
Store an encrypted backup of the private key under the maintainer's control.
Keep the same key for subsequent releases. Each binary embeds the public key
it will use for the next update. Changing the repository variable alone
breaks that chain. If the key is lost or compromised, distribute a new trusted
binary and public key through a manual install.

## Publish

1. Add release notes to the changelog. Confirm the signed upgrade test passes
   on Linux, macOS and Windows for the commit you intend to tag.
2. Create and push the version tag, for example `git tag v0.1.0-alpha.2` and
   `git push origin v0.1.0-alpha.2`. The workflow checks that the signing
   secret and public variable match before it publishes.
3. Confirm the release has six archives, `install.sh`, `SHA256SUMS`, and
   `SHA256SUMS.sig`. Update the README download links to this version. At the
   top of **Install**, add the following commands for Linux and macOS with
   `TAG` replaced by the published tag. Keep the direct downloads for Windows
   and manual installs.

```sh
curl -fsSL --proto '=https' --proto-redir '=https' 'https://github.com/friesencr/transcoop/releases/download/TAG/install.sh' | sh
"$HOME/.local/bin/transcoop" setup
```

The first command trusts the installer served by that versioned GitHub URL.
The installer uses its embedded key to verify the signed checksums before it
installs the matching archive. It needs curl, tar, and OpenSSL 3. On macOS it
also recognizes Homebrew's `openssl@3`. The second command starts interactive
setup and offers service installation on Linux with systemd.

To verify a downloaded archive manually, obtain the public key from a trusted
copy of the release key. With its PEM public key in `release-public.pem`, run
these commands in the directory with the archive and both checksum files
(Linux with OpenSSL 3 and GNU coreutils).

```sh
openssl pkeyutl -verify -rawin -pubin -inkey release-public.pem -in SHA256SUMS -sigfile SHA256SUMS.sig &&
  sha256sum --check --ignore-missing SHA256SUMS
```

To exercise an update locally with an ephemeral test key and two real CLI
binaries, run the integration test from the repository root.

```sh
TRANSCOOP_UPDATE_INTEGRATION=1 go test -count=1 -run TestPackagedUpgrade -v ./internal/update
```

Run `python3 scripts/test-install.py` on Linux or macOS to test a fresh
installation from a temporary HTTPS release server.
