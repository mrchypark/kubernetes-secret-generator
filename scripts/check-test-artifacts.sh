#!/bin/sh
set -eu

TOOL_NAME=$(basename "$0" .sh)
SCRIPT_PATH=$(
	if readlink -f "$0" >/dev/null 2>&1; then
		readlink -f "$0"
	elif realpath "$0" >/dev/null 2>&1; then
		realpath "$0"
	elif [ "${0#*/*}" != "$0" ]; then
		printf '%s\n' "$0"
	else
		command -v "$0"
	fi
)
SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname "$SCRIPT_PATH")" && pwd)
SRC_DIR="$SCRIPT_DIR/$TOOL_NAME-src"
CACHE_ROOT="${CODEX_GO_SCRIPT_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/codex-go-scripts}"
PLATFORM=$(go env GOOS GOARCH | tr '\n' '-' | sed 's/-$//')
[ -f "$SRC_DIR/go.mod" ] || { printf 'missing Go module: %s/go.mod\n' "$SRC_DIR" >&2; exit 1; }
KEY=$(
	cd "$SRC_DIR" && {
		go env GOVERSION GOOS GOARCH GOWORK GOFLAGS CGO_ENABLED GOEXPERIMENT CC CXX
		if command -v shasum >/dev/null 2>&1; then
			find . -type d \( \( -name ".*" ! -name "." \) -o -name vendor \) -prune -o -type f -exec shasum -a 256 {} + | sort
		else
			find . -type d \( \( -name ".*" ! -name "." \) -o -name vendor \) -prune -o -type f -exec sha256sum {} + | sort
		fi
	} | if command -v shasum >/dev/null 2>&1; then shasum -a 256; else sha256sum; fi | awk '{print $1}'
)
[ -n "$KEY" ] || { printf '%s\n' 'failed to compute cache key' >&2; exit 1; }
BIN_DIR="$CACHE_ROOT/$TOOL_NAME/$PLATFORM/$KEY"
BIN="$BIN_DIR/$TOOL_NAME"

if [ "${CACHED_GO_REBUILD:-0}" = 1 ] || [ ! -x "$BIN" ]; then
	mkdir -p "$BIN_DIR"
	[ "${CACHED_GO_DEBUG:-0}" = 1 ] && printf 'building %s\n' "$BIN" >&2
	TMP="$BIN.tmp.$$"
	rm -f "$TMP"
	trap 'rm -f "$TMP"' 0 1 2 3 15
	(cd "$SRC_DIR" && go build -trimpath -o "$TMP" .)
	mv "$TMP" "$BIN"
	trap - 0 1 2 3 15
fi

exec "$BIN" "$@"
