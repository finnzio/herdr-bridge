#!/bin/sh
# Thin wrapper so Herdr plugin actions use the same binary as the CLI.
set -u

BRIDGE="${HERDR_BRIDGE_BIN:-herdr-bridge}"
if ! command -v "$BRIDGE" >/dev/null 2>&1; then
  echo "herdr-bridge not found in PATH; build it with: go build -o ~/.local/bin/herdr-bridge ." >&2
  exit 1
fi

CONFIG="${HERDR_BRIDGE_CONFIG:-$HOME/.config/herdr-bridge/config.json}"
exec "$BRIDGE" "$@" --config "$CONFIG"
