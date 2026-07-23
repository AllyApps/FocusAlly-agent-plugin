#!/bin/sh
# Launches the per-platform tracker binary in MCP stdio proxy mode.
# Unlike the hook dispatcher, a missing binary is a hard error — an MCP
# server that silently exits would just look broken.

plugin_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd) || exit 1

case "$(uname -s)" in
    Darwin)               goos=darwin ;;
    Linux)                goos=linux ;;
    MINGW*|MSYS*|CYGWIN*) goos=windows ;;
    *)                    echo "focusally: unsupported platform" >&2; exit 1 ;;
esac

case "$(uname -m)" in
    arm64|aarch64) goarch=arm64 ;;
    x86_64|amd64)  goarch=amd64 ;;
    *)             echo "focusally: unsupported architecture" >&2; exit 1 ;;
esac

bin="$plugin_root/bin/tracker-$goos-$goarch"
[ "$goos" = "windows" ] && bin="$bin.exe"
if [ ! -x "$bin" ]; then
    echo "focusally: tracker binary not found: $bin" >&2
    exit 1
fi

exec "$bin" mcp "$@"
