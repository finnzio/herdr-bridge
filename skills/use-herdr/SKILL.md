---
name: use-herdr
description: >
  Coordinate authorized agents and event waits in herdr-core (hc) or
  herdr-tui without model polling. Use when asked to use Herdr, communicate
  with workers, or wait for managed tasks. Select the control plane from
  runtime context: native hc messaging when HC_TERMINAL_ID is set, otherwise
  herdr-bridge for validated herdr-tui panes.
metadata:
  short-description: "Agent coordination in hc and herdr-tui"
---

# use-herdr

Install this skill once into `~/.agents/skills/use-herdr`. Codex, Grok,
Cursor, Devin, and OMP discover that directory directly. Claude and AGY need
a link from `~/.claude/skills/use-herdr` and
`~/.gemini/config/skills/use-herdr`, respectively, to the shared copy.

## Select the control plane first

Inspect `HC_TERMINAL_ID`, `HERDR_ENV`, and `HERDR_PANE_ID` in the current
shell. Recheck after changing terminals; installed binaries do not identify
the active control plane.

- **herdr-core (hc):** if `HC_TERMINAL_ID` is nonempty, read `hc --skill`
  and validate the caller with `hc get "$HC_TERMINAL_ID"`. This takes
  precedence over herdr-tui variables. Use the native workflow below and
  do not run bridge `start`, `arm`, `check`, or `disarm` for hc coordination.
  Do not stop an existing bridge daemon; it may serve herdr-tui sessions.
- **herdr-tui:** without hc context, `HERDR_ENV=1` and a nonempty
  `HERDR_PANE_ID` select the bridge workflow below.
- **Neither:** do not select another client's session or focused pane.
  Report that managed coordination is unavailable. Explicit bridge
  administration outside herdr-tui requires a user-selected session/target.

If hc caller validation fails, report the invalid context; do not fall back
to a herdr-tui session.

### Native hc coordination

Use `hc run --bg` / `hc send` for authorized tasks and `hc wait` for event
waits. Keep each worker's returned terminal address and its initiating
parent address; Codex workers must start with `--no-daemon`.

- Require workers to report results with `hc send <initiator-address>`.
  Pass that address when assigning the task. A worker's own
  `HC_TERMINAL_ID` is not its parent's address.
- Normal `done`/`idle` does not automatically send a completion note.
  Native parent notes cover blocked/error/exited children. Do not disable
  these notes for workers; `notes=false` and `--no-notify` affect delivery.
- When ending the turn to await work, rely on explicit result messages and
  native exception notes. Do not keep a model turn alive by polling.
  A `hc wait` return with `timeout`, `message`, or `not_started` is not
  proof of completion. Read the actual delivery before accepting it.
- Match returned addresses to assigned workers. `--children` can include
  log viewers; children normally end with their parent unless explicitly
  started with `--outlive-parent`.
- `hc wait --output` matches new literal output after the wait starts.
  It does not replace bridge regex rules, custom batching, or rate limits.
  Handle those requirements explicitly rather than invoking herdr-tui.
- Keep `codex-bg` for Codex long tasks; its `codex queue` wake remains
  separate from hc worker communication. Do not add duplicate completion
  watchers for its read-only progress terminal.

#### Interactive slash commands such as /clear

An Agent generally cannot execute an interactive slash command on itself
from inside its current turn. For an authorized command on a specific
target, use a separate ordinary shell or another Agent to operate that
target. First inspect it with `hc get` / `hc read`: it must be idle at its
input box, with no draft or confirmation dialog. Do not interrupt work,
erase a draft, or answer a dialog just to make the command fit.

**Ordinary shell:** a caller that is not an Agent terminal can send the
literal text and Enter directly:

```sh
hc send t7 "/clear"
```

**Agent sending to another Agent:** `hc send` normally adds a source tag,
such as `<hc from="t15">/clear</hc>`. The target receives a message rather
than a bare slash command. Starting `bash` inside an Agent's shell tool
does not turn that caller into an ordinary shell. For this command, send
individual keys instead:

```sh
hc keys t7 / c l e a r enter
```

Remote terminals use the same key sequence with their full address:

```sh
hc keys vps/t7 / c l e a r enter
```

Replace the example address with the verified target and inspect the
result afterward. `hc keys` sends keys without message wrapping; it is not
a general exact-text interface for arbitrary punctuation or Unicode.

In hc 0.1.20, `hc send --raw` is unavailable. The proposed fix in
[PR #141](https://github.com/herdrdev/herdr-core/pull/141) was still open
when checked; only use such a flag after the installed `hc send --help`
documents support. See the [v0.1.20 commands](https://github.com/herdrdev/herdr-core/blob/v0.1.20/docs/commands.md)
and [message-wrapping issue #97](https://github.com/herdrdev/herdr-core/issues/97).

Continue with bridge commands only in the herdr-tui workflow below.

## herdr-tui bridge workflow

**Core principle:** a model turn must never be spent on waiting. Waiting happens
in the `herdr-bridge` daemon, which holds a `events.subscribe` stream on the
herdr-tui socket and prompts your pane only when something actually happened.

### Hard rules

1. **Never poll.** Do not run `herdr agent wait` in the foreground, loop
   `write_stdin`, or sleep-and-check from inside a turn to wait for other
   panes. Arm the bridge, print the armed rule, and end your turn.
2. **One daemon.** `herdr-bridge start` once per machine/session; rules are
   added at runtime with `herdr-bridge arm` and take effect within ~1s.
3. **Notify the initiator.** Omit `--target`: `arm` defaults to `self` and
   captures `$HERDR_PANE_ID` when the rule is created, regardless of agent
   kind or where the daemon runs. Explicit `--target self` also works; use
   `--target <pane-id|agent-name>` when another recipient is intended. Explicit
   administration outside herdr-tui requires a target. If your pane is
   replaced, re-arm. Rule names are shared: choose distinct names for
   independent waits from different panes.
4. **One owner per worker pane.** herdr-tui does not record who asked a worker to
   run something, so a worker pane can only have one waiter. Watch exactly the
   panes *you* started (`--pane w1B:p4`) or agent names only you use
   (`herdr agent rename`), never a kind-wide selector that also covers another
   orchestrator's workers. `arm --replace` refuses to overwrite a rule armed by
   another pane unless you pass `--force`; the rule file stores
   `owner=<pane>`. `herdr-bridge check` and the daemon log warn when one pane is
   watched by rules with different targets — that warning is the cross-wake bug,
   not a style nit.
5. **Do not watch your own pane when targeting it** —
   the bridge drops self-referential events to prevent wake loops.
6. **Validate the armed wait:** after `arm`, run `herdr-bridge check` before
   submitting work or ending the turn. It must exit 0: it resolves the stored
   rules against the live session and confirms herdr-tui accepts the subscriptions.

### Arm a wait

```bash
herdr-bridge status || herdr-bridge start          # daemon must run
herdr-bridge arm --name <rule> \
  --agent 'worker-*' --status done --status blocked \
  --message 'worker {name} ({pane_id}) is {status}; {title}'
herdr-bridge check
```

Selector flags: `--agent <name|glob>` (repeatable),
`--kind <codex|claude|grok|cursor|devin|omp|agy>`
(repeatable), `--pane <w1:p2>` (repeatable), `--workspace <w1>`, `--status`
(repeatable, default `done,blocked`), plus output watches with
`--match-pane <id> --match-regex '<re>'` or `--match-substring <text>`, and
`--pane-exited`.

Use herdr-tui's `cursor` kind for Cursor's `agent` CLI and `omp` for Oh My Pi.
The default statuses are `done` and `blocked`. `done` and `idle` are the same
status: herdr-tui reports a worker that finished in a background tab as `done` and
rewrites it to `idle` after you visit that tab, and the bridge matches both.
A wake-up never depends on the worker tab being active.

Delivery: `--mode prompt` (default, wakes an agent pane), `--mode type` (raw
input into any pane), `--mode notify` (herdr-tui notification only). Tuning:
`--coalesce-ms`, `--rate-limit`, `--message`, `--replace`.

Typical orchestrator rule set:

```bash
# any of my workers settling
herdr-bridge arm --name fleet \
  --pane w1:p2 --pane w1:p3 --status done --status blocked --replace

# a log marker in a build pane
herdr-bridge arm --name build-log \
  --match-pane w1:p5 --match-regex 'BUILD (OK|FAILED)' \
  --message 'build pane: {matched_line}' --replace
```

Then end the turn with a one-line note: which rule is armed and what will wake
you. Do not keep the turn alive.

### When woken

The wake message is one line per event (or a `herdr-bridge: N events` batch when
several landed together). On receiving it:

1. Read the affected panes (`herdr agent read <target> --source recent-unwrapped
   --lines 120`) — the wake tells you *that* something changed, not the result.
2. Do the work the event unblocks.
3. Disarm finished rules (`herdr-bridge disarm --name <rule>`) so a later event
   cannot wake you for stale work, and re-arm for the next phase.

The status in a wake message can read `done` or `idle`. Both mean the worker
stopped working; `done` additionally means that pane has not been looked at
yet.

### Inspecting the bridge

```bash
herdr-bridge status                 # subscriptions, counters, last delivery
herdr-bridge rules                  # effective rules and origins
herdr-bridge logs --lines 50        # JSON lines: matched events, deliveries
herdr-bridge watch                  # foreground dry run: what would fire now
```

`watch` (or `run --dry-run`) is the way to debug a selector: it logs every match
and the message it would deliver without touching any pane.

### Cost model

- Waiting: one idle process, no model requests.
- A delivered prompt can start a normal agent turn; successful submission
  does not prove the recipient started or completed it.
- Coalescing reduces submissions, not a guaranteed number of billed turns.
