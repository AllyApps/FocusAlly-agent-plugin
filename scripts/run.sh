#!/bin/sh
# Dispatches a Claude Code hook event to the per-platform tracker
# binary. Missing/unknown platform or binary => exit 0 silently: the
# tracker must never disturb the Claude session.

plugin_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd) || exit 0

case "$(uname -s)" in
    Darwin)               goos=darwin ;;
    Linux)                goos=linux ;;
    MINGW*|MSYS*|CYGWIN*) goos=windows ;;
    *)                    exit 0 ;;
esac

case "$(uname -m)" in
    arm64|aarch64) goarch=arm64 ;;
    x86_64|amd64)  goarch=amd64 ;;
    *)             exit 0 ;;
esac

bin="$plugin_root/bin/tracker-$goos-$goarch"
[ "$goos" = "windows" ] && bin="$bin.exe"
[ -x "$bin" ] || exit 0

exec "$bin" hook "$1"
