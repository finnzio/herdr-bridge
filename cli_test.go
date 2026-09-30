package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArmCapturesCallerAndAllowsTargetOverride(t *testing.T) {
	cases := []struct {
		name, caller, target, want string
		code                       int
	}{
		{name: "default caller", caller: "w1:p1", want: "w1:p1"},
		{name: "explicit self", caller: "w1:p3", target: "self", want: "w1:p3"},
		{name: "explicit pane", caller: "w1:p1", target: "w1:p9", want: "w1:p9"},
		{name: "agent name outside Herdr", target: "supervisor", want: "supervisor"},
		{name: "default outside Herdr", code: 1},
		{name: "self outside Herdr", target: "self", code: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HERDR_PANE_ID", tc.caller)
			stateDir := filepath.Join(t.TempDir(), "state")
			cfgPath := writeTestConfig(t, map[string]any{"state_dir": stateDir})
			args := []string{"--config", cfgPath, "--name", "wait", "--pane", "w1:p2"}
			if tc.target != "" {
				args = append(args, "--target", tc.target)
			}
			if got := cmdArm(args); got != tc.code {
				t.Fatalf("arm returned %d, want %d", got, tc.code)
			}
			if tc.code != 0 {
				if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
					t.Fatalf("failed arm created state directory: %v", err)
				}
				return
			}
			// Reload from disk with a different caller, as the shared daemon
			// would. A persisted self selector would notify the wrong pane.
			t.Setenv("HERDR_PANE_ID", "w2:p7")
			cfg, err := LoadConfig(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			rules, err := cfg.AllRules()
			if err != nil {
				t.Fatal(err)
			}
			if len(rules) != 1 || rules[0].Target != tc.want {
				t.Fatalf("persisted rules = %+v, want target %q", rules, tc.want)
			}
		})
	}
}

func TestArmRefusesToTakeOverAnotherPanesRule(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	cfgPath := writeTestConfig(t, map[string]any{"state_dir": stateDir})
	rulePath := filepath.Join(stateDir, "rules", "wait.json")
	arm := func(pane string, extra ...string) int {
		t.Setenv("HERDR_PANE_ID", pane)
		args := append([]string{"--config", cfgPath, "--name", "wait", "--pane", "w1:p2"}, extra...)
		return cmdArm(args)
	}

	if code := arm("w1:p1"); code != 0 {
		t.Fatalf("first arm returned %d", code)
	}
	if owner, ok := readRuleOwner(rulePath, "wait"); !ok || owner != "w1:p1" {
		t.Fatalf("stored owner = %q (ok=%v), want w1:p1", owner, ok)
	}

	if code := arm("w1:p9", "--replace"); code == 0 {
		t.Fatal("--replace from another pane overwrote the rule; want refusal")
	}
	if owner, _ := readRuleOwner(rulePath, "wait"); owner != "w1:p1" {
		t.Fatalf("refused arm still changed the owner to %q", owner)
	}

	if code := arm("w1:p9", "--replace", "--force"); code != 0 {
		t.Fatalf("forced take-over returned %d", code)
	}
	if owner, _ := readRuleOwner(rulePath, "wait"); owner != "w1:p9" {
		t.Fatalf("owner after force = %q, want w1:p9", owner)
	}

	// Re-arming from the owner keeps working without --force.
	if code := arm("w1:p9", "--replace"); code != 0 {
		t.Fatalf("owner re-arm returned %d", code)
	}
}

func TestArmedEventsReturnToEachInitiator(t *testing.T) {
	fake := newFakeHerdr(t)
	kinds := []string{"codex", "claude", "grok", "cursor", "devin", "omp", "agy"}
	agents := []AgentInfo{testAgent("w1:pZ", "codex", "idle", "daemon-pane")}
	panes := []PaneInfo{testPane("w1:pZ")}
	callers := make([]string, len(kinds))
	workers := make([]string, len(kinds))
	for i, kind := range kinds {
		callers[i] = fmt.Sprintf("w1:p%d", i+1)
		workers[i] = fmt.Sprintf("w1:p%d", i+11)
		agents = append(agents, testAgent(callers[i], kind, "idle", ""), testAgent(workers[i], kind, "working", ""))
		panes = append(panes, testPane(callers[i]), testPane(workers[i]))
	}
	fake.setAgents(agents...)
	fake.setPanes(panes...)
	dir := t.TempDir()
	cfgPath := writeTestConfig(t, map[string]any{
		"socket_path": fake.path,
		"state_dir":   filepath.Join(dir, "state"),
		"defaults":    map[string]any{"coalesce_ms": 250},
	})
	for i, caller := range callers {
		t.Setenv("HERDR_PANE_ID", caller)
		args := []string{"--config", cfgPath, "--name", "wait-" + kinds[i], "--message", "{agent} {pane_id} {status}"}
		for _, kind := range kinds {
			args = append(args, "--kind", kind)
		}
		if code := cmdArm(args); code != 0 {
			t.Fatalf("arm from %s returned %d", caller, code)
		}
	}
	// The daemon starts in a separate pane, with none of the initiators' context.
	t.Setenv("HERDR_PANE_ID", "w1:pZ")
	startEngineInDir(t, fake, dir, nil, nil)
	waitForSubscribe(t, fake, 1)
	statuses := []string{"done", "blocked"}
	for i, worker := range workers {
		status := statuses[i%len(statuses)]
		fake.setAgentStatus(worker, status)
		fake.Push("pane.agent_status_changed", map[string]any{"pane_id": worker, "agent_status": status})
	}
	waitFor(t, 3*time.Second, "all initiators notified", func() bool { return fake.promptCount() >= len(callers) })
	prompts := fake.promptsSnapshot()
	counts := map[string]int{}
	for _, p := range prompts {
		counts[p.Target]++
		for i, kind := range kinds {
			want := fmt.Sprintf("%s %s %s", kind, workers[i], statuses[i%len(statuses)])
			if !strings.Contains(p.Text, want) {
				t.Errorf("notification to %s missing worker result %q: %s", p.Target, want, p.Text)
			}
		}
	}
	if len(prompts) != len(callers) {
		t.Fatalf("got %d notifications, want %d: %+v", len(prompts), len(callers), prompts)
	}
	for _, caller := range callers {
		if counts[caller] != 1 {
			t.Errorf("got %d notifications to %s, want 1", counts[caller], caller)
		}
	}
}
