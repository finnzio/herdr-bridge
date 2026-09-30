package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// EventContext carries one matched event into message rendering.
type EventContext struct {
	Event        string
	Rule         string
	PaneID       string
	WorkspaceID  string
	TabID        string
	Status       string
	AgentKind    string
	AgentName    string
	DisplayAgent string
	Title        string
	MatchedLine  string
	Notice       string
	Extra        map[string]string
	Synthetic    bool
	Time         time.Time
	Raw          json.RawMessage
}

func (c *EventContext) label() string {
	for _, s := range []string{c.AgentName, c.DisplayAgent, c.AgentKind} {
		if s != "" {
			return s
		}
	}
	return "agent"
}

// summary renders the default one-line description of an event.
func (c *EventContext) summary() string {
	switch {
	case c.Notice != "":
		return c.Notice
	case c.Event == "pane.output_matched":
		line := strings.TrimSpace(c.MatchedLine)
		if line == "" {
			line = "(matched)"
		}
		return fmt.Sprintf("pane %s output matched: %s", c.PaneID, line)
	case c.Event == "pane.exited" || c.Event == "pane.closed":
		verb := c.Extra["verb"]
		if verb == "" {
			verb = "exited"
		}
		return fmt.Sprintf("pane %s %s", c.PaneID, verb)
	default:
		s := fmt.Sprintf("pane %s (%s) status: %s", c.PaneID, c.label(), c.Status)
		if c.Title != "" {
			s += " - " + c.Title
		}
		if c.Synthetic {
			s += " [detected by state comparison, no event was emitted]"
		}
		return s
	}
}

func (c *EventContext) placeholders() map[string]string {
	vals := map[string]string{
		"event":         c.Event,
		"summary":       c.summary(),
		"pane_id":       c.PaneID,
		"workspace_id":  c.WorkspaceID,
		"tab_id":        c.TabID,
		"status":        c.Status,
		"agent":         c.AgentKind,
		"name":          c.AgentName,
		"display_agent": c.DisplayAgent,
		"title":         c.Title,
		"matched_line":  c.MatchedLine,
		"time":          c.Time.Format(time.RFC3339),
		"rule":          c.Rule,
	}
	if c.Notice != "" {
		vals["summary"] = c.Notice
	}
	return vals
}

// renderMessage expands {placeholders} in a rule message. Unknown tokens are
// rejected at config validation time; anything left here is preserved.
func renderMessage(tmpl string, ctx *EventContext, extra map[string]string) string {
	vals := ctx.placeholders()
	for k, v := range extra {
		vals[k] = v
	}
	return placeholderRe.ReplaceAllStringFunc(tmpl, func(match string) string {
		key := match[1 : len(match)-1]
		if v, ok := vals[key]; ok {
			return v
		}
		return match
	})
}

type pendingItem struct {
	rule *Rule
	ctx  *EventContext
	text string
}

type targetQueue struct {
	target     string
	items      []pendingItem
	timer      *time.Timer
	delivering bool
	lastText   string
	lastAt     time.Time
}

// Waker delivers matched events to their target pane. Delivery happens on a
// per-target queue so several events that land together become one wake-up,
// and so a target is never prompted twice concurrently.
type Waker struct {
	client *Client
	log    *Logger
	state  *State
	dryRun bool
	cfgFn  func() *Config

	mu         sync.Mutex
	queues     map[string]*targetQueue
	buckets    map[string]*bucket
	suppressed map[string]int
}

func NewWaker(client *Client, log *Logger, state *State, dryRun bool, cfgFn func() *Config) *Waker {
	return &Waker{
		client:     client,
		log:        log,
		state:      state,
		dryRun:     dryRun,
		cfgFn:      cfgFn,
		queues:     map[string]*targetQueue{},
		buckets:    map[string]*bucket{},
		suppressed: map[string]int{},
	}
}

// Enqueue adds a matched event to its target queue, starting the coalescing
// window on the first item.
func (w *Waker) Enqueue(rule *Rule, ctx *EventContext) {
	text := renderMessage(rule.Message, ctx, map[string]string{
		"count":      "1",
		"events":     ctx.summary(),
		"suppressed": "0",
	})
	item := pendingItem{rule: rule, ctx: ctx, text: text}

	w.mu.Lock()
	defer w.mu.Unlock()
	q := w.queues[rule.Target]
	if q == nil {
		q = &targetQueue{target: rule.Target}
		w.queues[rule.Target] = q
	}
	q.items = append(q.items, item)
	if q.timer == nil && !q.delivering {
		q.timer = time.AfterFunc(coalesceWindow(rule), func() { w.flush(rule.Target) })
	}
}

func coalesceWindow(rule *Rule) time.Duration {
	ms := defaultCoalesceMS
	if rule.CoalesceMS != nil {
		ms = *rule.CoalesceMS
	}
	if ms <= 0 {
		return time.Millisecond
	}
	return time.Duration(ms) * time.Millisecond
}

func (w *Waker) flush(target string) {
	w.mu.Lock()
	q := w.queues[target]
	if q == nil || len(q.items) == 0 || q.delivering {
		w.mu.Unlock()
		return
	}
	items := q.items
	q.items = nil
	if q.timer != nil {
		q.timer.Stop()
		q.timer = nil
	}
	q.delivering = true
	w.mu.Unlock()

	w.deliver(q, items)

	w.mu.Lock()
	q.delivering = false
	if len(q.items) > 0 && q.timer == nil {
		q.timer = time.AfterFunc(coalesceWindow(q.items[0].rule), func() { w.flush(target) })
	}
	w.mu.Unlock()
}

func (w *Waker) deliver(q *targetQueue, items []pendingItem) {
	rule := items[0].rule

	w.mu.Lock()
	suppressed := w.suppressed[rule.Target]
	w.suppressed[rule.Target] = 0
	w.mu.Unlock()

	extra := map[string]string{
		"count":      fmt.Sprint(len(items)),
		"events":     joinSummaries(items),
		"suppressed": fmt.Sprint(suppressed),
	}
	seen := map[string]bool{}
	lines := make([]string, 0, len(items))
	for _, it := range items {
		text := renderMessage(it.rule.Message, it.ctx, extra)
		if seen[text] {
			continue
		}
		seen[text] = true
		lines = append(lines, text)
	}
	text := strings.Join(lines, "\n")
	if len(lines) > 1 {
		text = fmt.Sprintf("herdr-bridge: %d events\n%s", len(items), text)
	}
	if suppressed > 0 {
		text += fmt.Sprintf("\n(+%d event(s) suppressed by rate limit)", suppressed)
	}

	if !w.allow(rule) {
		w.mu.Lock()
		w.suppressed[rule.Target] += len(items)
		w.mu.Unlock()
		w.log.Warn("rate limit reached; event suppressed",
			"target", rule.Target, "rule", rule.Name, "events", len(items))
		w.state.AddCounters(func(c *Counters) { c.Suppressed += len(items) })
		return
	}

	w.mu.Lock()
	dup := text == q.lastText && time.Since(q.lastAt) < 2*time.Second
	q.lastText = text
	q.lastAt = time.Now()
	w.mu.Unlock()
	if dup {
		w.log.Debug("duplicate delivery suppressed", "target", rule.Target)
		return
	}

	record := &DeliveryRecord{At: time.Now(), Rule: rule.Name, Target: rule.Target, Mode: rule.Mode, Text: text}
	err := w.send(rule, text)
	if err != nil {
		record.Error = err.Error()
		w.log.Error("delivery failed", "target", rule.Target, "rule", rule.Name, "err", err.Error())
		w.state.AddCounters(func(c *Counters) { c.Failed++ })
	} else {
		w.log.Info("delivered", "target", rule.Target, "rule", rule.Name, "mode", rule.Mode, "text", text)
		w.state.AddCounters(func(c *Counters) { c.Delivered++ })
	}
	w.state.Update(func(st *Status) { st.LastDelivery = record })
}

func joinSummaries(items []pendingItem) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, it.ctx.summary())
	}
	return strings.Join(parts, "; ")
}

// send performs the actual wake-up. Nothing here talks to a language model:
// prompt mode only hands text to the target pane's terminal.
func (w *Waker) send(rule *Rule, text string) error {
	cfg := w.cfgFn()
	if rule.Mode == modeNotify {
		if w.dryRun {
			return nil
		}
		return w.client.Notify("herdr-bridge", text, "")
	}

	target, err := w.resolve(rule.Target)
	if err != nil {
		return err
	}
	if w.dryRun {
		w.log.Info("dry run: would deliver", "mode", rule.Mode, "pane", target.PaneID, "text", text)
		return nil
	}

	switch rule.Mode {
	case modeType:
		if err := w.client.SendText(target.PaneID, text); err != nil {
			return fmt.Errorf("send_text: %w", err)
		}
		return w.client.SendKeys(target.PaneID, []string{"enter"})
	default:
		return w.promptAgent(rule, target, text, cfg)
	}
}

func (w *Waker) promptAgent(rule *Rule, target *resolvedTarget, text string, cfg *Config) error {
	if target.Agent == nil {
		return fmt.Errorf("target pane %s hosts no recognized agent; use mode=type for raw terminals", target.PaneID)
	}
	switch target.Agent.AgentStatus {
	case "blocked":
		msg := fmt.Sprintf("%s is waiting for interactive input; event not delivered (%s)", target.PaneID, text)
		w.log.Warn("target blocked; delivery skipped", "target", target.PaneID, "rule", rule.Name)
		if cfg.Defaults.NotifyOnBlocked != nil && *cfg.Defaults.NotifyOnBlocked {
			if err := w.client.Notify("herdr-bridge: target blocked", msg, "request"); err != nil {
				w.log.Debug("blocked notification failed", "err", err.Error())
			}
		}
		return fmt.Errorf("target %s is blocked", target.PaneID)
	case "working":
		timeoutMS := defaultMaxWaitMS
		if rule.MaxWaitTargetMS != nil {
			timeoutMS = *rule.MaxWaitTargetMS
		}
		w.log.Info("target busy; waiting for it to settle", "target", target.PaneID, "timeout_ms", timeoutMS)
		err := w.client.WaitAgent(target.PaneID, []string{"idle", "done", "blocked"}, timeoutMS)
		fresh, rerr := w.resolve(rule.Target)
		if rerr == nil {
			target = fresh
		}
		if err != nil {
			return fmt.Errorf("target did not settle before delivery: %w", err)
		}
		if target.Agent != nil && target.Agent.AgentStatus == "blocked" {
			return fmt.Errorf("target %s became blocked while waiting", target.PaneID)
		}
	}
	if err := w.client.PromptAgent(target.PaneID, text); err != nil {
		return fmt.Errorf("agent.prompt: %w", err)
	}
	return nil
}

type resolvedTarget struct {
	PaneID string
	Agent  *AgentInfo
}

var paneIDRe = regexp.MustCompile(`^[A-Za-z0-9_]+:p[A-Za-z0-9]+$`)

// resolve maps a rule target (pane id, live agent name, or "self") to a pane.
func (w *Waker) resolve(target string) (*resolvedTarget, error) {
	if target == "self" {
		target = os.Getenv("HERDR_PANE_ID")
		if target == "" {
			return nil, fmt.Errorf("rule target is \"self\" but HERDR_PANE_ID is not set")
		}
	}
	agents, err := w.client.ListAgents()
	if err != nil {
		return nil, fmt.Errorf("agent.list: %w", err)
	}
	if paneIDRe.MatchString(target) {
		for i := range agents {
			if agents[i].PaneID == target {
				return &resolvedTarget{PaneID: target, Agent: &agents[i]}, nil
			}
		}
		panes, err := w.client.ListPanes()
		if err != nil {
			return nil, fmt.Errorf("pane.list: %w", err)
		}
		for i := range panes {
			if panes[i].PaneID == target {
				return &resolvedTarget{PaneID: target}, nil
			}
		}
		return nil, fmt.Errorf("target pane %s does not exist", target)
	}
	for i := range agents {
		if agents[i].AgentName() == target {
			return &resolvedTarget{PaneID: agents[i].PaneID, Agent: &agents[i]}, nil
		}
		if agents[i].PaneID == target {
			return &resolvedTarget{PaneID: agents[i].PaneID, Agent: &agents[i]}, nil
		}
	}
	return nil, fmt.Errorf("target %q matches no live agent name or pane id", target)
}

// NotifyBridge wakes the configured notify target with a bridge-level notice
// (reconnects, and other events that are not tied to a worker).
func (w *Waker) NotifyBridge(title, body string) {
	cfg := w.cfgFn()
	if cfg.NotifyTarget == "" {
		w.log.Info("bridge notice (no notify_target configured)", "title", title, "body", body)
		return
	}
	rule := &Rule{
		Name:               "bridge-notice",
		Target:             cfg.NotifyTarget,
		Mode:               modePrompt,
		Message:            "{summary}",
		CoalesceMS:         intPtr(cfg.Defaults.CoalesceMS),
		RateLimitPerMinute: intPtr(cfg.Defaults.RateLimitPerMinute),
		MaxWaitTargetMS:    intPtr(cfg.Defaults.MaxWaitTargetMS),
	}
	ctx := &EventContext{
		Event:  "bridge.notice",
		Rule:   "bridge-notice",
		Notice: fmt.Sprintf("%s: %s", title, body),
		Time:   time.Now(),
	}
	w.Enqueue(rule, ctx)
}

// Heartbeat is called on the engine tick; it currently only reports queue
// depth so a stuck delivery is visible in the log.
func (w *Waker) Heartbeat() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	for target, q := range w.queues {
		if len(q.items) > 0 && q.timer == nil && !q.delivering {
			q.timer = time.AfterFunc(coalesceWindow(q.items[0].rule), func() { w.flush(target) })
		}
	}
	return nil
}

type bucket struct {
	tokens float64
	last   time.Time
}

func (w *Waker) allow(rule *Rule) bool {
	rate := defaultRateLimit
	if rule.RateLimitPerMinute != nil {
		rate = *rule.RateLimitPerMinute
	}
	if rate <= 0 {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	b := w.buckets[rule.Target]
	if b == nil {
		b = &bucket{tokens: float64(rate), last: time.Now()}
		w.buckets[rule.Target] = b
	}
	now := time.Now()
	elapsed := now.Sub(b.last).Minutes()
	if elapsed > 0 {
		b.tokens = math.Min(float64(rate), b.tokens+elapsed*float64(rate))
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}
