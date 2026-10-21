package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testLogger captures bridge logs so a failing test can show them.
type testLogger struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *testLogger) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *testLogger) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// harness runs a bridge engine against the fake herdr server.
type harness struct {
	engine  *Engine
	state   *State
	log     *testLogger
	dir     string
	cfgPath string
	fake    *fakeHerdr
	t       *testing.T
	cancel  context.CancelFunc
	done    chan struct{}
}

func startEngine(t *testing.T, fake *fakeHerdr, rules []map[string]any, defaults map[string]any) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{t: t, fake: fake, dir: dir, cfgPath: filepath.Join(dir, "config.json")}
	h.writeConfig(rules, defaults)

	cfg, err := LoadConfig(h.cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	h.state = NewState(cfg.StateDir)
	if err := h.state.EnsureDir(); err != nil {
		t.Fatalf("state dir: %v", err)
	}
	h.log = &testLogger{}
	h.engine = NewEngine(cfg, NewClient(cfg.SocketPathOr()), h.state, NewLogger(h.log, LevelDebug), false)

	h.begin()
	t.Cleanup(h.stop)
	return h
}

func (h *harness) begin() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan struct{})
	go func() {
		defer close(h.done)
		_ = h.engine.Run(ctx)
	}()
}

// stop shuts the engine down and flushes its state to disk.
func (h *harness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	h.cancel = nil
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		h.t.Errorf("engine did not stop")
	}
	if err := h.state.Flush(true); err != nil {
		h.t.Logf("state flush: %v", err)
	}
	if h.t.Failed() {
		h.t.Logf("bridge log:\n%s", h.log.String())
	}
}

func (h *harness) writeConfig(rules []map[string]any, defaults map[string]any) {
	h.t.Helper()
	if defaults == nil {
		defaults = map[string]any{}
	}
	if _, ok := defaults["coalesce_ms"]; !ok {
		defaults["coalesce_ms"] = 40
	}
	if _, ok := defaults["rate_limit_per_minute"]; !ok {
		defaults["rate_limit_per_minute"] = 120
	}
	if _, ok := defaults["max_wait_target_ms"]; !ok {
		defaults["max_wait_target_ms"] = 2000
	}
	raw := map[string]any{
		"socket_path": h.fake.path,
		"state_dir":   filepath.Join(h.dir, "state"),
		"defaults":    defaults,
		"rules":       rules,
	}
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		h.t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(h.cfgPath, data, 0o644); err != nil {
		h.t.Fatalf("write config: %v", err)
	}
}

// rewriteConfig replaces the config file with a clearly newer mtime so the
// running engine notices the change on its next poll.
func (h *harness) rewriteConfig(rules []map[string]any, defaults map[string]any) {
	h.t.Helper()
	h.writeConfig(rules, defaults)
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(h.cfgPath, future, future); err != nil {
		h.t.Fatalf("chtimes: %v", err)
	}
}

func waitForSubscribe(t *testing.T, fake *fakeHerdr, n int) {
	t.Helper()
	waitFor(t, 4*time.Second, "subscription", func() bool { return fake.subscribeCount() >= n })
}

// startEngineInDir restarts a bridge over an existing state and config
// directory, which is how a restart is simulated.
func startEngineInDir(t *testing.T, fake *fakeHerdr, dir string, rules []map[string]any, defaults map[string]any) *harness {
	t.Helper()
	h := &harness{t: t, fake: fake, dir: dir, cfgPath: filepath.Join(dir, "config.json")}
	if defaults == nil {
		defaults = map[string]any{"coalesce_ms": 40, "rate_limit_per_minute": 120, "max_wait_target_ms": 2000}
	}
	h.writeConfig(rules, defaults)
	cfg, err := LoadConfig(h.cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	h.state = NewState(cfg.StateDir)
	h.log = &testLogger{}
	h.engine = NewEngine(cfg, NewClient(cfg.SocketPathOr()), h.state, NewLogger(h.log, LevelDebug), false)
	h.begin()
	t.Cleanup(h.stop)
	return h
}

func TestPromptOnAgentStatusChange(t *testing.T) {
	fake := newFakeHerdr(t)
	fake.setAgents(
		testAgent("w1:p2", "codex", "working", "worker"),
		testAgent("w1:p1", "claude", "idle", "supervisor"),
	)
	fake.setPanes(testPane("w1:p1"), testPane("w1:p2"))
	startEngine(t, fake, []map[string]any{{
		"name":    "workers",
		"watch":   map[string]any{"kinds": []string{"codex"}, "statuses": []string{"done", "blocked"}},
		"target":  "w1:p1",
		"message": "{summary}",
	}}, nil)
	waitForSubscribe(t, fake, 1)

	fake.setAgentStatus("w1:p2", "done")
	fake.Push("pane.agent_status_changed", map[string]any{
		"pane_id": "w1:p2", "workspace_id": "w1", "agent_status": "done", "agent": "codex",
	})
	waitFor(t, 3*time.Second, "prompt", func() bool { return fake.promptCount() == 1 })

	got := fake.promptsSnapshot()[0]
	if got.Target != "w1:p1" {
		t.Errorf("prompt target = %q, want w1:p1", got.Target)
	}
	if want := "pane w1:p2 (worker) status: done"; got.Text != want {
		t.Errorf("prompt text = %q, want %q", got.Text, want)
	}
}

func TestBaselineIsSilentAndRepeatedStatusIgnored(t *testing.T) {
	fake := newFakeHerdr(t)
	fake.setAgents(testAgent("w1:p2", "codex", "done", "worker"))
	fake.setPanes(testPane("w1:p2"))
	startEngine(t, fake, []map[string]any{{
		"name":   "workers",
		"watch":  map[string]any{"kinds": []string{"codex"}},
		"target": "w1:p2",
	}}, nil)
	waitForSubscribe(t, fake, 1)
	time.Sleep(300 * time.Millisecond)
	if n := fake.promptCount(); n != 0 {
		t.Fatalf("baseline status produced %d prompt(s); want 0", n)
	}

	fake.Push("pane.agent_status_changed", map[string]any{
		"pane_id": "w1:p2", "workspace_id": "w1", "agent_status": "done",
	})
	time.Sleep(300 * time.Millisecond)
	if n := fake.promptCount(); n != 0 {
		t.Fatalf("unchanged status produced %d prompt(s); want 0", n)
	}
}

// Herdr spells a finished worker "done" while the completion is unseen and
// "idle" once it has been viewed. Both mean the worker stopped, so the default
// statuses must catch a completion no matter which tab the worker sits in.
func TestDefaultWatchesCompletionInBothSpellings(t *testing.T) {
	fake := newFakeHerdr(t)
	fake.setAgents(
		testAgent("w1:p2", "codex", "working", "worker"),
		testAgent("w1:p3", "codex", "working", "worker-2"),
		testAgent("w1:p1", "claude", "idle", "supervisor"),
	)
	fake.setPanes(testPane("w1:p1"), testPane("w1:p2"), testPane("w1:p3"))
	startEngine(t, fake, []map[string]any{{
		"name":   "workers",
		"watch":  map[string]any{"kinds": []string{"codex"}},
		"target": "w1:p1",
	}}, nil)
	waitForSubscribe(t, fake, 1)

	// A worker in the active tab: herdr reports "idle".
	fake.setAgentStatus("w1:p2", "idle")
	fake.Push("pane.agent_status_changed", map[string]any{"pane_id": "w1:p2", "agent_status": "idle"})
	waitFor(t, 3*time.Second, "prompt on idle completion", func() bool { return fake.promptCount() == 1 })

	// A worker in a background tab: herdr reports "done".
	fake.setAgentStatus("w1:p3", "done")
	fake.Push("pane.agent_status_changed", map[string]any{"pane_id": "w1:p3", "agent_status": "done"})
	waitFor(t, 3*time.Second, "prompt on done completion", func() bool { return fake.promptCount() == 2 })
}

func TestCaughtUpStateChangeAfterRestart(t *testing.T) {
	fake := newFakeHerdr(t)
	fake.setAgents(testAgent("w1:p2", "codex", "done", "worker"))
	fake.setPanes(testPane("w1:p1"), testPane("w1:p2"))
	rules := []map[string]any{{
		"name":    "workers",
		"watch":   map[string]any{"kinds": []string{"codex"}, "statuses": []string{"done", "blocked"}},
		"target":  "w1:p1",
		"mode":    "type",
		"message": "{summary}",
	}}

	first := startEngine(t, fake, rules, nil)
	waitForSubscribe(t, fake, 1)
	time.Sleep(200 * time.Millisecond)
	first.stop()

	// The worker changes state while no bridge is listening.
	fake.setAgentStatus("w1:p2", "blocked")

	second := startEngineInDir(t, fake, first.dir, rules, nil)
	waitForSubscribe(t, fake, 2)
	waitFor(t, 4*time.Second, "catch-up delivery", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.sentText) >= 1
	})
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !strings.Contains(fake.sentText[0].Text, "no event was emitted") {
		t.Errorf("catch-up message should flag the gap: %q", fake.sentText[0].Text)
	}
	_ = second
}

func TestPollFallbackDetectsStatesThatEmitNoEvent(t *testing.T) {
	fake := newFakeHerdr(t)
	fake.setAgents(
		testAgent("w1:p2", "codex", "working", "worker"),
		testAgent("w1:p1", "claude", "idle", "supervisor"),
	)
	fake.setPanes(testPane("w1:p1"), testPane("w1:p2"))
	startEngine(t, fake, []map[string]any{{
		"name":    "workers",
		"watch":   map[string]any{"kinds": []string{"codex"}, "statuses": []string{"done"}},
		"target":  "w1:p1",
		"mode":    "type",
		"message": "{summary}",
	}}, map[string]any{"poll_interval_ms": 300})
	waitForSubscribe(t, fake, 1)

	// Integration-reported state changes do not reach subscribers.
	fake.setAgentStatus("w1:p2", "done")
	waitFor(t, 5*time.Second, "poll-detected delivery", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.sentText) >= 1
	})
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !strings.Contains(fake.sentText[0].Text, "no event was emitted") {
		t.Errorf("poll-detected message should say so: %q", fake.sentText[0].Text)
	}
}

func TestCoalescesNearbyEventsIntoOnePrompt(t *testing.T) {
	fake := newFakeHerdr(t)
	fake.setAgents(
		testAgent("w1:p2", "codex", "working", "worker-a"),
		testAgent("w1:p3", "codex", "working", "worker-b"),
		testAgent("w1:p1", "claude", "idle", "supervisor"),
	)
	fake.setPanes(testPane("w1:p1"), testPane("w1:p2"), testPane("w1:p3"))
	startEngine(t, fake, []map[string]any{{
		"name":        "workers",
		"watch":       map[string]any{"kinds": []string{"codex"}, "statuses": []string{"done"}},
		"target":      "w1:p1",
		"message":     "{summary}",
		"coalesce_ms": 400,
	}}, nil)
	waitForSubscribe(t, fake, 1)

	fake.setAgentStatus("w1:p2", "done")
	fake.Push("pane.agent_status_changed", map[string]any{"pane_id": "w1:p2", "agent_status": "done"})
	fake.Push("pane.agent_status_changed", map[string]any{"pane_id": "w1:p3", "agent_status": "done"})
	waitFor(t, 3*time.Second, "prompt", func() bool { return fake.promptCount() >= 1 })
	time.Sleep(400 * time.Millisecond)

	prompts := fake.promptsSnapshot()
	if len(prompts) != 1 {
		t.Fatalf("got %d prompts, want 1 coalesced prompt: %+v", len(prompts), prompts)
	}
	if !strings.Contains(prompts[0].Text, "w1:p2") || !strings.Contains(prompts[0].Text, "w1:p3") {
		t.Errorf("coalesced prompt missing events: %q", prompts[0].Text)
	}
	if !strings.HasPrefix(prompts[0].Text, "herdr-bridge: 2 events") {
		t.Errorf("coalesced prompt missing batch header: %q", prompts[0].Text)
	}
}

func TestRateLimitSuppressesBurst(t *testing.T) {
	fake := newFakeHerdr(t)
	fake.setAgents(
		testAgent("w1:p2", "codex", "idle", "worker"),
		testAgent("w1:p1", "claude", "idle", "supervisor"),
	)
	fake.setPanes(testPane("w1:p1"), testPane("w1:p2"))
	h := startEngine(t, fake, []map[string]any{{
		"name":                  "workers",
		"watch":                 map[string]any{"kinds": []string{"codex"}},
		"target":                "w1:p1",
		"coalesce_ms":           1,
		"rate_limit_per_minute": 1,
	}}, nil)
	waitForSubscribe(t, fake, 1)

	for _, status := range []string{"working", "done", "working", "blocked"} {
		fake.Push("pane.agent_status_changed", map[string]any{"pane_id": "w1:p2", "agent_status": status})
		time.Sleep(150 * time.Millisecond)
	}
	waitFor(t, 3*time.Second, "suppression", func() bool {
		return h.state.StatusSnapshot().Counters.Suppressed >= 1
	})
	time.Sleep(200 * time.Millisecond)
	if n := fake.promptCount(); n != 1 {
		t.Fatalf("got %d prompts under a 1/min limit, want 1", n)
	}
}

func TestBlockedTargetIsNotifiedAndNotPrompted(t *testing.T) {
	fake := newFakeHerdr(t)
	fake.setAgents(
		testAgent("w1:p2", "codex", "working", "worker"),
		testAgent("w1:p1", "claude", "blocked", "supervisor"),
	)
	fake.setPanes(testPane("w1:p1"), testPane("w1:p2"))
	h := startEngine(t, fake, []map[string]any{{
		"name":   "workers",
		"watch":  map[string]any{"kinds": []string{"codex"}},
		"target": "w1:p1",
	}}, nil)
	waitForSubscribe(t, fake, 1)

	fake.Push("pane.agent_status_changed", map[string]any{"pane_id": "w1:p2", "agent_status": "done"})
	waitFor(t, 3*time.Second, "notification", func() bool { return len(fake.notificationsSnapshot()) == 1 })
	time.Sleep(200 * time.Millisecond)
	if n := fake.promptCount(); n != 0 {
		t.Fatalf("blocked target received %d prompts, want 0", n)
	}
	st := h.state.StatusSnapshot()
	if st.LastDelivery == nil || !strings.Contains(st.LastDelivery.Error, "blocked") {
		t.Fatalf("last delivery should record the blocked target, got %+v", st.LastDelivery)
	}
}

func TestBusyTargetWaitsForSettleBeforePrompting(t *testing.T) {
	fake := newFakeHerdr(t)
	fake.setAgents(
		testAgent("w1:p2", "codex", "working", "worker"),
		testAgent("w1:p1", "claude", "working", "supervisor"),
	)
	fake.setPanes(testPane("w1:p1"), testPane("w1:p2"))
	startEngine(t, fake, []map[string]any{{
		"name":    "workers",
		"watch":   map[string]any{"kinds": []string{"codex"}, "statuses": []string{"done"}},
		"target":  "w1:p1",
		"message": "{summary}",
	}}, nil)
	waitForSubscribe(t, fake, 1)

	fake.Push("pane.agent_status_changed", map[string]any{"pane_id": "w1:p2", "agent_status": "done"})
	waitFor(t, 3*time.Second, "wait on busy target", func() bool { return len(fake.waitsSnapshot()) >= 1 })
	if got := fake.waitsSnapshot()[0]; got != "w1:p1" {
		t.Errorf("agent.wait target = %q, want w1:p1", got)
	}
	waitFor(t, 3*time.Second, "prompt after settle", func() bool { return fake.promptCount() == 1 })
}

func TestTypeModeWritesIntoPlainPane(t *testing.T) {
	fake := newFakeHerdr(t)
	fake.setAgents(testAgent("w1:p2", "codex", "working", "worker"))
	fake.setPanes(testPane("w1:p1"), testPane("w1:p2"))
	startEngine(t, fake, []map[string]any{{
		"name":    "workers",
		"watch":   map[string]any{"kinds": []string{"codex"}},
		"target":  "w1:p1",
		"mode":    "type",
		"message": "worker finished: {pane_id} {status}",
	}}, nil)
	waitForSubscribe(t, fake, 1)

	fake.Push("pane.agent_status_changed", map[string]any{"pane_id": "w1:p2", "agent_status": "done"})
	waitFor(t, 3*time.Second, "typed input", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.sentText) >= 1 && len(fake.sendKeys) >= 1
	})
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got := fake.sentText[0]; got.Pane != "w1:p1" || got.Text != "worker finished: w1:p2 done" {
		t.Errorf("typed text = %+v", got)
	}
	if len(fake.sendKeys[0]) != 1 || fake.sendKeys[0][0] != "enter" {
		t.Errorf("send keys = %+v, want [enter]", fake.sendKeys[0])
	}
}

func TestOutputMatchRule(t *testing.T) {
	fake := newFakeHerdr(t)
	fake.setPanes(testPane("w1:p4"), testPane("w1:p1"))
	startEngine(t, fake, []map[string]any{{
		"name": "logs",
		"watch": map[string]any{"output_match": map[string]any{
			"pane_id": "w1:p4",
			"source":  "recent-unwrapped",
			"match":   map[string]any{"type": "substring", "value": "READY"},
		}},
		"target":  "w1:p1",
		"mode":    "type",
		"message": "{summary}",
	}}, nil)
	waitForSubscribe(t, fake, 1)

	found := false
	for _, s := range fake.lastSubscriptions() {
		if s["type"] == "pane.output_matched" && s["pane_id"] == "w1:p4" {
			found = true
		}
	}
	if !found {
		t.Fatalf("pane.output_matched subscription missing: %+v", fake.lastSubscriptions())
	}

	fake.Push("pane.output_matched", map[string]any{"pane_id": "w1:p4", "matched_line": "READY to serve"})
	waitFor(t, 3*time.Second, "typed text", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.sentText) >= 1
	})
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if want := "pane w1:p4 output matched: READY to serve"; fake.sentText[0].Text != want {
		t.Errorf("typed text = %q, want %q", fake.sentText[0].Text, want)
	}
}

func TestPreExistingOutputMatchIsNotDelivered(t *testing.T) {
	fake := newFakeHerdr(t)
	fake.setPanes(testPane("w1:p4"), testPane("w1:p1"))
	fake.setReadText("w1:p4", "old line\nREADY to serve\n")
	rules := []map[string]any{{
		"name": "logs",
		"watch": map[string]any{"output_match": map[string]any{
			"pane_id": "w1:p4",
			"match":   map[string]any{"type": "substring", "value": "READY"},
		}},
		"target":  "w1:p1",
		"mode":    "type",
		"message": "{summary}",
	}}
	startEngine(t, fake, rules, nil)
	waitForSubscribe(t, fake, 1)

	// A fresh subscription reports the standing match once; it is old news.
	fake.Push("pane.output_matched", map[string]any{"pane_id": "w1:p4", "matched_line": "READY to serve"})
	time.Sleep(400 * time.Millisecond)
	fake.mu.Lock()
	n := len(fake.sentText)
	fake.mu.Unlock()
	if n != 0 {
		t.Fatalf("standing match was delivered %d time(s); want 0", n)
	}

	// A match that appears later is delivered.
	fake.setReadText("w1:p4", "old line\nREADY to serve\nREADY again\n")
	fake.Push("pane.output_matched", map[string]any{"pane_id": "w1:p4", "matched_line": "READY again"})
	waitFor(t, 3*time.Second, "new match delivery", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.sentText) >= 1
	})
}

func TestNotifyPreexistingOptInDeliversStandingMatch(t *testing.T) {
	fake := newFakeHerdr(t)
	fake.setPanes(testPane("w1:p4"), testPane("w1:p1"))
	fake.setReadText("w1:p4", "READY to serve")
	startEngine(t, fake, []map[string]any{{
		"name": "logs",
		"watch": map[string]any{"output_match": map[string]any{
			"pane_id":            "w1:p4",
			"match":              map[string]any{"type": "substring", "value": "READY"},
			"notify_preexisting": true,
		}},
		"target": "w1:p1",
		"mode":   "type",
	}}, nil)
	waitForSubscribe(t, fake, 1)
	fake.Push("pane.output_matched", map[string]any{"pane_id": "w1:p4", "matched_line": "READY to serve"})
	waitFor(t, 3*time.Second, "standing match delivery", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.sentText) >= 1
	})
}

func TestSelfReferentialEventsAreDropped(t *testing.T) {
	fake := newFakeHerdr(t)
	fake.setAgents(testAgent("w1:p1", "claude", "idle", "supervisor"))
	fake.setPanes(testPane("w1:p1"))
	h := startEngine(t, fake, []map[string]any{{
		"name":   "loop",
		"watch":  map[string]any{"kinds": []string{"claude"}},
		"target": "w1:p1",
	}}, nil)
	waitForSubscribe(t, fake, 1)

	fake.Push("pane.agent_status_changed", map[string]any{"pane_id": "w1:p1", "agent_status": "done"})
	time.Sleep(300 * time.Millisecond)
	if n := fake.promptCount(); n != 0 {
		t.Fatalf("self-referential event produced %d prompt(s), want 0", n)
	}
	if !strings.Contains(h.log.String(), "self-referential") {
		t.Errorf("expected a self-referential warning in the log")
	}
}

// Herdr reports a worker that finishes in a background tab as "done" and only
// rewrites it to "idle" once the user visits that tab. A rule that watches
// "idle" must still wake the initiator when the worker finishes.
func TestBackgroundCompletionWakesWithoutTabActivation(t *testing.T) {
	fake := newFakeHerdr(t)
	fake.setAgents(
		testAgent("w1:p2", "agy", "working", "worker"),
		testAgent("w1:p1", "claude", "idle", "supervisor"),
	)
	fake.setPanes(testPane("w1:p1"), testPane("w1:p2"))
	startEngine(t, fake, []map[string]any{{
		"name":    "workers",
		"watch":   map[string]any{"panes": []string{"w1:p2"}, "statuses": []string{"idle", "blocked"}},
		"target":  "w1:p1",
		"message": "{summary}",
	}}, nil)
	waitForSubscribe(t, fake, 1)

	// The worker finishes while its tab sits in the background.
	fake.setAgentStatus("w1:p2", "done")
	fake.Push("pane.agent_status_changed", map[string]any{
		"pane_id": "w1:p2", "workspace_id": "w1", "agent_status": "done", "agent": "agy",
	})
	waitFor(t, 3*time.Second, "prompt on background completion", func() bool { return fake.promptCount() == 1 })

	got := fake.promptsSnapshot()[0]
	if got.Target != "w1:p1" {
		t.Errorf("prompt target = %q, want w1:p1", got.Target)
	}
}

// Visiting the finished pane rewrites "done" to "idle". That acknowledges the
// completion the bridge already reported; it is not a second completion.
func TestViewingCompletedPaneDoesNotWakeAgain(t *testing.T) {
	fake := newFakeHerdr(t)
	fake.setAgents(
		testAgent("w1:p2", "agy", "working", "worker"),
		testAgent("w1:p1", "claude", "idle", "supervisor"),
	)
	fake.setPanes(testPane("w1:p1"), testPane("w1:p2"))
	startEngine(t, fake, []map[string]any{{
		"name":    "workers",
		"watch":   map[string]any{"panes": []string{"w1:p2"}, "statuses": []string{"idle", "blocked"}},
		"target":  "w1:p1",
		"message": "{summary}",
	}}, nil)
	waitForSubscribe(t, fake, 1)

	fake.setAgentStatus("w1:p2", "done")
	fake.Push("pane.agent_status_changed", map[string]any{
		"pane_id": "w1:p2", "workspace_id": "w1", "agent_status": "done", "agent": "agy",
	})
	waitFor(t, 3*time.Second, "prompt on background completion", func() bool { return fake.promptCount() == 1 })

	// The user activates the worker tab, so herdr now reports "idle".
	fake.setAgentStatus("w1:p2", "idle")
	fake.Push("pane.agent_status_changed", map[string]any{
		"pane_id": "w1:p2", "workspace_id": "w1", "agent_status": "idle", "agent": "agy",
	})
	time.Sleep(500 * time.Millisecond)
	if n := fake.promptCount(); n != 1 {
		t.Fatalf("viewing the finished pane produced %d prompt(s); want 1", n)
	}
}

// A worker in the active tab reports "idle" when it finishes; a rule that
// watches "done" must still see that completion.
func TestForegroundCompletionWakesRuleThatWatchesDone(t *testing.T) {
	fake := newFakeHerdr(t)
	fake.setAgents(
		testAgent("w1:p2", "codex", "working", "worker"),
		testAgent("w1:p1", "claude", "idle", "supervisor"),
	)
	fake.setPanes(testPane("w1:p1"), testPane("w1:p2"))
	startEngine(t, fake, []map[string]any{{
		"name":    "workers",
		"watch":   map[string]any{"panes": []string{"w1:p2"}, "statuses": []string{"done", "blocked"}},
		"target":  "w1:p1",
		"message": "{summary}",
	}}, nil)
	waitForSubscribe(t, fake, 1)

	fake.setAgentStatus("w1:p2", "idle")
	fake.Push("pane.agent_status_changed", map[string]any{
		"pane_id": "w1:p2", "workspace_id": "w1", "agent_status": "idle", "agent": "codex",
	})
	waitFor(t, 3*time.Second, "prompt on foreground completion", func() bool { return fake.promptCount() == 1 })
}

func TestResubscribesWhenTopologyChanges(t *testing.T) {
	fake := newFakeHerdr(t)
	fake.setAgents(testAgent("w1:p2", "codex", "working", "worker"))
	fake.setPanes(testPane("w1:p1"), testPane("w1:p2"))
	startEngine(t, fake, []map[string]any{{
		"name":   "workers",
		"watch":  map[string]any{"kinds": []string{"codex"}},
		"target": "w1:p1",
		"mode":   "type",
	}}, nil)
	waitForSubscribe(t, fake, 1)

	fake.setAgents(
		testAgent("w1:p2", "codex", "working", "worker"),
		testAgent("w1:p7", "codex", "working", "worker-2"),
	)
	fake.setPanes(testPane("w1:p1"), testPane("w1:p2"), testPane("w1:p7"))
	fake.Push("pane.agent_detected", map[string]any{"pane_id": "w1:p7", "agent": "codex"})

	waitFor(t, 6*time.Second, "resubscribe", func() bool { return fake.subscribeCount() >= 2 })
	waitFor(t, 3*time.Second, "new pane subscription", func() bool {
		for _, s := range fake.lastSubscriptions() {
			if s["type"] == "pane.agent_status_changed" && s["pane_id"] == "w1:p7" {
				return true
			}
		}
		return false
	})
}

func TestReloadsRulesWhenConfigChanges(t *testing.T) {
	fake := newFakeHerdr(t)
	fake.setPanes(testPane("w1:p1"), testPane("w1:p4"))
	h := startEngine(t, fake, []map[string]any{{
		"name":   "workers",
		"watch":  map[string]any{"panes": []string{"w1:p4"}},
		"target": "w1:p1",
		"mode":   "type",
	}}, nil)
	waitForSubscribe(t, fake, 1)

	h.rewriteConfig([]map[string]any{{
		"name": "logs",
		"watch": map[string]any{"output_match": map[string]any{
			"pane_id": "w1:p4",
			"match":   map[string]any{"type": "substring", "value": "READY"},
		}},
		"target": "w1:p1",
		"mode":   "type",
	}}, nil)

	waitFor(t, 6*time.Second, "reload", func() bool { return fake.subscribeCount() >= 2 })
	waitFor(t, 3*time.Second, "reloaded subscription", func() bool {
		for _, s := range fake.lastSubscriptions() {
			if s["type"] == "pane.output_matched" {
				return true
			}
		}
		return false
	})
}

func TestMissingConfigFileDoesNotTriggerReloadLoop(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	cfgPath := filepath.Join(dir, "config.json") // never created
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	engine := NewEngine(cfg, NewClient(cfg.SocketPathOr()), NewState(cfg.StateDir), NewLogger(&testLogger{}, LevelError), true)
	if err := engine.reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if engine.configChanged() {
		t.Fatal("absent config file reported as changed; the daemon would reload every tick")
	}

	// A rule armed while the daemon runs must still be picked up.
	rulePath := RuntimeRulePath(cfg.StateDir, "armed")
	if err := os.MkdirAll(filepath.Dir(rulePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rulePath, []byte(`{"name":"armed","target":"w1:p1","watch":{"panes":["w1:p2"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !engine.configChanged() {
		t.Fatal("new runtime rule not detected")
	}
	if err := engine.reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if engine.configChanged() {
		t.Fatal("unchanged runtime rule reported as changed")
	}

	// Disarming a rule must be noticed too.
	if err := os.Remove(rulePath); err != nil {
		t.Fatal(err)
	}
	if !engine.configChanged() {
		t.Fatal("removed runtime rule not detected")
	}

	// A config file created after startup is still detected.
	if err := os.WriteFile(cfgPath, []byte(`{"rules":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !engine.configChanged() {
		t.Fatal("config file created after startup not detected")
	}
}

func TestWatchConflicts(t *testing.T) {
	agents := map[string]AgentInfo{
		"w1:p1": testAgent("w1:p1", "claude", "idle", "lead"),
		"w1:p2": testAgent("w1:p2", "codex", "working", "worker-a"),
		"w1:p3": testAgent("w1:p3", "codex", "working", "worker-b"),
	}
	paneRule := func(name, target string, panes ...string) Rule {
		return Rule{Name: name, Target: target, Watch: Watch{Panes: panes, Statuses: []string{"done", "blocked"}}}
	}

	// Two initiators watching the same worker pane: the event wakes both.
	got := watchConflicts([]Rule{
		paneRule("codex-wait", "w1:p1", "w1:p2"),
		paneRule("claude-wait", "w1:p9", "w1:p2"),
	}, agents)
	if len(got) != 1 || got[0].Pane != "w1:p2" || len(got[0].Rules) != 2 {
		t.Fatalf("conflicts = %+v, want one conflict on w1:p2 with two watchers", got)
	}
	if got[0].Rules[0] != "claude-wait -> w1:p9" || got[0].Rules[1] != "codex-wait -> w1:p1" {
		t.Errorf("watcher list = %v", got[0].Rules)
	}

	// The same target written as a pane id and as an agent name is not a conflict.
	byName := []Rule{
		paneRule("codex-wait", "w1:p1", "w1:p2", "w1:p3"),
		paneRule("lead-wait", "lead", "w1:p2", "w1:p3"),
	}
	if got := watchConflicts(byName, agents); len(got) != 0 {
		t.Errorf("same resolved target reported as a conflict: %+v", got)
	}

	// A disabled rule does not participate.
	disabled := paneRule("codex-wait", "w1:p1", "w1:p2")
	disabled.Enabled = boolPtr(false)
	if got := watchConflicts([]Rule{disabled, paneRule("claude-wait", "w1:p9", "w1:p2")}, agents); len(got) != 0 {
		t.Errorf("disabled rule reported as a conflict: %+v", got)
	}
}

func TestSelfTargetResolvesThroughEnvironment(t *testing.T) {
	t.Setenv("HERDR_PANE_ID", "w1:p9")
	fake := newFakeHerdr(t)
	fake.setAgents(testAgent("w1:p2", "codex", "working", "worker"))
	fake.setPanes(testPane("w1:p9"), testPane("w1:p2"))
	startEngine(t, fake, []map[string]any{{
		"name":   "workers",
		"watch":  map[string]any{"kinds": []string{"codex"}},
		"target": "self",
		"mode":   "type",
	}}, nil)
	waitForSubscribe(t, fake, 1)

	fake.Push("pane.agent_status_changed", map[string]any{"pane_id": "w1:p2", "agent_status": "done"})
	waitFor(t, 3*time.Second, "typed text", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.sentText) >= 1
	})
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.sentText[0].Pane != "w1:p9" {
		t.Errorf("self target resolved to %q, want w1:p9", fake.sentText[0].Pane)
	}
}
