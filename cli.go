package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const version = "0.1.0"

func main() { os.Exit(runCLI(os.Args[1:])) }

func usage() {
	fmt.Fprint(os.Stderr, `herdr-bridge - wake a Herdr pane when events happen, at zero token cost

Usage:
  herdr-bridge run      [--config PATH] [--dry-run] [--verbose]
  herdr-bridge start    [--config PATH] [--dry-run]
  herdr-bridge stop     [--config PATH]
  herdr-bridge restart  [--config PATH]
  herdr-bridge status   [--config PATH] [--json]
  herdr-bridge check    [--config PATH]
  herdr-bridge watch    [--config PATH]          # foreground dry run
  herdr-bridge arm      --name N [--target T] [selectors] [--message M]
  herdr-bridge disarm   (--name N | --all) [--config PATH]
  herdr-bridge rules    [--config PATH] [--json]
  herdr-bridge logs     [--lines N] [-f] [--config PATH]
  herdr-bridge version

The bridge holds a long-lived events.subscribe stream on the Herdr socket and
never talks to a language model. When a watched event arrives it wakes the
target pane with agent.prompt (or raw pane input), which is the only step that
can start a model turn.

Arm defaults to notifying the calling pane (HERDR_PANE_ID), regardless of
agent kind. Use --target to override it.
Run 'herdr-bridge arm -h' for the selector flags.
`)
}

func runCLI(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	rest := args[1:]
	switch args[0] {
	case "run":
		return cmdRun(rest)
	case "start":
		return cmdStart(rest)
	case "stop":
		return cmdStop(rest)
	case "restart":
		if code := cmdStop(rest); code != 0 {
			return code
		}
		return cmdStart(rest)
	case "status":
		return cmdStatus(rest)
	case "check":
		return cmdCheck(rest)
	case "watch":
		return cmdRun(append([]string{"--dry-run"}, rest...))
	case "arm":
		return cmdArm(rest)
	case "disarm":
		return cmdDisarm(rest)
	case "rules":
		return cmdRules(rest)
	case "logs":
		return cmdLogs(rest)
	case "version", "--version", "-V":
		fmt.Println("herdr-bridge " + version)
		return 0
	case "help", "--help", "-h":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		usage()
		return 2
	}
}

func loadConfigOrDie(path string) *Config {
	cfg, err := LoadConfig(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herdr-bridge: %v\n", err)
		os.Exit(1)
	}
	return cfg
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	configPath := fs.String("config", DefaultConfigPath(), "config file path")
	dryRun := fs.Bool("dry-run", false, "log would-be deliveries instead of sending them")
	verbose := fs.Bool("verbose", false, "debug logging")
	level := fs.String("log-level", "", "debug|info|warn|error")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg := loadConfigOrDie(*configPath)
	lvl, err := ParseLevel(firstNonEmpty(*level, cfg.LogLevel))
	if err != nil {
		fmt.Fprintf(os.Stderr, "herdr-bridge: %v\n", err)
		return 2
	}
	if *verbose {
		lvl = LevelDebug
	}
	log := NewLogger(os.Stderr, lvl)

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	state := NewState(cfg.StateDir)
	if err := state.EnsureDir(); err != nil {
		fmt.Fprintf(os.Stderr, "herdr-bridge: state dir: %v\n", err)
		return 1
	}
	if err := state.WritePID(os.Getpid()); err != nil {
		log.Warn("could not write pid file", "err", err.Error())
	}
	defer state.RemovePID()

	state.Update(func(st *Status) {
		st.PID = os.Getpid()
		st.StartedAt = time.Now()
		st.DryRun = *dryRun
		st.ConfigPath = cfg.Path
	})

	engine := NewEngine(cfg, NewClient(cfg.SocketPathOr()), state, log, *dryRun)
	engine.reloadCh = hup
	if err := engine.Run(sigCtx); err != nil {
		log.Error("bridge stopped with error", "err", err.Error())
		state.Flush(true)
		return 1
	}
	if err := state.Flush(true); err != nil {
		log.Warn("final state flush failed", "err", err.Error())
	}
	log.Info("bridge stopped")
	return 0
}

func cmdStart(args []string) int {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	configPath := fs.String("config", DefaultConfigPath(), "config file path")
	dryRun := fs.Bool("dry-run", false, "log would-be deliveries instead of sending them")
	verbose := fs.Bool("verbose", false, "debug logging")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg := loadConfigOrDie(*configPath)
	state := NewState(cfg.StateDir)
	if err := state.EnsureDir(); err != nil {
		fmt.Fprintf(os.Stderr, "herdr-bridge: state dir: %v\n", err)
		return 1
	}
	if pid := state.ReadPID(); ProcessAlive(pid) {
		fmt.Fprintf(os.Stderr, "herdr-bridge: already running (pid %d)\n", pid)
		return 1
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herdr-bridge: cannot locate own binary: %v\n", err)
		return 1
	}
	logFile, err := os.OpenFile(state.LogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herdr-bridge: open log: %v\n", err)
		return 1
	}
	defer logFile.Close()

	childArgs := []string{"run", "--config", cfg.Path}
	if *dryRun {
		childArgs = append(childArgs, "--dry-run")
	}
	if *verbose {
		childArgs = append(childArgs, "--verbose")
	}
	cmd := exec.Command(exe, childArgs...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "herdr-bridge: start: %v\n", err)
		return 1
	}
	_ = state.WritePID(cmd.Process.Pid)

	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		st, err := state.ReadStatusFromDisk()
		if err == nil && st.PID == cmd.Process.Pid && time.Since(st.UpdatedAt) < 3*time.Second {
			fmt.Printf("herdr-bridge started (pid %d)\n", cmd.Process.Pid)
			fmt.Printf("log: %s\n", state.LogPath())
			return 0
		}
		if !ProcessAlive(cmd.Process.Pid) {
			fmt.Fprintf(os.Stderr, "herdr-bridge: daemon exited during startup; see %s\n", state.LogPath())
			state.RemovePID()
			return 1
		}
		time.Sleep(150 * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "herdr-bridge: daemon did not report readiness; see %s\n", state.LogPath())
	return 1
}

func cmdStop(args []string) int {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	configPath := fs.String("config", DefaultConfigPath(), "config file path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg := loadConfigOrDie(*configPath)
	state := NewState(cfg.StateDir)
	pid := state.ReadPID()
	if pid == 0 || !ProcessAlive(pid) {
		state.RemovePID()
		fmt.Println("herdr-bridge is not running")
		return 0
	}
	if err := SignalPID(pid, syscall.SIGTERM); err != nil {
		fmt.Fprintf(os.Stderr, "herdr-bridge: signal %d: %v\n", pid, err)
		return 1
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !ProcessAlive(pid) {
			state.RemovePID()
			fmt.Printf("herdr-bridge stopped (pid %d)\n", pid)
			return 0
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "herdr-bridge: pid %d did not exit within 5s\n", pid)
	return 1
}

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	configPath := fs.String("config", DefaultConfigPath(), "config file path")
	asJSON := fs.Bool("json", false, "print raw status JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg := loadConfigOrDie(*configPath)
	state := NewState(cfg.StateDir)
	st, err := state.ReadStatusFromDisk()
	if err != nil {
		st = state.StatusSnapshot()
	}
	alive := ProcessAlive(st.PID)
	if *asJSON {
		out := map[string]any{
			"running": alive,
			"status":  st,
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		if !alive {
			return 1
		}
		return 0
	}
	if !alive {
		fmt.Println("running:   no")
		fmt.Println("state dir: " + cfg.StateDir)
		return 1
	}
	fmt.Printf("running:        yes (pid %d, up %s)\n", st.PID, time.Since(st.StartedAt).Round(time.Second))
	fmt.Printf("socket:         %s\n", st.Socket)
	fmt.Printf("config:         %s\n", st.ConfigPath)
	fmt.Printf("dry run:        %t\n", st.DryRun)
	fmt.Printf("rules:          %s\n", strings.Join(st.Rules, ", "))
	fmt.Printf("subscriptions:  %d\n", st.Subscriptions)
	fmt.Printf("watched panes:  %s\n", strings.Join(st.WatchedPanes, ", "))
	fmt.Printf("events:         %d seen, %d matched, %d delivered, %d failed, %d suppressed\n",
		st.Counters.Events, st.Counters.Matched, st.Counters.Delivered, st.Counters.Failed, st.Counters.Suppressed)
	fmt.Printf("reconnects:     %d\n", st.Counters.Reconnects)
	if st.LastEventAt != nil {
		fmt.Printf("last event:     %s\n", st.LastEventAt.Format(time.RFC3339))
	}
	if st.LastDelivery != nil {
		text := strings.ReplaceAll(st.LastDelivery.Text, "\n", " | ")
		status := "ok"
		if st.LastDelivery.Error != "" {
			status = "failed: " + st.LastDelivery.Error
		}
		fmt.Printf("last delivery:  %s -> %s (%s) %s\n",
			st.LastDelivery.Rule, st.LastDelivery.Target, status, truncate(text, 160))
	}
	if st.LastError != "" {
		fmt.Printf("last error:     %s\n", st.LastError)
	}
	return 0
}

func cmdCheck(args []string) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	configPath := fs.String("config", DefaultConfigPath(), "config file path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg := loadConfigOrDie(*configPath)
	rules, err := cfg.AllRules()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herdr-bridge: %v\n", err)
		return 1
	}
	fmt.Printf("config:  %s\n", cfg.Path)
	fmt.Printf("socket:  %s\n", cfg.SocketPathOr())
	fmt.Printf("state:   %s\n", cfg.StateDir)
	fmt.Printf("rules:   %d (%d from runtime files)\n", len(rules), countRuntime(rules))

	client := NewClient(cfg.SocketPathOr())
	client.Timeout = time.Duration(cfg.Defaults.RequestTimeoutMS) * time.Millisecond
	if err := client.Ping(); err != nil {
		fmt.Fprintf(os.Stderr, "herdr-bridge: herdr socket unreachable: %v\n", err)
		return 1
	}
	fmt.Println("socket:  reachable")

	agents, err := client.ListAgents()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herdr-bridge: agent.list failed: %v\n", err)
		return 1
	}
	panes, err := client.ListPanes()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herdr-bridge: pane.list failed: %v\n", err)
		return 1
	}
	fmt.Printf("session: %d agents, %d panes\n", len(agents), len(panes))

	// Validate the exact subscription set against the live server so a wire
	// format mistake is caught here instead of at daemon start.
	probe := &Engine{
		client: client,
		rules:  rules,
		agents: map[string]AgentInfo{},
		panes:  map[string]PaneInfo{},
	}
	for _, a := range agents {
		probe.agents[a.PaneID] = a
	}
	for _, p := range panes {
		probe.panes[p.PaneID] = p
	}
	plan := probe.planFromRules()
	failed := false
	if len(plan.Specs) == 0 {
		fmt.Println("subscriptions: none (no enabled rule selects anything)")
		failed = true
	} else if sub, err := client.Subscribe(plan.Specs); err != nil {
		fmt.Fprintf(os.Stderr, "herdr-bridge: herdr rejected the subscription set: %v\n", err)
		failed = true
	} else {
		sub.Close()
		fmt.Printf("subscriptions: %d accepted by herdr\n", len(plan.Specs))
	}

	for i := range rules {
		r := &rules[i]
		if !r.EnabledNow() {
			fmt.Printf("rule %-20s disabled\n", r.Name)
			continue
		}
		matched := 0
		for _, a := range agents {
			if watchMatchesAgent(&r.Watch, a) {
				matched++
			}
		}
		selectorNote := fmt.Sprintf("%d agent pane(s)", matched)
		if om := r.Watch.OutputMatch; om != nil {
			exists := false
			for _, p := range panes {
				if p.PaneID == om.PaneID {
					exists = true
				}
			}
			selectorNote += fmt.Sprintf(", output_match pane %s", om.PaneID)
			if !exists {
				selectorNote += " (MISSING)"
				failed = true
			}
		}
		if r.Watch.PaneExited {
			selectorNote += ", pane.exited"
		}
		targetNote := r.Target
		if r.Mode != modeNotify {
			target, err := checkTarget(client, agents, panes, r.Target)
			if err != nil {
				targetNote = fmt.Sprintf("%s (UNRESOLVED: %v)", r.Target, err)
				failed = true
			} else {
				targetNote = fmt.Sprintf("%s -> %s", r.Target, target)
			}
		}
		// A rule that watches its own target would wake itself in a loop.
		for _, a := range agents {
			if a.PaneID == r.Target && watchMatchesAgent(&r.Watch, a) {
				fmt.Printf("rule %-20s WARNING target %s matches its own watch selector\n", r.Name, r.Target)
			}
		}
		fmt.Printf("rule %-20s mode=%s watch[%s] target=%s\n", r.Name, r.Mode, selectorNote, targetNote)
	}
	for _, c := range watchConflicts(rules, probe.agents) {
		fmt.Printf("WARNING pane %s is watched by rules with different targets: %s\n",
			c.Pane, strings.Join(c.Rules, ", "))
	}
	if failed {
		return 1
	}
	return 0
}

func checkTarget(client *Client, agents []AgentInfo, panes []PaneInfo, target string) (string, error) {
	if target == "self" {
		pane := os.Getenv("HERDR_PANE_ID")
		if pane == "" {
			return "", fmt.Errorf("HERDR_PANE_ID is not set")
		}
		return pane + " (self)", nil
	}
	if paneIDRe.MatchString(target) {
		for _, a := range agents {
			if a.PaneID == target {
				return target + " (agent " + a.Kind() + ")", nil
			}
		}
		for _, p := range panes {
			if p.PaneID == target {
				return target + " (no agent)", nil
			}
		}
		return "", fmt.Errorf("pane not found")
	}
	for _, a := range agents {
		if a.AgentName() == target {
			return a.PaneID + " (agent " + a.Kind() + ")", nil
		}
	}
	return "", fmt.Errorf("no live agent has this name")
}

func countRuntime(rules []Rule) int {
	n := 0
	for _, r := range rules {
		if r.Origin != "config" {
			n++
		}
	}
	return n
}

func cmdArm(args []string) int {
	fs := flag.NewFlagSet("arm", flag.ContinueOnError)
	configPath := fs.String("config", DefaultConfigPath(), "config file path")
	name := fs.String("name", "", "rule name (required)")
	target := fs.String("target", "self", "wake target: pane id, live agent name, or self (calling pane)")
	mode := fs.String("mode", modePrompt, "delivery mode: prompt|type|notify")
	message := fs.String("message", "", "message template (defaults to the config default)")
	var agents, kinds, panes, workspaces, statuses stringList
	fs.Var(&agents, "agent", "watch agent name or glob (repeatable)")
	fs.Var(&kinds, "kind", "watch agent kind, e.g. codex (repeatable)")
	fs.Var(&panes, "pane", "watch exact pane id (repeatable)")
	fs.Var(&workspaces, "workspace", "watch workspace id (repeatable)")
	fs.Var(&statuses, "status", "watch agent status: done|blocked|idle|working|unknown (repeatable, default done,blocked)")
	matchPane := fs.String("match-pane", "", "watch pane output on this pane id")
	matchRe := fs.String("match-regex", "", "output match regex")
	matchSub := fs.String("match-substring", "", "output match literal substring")
	matchSource := fs.String("match-source", "recent_unwrapped", "output match read source: visible|recent|recent_unwrapped|detection")
	matchLines := fs.Int("match-lines", 0, "output match row limit (0 = server default)")
	paneExited := fs.Bool("pane-exited", false, "also watch pane exit events")
	coalesce := fs.Int("coalesce-ms", 0, "coalesce window override")
	rate := fs.Int("rate-limit", 0, "max deliveries per minute for this target")
	replace := fs.Bool("replace", false, "overwrite an existing rule with the same name")
	force := fs.Bool("force", false, "with --replace, take over a rule armed by another pane")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *name == "" {
		fmt.Fprintln(os.Stderr, "herdr-bridge arm: --name is required")
		return 2
	}
	if *target == "" {
		fmt.Fprintln(os.Stderr, "herdr-bridge arm: --target must not be empty")
		return 2
	}
	// Capture the caller before writing the rule. The shared daemon may have
	// been started from a different pane (or without any pane environment).
	callerPane := os.Getenv("HERDR_PANE_ID")
	resolvedTarget := *target
	if resolvedTarget == "self" {
		if callerPane == "" {
			fmt.Fprintln(os.Stderr, "herdr-bridge arm: default target / --target self needs HERDR_PANE_ID; run inside a Herdr pane or specify --target <pane-id|agent-name>")
			return 1
		}
		resolvedTarget = callerPane
	}
	cfg := loadConfigOrDie(*configPath)
	state := NewState(cfg.StateDir)
	if err := state.EnsureDir(); err != nil {
		fmt.Fprintf(os.Stderr, "herdr-bridge: %v\n", err)
		return 1
	}

	path := RuntimeRulePath(cfg.StateDir, *name)
	if _, err := os.Stat(path); err == nil {
		if !*replace {
			fmt.Fprintf(os.Stderr, "herdr-bridge arm: rule %q already exists at %s (use --replace)\n", *name, path)
			return 1
		}
		// A rule armed from another pane belongs to that pane. Taking it over
		// silently would route that pane's notifications here instead.
		if owner, ok := readRuleOwner(path, *name); ok && owner != "" && owner != callerPane && !*force {
			fmt.Fprintf(os.Stderr, "herdr-bridge arm: rule %q is owned by pane %s; use a different --name, or --replace --force to take it over\n", *name, owner)
			return 1
		}
	}

	rule := Rule{
		Name:    *name,
		Target:  resolvedTarget,
		Owner:   callerPane,
		Mode:    *mode,
		Message: *message,
		Watch: Watch{
			Agents:     agents,
			Kinds:      kinds,
			Panes:      panes,
			Workspaces: workspaces,
			PaneExited: *paneExited,
		},
	}
	if len(statuses) > 0 {
		for _, s := range statuses {
			for _, part := range strings.Split(s, ",") {
				if p := strings.TrimSpace(part); p != "" {
					rule.Watch.Statuses = append(rule.Watch.Statuses, p)
				}
			}
		}
	}
	if *matchPane != "" || *matchRe != "" || *matchSub != "" {
		if *matchPane == "" {
			fmt.Fprintln(os.Stderr, "herdr-bridge arm: --match-regex/--match-substring need --match-pane")
			return 2
		}
		om := &OutputMatch{PaneID: *matchPane, Source: *matchSource}
		switch {
		case *matchRe != "" && *matchSub != "":
			fmt.Fprintln(os.Stderr, "herdr-bridge arm: use either --match-regex or --match-substring")
			return 2
		case *matchRe != "":
			om.Match = MatchSpec{Type: "regex", Value: *matchRe}
		default:
			om.Match = MatchSpec{Type: "substring", Value: *matchSub}
		}
		if *matchLines > 0 {
			lines := uint32(*matchLines)
			om.Lines = &lines
		}
		rule.Watch.OutputMatch = om
	}
	if *coalesce > 0 {
		rule.CoalesceMS = intPtr(*coalesce)
	}
	if *rate > 0 {
		rule.RateLimitPerMinute = intPtr(*rate)
	}
	cfg.applyDefaults(&rule)
	if err := rule.validate(); err != nil {
		fmt.Fprintf(os.Stderr, "herdr-bridge arm: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(state.RulesDir(), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "herdr-bridge: %v\n", err)
		return 1
	}
	if err := writeJSONAtomic(path, rule); err != nil {
		fmt.Fprintf(os.Stderr, "herdr-bridge: write rule: %v\n", err)
		return 1
	}
	b, _ := json.MarshalIndent(rule, "", "  ")
	fmt.Printf("%s\n", b)
	fmt.Printf("rule %q armed at %s\n", rule.Name, path)
	if pid := state.ReadPID(); ProcessAlive(pid) {
		fmt.Println("running bridge reloads rules within ~1s")
	} else {
		fmt.Println("no bridge is running; start one with: herdr-bridge start")
	}
	return 0
}

func cmdDisarm(args []string) int {
	fs := flag.NewFlagSet("disarm", flag.ContinueOnError)
	configPath := fs.String("config", DefaultConfigPath(), "config file path")
	name := fs.String("name", "", "rule name to remove")
	all := fs.Bool("all", false, "remove every runtime rule")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg := loadConfigOrDie(*configPath)
	state := NewState(cfg.StateDir)
	if *all {
		entries, err := os.ReadDir(state.RulesDir())
		if err != nil {
			fmt.Println("no runtime rules")
			return 0
		}
		removed := 0
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
				if err := os.Remove(filepath.Join(state.RulesDir(), e.Name())); err == nil {
					removed++
				}
			}
		}
		fmt.Printf("removed %d runtime rule(s)\n", removed)
		return 0
	}
	if *name == "" {
		fmt.Fprintln(os.Stderr, "herdr-bridge disarm: --name or --all is required")
		return 2
	}
	path := RuntimeRulePath(cfg.StateDir, *name)
	if err := os.Remove(path); err != nil {
		fmt.Fprintf(os.Stderr, "herdr-bridge disarm: %v\n", err)
		return 1
	}
	fmt.Printf("rule %q disarmed\n", *name)
	return 0
}

func cmdRules(args []string) int {
	fs := flag.NewFlagSet("rules", flag.ContinueOnError)
	configPath := fs.String("config", DefaultConfigPath(), "config file path")
	asJSON := fs.Bool("json", false, "print rules as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg := loadConfigOrDie(*configPath)
	rules, err := cfg.AllRules()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herdr-bridge: %v\n", err)
		return 1
	}
	if *asJSON {
		b, _ := json.MarshalIndent(rules, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	if len(rules) == 0 {
		fmt.Println("no rules configured")
		return 0
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].Name < rules[j].Name })
	for _, r := range rules {
		status := "enabled"
		if !r.EnabledNow() {
			status = "disabled"
		}
		owner := ""
		if r.Owner != "" {
			owner = " owner=" + r.Owner
		}
		fmt.Printf("%-20s %-8s mode=%-6s target=%-12s%s %s (%s)\n",
			r.Name, status, r.Mode, r.Target, owner, describeWatch(r.Watch), originLabel(r.Origin))
	}
	return 0
}

func originLabel(origin string) string {
	if origin == "config" {
		return "config"
	}
	return filepath.Base(origin)
}

func describeWatch(w Watch) string {
	parts := []string{}
	if len(w.Agents) > 0 {
		parts = append(parts, "agents="+strings.Join(w.Agents, "|"))
	}
	if len(w.Kinds) > 0 {
		parts = append(parts, "kinds="+strings.Join(w.Kinds, "|"))
	}
	if len(w.Panes) > 0 {
		parts = append(parts, "panes="+strings.Join(w.Panes, "|"))
	}
	if len(w.Workspaces) > 0 {
		parts = append(parts, "workspaces="+strings.Join(w.Workspaces, "|"))
	}
	if len(w.Statuses) > 0 {
		parts = append(parts, "statuses="+strings.Join(w.Statuses, "|"))
	}
	if w.OutputMatch != nil {
		parts = append(parts, fmt.Sprintf("output_match(%s,%s:%s)", w.OutputMatch.PaneID, w.OutputMatch.Match.Type, w.OutputMatch.Match.Value))
	}
	if w.PaneExited {
		parts = append(parts, "pane_exited")
	}
	return strings.Join(parts, " ")
}

func cmdLogs(args []string) int {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	configPath := fs.String("config", DefaultConfigPath(), "config file path")
	lines := fs.Int("lines", 50, "number of lines to print")
	follow := fs.Bool("f", false, "follow the log")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg := loadConfigOrDie(*configPath)
	state := NewState(cfg.StateDir)
	return tailLog(state.LogPath(), *lines, *follow)
}

func tailLog(path string, lines int, follow bool) int {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herdr-bridge: %v\n", err)
		return 1
	}
	all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if lines > 0 && len(all) > lines {
		all = all[len(all)-lines:]
	}
	if len(all) > 0 && all[0] != "" {
		fmt.Println(strings.Join(all, "\n"))
	}
	if !follow {
		return 0
	}
	offset := int64(len(data))
	for {
		time.Sleep(300 * time.Millisecond)
		f, err := os.Open(path)
		if err != nil {
			return 1
		}
		if info, err := f.Stat(); err == nil && info.Size() > offset {
			buf := make([]byte, info.Size()-offset)
			if _, err := f.ReadAt(buf, offset); err == nil {
				fmt.Print(string(buf))
			}
			offset = info.Size()
		}
		f.Close()
	}
}

// stringList collects repeatable flags.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			*s = append(*s, p)
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
