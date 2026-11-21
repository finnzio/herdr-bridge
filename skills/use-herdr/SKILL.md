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

#### Reuse workspace agents first; spawn only as fallback

1. **Always reuse existing workspace agents first:** before dispatching,
   inspect your own context with `hc get "$HC_TERMINAL_ID" --json` (recording
   your `cwd` and `labels["herdr.space"]`), then run `hc ls --json` and
   `hc get <id> --json` to inventory live agents in the same workspace
   (matching `herdr.space` or `cwd`).
   - **Do not filter only for `status == "idle"`:** reusable workspace agents
     may report `status: "idle"`, `status: "done"` (finished a previous turn
     and awaiting assignment), or `status: null` with an interactive agent CLI
     as `foreground.name` (e.g. `grok` inside a `zsh` tab). Before reusing a
     `done` or `null` candidate, run `hc read --lines 30 <id>` to confirm it is
     sitting at its prompt with no active work, unsent draft, or confirmation
     dialog.
   - Assign work to an available workspace agent via
     `hc send <existing-id> "<task contract; report with hc send $HC_TERMINAL_ID>"`
     (for an unrelated fresh task, confirm the prompt is clear and reset
     context first with `hc keys <id> / c l e a r enter`).
   - Never hijack terminals in other `herdr.space` tabs, sessions, or
     worktrees, and never interrupt `working` or `blocked` terminals.
2. **Spawn new workers only as fallback:** use `hc run --bg` only when the
   current workspace has no existing agent suitable for the role, when all
   matching workspace agents are `working` and additional parallelism is
   justified, or when the user explicitly asks to launch new ones.
   - Roles are decoupled from specific agent brands (templates provide
     defaults and can be overridden with another CLI or model as needed).
   - Once interfaces and non-overlapping file ownership are locked, parallelize
     discovery, Frontend, Backend, incremental read-only cross-model
     Reviewer, and Executor tasks (or multiple non-overlapping
     instances/worktrees of the same role) to shorten the critical path. Keep
     one writer per file per phase in a shared workspace; run local checks in
     parallel and run full repo gates through a single coordinator or
     Executor.
3. **Avoid background `PATH` / `exit 127` launch failures:**
   - **Why `exit 127` happens:** on Linux, `herdr-core serve` (when started
     over SSH) snapshots a **non-interactive login shell** environment once at
     startup and never reads `~/.zshrc` or `~/.bashrc`. `hc run <template>`
     runs `command:` via `sh -c` under the core's `PATH` (not the caller's
     interactive shell `PATH`), and `zsh -lc` also skips `~/.zshrc` (reading
     only `~/.zshenv` and `~/.zprofile`).
   - **Preferred (templates):** ensure `~/.config/herdr-core/templates/*.md`
     source the user's POSIX CLI `PATH` setup before `exec <cli> <flags>`
     (e.g., `. "$HOME/.config/herdr-core/env.sh" && exec codex --yolo --no-daemon`),
     then launch with `hc templates` verified first:

```sh
hc run --bg --name <role>-<slug> --parent "$HC_TERMINAL_ID" --cwd "$PWD" \
  <template> "<task contract; report with hc send $HC_TERMINAL_ID>"
```

   - **Direct launch (without a template):** pass **both** your explicit
     `PATH` via `env` **and** the resolved binary path. Never launch by bare
     command name, never rely on `zsh -lc` alone, and never pass an absolute
     CLI path without `PATH="$PATH"` (otherwise tools the worker runs — `hc`,
     `node`, `pnpm` — still fail with `exit 127`):

```sh
hc run --bg --name <role>-<slug> --parent "$HC_TERMINAL_ID" --cwd "$PWD" -- \
  env "PATH=$PATH" "$(command -v codex)" --yolo --no-daemon \
  "<task contract; report with hc send $HC_TERMINAL_ID>"
```

   - **Persistent machine-level fix:** place user CLI `PATH` exports in
     `~/.zprofile` (or a POSIX login snippet sourced there); the user can then
     refresh an older running core from a shell **outside Herdr** via
     `hc server restart --fresh` (never run `hc server stop/restart` from an
     agent unless explicitly asked).
   - After launching, check `first_message.sent` and `hc get <id>`. If a
     worker exits with `127`, run `hc read <id>` to inspect the shell error and
     fix `PATH` / template before relaunching — never retry blindly with
     `zsh -lc`.
4. **Unattended background launch flags:** to prevent background `hc run --bg`
   workers from stalling on interactive permission prompts, use:
   - Claude Code: `claude --dangerously-skip-permissions`
   - Codex: `codex --yolo --no-daemon` (always keep `--no-daemon` in `hc`)
   - Antigravity: `agy --dangerously-skip-permissions`
   - Grok: `grok --always-approve`
5. **Window visibility & single-target cleanup:** unlabeled `hc run --bg`
   workers stay hidden in `herdr-app` to avoid tab clutter. To inspect or take
   over a running worker in the window, surface it on demand
   (`herdr-app do terminal.keep` or label it with `herdr.space=<id>` and
   optional `herdr.tab=<id>`).
   - Only clean up workers you spawned — **never** `hc kill` or `hc rm`
     pre-existing workspace agents you reused.
   - **`hc kill <t>` and `hc rm <t>` accept only ONE terminal address per
     invocation** (passing multiple IDs such as `hc rm t12c t12d` fails with
     exit code 2 `unexpected argument`). Remove multiple workers one by one:

```sh
for t in <id1> <id2>; do
  hc kill "$t" 2>/dev/null || true
  hc rm "$t"
done
```

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
