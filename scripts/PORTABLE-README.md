# herdr-bridge (portable drop)

Wake a Herdr pane when events happen — at zero token cost while waiting.
This archive carries both Linux binaries, the agent skill, the optional Herdr
plugin, a config example, and the full Go source.

The project started with a need to let Codex wait for shell output or worker
completion without spending model tokens on repeated checks. The daemon waits
locally and returns a prompt to the initiating pane when a matching event
arrives. A wake-up can start a normal, billable agent turn.

## Install on the target machine

```bash
tar -xzf herdr-bridge-<version>-linux-portable.tar.gz
cd herdr-bridge-<version>
./install.sh                 # binary -> ~/.local/bin, skill for every agent
```

Options:

| Flag | Effect |
| --- | --- |
| `--prefix DIR` | Install the binary somewhere else (default `~/.local/bin`) |
| `--no-skill` | Binary only |
| `--with-plugin` | Also run `herdr plugin link ./plugin` (auto-start + keybindable actions) |

The installer picks `bin/herdr-bridge-linux-amd64` or `linux-arm64` from
`uname -m`. Both are static (`CGO_ENABLED=0`), so nothing else is needed on the
target besides a Unix socket-capable Linux (WSL2 is fine).

Supported Herdr agent kinds: `codex`, `claude`, `grok`, `cursor` (Cursor's
`agent` CLI), `devin`, `omp` (Oh My Pi), and `agy` (Antigravity CLI).
The installer saves one skill copy under `~/.agents/skills/herdr-bridge`.
Codex, Grok, Cursor, Devin, and OMP read it directly. For Claude and AGY, it
adds links from `~/.claude/skills` and `~/.gemini/config/skills` when those
configuration roots exist. Rerunning the installer refreshes the shared copy.

After install:

```bash
herdr-bridge version
mkdir -p ~/.config/herdr-bridge
cp examples/config.example.json ~/.config/herdr-bridge/config.json
$EDITOR ~/.config/herdr-bridge/config.json     # socket_path, rules, targets
herdr-bridge check                             # resolves selectors live
herdr-bridge start                             # detached daemon
herdr-bridge status
```

Or arm a single rule from inside an agent pane without any config file:

```bash
herdr-bridge start
herdr-bridge arm --name workers \
  --agent 'worker-*' --status done --status blocked \
  --message 'worker {name} ({pane_id}) is {status}'
```

The pane that arms the rule is its default notification target, independent
of agent kind and where the daemon was started. Use `--target` to override it.

## What is inside

```text
install.sh                  arch detection + install (idempotent)
VERSION                     herdr-bridge version
SHA256SUMS                  checksums for every file below
bin/herdr-bridge-linux-*    static binaries (amd64, arm64)
skill/SKILL.md              agent skill (armed rules, no polling loops)
plugin/                     optional Herdr plugin: actions + autostart hook
examples/config.example.json
src/                        full Go source (go build -o herdr-bridge .)
```

## Requirements

| Need | Version |
| --- | --- |
| Herdr | 0.9+ (socket protocol 22), server running, `HERDR_ENV=1` inside a pane |
| OS | Linux (WSL2 included); the transport is a Unix domain socket |
| Go | only to rebuild from `src/` (1.24+) |

## Updating

Re-copy the archive, run `./install.sh` again, and `herdr-bridge restart`.
Rules and state live in `~/.config/herdr-bridge/` and
`~/.local/state/herdr-bridge/` and are never touched by the installer.

Full documentation and rule reference:
[herdr-bridge README](https://github.com/finnzio/herdr-bridge#readme).
The `src/` directory contains the Go source for rebuilding the binary.
