#!/bin/sh
# Start the herdr-bridge daemon if a config exists and it is not already up.
# Runs as a Herdr plugin startup hook: it must never block or fail loudly.
set -u

BRIDGE="${HERDR_BRIDGE_BIN:-herdr-bridge}"
command -v "$BRIDGE" >/dev/null 2>&1 || exit 0

CONFIG="${HERDR_BRIDGE_CONFIG:-$HOME/.config/herdr-bridge/config.json}"
[ -f "$CONFIG" ] || exit 0

"$BRIDGE" status --config "$CONFIG" >/dev/null 2>&1 && exit 0
"$BRIDGE" start --config "$CONFIG" >/dev/null 2>&1 || true
exit 0
