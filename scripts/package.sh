#!/usr/bin/env bash
# Build a portable drop of herdr-bridge: binaries for linux/amd64 and
# linux/arm64, the agent skill, the Herdr plugin, and an install script.
#
# Usage: scripts/package.sh [output-dir]   (default: dist/)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT_DIR="${1:-$REPO_ROOT/dist}"
VERSION="$(sed -n 's/^const version = "\(.*\)"$/\1/p' "$REPO_ROOT/cli.go")"
NAME="herdr-bridge-$VERSION"
STAGE="$OUT_DIR/$NAME"
TARBALL="$OUT_DIR/$NAME-linux-portable.tar.gz"

command -v go >/dev/null || { echo "go is required to build" >&2; exit 1; }

rm -rf "$STAGE"
mkdir -p "$STAGE/bin" "$STAGE/skill" "$STAGE/plugin/bin" "$STAGE/examples" "$STAGE/src"

echo "building linux/amd64"
( cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags "-s -w" -o "$STAGE/bin/herdr-bridge-linux-amd64" . )
echo "building linux/arm64"
( cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
    go build -trimpath -ldflags "-s -w" -o "$STAGE/bin/herdr-bridge-linux-arm64" . )

cp "$REPO_ROOT/skills/herdr-bridge/SKILL.md" "$STAGE/skill/SKILL.md"
cp "$REPO_ROOT/plugin/herdr-plugin.toml" "$STAGE/plugin/herdr-plugin.toml"
cp "$REPO_ROOT/plugin/bin/"*.sh "$STAGE/plugin/bin/"
chmod +x "$STAGE/plugin/bin/"*.sh
cp "$REPO_ROOT/examples/config.example.json" "$STAGE/examples/config.example.json"
cp "$REPO_ROOT"/*.go "$REPO_ROOT/go.mod" "$STAGE/src/"
cp "$REPO_ROOT/scripts/install-portable.sh" "$STAGE/install.sh"
chmod +x "$STAGE/install.sh"
cp "$REPO_ROOT/scripts/PORTABLE-README.md" "$STAGE/README.md"
printf '%s\n' "$VERSION" > "$STAGE/VERSION"

( cd "$STAGE" && find . -type f ! -name SHA256SUMS -print0 \
    | sort -z | xargs -0 sha256sum > SHA256SUMS )

tar -C "$OUT_DIR" -czf "$TARBALL" "$NAME"
( cd "$OUT_DIR" && sha256sum "$(basename "$TARBALL")" > "$TARBALL.sha256" )

echo
echo "package:  $TARBALL"
echo "checksum: $TARBALL.sha256"
echo "contents:"
( cd "$STAGE" && find . -type f | sort | sed 's/^/  /' )
