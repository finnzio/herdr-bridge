package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	tickInterval        = time.Second
	lifecycleRefreshLag = 400 * time.Millisecond
	maxBackoff          = 30 * time.Second
	defaultPollMS       = 10000
)

var errReload = errors.New("configuration changed")

// Plan is the concrete subscription set derived from the rules plus the
// selector resolution that produced it.
type Plan struct {
	Specs          []SubscriptionSpec
	WatchedPanes   map[string]bool            // panes with an agent_status subscription
	AgentRules     map[string]map[string]bool // rule name -> pane ids
	OutputRules    map[string]map[string]bool // rule name -> pane ids
	ExitedRules    []string
	ExitedPanes    map[string]map[string]bool // rule name -> pane ids (empty = any)
	ExitedWorktree map[string]map[string]bool // rule name -> workspace ids
	Key            string
}

// Engine owns the long-lived event stream: it resolves selectors to pane
// subscriptions, keeps them in sync with the live session, and hands matched
// events to the Waker. It never starts a model turn by itself.
type Engine struct {
	client *Client
	log    *Logger
	state  *State
	cfg    *Config
	waker  *Waker
	dryRun bool

	rules  []Rule
	agents map[string]AgentInfo
	panes  map[string]PaneInfo

	configMtimes map[string]time.Time
	startedAt    time.Time
	connected    bool

	// reloadCh lets a signal force an immediate config reload.
	reloadCh <-chan os.Signal

	markDirty bool
	dirtyAt   time.Time

	// preMatching marks panes whose configured output pattern already matched
	// when the subscription was created. Herdr reports that standing match as
	// the first event of a fresh subscription, which is old news.
	preMatching map[string]bool
}

func NewEngine(cfg *Config, client *Client, state *State, log *Logger, dryRun bool) *Engine {
	e := &Engine{
		client:       client,
		log:          log,
		state:        state,
		cfg:          cfg,
		dryRun:       dryRun,
		agents:       map[string]AgentInfo{},
		panes:        map[string]PaneInfo{},
		preMatching:  map[string]bool{},
		configMtimes: map[string]time.Time{},
		startedAt:    time.Now(),
	}
	e.waker = NewWaker(client, log, state, dryRun, func() *Config { return e.cfg })
	return e
}

// Run drives the reconnect/reload loop until the context is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	backoff := 500 * time.Millisecond
	if err := e.state.LoadKnown(); err != nil {
		e.log.Warn("could not load prior pane state", "err", err.Error())
	}
	go e.flushLoop(ctx)

	for {
		if ctx.Err() != nil {
			return nil
		}
		if err := e.reload(); err != nil {
			e.log.Error("config load failed", "err", err.Error())
			e.state.Update(func(st *Status) { st.LastError = err.Error() })
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			backoff = minDuration(backoff*2, maxBackoff)
			continue
		}
		err := e.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errReload) {
			backoff = 500 * time.Millisecond
			e.log.Info("reloading rules")
			continue
		}
		if err != nil {
			e.log.Warn("event stream ended", "err", err.Error())
			e.state.Update(func(st *Status) { st.LastError = err.Error() })
		} else {
			e.log.Info("event stream closed")
		}
		if e.connected {
			e.state.AddCounters(func(c *Counters) { c.Reconnects++ })
		}
		if !sleepCtx(ctx, backoff) {
			return nil
		}
		backoff = minDuration(backoff*2, maxBackoff)
	}
}

func (e *Engine) flushLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.state.Flush(false); err != nil {
				e.log.Warn("state flush failed", "err", err.Error())
			}
		}
	}
}

// reload reads the config and runtime rule files, noting their mtimes so a
// later change can be detected.
func (e *Engine) reload() error {
	cfg, err := LoadConfig(e.cfg.Path)
	if err != nil {
		return err
	}
	rules, err := cfg.AllRules()
	if err != nil {
		return err
	}
	e.cfg = cfg
	e.rules = rules
	e.client.SocketPath = cfg.SocketPathOr()
	e.client.Timeout = time.Duration(cfg.Defaults.RequestTimeoutMS) * time.Millisecond
	e.configMtimes = fileMTimes(ConfigFilesForWatch(cfg))
	e.log.Info("rules loaded", "count", len(rules), "socket", e.client.SocketPath)
	for _, r := range rules {
		e.log.Debug("rule", "name", r.Name, "target", r.Target, "mode", r.Mode, "origin", r.Origin)
	}
	names := make([]string, 0, len(rules))
	for _, r := range rules {
		if r.EnabledNow() {
			names = append(names, r.Name)
		}
	}
	e.state.Update(func(st *Status) {
		st.Rules = names
		st.Socket = e.client.SocketPath
		st.ConfigPath = e.cfg.Path
	})
	return nil
}

// session resolves selectors, subscribes, and pumps events until the stream
// ends, the rules change, or the topology moves enough to need a new plan.
func (e *Engine) session(ctx context.Context) error {
	plan, err := e.buildPlan()
	if err != nil {
		return err
	}
	if len(plan.Specs) == 0 {
		e.log.Warn("no active rules; waiting for configuration")
		return sleepCtxErr(ctx, 5*time.Second)
	}
	sub, err := e.client.Subscribe(plan.Specs)
	if err != nil {
		e.connected = false
		return fmt.Errorf("subscribe: %w", err)
	}
	defer sub.Close()

	reconnected := e.connected
	e.connected = true
	now := time.Now()
	e.state.Update(func(st *Status) {
		st.Subscriptions = len(plan.Specs)
		st.WatchedPanes = sortedKeys(plan.WatchedPanes)
		st.SubscribedAt = &now
		st.LastError = ""
	})
	e.log.Info("subscribed", "subscriptions", len(plan.Specs), "watched_panes", len(plan.WatchedPanes))

	if err := e.reconcile(plan); err != nil {
		e.log.Warn("state reconcile failed", "err", err.Error())
	}
	for _, c := range watchConflicts(e.rules, e.agents) {
		e.log.Warn("pane watched by rules with different targets",
			"pane", c.Pane, "watchers", strings.Join(c.Rules, ", "))
	}
	e.probePreMatching(plan)
	if reconnected && e.cfg.Defaults.ReconnectNotice != nil && *e.cfg.Defaults.ReconnectNotice {
		e.waker.NotifyBridge("herdr-bridge reconnected",
			fmt.Sprintf("Event stream resumed with %d subscriptions; worker states were re-checked.", len(plan.Specs)))
	}

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	lastTopology := time.Now()
	specKey := plan.Key

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-sub.Events:
			if !ok {
				if err := sub.Err(); err != nil {
					return err
				}
				return errors.New("subscription stream closed by server")
			}
			e.handleEvent(plan, ev)
		case <-ticker.C:
			select {
			case <-e.reloadCh:
				e.log.Info("reload requested by signal")
				return errReload
			default:
			}
			if e.configChanged() {
				return errReload
			}
			if e.markDirty && time.Since(e.dirtyAt) >= lifecycleRefreshLag {
				e.markDirty = false
				lastTopology = time.Now()
				rebuilt, key, err := e.refreshTopology()
				if err != nil {
					e.log.Debug("topology refresh failed", "err", err.Error())
				} else if key != specKey {
					e.log.Info("topology changed; resubscribing", "subscriptions", len(rebuilt.Specs))
					return nil
				}
			} else if time.Since(lastTopology) >= e.pollInterval() {
				lastTopology = time.Now()
				_, key, err := e.refreshTopology()
				if err != nil {
					e.log.Debug("periodic topology refresh failed", "err", err.Error())
				} else if key != specKey {
					e.log.Info("agent topology changed; resubscribing")
					return nil
				} else if err := e.reconcile(plan); err != nil {
					// State reported through integrations does not emit
					// subscription events, so poll the session as a fallback.
					e.log.Debug("periodic reconcile failed", "err", err.Error())
				}
			}
			if err := e.waker.Heartbeat(); err != nil {
				return err
			}
		}
	}
}

// buildPlan resolves every rule's selectors against the live session.
func (e *Engine) buildPlan() (*Plan, error) {
	if _, _, err := e.refreshTopology(); err != nil {
		return nil, err
	}
	plan := e.planFromRules()
	return plan, nil
}

// refreshTopology re-reads agents and panes and returns the recomputed plan
// and its key.
func (e *Engine) refreshTopology() (*Plan, string, error) {
	agents, err := e.client.ListAgents()
	if err != nil {
		return nil, "", err
	}
	panes, err := e.client.ListPanes()
	if err != nil {
		return nil, "", err
	}
	e.agents = make(map[string]AgentInfo, len(agents))
	for _, a := range agents {
		e.agents[a.PaneID] = a
	}
	e.panes = make(map[string]PaneInfo, len(panes))
	for _, p := range panes {
		e.panes[p.PaneID] = p
	}
	plan := e.planFromRules()
	return plan, plan.Key, nil
}

func (e *Engine) planFromRules() *Plan {
	plan := &Plan{
		WatchedPanes:   map[string]bool{},
		AgentRules:     map[string]map[string]bool{},
		OutputRules:    map[string]map[string]bool{},
		ExitedPanes:    map[string]map[string]bool{},
		ExitedWorktree: map[string]map[string]bool{},
	}
	seen := map[string]bool{}
	add := func(spec SubscriptionSpec) {
		b, err := json.Marshal(spec)
		if err != nil {
			return
		}
		key := string(b)
		if seen[key] {
			return
		}
		seen[key] = true
		plan.Specs = append(plan.Specs, spec)
	}

	for i := range e.rules {
		r := &e.rules[i]
		if !r.EnabledNow() {
			continue
		}
		w := &r.Watch
		for _, a := range e.agents {
			if !watchMatchesAgent(w, a) {
				continue
			}
			if len(w.Statuses) > 0 {
				if plan.AgentRules[r.Name] == nil {
					plan.AgentRules[r.Name] = map[string]bool{}
				}
				plan.AgentRules[r.Name][a.PaneID] = true
			}
			plan.WatchedPanes[a.PaneID] = true
			add(SubscriptionSpec{"type": "pane.agent_status_changed", "pane_id": a.PaneID})
		}
		if om := w.OutputMatch; om != nil {
			spec := SubscriptionSpec{
				"type":    "pane.output_matched",
				"pane_id": om.PaneID,
				"source":  om.Source,
				"match":   map[string]any{"type": om.Match.Type, "value": om.Match.Value},
			}
			if om.Lines != nil {
				spec["lines"] = *om.Lines
			}
			if om.StripANSI != nil {
				spec["strip_ansi"] = *om.StripANSI
			}
			add(spec)
			if plan.OutputRules[r.Name] == nil {
				plan.OutputRules[r.Name] = map[string]bool{}
			}
			plan.OutputRules[r.Name][om.PaneID] = true
		}
		if w.PaneExited {
			plan.ExitedRules = append(plan.ExitedRules, r.Name)
			if len(w.Panes) > 0 {
				plan.ExitedPanes[r.Name] = map[string]bool{}
				for _, p := range w.Panes {
					plan.ExitedPanes[r.Name][p] = true
				}
			}
			if len(w.Workspaces) > 0 {
				plan.ExitedWorktree[r.Name] = map[string]bool{}
				for _, ws := range w.Workspaces {
					plan.ExitedWorktree[r.Name][ws] = true
				}
			}
		}
	}

	if len(plan.ExitedRules) > 0 {
		add(SubscriptionSpec{"type": "pane.exited"})
	}
	// Lifecycle events drive selector re-resolution; they are global and rare.
	for _, t := range []string{
		"pane.created", "pane.closed", "pane.exited", "pane.moved",
		"pane.agent_detected", "workspace.created", "workspace.closed",
		"tab.created", "tab.closed",
	} {
		add(SubscriptionSpec{"type": t})
	}
	sort.Slice(plan.Specs, func(i, j int) bool {
		a, _ := json.Marshal(plan.Specs[i])
		b, _ := json.Marshal(plan.Specs[j])
		return string(a) < string(b)
	})
	sum := sha256.Sum256([]byte(mustJSON(plan.Specs)))
	plan.Key = hex.EncodeToString(sum[:8])
	return plan
}

// reconcile compares the persisted view of each watched pane with the live
// session, so a state change that happened while the bridge was down is
// reported exactly once.
func (e *Engine) reconcile(plan *Plan) error {
	live := map[string]bool{}
	for paneID := range e.panes {
		live[paneID] = true
	}
	if removed := e.state.Prune(live); removed > 0 {
		e.log.Debug("pruned stale pane state", "count", removed)
	}
	for paneID := range plan.WatchedPanes {
		a, ok := e.agents[paneID]
		if !ok {
			continue
		}
		if changed, existed := e.state.Observe(a.PaneID, a.AgentStatus, a.StateChangeSeq); changed && existed {
			e.log.Info("caught up missed state change",
				"pane", a.PaneID, "status", a.AgentStatus)
			e.dispatchAgentStatus(plan, a.PaneID, a.WorkspaceID, a.TabID, a.AgentStatus, a, true)
		}
	}
	return nil
}

func (e *Engine) handleEvent(plan *Plan, ev Event) {
	e.state.AddCounters(func(c *Counters) { c.Events++ })
	t := time.Now()
	e.state.Update(func(st *Status) { st.LastEventAt = &t })

	switch ev.Event {
	case "pane.agent_status_changed":
		var data struct {
			PaneID       string            `json:"pane_id"`
			WorkspaceID  string            `json:"workspace_id"`
			AgentStatus  string            `json:"agent_status"`
			Agent        *string           `json:"agent"`
			DisplayAgent *string           `json:"display_agent"`
			Title        *string           `json:"title"`
			StateLabels  map[string]string `json:"state_labels"`
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			e.log.Warn("bad pane.agent_status_changed payload", "err", err.Error())
			return
		}
		if !plan.WatchedPanes[data.PaneID] {
			return
		}
		changed, existed := e.state.Observe(data.PaneID, data.AgentStatus, 0)
		if !changed || !existed {
			return
		}
		info := e.agents[data.PaneID]
		info.PaneID = data.PaneID
		if info.WorkspaceID == "" {
			info.WorkspaceID = data.WorkspaceID
		}
		if info.Agent == nil {
			info.Agent = data.Agent
		}
		if info.DisplayAgent == nil {
			info.DisplayAgent = data.DisplayAgent
		}
		if info.Title == nil {
			info.Title = data.Title
		}
		e.dispatchAgentStatus(plan, data.PaneID, data.WorkspaceID, info.TabID, data.AgentStatus, info, false)

	case "pane.output_matched":
		var data struct {
			PaneID      string `json:"pane_id"`
			MatchedLine string `json:"matched_line"`
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			e.log.Warn("bad pane.output_matched payload", "err", err.Error())
			return
		}
		if e.preMatching[data.PaneID] {
			// The standing match at subscribe time was already in the buffer;
			// herdr reports it once, and it is not new information.
			delete(e.preMatching, data.PaneID)
			e.log.Info("ignoring output match that predates the subscription",
				"pane", data.PaneID, "line", data.MatchedLine)
			return
		}
		for ruleName, panes := range plan.OutputRules {
			if !panes[data.PaneID] {
				continue
			}
			rule := e.ruleByName(ruleName)
			if rule == nil {
				continue
			}
			ctx := &EventContext{
				Event:       ev.Event,
				Rule:        ruleName,
				PaneID:      data.PaneID,
				MatchedLine: data.MatchedLine,
				Time:        time.Now(),
				Raw:         ev.Data,
			}
			e.decorate(ctx)
			e.dispatch(rule, ctx)
		}

	case "pane.exited", "pane.closed":
		var data struct {
			PaneID      string `json:"pane_id"`
			WorkspaceID string `json:"workspace_id"`
		}
		_ = json.Unmarshal(ev.Data, &data)
		wsID := data.WorkspaceID
		if wsID == "" {
			if p, ok := e.panes[data.PaneID]; ok {
				wsID = p.WorkspaceID
			}
		}
		verb := "exited"
		if ev.Event == "pane.closed" {
			verb = "closed"
		}
		for _, ruleName := range plan.ExitedRules {
			if panes := plan.ExitedPanes[ruleName]; panes != nil && !panes[data.PaneID] {
				continue
			}
			if wss := plan.ExitedWorktree[ruleName]; wss != nil && !wss[wsID] {
				continue
			}
			rule := e.ruleByName(ruleName)
			if rule == nil {
				continue
			}
			ctx := &EventContext{
				Event:       ev.Event,
				Rule:        ruleName,
				PaneID:      data.PaneID,
				WorkspaceID: wsID,
				Extra:       map[string]string{"verb": verb},
				Time:        time.Now(),
				Raw:         ev.Data,
			}
			e.decorate(ctx)
			e.dispatch(rule, ctx)
		}
		e.markDirty = true
		e.dirtyAt = time.Now()

	case "pane.created", "pane.moved", "pane.agent_detected", "workspace.created",
		"workspace.closed", "tab.created", "tab.closed":
		e.markDirty = true
		e.dirtyAt = time.Now()
	}
}

func (e *Engine) dispatchAgentStatus(plan *Plan, paneID, workspaceID, tabID, status string, info AgentInfo, synthetic bool) {
	for ruleName, panes := range plan.AgentRules {
		if !panes[paneID] {
			continue
		}
		rule := e.ruleByName(ruleName)
		if rule == nil || !statusAllowed(rule.Watch.Statuses, status) {
			continue
		}
		ctx := &EventContext{
			Event:        "pane.agent_status_changed",
			Rule:         ruleName,
			PaneID:       paneID,
			WorkspaceID:  workspaceID,
			TabID:        tabID,
			Status:       status,
			AgentKind:    info.Kind(),
			AgentName:    info.AgentName(),
			DisplayAgent: ptrString(info.DisplayAgent),
			Title:        info.TitleText(),
			Synthetic:    synthetic,
			Time:         time.Now(),
		}
		e.dispatch(rule, ctx)
	}
}

func (e *Engine) dispatch(rule *Rule, ctx *EventContext) {
	e.state.AddCounters(func(c *Counters) { c.Matched++ })
	if e.isSelfLoop(rule, ctx) {
		e.log.Warn("dropping self-referential event", "rule", rule.Name, "target", rule.Target, "pane", ctx.PaneID)
		return
	}
	e.log.Info("event matched",
		"rule", rule.Name, "event", ctx.Event, "pane", ctx.PaneID,
		"status", ctx.Status, "target", rule.Target, "synthetic", ctx.Synthetic)
	e.waker.Enqueue(rule, ctx)
}

// isSelfLoop reports whether the event source is the rule's own wake target,
// which would otherwise let the bridge wake itself forever.
func (e *Engine) isSelfLoop(rule *Rule, ctx *EventContext) bool {
	target := rule.Target
	if target == "self" {
		target = os.Getenv("HERDR_PANE_ID")
	}
	if target == "" {
		return false
	}
	if target == ctx.PaneID {
		return true
	}
	for _, a := range e.agents {
		if a.AgentName() == target && a.PaneID == ctx.PaneID {
			return true
		}
	}
	return false
}

func (e *Engine) ruleByName(name string) *Rule {
	for i := range e.rules {
		if e.rules[i].Name == name {
			return &e.rules[i]
		}
	}
	return nil
}

// decorate fills in labels looked up from the live session.
func (e *Engine) decorate(ctx *EventContext) {
	if a, ok := e.agents[ctx.PaneID]; ok {
		ctx.AgentKind = a.Kind()
		ctx.AgentName = a.AgentName()
		ctx.DisplayAgent = ptrString(a.DisplayAgent)
		if ctx.Title == "" {
			ctx.Title = a.TitleText()
		}
		if ctx.WorkspaceID == "" {
			ctx.WorkspaceID = a.WorkspaceID
		}
		if ctx.TabID == "" {
			ctx.TabID = a.TabID
		}
	}
	if p, ok := e.panes[ctx.PaneID]; ok {
		if ctx.AgentKind == "" && p.Agent != nil {
			ctx.AgentKind = *p.Agent
		}
		if ctx.Title == "" && p.TerminalTitle != nil {
			ctx.Title = *p.TerminalTitle
		}
		if ctx.WorkspaceID == "" {
			ctx.WorkspaceID = p.WorkspaceID
		}
		if ctx.TabID == "" {
			ctx.TabID = p.TabID
		}
	}
}

// probePreMatching records whether each output-matched pane already matches at
// subscription time, so the first event of a fresh subscription can be
// recognised as pre-existing output instead of a new signal.
func (e *Engine) probePreMatching(plan *Plan) {
	e.preMatching = map[string]bool{}
	seen := map[string]bool{}
	for i := range e.rules {
		r := &e.rules[i]
		om := r.Watch.OutputMatch
		if !r.EnabledNow() || om == nil || om.NotifyPreexisting || seen[om.PaneID] {
			continue
		}
		seen[om.PaneID] = true
		text, err := e.client.ReadPane(om.PaneID, om.Source, om.Lines, om.StripANSI)
		if err != nil {
			e.log.Debug("pre-existing match probe failed", "pane", om.PaneID, "err", err.Error())
			continue
		}
		if matched, line := matchLines(text, om.Match); matched {
			e.preMatching[om.PaneID] = true
			e.log.Info("pane already matches at subscription time",
				"pane", om.PaneID, "rule", r.Name, "line", line)
		}
	}
}

// matchLines mirrors herdr's own matcher: the first line that matches wins.
func matchLines(text string, spec MatchSpec) (bool, string) {
	if spec.Type == "regex" {
		re, err := regexp.Compile(spec.Value)
		if err != nil {
			return false, ""
		}
		for _, line := range strings.Split(text, "\n") {
			if re.MatchString(line) {
				return true, line
			}
		}
		return false, ""
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, spec.Value) {
			return true, line
		}
	}
	return false, ""
}

// Heartbeat checks the server is still answering on a fresh connection; a
// stale subscription socket cannot be detected from an idle stream alone.
func (e *Engine) Heartbeat() error {
	if err := e.client.Ping(); err != nil {
		return fmt.Errorf("server unreachable: %w", err)
	}
	return nil
}

// pollInterval is how often selectors and agent states are re-checked on top
// of the event stream.
func (e *Engine) pollInterval() time.Duration {
	ms := e.cfg.Defaults.PollIntervalMS
	if ms <= 0 {
		ms = defaultPollMS
	}
	return time.Duration(ms) * time.Millisecond
}

// configChanged reports whether a watched file appeared, disappeared, or was
// modified since the last reload. A file that never existed is not a change:
// the config file is optional, and treating its absence as a change made the
// daemon reload on every tick.
func (e *Engine) configChanged() bool {
	files := ConfigFilesForWatch(e.cfg)
	current := make(map[string]bool, len(files))
	for _, f := range files {
		current[f] = true
		info, err := os.Stat(f)
		mt, watched := e.configMtimes[f]
		switch {
		case err != nil:
			if watched {
				return true
			}
		case !watched || !info.ModTime().Equal(mt):
			return true
		}
	}
	// A watched file that no longer shows up in the listing was removed.
	for f := range e.configMtimes {
		if !current[f] {
			return true
		}
	}
	return false
}

func fileMTimes(files []string) map[string]time.Time {
	out := make(map[string]time.Time, len(files))
	for _, f := range files {
		if info, err := os.Stat(f); err == nil {
			out[f] = info.ModTime()
		}
	}
	return out
}

// watchConflict is one pane that several rules watch with different wake
// targets, so a single event would wake more than one initiator.
type watchConflict struct {
	Pane  string
	Rules []string // "<rule> -> <target>", sorted
}

// watchConflicts lists panes watched by more than one enabled rule whose
// resolved targets differ. It is the signal that two initiators grabbed the
// same worker; the fix is one owner per pane.
func watchConflicts(rules []Rule, agents map[string]AgentInfo) []watchConflict {
	byName := map[string]string{}
	for _, a := range agents {
		if n := a.AgentName(); n != "" {
			byName[n] = a.PaneID
		}
	}
	resolve := func(target string) string {
		if target == "self" {
			return "self"
		}
		if paneIDRe.MatchString(target) {
			return target
		}
		if pane, ok := byName[target]; ok {
			return pane
		}
		return target
	}

	entries := map[string][]string{}
	targets := map[string]map[string]bool{}
	for i := range rules {
		r := &rules[i]
		if !r.EnabledNow() {
			continue
		}
		for _, a := range agents {
			if !watchMatchesAgent(&r.Watch, a) {
				continue
			}
			entries[a.PaneID] = append(entries[a.PaneID], fmt.Sprintf("%s -> %s", r.Name, r.Target))
			if targets[a.PaneID] == nil {
				targets[a.PaneID] = map[string]bool{}
			}
			targets[a.PaneID][resolve(r.Target)] = true
		}
	}

	out := []watchConflict{}
	for pane, set := range targets {
		if len(set) < 2 {
			continue
		}
		watchers := entries[pane]
		sort.Strings(watchers)
		out = append(out, watchConflict{Pane: pane, Rules: watchers})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pane < out[j].Pane })
	return out
}

func watchMatchesAgent(w *Watch, a AgentInfo) bool {
	if len(w.Workspaces) > 0 && !containsString(w.Workspaces, a.WorkspaceID) {
		return false
	}
	if len(w.Panes) > 0 && !containsString(w.Panes, a.PaneID) {
		return false
	}
	if len(w.Agents) == 0 && len(w.Kinds) == 0 {
		// Selector was a pane or workspace list.
		return len(w.Panes) > 0 || len(w.Workspaces) > 0
	}
	if len(w.Agents) > 0 {
		name := a.AgentName()
		for _, pattern := range w.Agents {
			if name != "" && globMatch(pattern, name) {
				return true
			}
		}
	}
	if len(w.Kinds) > 0 {
		for _, kind := range w.Kinds {
			if a.Kind() == kind {
				return true
			}
		}
	}
	return false
}

func globMatch(pattern, value string) bool {
	if pattern == value {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return false
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(value, parts[0]) {
		return false
	}
	value = value[len(parts[0]):]
	for i := 1; i < len(parts)-1; i++ {
		idx := strings.Index(value, parts[i])
		if idx < 0 {
			return false
		}
		value = value[idx+len(parts[i]):]
	}
	return strings.HasSuffix(value, parts[len(parts)-1])
}

func statusAllowed(statuses []string, status string) bool {
	for _, s := range statuses {
		if s == status {
			return true
		}
	}
	return false
}

func containsString(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func ptrString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	return sleepCtxErr(ctx, d) == nil
}

func sleepCtxErr(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
