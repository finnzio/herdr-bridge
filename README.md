# herdr-bridge

Event-driven notifications for [Herdr](https://herdr.dev) agents, with no model
calls while waiting.

Arm a wait from a Herdr pane. When a worker finishes, a pane exits, or a log
pattern matches, the bridge notifies the pane that armed it. Codex, Claude,
Grok, Cursor, Devin, Oh My Pi, and Antigravity CLI all use the same mechanism.

## Why this project exists

This project started with a practical need in Codex: **listen for shell output
and worker completion without spending model tokens while waiting**.

A long build, a background command, or another agent might take minutes to
finish. Keeping Codex involved through repeated status checks or shell-output
reads makes waiting part of the model loop. I wanted to end the current turn
and have an event bring the agent back when there was something to act on.

`herdr-bridge` supplies that connection. A local daemon holds the Herdr event
stream, watches pane state and output, and sends a prompt back to the initiating
pane when a rule matches. The model can remain inactive during the wait.

```text
Initiator pane ── arm a rule ──► herdr-bridge daemon
                                      ▲
Shell / worker ── Herdr events ────────┘
                                      │
Initiator pane ◄── prompt / input ─────┘
```

The bridge makes no model API calls. Its event listener and state checks run
locally. A delivered prompt can start a normal, billable agent turn; the
zero-token claim applies to waiting, not the work performed after a wake-up.

## Requirements

| Requirement | Details |
| --- | --- |
| Herdr | 0.9+ with socket protocol 22 and a running session |
| Platform | Linux, WSL2, or macOS with a Unix domain socket |
| Go | 1.24+ to build from source; unnecessary for portable binaries |
| Caller | Run `arm` inside a Herdr pane, or provide an explicit `--target` |

The Go program has no third-party dependencies. Portable packages include
static Linux binaries for `amd64` and `arm64`. Native Windows named pipes are
not supported; Windows users can run the bridge in WSL2.

## Install from source

```bash
git clone https://github.com/finnzio/herdr-bridge.git
cd herdr-bridge
mkdir -p ~/.local/bin
go build -o ~/.local/bin/herdr-bridge .
herdr-bridge version
```

Ensure `~/.local/bin` is in `PATH`. Install the shared agent skill once:

```bash
mkdir -p ~/.agents/skills/herdr-bridge
cp skills/herdr-bridge/SKILL.md ~/.agents/skills/herdr-bridge/SKILL.md
```

Codex, Grok, Cursor, Devin, and OMP discover `~/.agents/skills` directly. Claude
and AGY use different global skill roots. If you use them, create links to the
same shared copy, preserving any existing entries:

```bash
mkdir -p ~/.claude/skills ~/.gemini/config/skills

if [ ! -e ~/.claude/skills/herdr-bridge ] && [ ! -L ~/.claude/skills/herdr-bridge ]; then
  ln -s ../../.agents/skills/herdr-bridge ~/.claude/skills/herdr-bridge
fi

if [ ! -e ~/.gemini/config/skills/herdr-bridge ] && [ ! -L ~/.gemini/config/skills/herdr-bridge ]; then
  ln -s ../../../.agents/skills/herdr-bridge ~/.gemini/config/skills/herdr-bridge
fi
```

The skill teaches agents to register a wait, end their turn, inspect the result
when notified, and remove finished rules. See [the skill](skills/herdr-bridge/SKILL.md).

## Quick start: notify the initiator

Run these commands from the agent pane that should receive the notification.
No configuration file is required.

```bash
# Discover the worker's actual pane ID.
herdr agent list

# Replace w1:p2 with that ID. The default states are done, idle, and blocked.
herdr-bridge arm --name worker-finished --pane w1:p2

# Validate the armed rule against the live Herdr session.
herdr-bridge check
herdr-bridge status || herdr-bridge start
herdr-bridge status
```

Arm the rule and start the daemon **before submitting work**. Initial worker
states are a baseline; the bridge reports later transitions, not work that was
already finished when it first observed the pane. Once armed, the initiating
agent can end its turn. When notified, inspect the worker's output:

```bash
herdr agent read w1:p2 --source recent-unwrapped --lines 120
herdr-bridge disarm --name worker-finished
```

`arm` defaults to `--target self`. It captures the caller's `HERDR_PANE_ID`
immediately and stores the concrete pane ID in the rule. The daemon can be
started from another pane without changing the recipient. Existing rules keep
their stored targets; they are not redirected when a different pane starts the
daemon.

Use `--target w1:p3` or `--target reviewer` to select another pane or a unique
live agent name. Outside Herdr, an explicit target is required. In hand-written
config rules, `"target": "self"` uses the **daemon's** environment, so use a
concrete pane ID or agent name there.

Rule names are shared within a state directory. Use distinct names for
independent waits; `--replace` intentionally overwrites a rule with that name.
If a target pane moves, closes, or hosts a different agent, inspect the live
session and re-arm the rule. Rules bind to panes, not persistent conversations.

Sending a task with `herdr agent prompt` does not automatically register a
bridge wait. The sender must also run `herdr-bridge arm`.

## Agent compatibility and hooks

These agents can both initiate waits and receive prompts. Herdr handles their
terminal input; the bridge does not need a separate input adapter for each
agent. Use Herdr's kind identifier when selecting workers:

| Agent | `--kind` | Global skill discovery |
| --- | --- | --- |
| Codex | `codex` | `~/.agents/skills` directly |
| Claude Code | `claude` | Link from `~/.claude/skills/herdr-bridge` |
| Grok | `grok` | `~/.agents/skills` directly |
| Cursor CLI (`agent`) | `cursor` | `~/.agents/skills` directly |
| Devin | `devin` | `~/.agents/skills` directly |
| Oh My Pi (`omp`) | `omp` | `~/.agents/skills` directly |
| Antigravity CLI (`agy`) | `agy` | Link from `~/.gemini/config/skills/herdr-bridge` |

Shared discovery is documented by [Codex](https://developers.openai.com/codex/skills),
[Cursor](https://cursor.com/docs/skills), and [OMP](https://github.com/can1357/oh-my-pi/blob/main/docs/skills.md).
[Claude's documented global root](https://code.claude.com/docs/en/skills) is
`~/.claude/skills`. Grok's bundled skill guide and `devin skills paths` describe
their shared roots. AGY's bundled `agy-customizations` guide identifies
`~/.gemini/config` as its global configuration root. Discovery may depend on
agent version and local settings.

Inspect installed Herdr integrations with:

```bash
herdr integration status
```

Herdr hooks report agent session identity and, for integrations such as OMP,
lifecycle state. The bridge consumes Herdr's resulting states and events;
hooks do not identify who requested a particular wait. That relationship is
stored by `arm`. No changes to Herdr-managed hook scripts are needed.

Keep `idle` in the watched states for agents that report completion that way.
The default `done,idle,blocked` covers both completion and requests for input.
`blocked` means the worker needs attention, not that it completed successfully.

## Common waits

### Several workers

```bash
herdr-bridge arm --name my-workers \
  --pane w1:p2 --pane w1:p3 \
  --message 'worker {pane_id} ({agent}) is {status}: {title}'
```

For named workers, use `--agent 'worker-*'`. `--agent` selects an agent name
or name glob; `--kind` selects a kind such as `omp`. Both flags are repeatable.
Use `--workspace w1` to restrict a broad selector to one workspace.

### A build or log marker

```bash
herdr-bridge arm --name build-log \
  --match-pane w1:p5 --match-regex 'BUILD (OK|FAILED)' \
  --message 'build pane {pane_id}: {matched_line}'
```

Use `--match-substring 'BUILD OK'` for literal text. The default source is
`recent_unwrapped`; `--match-source` and `--match-lines` control the snapshot.

### A pane exiting

```bash
herdr-bridge arm --name build-exit --pane w1:p5 --pane-exited
```

This reports exits and closures. It does not establish whether a command
succeeded; inspect the pane or use a specific output marker for that result.

### Other delivery modes

| Mode | Behavior |
| --- | --- |
| `prompt` (default) | Submit text through `agent.prompt` to a recognized agent |
| `type` | Send raw text and Enter to any pane, including an ordinary terminal |
| `notify` | Show a Herdr notification; do not submit input to a pane |

For a terminal recipient, explicitly use `--mode type --target w1:p6`. Raw
input is interpreted by whatever is running there. Use `--mode notify` when
you want a Herdr notification without starting an agent turn; this mode is not
a private message to the originating pane.

## Delivery behavior

- **Coalescing:** events for one target are batched over 1,200 ms by default.
  Override with `--coalesce-ms` or a rule's `coalesce_ms`.
- **Rate limiting:** the default is six deliveries per minute per target.
  Override with `--rate-limit` or `rate_limit_per_minute`. Suppressed events
  are counted and reported with a later delivery; their contents are not replayed.
- **Busy recipients:** prompt delivery waits through Herdr's `agent.wait`
  until the target settles, up to `max_wait_target_ms` (ten minutes by default).
- **Blocked recipients:** the bridge skips prompt delivery and normally shows
  a Herdr notification, allowing the user to handle the interactive dialog.
- **Loop protection:** an event from the rule's own target pane is dropped.
- **Failures:** failed deliveries are logged and counted; they are not a durable
  retry queue. Check `status` and `logs` when a notification is missing.

Successful submission means Herdr accepted terminal input; it does not
guarantee the recipient started or completed an agent turn. Coalescing reduces
submissions, not a guaranteed number of billed turns.

## Configuration and rule storage

Runtime rules live in `~/.local/state/herdr-bridge/rules/<name>.json`. `arm`
writes one rule per file. Config-file rules are optional; copy the example
only if you want persistent, hand-written rules:

```bash
mkdir -p ~/.config/herdr-bridge
cp examples/config.example.json ~/.config/herdr-bridge/config.json
# Edit the example pane IDs and targets before running check.
```

A minimal rule is:

```json
{
  "name": "workers",
  "watch": {
    "panes": ["w1:p2", "w1:p3"],
    "statuses": ["done", "idle", "blocked"]
  },
  "target": "w1:p1",
  "mode": "prompt",
  "message": "{summary}"
}
```

Agent-name and kind selectors are alternatives. When `panes` is also present,
it restricts those selectors to the listed panes; `workspaces` adds another
filter. `statuses` filters resulting status transitions. `output_match` and
`pane_exited` add independent watches to a rule. A rule must select at least
one agent, kind, pane, output pattern, or exit watch.

Runtime rules override config rules with the same name. Changes are detected
on the daemon's reload loop without a restart. Active sessions normally reload
within about a second; a session without active rules can take about five
seconds. Restart the daemon after replacing its binary.

| Setting | Default / purpose |
| --- | --- |
| Config file | `~/.config/herdr-bridge/config.json`; override with `--config` or `HERDR_BRIDGE_CONFIG` |
| State directory | `~/.local/state/herdr-bridge`; override with config `state_dir` |
| Socket | `HERDR_SOCKET_PATH`, then `HERDR_SESSION`, then the default Herdr session |
| `socket_path` / `session` | Explicit Herdr socket or named session in the config |
| `notify_target` | Optional recipient for bridge-level notices such as reconnects |
| `defaults.poll_interval_ms` | `10000`; state reconciliation interval |
| `defaults.request_timeout_ms` | `60000`; socket request timeout |
| `defaults.reconnect_notice` | `true` |
| `defaults.notify_on_blocked` | `true` |

`defaults` also accepts `message`, `coalesce_ms`, `rate_limit_per_minute`, and
`max_wait_target_ms`. Rules can override those four settings. XDG config and
state directories are respected. Each daemon connects to one Herdr socket;
use separate configs and state directories for separate sessions. Pane IDs
and live agent names are scoped to that session.

Message placeholders:

```text
{summary} {event} {pane_id} {workspace_id} {tab_id} {status}
{agent} {name} {title} {display_agent} {matched_line} {time}
{count} {events} {suppressed} {rule}
```

`{agent}` is the kind; `{name}` is the optional live agent name. Unknown
placeholders are rejected during rule validation.

## Commands and troubleshooting

| Command | Purpose |
| --- | --- |
| `start`, `stop`, `restart` | Manage the detached daemon |
| `run [--dry-run]` | Run in the foreground |
| `status [--json]` | Inspect liveness, subscriptions, counters, and last delivery |
| `arm`, `disarm`, `rules [--json]` | Create, remove, and inspect runtime rules |
| `check` | Validate rules, resolve live targets, and probe subscriptions |
| `watch` | Foreground dry run: log matches without sending input |
| `logs [--lines N] [-f]` | Inspect or follow the daemon log |
| `version` | Show the binary version |

Run `herdr-bridge arm -h` for all selector flags. To diagnose a missing wake:

```bash
herdr-bridge rules
herdr-bridge check
herdr-bridge status
herdr-bridge logs --lines 50
```

Check the stored target, the worker selector, watched statuses, and whether the
recipient is blocked or still working. `check` probes the currently registered
rules, so arm a rule before using it to validate a new wait. Use `watch` for a
dry run when changing a selector.

Important limits:

- **State reconciliation:** some integration updates do not emit a subscription
  event. The daemon also reads agent states every ten seconds by default and
  reports changes it missed, marked as detected by state comparison. Transient
  states between samples may be missed.
- **Restart behavior:** `known.json` stores observed states. A changed state
  after downtime can be reported once; an entirely new pane starts with a silent
  baseline. The daemon does not replay a complete event history.
- **Output matches:** a standing match can be emitted again on subscription.
  The bridge suppresses preexisting matches by default; config
  `output_match.notify_preexisting: true` opts in. Prefer markers that stop
  matching between runs.
- **Rule lifetime:** rules persist until disarmed. Remove waits that have served
  their purpose to avoid stale notifications.

## Portable packages and updates

Build a Linux package locally:

```bash
scripts/package.sh
# Output: dist/herdr-bridge-<version>-linux-portable.tar.gz
```

The archive contains both Linux binaries, the shared skill, optional Herdr
plugin, example config, Go source, installer, and checksums. On the target
machine:

```bash
tar -xzf herdr-bridge-<version>-linux-portable.tar.gz
cd herdr-bridge-<version>
sha256sum -c SHA256SUMS
./install.sh
```

The installer refreshes one shared skill copy and adds only the Claude and AGY
links when their configuration roots exist. It preserves existing agent skill
entries. Options: `--prefix DIR`, `--no-skill`, and `--with-plugin`.
See [the portable guide](scripts/PORTABLE-README.md).

To update a source installation, pull the repository, rebuild the binary,
refresh the shared skill, then run `herdr-bridge restart`. For a portable
installation, verify the new package, rerun its installer, and restart.
Installed rules and daemon state are retained.

## Optional Herdr plugin

The plugin adds start, stop, status, and rule-list actions. Its startup hook
starts the daemon when a bridge config file exists and the daemon is not
already running.

```bash
herdr plugin link "$PWD/plugin"
herdr plugin action list --plugin herdr-bridge
```

The bridge also works without the plugin. To remove the plugin registration:

```bash
herdr plugin unlink herdr-bridge
```

## Development and verification

```bash
go test ./...
go vet ./...
go test -race -run 'TestArmCapturesCallerAndAllowsTargetOverride|TestArmedEventsReturnToEachInitiator' ./...
```

Tests use a simulated Herdr Unix socket. They cover all seven agent kinds as
both wait initiators and workers, caller capture across daemon environments,
coalescing, rate limits, blocked and busy recipients, output matching,
reconciliation, and rule reloads. They do not launch paid model sessions or
establish end-to-end compatibility with every version of each agent.
