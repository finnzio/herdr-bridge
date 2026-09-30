---
name: herdr-bridge
description: >
  Wait for Herdr events (worker agents finishing, panes exiting, log output)
  without polling a model. Use from any Herdr agent pane when orchestrating
  other panes/agents, when asked to "wait for worker X", "notify me when the build finishes",
  "watch the log", "monitor other agents", or before writing any loop that calls
  `herdr agent wait`, `write_stdin`, or sleeps between status checks.
metadata:
  short-description: "Zero-token Herdr event waiting"
---

# herdr-bridge

Install this skill once into `~/.agents/skills/herdr-bridge`. Codex, Grok,
Cursor, Devin, and OMP discover that directory directly. Claude and AGY need
a link from `~/.claude/skills/herdr-bridge` and
`~/.gemini/config/skills/herdr-bridge`, respectively, to the shared copy.

**Core principle:** a model turn must never be spent on waiting. Waiting happens
in the `herdr-bridge` daemon, which holds a `events.subscribe` stream on the
Herdr socket and prompts your pane only when something actually happened.

## Hard rules

1. **Never poll.** Do not run `herdr agent wait` in the foreground, loop
   `write_stdin`, or sleep-and-check from inside a turn to wait for other
   panes. Arm the bridge, print the armed rule, and end your turn.
2. **One daemon.** `herdr-bridge start` once per machine/session; rules are
   added at runtime with `herdr-bridge arm` and take effect within ~1s.
3. **Notify the initiator.** Omit `--target`: `arm` defaults to `self` and
   captures `$HERDR_PANE_ID` when the rule is created, regardless of agent
   kind or where the daemon runs. Explicit `--target self` also works; use
   `--target <pane-id|agent-name>` when another recipient is intended. Outside
   Herdr a target is required. If your pane is replaced, re-arm. Rule names
   are shared: choose distinct names for independent waits from different panes.
4. **One owner per worker pane.** Herdr does not record who asked a worker to
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
   rules against the live session and confirms Herdr accepts the subscriptions.

## Arm a wait

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

Use Herdr's `cursor` kind for Cursor's `agent` CLI and `omp` for Oh My Pi.
The default statuses are `done` and `blocked`. `idle` also fires every time a
worker settles between turns, so add `--status idle` explicitly only for
agents such as OMP that report completion that way.

Delivery: `--mode prompt` (default, wakes an agent pane), `--mode type` (raw
input into any pane), `--mode notify` (Herdr notification only). Tuning:
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

## When woken

The wake message is one line per event (or a `herdr-bridge: N events` batch when
several landed together). On receiving it:

1. Read the affected panes (`herdr agent read <target> --source recent-unwrapped
   --lines 120`) — the wake tells you *that* something changed, not the result.
2. Do the work the event unblocks.
3. Disarm finished rules (`herdr-bridge disarm --name <rule>`) so a later event
   cannot wake you for stale work, and re-arm for the next phase.

## Inspecting the bridge

```bash
herdr-bridge status                 # subscriptions, counters, last delivery
herdr-bridge rules                  # effective rules and origins
herdr-bridge logs --lines 50        # JSON lines: matched events, deliveries
herdr-bridge watch                  # foreground dry run: what would fire now
```

`watch` (or `run --dry-run`) is the way to debug a selector: it logs every match
and the message it would deliver without touching any pane.

## Cost model

- Waiting: one idle process, no model requests.
- A wake: exactly one turn in the target pane; nothing else is charged.
- A batch of N events inside the coalescing window: still one turn.
