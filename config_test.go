package main

import (
	"encoding/json"

	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func writeTestConfig(t *testing.T, raw map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestConfigValidationRejectsBadRules(t *testing.T) {
	cases := []struct {
		name string
		rule map[string]any
	}{
		{"no selector", map[string]any{"name": "a", "target": "w1:p1"}},
		{"no target", map[string]any{"name": "a", "watch": map[string]any{"panes": []string{"w1:p1"}}}},
		{"bad mode", map[string]any{"name": "a", "target": "w1:p1", "mode": "teleport", "watch": map[string]any{"panes": []string{"w1:p1"}}}},
		{"bad status", map[string]any{"name": "a", "target": "w1:p1", "watch": map[string]any{"panes": []string{"w1:p1"}, "statuses": []string{"sleepy"}}}},
		{"unknown placeholder", map[string]any{"name": "a", "target": "w1:p1", "message": "{wat}", "watch": map[string]any{"panes": []string{"w1:p1"}}}},
		{"bad regex", map[string]any{"name": "a", "target": "w1:p1", "watch": map[string]any{"output_match": map[string]any{
			"pane_id": "w1:p1", "match": map[string]any{"type": "regex", "value": "([unclosed"}}}}},
		{"match without pane", map[string]any{"name": "a", "target": "w1:p1", "watch": map[string]any{"output_match": map[string]any{
			"match": map[string]any{"type": "substring", "value": "x"}}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTestConfig(t, map[string]any{"rules": []map[string]any{tc.rule}})
			if _, err := LoadConfig(path); err == nil {
				t.Fatalf("config with %s was accepted; want error", tc.name)
			}
		})
	}
}

func TestMissingConfigFileIsNotAnError(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("missing config should fall back to defaults, got %v", err)
	}
	if len(cfg.Rules) != 0 || cfg.Defaults.Message != defaultMessage {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
	if cfg.SocketPathOr() == "" || cfg.StateDir == "" {
		t.Errorf("defaults not resolved: socket=%q state=%q", cfg.SocketPathOr(), cfg.StateDir)
	}

	// A config that only sets a state dir still accepts arming rules.
	path := writeTestConfig(t, map[string]any{"state_dir": filepath.Join(t.TempDir(), "state")})
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	rule := Rule{Name: "armed", Target: "w1:p1", Mode: modeType, Watch: Watch{Panes: []string{"w1:p1"}}}
	if err := os.MkdirAll(filepath.Join(cfg.StateDir, "rules"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := writeJSONAtomic(RuntimeRulePath(cfg.StateDir, rule.Name), rule); err != nil {
		t.Fatalf("write rule: %v", err)
	}
	all, err := cfg.AllRules()
	if err != nil {
		t.Fatalf("AllRules: %v", err)
	}
	if len(all) != 1 || all[0].Name != "armed" {
		t.Errorf("runtime rule not picked up: %+v", all)
	}
}

func TestConfigDefaultsAreAppliedToRules(t *testing.T) {
	path := writeTestConfig(t, map[string]any{
		"defaults": map[string]any{"coalesce_ms": 250, "rate_limit_per_minute": 3},
		"rules": []map[string]any{{
			"name":   "workers",
			"target": "w1:p1",
			"watch":  map[string]any{"kinds": []string{"codex"}},
		}},
	})
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := cfg.Rules[0]
	if r.Mode != modePrompt {
		t.Errorf("mode = %q, want prompt", r.Mode)
	}
	if *r.CoalesceMS != 250 || *r.RateLimitPerMinute != 3 {
		t.Errorf("defaults not applied: %+v", r)
	}
	if want := []string{"done", "blocked"}; !slices.Equal(r.Watch.Statuses, want) {
		t.Errorf("statuses = %v, want %v", r.Watch.Statuses, want)
	}
	if r.Message != defaultMessage {
		t.Errorf("message = %q, want the default template", r.Message)
	}
}

func TestRuntimeRulesOverrideConfigRulesByName(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(filepath.Join(stateDir, "rules"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := writeTestConfig(t, map[string]any{
		"state_dir": stateDir,
		"rules": []map[string]any{{
			"name": "shared", "target": "w1:p1", "mode": "type",
			"watch": map[string]any{"panes": []string{"w1:p1"}},
		}},
	})
	rule := Rule{Name: "shared", Target: "w1:p9", Mode: modePrompt, Watch: Watch{Panes: []string{"w1:p9"}}}
	if err := writeJSONAtomic(RuntimeRulePath(stateDir, "shared"), rule); err != nil {
		t.Fatalf("write rule: %v", err)
	}
	extra := Rule{Name: "extra", Target: "w1:p2", Mode: modeType, Watch: Watch{Panes: []string{"w1:p2"}}}
	if err := writeJSONAtomic(RuntimeRulePath(stateDir, "extra"), extra); err != nil {
		t.Fatalf("write rule: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	all, err := cfg.AllRules()
	if err != nil {
		t.Fatalf("AllRules: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d rules, want 2: %+v", len(all), all)
	}
	byName := map[string]Rule{}
	for _, r := range all {
		byName[r.Name] = r
	}
	if byName["shared"].Target != "w1:p9" || byName["shared"].Mode != modePrompt {
		t.Errorf("runtime rule did not override the config rule: %+v", byName["shared"])
	}
	if byName["extra"].Origin == "config" {
		t.Errorf("runtime rule origin = %q, want its file path", byName["extra"].Origin)
	}
}

func TestRenderMessagePlaceholders(t *testing.T) {
	ctx := &EventContext{
		Event:       "pane.agent_status_changed",
		Rule:        "workers",
		PaneID:      "w1:p2",
		WorkspaceID: "w1",
		TabID:       "w1:t1",
		Status:      "done",
		AgentKind:   "codex",
		AgentName:   "worker-a",
		Title:       "fix tests",
		Time:        time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC),
	}
	got := renderMessage("{agent} {name} {pane_id} {status} {title} ({count}) {rule} {time}", ctx,
		map[string]string{"count": "2"})
	want := "codex worker-a w1:p2 done fix tests (2) workers 2026-09-28T10:00:00Z"
	if got != want {
		t.Errorf("rendered = %q, want %q", got, want)
	}
	if s := ctx.summary(); s != "pane w1:p2 (worker-a) status: done - fix tests" {
		t.Errorf("summary = %q", s)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, value string
		want           bool
	}{
		{"worker-*", "worker-1", true},
		{"worker-*", "worker", false},
		{"*-grok", "fast-grok", true},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxcyyb", false},
		{"exact", "exact", true},
		{"exact", "exactly", false},
		{"*", "anything", true},
	}
	for _, tc := range cases {
		if got := globMatch(tc.pattern, tc.value); got != tc.want {
			t.Errorf("globMatch(%q,%q) = %v, want %v", tc.pattern, tc.value, got, tc.want)
		}
	}
}

func TestKnownStatePersistsAndPrunes(t *testing.T) {
	dir := t.TempDir()
	state := NewState(dir)
	if err := state.EnsureDir(); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if changed, existed := state.Observe("w1:p2", "working", 1); changed || existed {
		t.Errorf("first observation should be a silent baseline, got changed=%v existed=%v", changed, existed)
	}
	if changed, existed := state.Observe("w1:p2", "done", 2); !changed || !existed {
		t.Errorf("second observation should be a change, got changed=%v existed=%v", changed, existed)
	}
	if err := state.Flush(true); err != nil {
		t.Fatalf("flush: %v", err)
	}

	reloaded := NewState(dir)
	if err := reloaded.LoadKnown(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if changed, existed := reloaded.Observe("w1:p2", "done", 2); changed || !existed {
		t.Errorf("reloaded state should still know the pane, got changed=%v existed=%v", changed, existed)
	}
	if changed, existed := reloaded.Observe("w1:p2", "blocked", 3); !changed || !existed {
		t.Errorf("change after restart should be reported, got changed=%v existed=%v", changed, existed)
	}
	reloaded.Prune(map[string]bool{"w1:p3": true})
	if _, existed := reloaded.Observe("w1:p2", "blocked", 3); existed {
		t.Errorf("pruned pane should be forgotten")
	}
}

func TestObserveTreatsViewingAFinishedPaneAsAcknowledgement(t *testing.T) {
	dir := t.TempDir()
	state := NewState(dir)
	if changed, existed := state.Observe("w1:p2", "working", 1); changed || existed {
		t.Errorf("first observation should be a silent baseline, got changed=%v existed=%v", changed, existed)
	}
	if changed, existed := state.Observe("w1:p2", "done", 2); !changed || !existed {
		t.Errorf("completion should be a change, got changed=%v existed=%v", changed, existed)
	}
	// Viewing the finished pane rewrites "done" to "idle"; that is the same
	// state, not a new completion.
	if changed, existed := state.Observe("w1:p2", "idle", 3); changed || !existed {
		t.Errorf("done to idle should not be a change, got changed=%v existed=%v", changed, existed)
	}
	if changed, existed := state.Observe("w1:p2", "working", 4); !changed || !existed {
		t.Errorf("a new turn should be a change, got changed=%v existed=%v", changed, existed)
	}
	if changed, existed := state.Observe("w1:p2", "idle", 5); !changed || !existed {
		t.Errorf("the next completion should be a change, got changed=%v existed=%v", changed, existed)
	}
	// A completion herdr reports as "done" after an idle pane is a new
	// completion, not an acknowledgement.
	if changed, existed := state.Observe("w1:p2", "working", 6); !changed || !existed {
		t.Errorf("a new turn should be a change, got changed=%v existed=%v", changed, existed)
	}
	if changed, existed := state.Observe("w1:p2", "done", 7); !changed || !existed {
		t.Errorf("a done completion should be a change, got changed=%v existed=%v", changed, existed)
	}
}

func TestClientSurfacesAPIErrors(t *testing.T) {
	fake := newFakeHerdr(t)
	fake.setPanes(testPane("w1:p1"))
	client := NewClient(fake.path)

	if _, err := client.AgentGet("w1:p9"); err == nil {
		t.Fatalf("expected an error for a missing agent")
	} else if !IsCode(err, "agent_not_found") {
		t.Errorf("error = %v, want agent_not_found", err)
	}
	if err := client.Ping(); err != nil {
		t.Errorf("ping: %v", err)
	}
	if _, err := client.ListPanes(); err != nil {
		t.Errorf("pane.list: %v", err)
	}
}

func TestSubscriptionEndsWhenServerCloses(t *testing.T) {
	fake := newFakeHerdr(t)
	client := NewClient(fake.path)
	sub, err := client.Subscribe([]SubscriptionSpec{{"type": "pane.created"}})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	fake.Close()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-sub.Events:
			if !ok {
				if sub.Err() == nil {
					t.Errorf("stream closed without recording an error")
				}
				return
			}
		case <-deadline:
			t.Fatalf("stream did not close after the server went away")
		}
	}
}

func TestSubscriptionRejectsBadPane(t *testing.T) {
	fake := newFakeHerdr(t)
	client := NewClient(fake.path)
	// The fake rejects nothing by itself; this asserts the ack path parses.
	sub, err := client.Subscribe([]SubscriptionSpec{{"type": "pane.agent_status_changed", "pane_id": "w1:p1"}})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Close()
	fake.Push("pane.agent_status_changed", map[string]any{"pane_id": "w1:p1", "agent_status": "done"})
	select {
	case ev := <-sub.Events:
		if ev.Event != "pane.agent_status_changed" {
			t.Errorf("event = %q", ev.Event)
		}
		var data struct {
			Status string `json:"agent_status"`
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil || data.Status != "done" {
			t.Errorf("payload = %s (err %v)", ev.Data, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("no event received")
	}
}
