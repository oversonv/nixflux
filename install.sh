#!/bin/sh

set -eu

VERSION=${NIXFLUX_VERSION:-v0.1.0-dev}
REPOSITORY=${NIXFLUX_REPOSITORY:-oversonv/nixflux}

fail() {
	printf 'nixflux installer: %s\n' "$*" >&2
	exit 1
}

case "$(uname -s)" in
	Linux) ;;
	*) fail "unsupported operating system: $(uname -s)" ;;
esac

case "$(uname -m)" in
	aarch64|arm64) ;;
	*) fail "unsupported architecture: $(uname -m); this installer supports ARM64 only" ;;
esac

if [ -n "${PREFIX:-}" ] && [ -d "$PREFIX" ]; then
	TARGET=nixflux-android-arm64
	INSTALL_DIR=${NIXFLUX_INSTALL_DIR:-$PREFIX/bin}
else
	TARGET=nixflux-linux-arm64
	INSTALL_DIR=${NIXFLUX_INSTALL_DIR:-$HOME/.local/bin}
fi

BASE_URL=${NIXFLUX_RELEASE_BASE_URL:-https://github.com/$REPOSITORY/releases/download/$VERSION}
URL=$BASE_URL/$TARGET

if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
	fail "curl or wget is required to download the binary"
fi

mkdir -p "$INSTALL_DIR" || fail "cannot create install directory: $INSTALL_DIR"
TMP=${TMPDIR:-/tmp}/nixflux-install-$$
trap 'rm -f "$TMP"' 0
trap 'exit 1' HUP INT TERM

printf 'Installing NixFlux %s for %s...\n' "$VERSION" "$TARGET"

download() {
	if command -v curl >/dev/null 2>&1; then
		curl -fLsS "$URL" -o "$TMP"
	else
		wget -q "$URL" -O "$TMP"
	fi
}

if [ -t 1 ]; then
	download &
	DOWNLOAD_PID=$!
	INDEX=0
	while kill -0 "$DOWNLOAD_PID" 2>/dev/null; do
		case $((INDEX % 3)) in
			0) FRAME='|' ;;
			1) FRAME='/' ;;
			*) FRAME='-' ;;
		esac
		printf '\r  %s Downloading binary' "$FRAME" >&2
		INDEX=$((INDEX + 1))
		sleep 0.1
	done
	if wait "$DOWNLOAD_PID"; then
		printf '\r\033[2K' >&2
	else
		printf '\r\033[2K' >&2
		fail "download failed: $URL"
	fi
else
	download || fail "download failed: $URL"
fi

[ -s "$TMP" ] || fail "downloaded file is empty"
chmod 755 "$TMP" || fail "cannot make downloaded binary executable"
mv -f "$TMP" "$INSTALL_DIR/nixflux" || fail "cannot install binary to $INSTALL_DIR"
printf 'Installed to %s/nixflux\n' "$INSTALL_DIR"
case ":${PATH:-}:" in
	*":$INSTALL_DIR:"*) ;;
	*) printf 'Add this directory to PATH: %s\n' "$INSTALL_DIR" ;;
esac
