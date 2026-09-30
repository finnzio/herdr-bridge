#!/usr/bin/env bash
# Install herdr-bridge from this drop. Safe to re-run.
#
#   ./install.sh                    # binary to ~/.local/bin, skill for all agents
#   ./install.sh --prefix /usr/local/bin
#   ./install.sh --no-skill         # binary only
#   ./install.sh --with-plugin      # also link the Herdr plugin
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PREFIX="${PREFIX:-$HOME/.local/bin}"
INSTALL_SKILL=1
LINK_PLUGIN=0

while [ $# -gt 0 ]; do
  case "$1" in
    --prefix) PREFIX="${2:?--prefix needs a directory}"; shift 2 ;;
    --no-skill) INSTALL_SKILL=0; shift ;;
    --with-plugin) LINK_PLUGIN=1; shift ;;
    -h|--help) sed -n '2,10p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

# --- binary -----------------------------------------------------------------
case "$(uname -s)/$(uname -m)" in
  Linux/x86_64)  SRC="$HERE/bin/herdr-bridge-linux-amd64" ;;
  Linux/aarch64|Linux/arm64) SRC="$HERE/bin/herdr-bridge-linux-arm64" ;;
  *) echo "unsupported platform: $(uname -s)/$(uname -m); build from src/ with 'go build -o herdr-bridge .'" >&2; exit 1 ;;
esac
mkdir -p "$PREFIX"
install -m 0755 "$SRC" "$PREFIX/herdr-bridge"
echo "installed: $PREFIX/herdr-bridge ($("$PREFIX/herdr-bridge" version))"

case ":$PATH:" in
  *":$PREFIX:"*) ;;
  *) echo "note: $PREFIX is not in PATH; add it to your shell profile" ;;
esac

# --- skill ------------------------------------------------------------------
if [ "$INSTALL_SKILL" = 1 ]; then
  SKILL_SRC="$HERE/skill/SKILL.md"
  SKILL_DEST="$HOME/.agents/skills/herdr-bridge/SKILL.md"
  mkdir -p "$(dirname "$SKILL_DEST")"
  cp "$SKILL_SRC" "$SKILL_DEST"
  echo "installed: ~/.agents/skills/herdr-bridge/SKILL.md"
  # Codex, Grok, Cursor, Devin, and OMP discover ~/.agents/skills directly.
  # Claude and AGY use their own global roots; link to the shared copy only.
  for agent_dir in "$HOME/.claude" "$HOME/.gemini/config"; do
    [ -d "$agent_dir" ] || continue
    mkdir -p "$agent_dir/skills"
    link="$agent_dir/skills/herdr-bridge"
    if [ -e "$link" ] || [ -L "$link" ]; then continue; fi
    case "$agent_dir" in
      "$HOME/.claude") skill_target="../../.agents/skills/herdr-bridge" ;;
      *) skill_target="../../../.agents/skills/herdr-bridge" ;;
    esac
    if ln -s "$skill_target" "$link"; then
      echo "linked:  $link"
    fi
  done
fi

# --- herdr plugin (optional) ------------------------------------------------
if [ "$LINK_PLUGIN" = 1 ]; then
  if command -v herdr >/dev/null 2>&1; then
    herdr plugin link "$HERE/plugin" >/dev/null 2>&1 \
      && echo "linked herdr plugin herdr-bridge" \
      || echo "herdr plugin link failed (is a herdr server running?)"
  else
    echo "herdr not found; skipped plugin link"
  fi
fi

cat <<EOF

Next steps
  1. cp $(echo "$HERE" | sed "s|$HOME|~|")/examples/config.example.json ~/.config/herdr-bridge/config.json
  2. edit ~/.config/herdr-bridge/config.json (socket_path, rules, targets)
  3. herdr-bridge check   # resolves selectors against the live Herdr session
     herdr-bridge start   # detached daemon; logs at ~/.local/state/herdr-bridge/bridge.log
     herdr-bridge status
EOF
