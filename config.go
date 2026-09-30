package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// MatchSpec is a literal or regex matcher for pane output watches.
type MatchSpec struct {
	Type  string `json:"type"` // substring | regex
	Value string `json:"value"`
}

// OutputMatch watches one pane's rendered output.
type OutputMatch struct {
	PaneID    string    `json:"pane_id"`
	Source    string    `json:"source"` // visible | recent | recent-unwrapped | detection
	Lines     *uint32   `json:"lines,omitempty"`
	Match     MatchSpec `json:"match"`
	StripANSI *bool     `json:"strip_ansi,omitempty"`
	// NotifyPreexisting delivers the match that already exists in the pane
	// buffer when the bridge (re)subscribes. Off by default so a restart does
	// not wake the target with old output.
	NotifyPreexisting bool `json:"notify_preexisting,omitempty"`
}

// Watch selects what a rule reacts to.
//
// Agents and kinds are alternative selectors; panes and workspaces restrict
// that selection, and statuses filter transitions. A rule with an output_match
// or pane_exited watch needs no agent selector.
type Watch struct {
	Agents      []string     `json:"agents,omitempty"`     // agent-name globs, e.g. "worker-*"
	Kinds       []string     `json:"kinds,omitempty"`      // agent kinds, e.g. "codex"
	Panes       []string     `json:"panes,omitempty"`      // exact pane ids, e.g. "w1:p2"
	Workspaces  []string     `json:"workspaces,omitempty"` // exact workspace ids
	Statuses    []string     `json:"statuses,omitempty"`   // agent statuses of interest
	OutputMatch *OutputMatch `json:"output_match,omitempty"`
	PaneExited  bool         `json:"pane_exited,omitempty"`
}

// Rule turns matching events into one delivered message for a target.
type Rule struct {
	Name               string `json:"name"`
	Enabled            *bool  `json:"enabled,omitempty"`
	Watch              Watch  `json:"watch"`
	Target             string `json:"target"`            // pane id, live agent name, or "self"
	Owner              string `json:"owner,omitempty"`   // pane that armed the rule; guards take-over
	Mode               string `json:"mode,omitempty"`    // prompt | type | notify
	Message            string `json:"message,omitempty"` // template
	CoalesceMS         *int   `json:"coalesce_ms,omitempty"`
	RateLimitPerMinute *int   `json:"rate_limit_per_minute,omitempty"`
	MaxWaitTargetMS    *int   `json:"max_wait_target_ms,omitempty"`
	IncludeEventJSON   bool   `json:"include_event_json,omitempty"`

	// Origin is set at load time ("config" or the runtime rule file path).
	Origin string `json:"-"`
}

// Defaults are applied to rules that omit a value.
type Defaults struct {
	Message            string `json:"message,omitempty"`
	CoalesceMS         int    `json:"coalesce_ms,omitempty"`
	RateLimitPerMinute int    `json:"rate_limit_per_minute,omitempty"`
	MaxWaitTargetMS    int    `json:"max_wait_target_ms,omitempty"`
	RequestTimeoutMS   int    `json:"request_timeout_ms,omitempty"`
	PollIntervalMS     int    `json:"poll_interval_ms,omitempty"`
	ReconnectNotice    *bool  `json:"reconnect_notice,omitempty"`
	NotifyOnBlocked    *bool  `json:"notify_on_blocked,omitempty"`
}

// Config is the on-disk bridge configuration.
type Config struct {
	SocketPath   string   `json:"socket_path,omitempty"`
	Session      string   `json:"session,omitempty"`
	StateDir     string   `json:"state_dir,omitempty"`
	LogLevel     string   `json:"log_level,omitempty"`
	NotifyTarget string   `json:"notify_target,omitempty"`
	Defaults     Defaults `json:"defaults"`
	Rules        []Rule   `json:"rules"`

	Path string `json:"-"`
}

const (
	defaultMessage     = "herdr-bridge: {summary}"
	defaultCoalesceMS  = 1200
	defaultRateLimit   = 6
	defaultMaxWaitMS   = 600000
	defaultReqTimeout  = 60000
	defaultLogLevel    = "info"
	modePrompt         = "prompt"
	modeType           = "type"
	modeNotify         = "notify"
	defaultStatusesCSV = "done,blocked"
)

var validStatuses = map[string]bool{
	"done": true, "idle": true, "working": true, "blocked": true, "unknown": true,
}

// The socket API spells the read source "recent_unwrapped"; the CLI wrapper
// accepts "recent-unwrapped". Both are accepted here and normalized to the
// wire form.
var validSources = map[string]bool{
	"visible": true, "recent": true, "recent_unwrapped": true, "detection": true,
}

// Template placeholders available in rule messages.
var knownPlaceholders = map[string]bool{
	"event": true, "summary": true, "pane_id": true, "workspace_id": true,
	"tab_id": true, "status": true, "agent": true, "name": true, "title": true,
	"display_agent": true, "matched_line": true, "time": true, "count": true,
	"events": true, "suppressed": true, "rule": true,
}

var placeholderRe = regexp.MustCompile(`\{([a-z_]+)\}`)

func boolPtr(v bool) *bool { return &v }
func intPtr(v int) *int    { return &v }

// LoadConfig reads the config file, fills defaults, and validates it. A
// missing file is not an error: rules can live entirely in runtime rule files,
// which is how `herdr-bridge arm` is used without any config.
func LoadConfig(path string) (*Config, error) {
	cfg := &Config{Path: path}
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := json.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("parse config %s: %w", path, err)
			}
		case errors.Is(err, os.ErrNotExist):
			// No config file: use defaults.
		default:
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
	}
	cfg.Path = path
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) normalize() error {
	if c.SocketPath == "" && c.Session == "" {
		c.SocketPath = DefaultSocketPath()
	}
	if c.StateDir == "" {
		c.StateDir = DefaultStateDir()
	}
	c.StateDir = ExpandPath(c.StateDir)
	if c.LogLevel == "" {
		c.LogLevel = defaultLogLevel
	}
	d := &c.Defaults
	if d.Message == "" {
		d.Message = defaultMessage
	}
	if d.CoalesceMS <= 0 {
		d.CoalesceMS = defaultCoalesceMS
	}
	if d.RateLimitPerMinute <= 0 {
		d.RateLimitPerMinute = defaultRateLimit
	}
	if d.MaxWaitTargetMS <= 0 {
		d.MaxWaitTargetMS = defaultMaxWaitMS
	}
	if d.RequestTimeoutMS <= 0 {
		d.RequestTimeoutMS = defaultReqTimeout
	}
	if d.PollIntervalMS <= 0 {
		d.PollIntervalMS = defaultPollMS
	}
	if d.ReconnectNotice == nil {
		d.ReconnectNotice = boolPtr(true)
	}
	if d.NotifyOnBlocked == nil {
		d.NotifyOnBlocked = boolPtr(true)
	}
	seen := map[string]bool{}
	for i := range c.Rules {
		r := &c.Rules[i]
		r.Origin = "config"
		c.applyDefaults(r)
		if err := r.validate(); err != nil {
			return err
		}
		if seen[r.Name] {
			return fmt.Errorf("duplicate rule name %q", r.Name)
		}
		seen[r.Name] = true
	}
	return nil
}

func (c *Config) applyDefaults(r *Rule) {
	if r.Mode == "" {
		r.Mode = modePrompt
	}
	if r.Message == "" {
		r.Message = c.Defaults.Message
	}
	if r.CoalesceMS == nil {
		r.CoalesceMS = intPtr(c.Defaults.CoalesceMS)
	}
	if r.RateLimitPerMinute == nil {
		r.RateLimitPerMinute = intPtr(c.Defaults.RateLimitPerMinute)
	}
	if r.MaxWaitTargetMS == nil {
		r.MaxWaitTargetMS = intPtr(c.Defaults.MaxWaitTargetMS)
	}
	if len(r.Watch.Statuses) == 0 {
		r.Watch.Statuses = strings.Split(defaultStatusesCSV, ",")
	}
}

func (r *Rule) EnabledNow() bool { return r.Enabled == nil || *r.Enabled }

func (r *Rule) validate() error {
	if r.Name == "" {
		return fmt.Errorf("rule is missing a name")
	}
	if r.Target == "" {
		return fmt.Errorf("rule %q is missing a target", r.Name)
	}
	switch r.Mode {
	case modePrompt, modeType, modeNotify:
	default:
		return fmt.Errorf("rule %q has unknown mode %q (want prompt, type, or notify)", r.Name, r.Mode)
	}
	w := &r.Watch
	if len(w.Agents) == 0 && len(w.Kinds) == 0 && len(w.Panes) == 0 && w.OutputMatch == nil && !w.PaneExited {
		return fmt.Errorf("rule %q selects nothing: set watch.agents, watch.kinds, watch.panes, watch.output_match, or watch.pane_exited", r.Name)
	}
	for _, s := range w.Statuses {
		if !validStatuses[s] {
			return fmt.Errorf("rule %q has unknown status %q", r.Name, s)
		}
	}
	if om := w.OutputMatch; om != nil {
		if om.PaneID == "" {
			return fmt.Errorf("rule %q output_match needs pane_id", r.Name)
		}
		if om.Source == "" {
			om.Source = "recent_unwrapped"
		}
		om.Source = strings.ReplaceAll(om.Source, "-", "_")
		if !validSources[om.Source] {
			return fmt.Errorf("rule %q output_match has unknown source %q", r.Name, om.Source)
		}
		if om.Match.Value == "" {
			return fmt.Errorf("rule %q output_match needs match.value", r.Name)
		}
		switch om.Match.Type {
		case "substring":
		case "regex":
			if _, err := regexp.Compile(om.Match.Value); err != nil {
				return fmt.Errorf("rule %q output_match regex: %w", r.Name, err)
			}
		default:
			return fmt.Errorf("rule %q output_match match.type must be substring or regex", r.Name)
		}
	}
	for _, token := range placeholderRe.FindAllStringSubmatch(r.Message, -1) {
		if !knownPlaceholders[token[1]] {
			return fmt.Errorf("rule %q message uses unknown placeholder {%s}", r.Name, token[1])
		}
	}
	if *r.CoalesceMS < 0 {
		return fmt.Errorf("rule %q coalesce_ms must be >= 0", r.Name)
	}
	if *r.RateLimitPerMinute < 0 {
		return fmt.Errorf("rule %q rate_limit_per_minute must be >= 0", r.Name)
	}
	if *r.MaxWaitTargetMS < 0 {
		return fmt.Errorf("rule %q max_wait_target_ms must be >= 0", r.Name)
	}
	return nil
}

// SocketPathOr resolves the socket path the bridge should talk to.
func (c *Config) SocketPathOr() string {
	if c.SocketPath != "" {
		return ExpandPath(c.SocketPath)
	}
	return SessionSocketPath(c.Session)
}

// RuntimeRulePath returns the file path for a runtime rule name.
func RuntimeRulePath(stateDir, name string) string {
	return filepath.Join(stateDir, "rules", name+".json")
}

// readRuleOwner returns the pane that armed the named rule, "" when the file
// does not record one, and ok=false when it cannot be read or parsed.
func readRuleOwner(path, name string) (owner string, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var single Rule
	if err := json.Unmarshal(data, &single); err == nil && single.Name != "" {
		if single.Name == name {
			return single.Owner, true
		}
		return "", false
	}
	var multi struct {
		Rules []Rule `json:"rules"`
	}
	if err := json.Unmarshal(data, &multi); err != nil {
		return "", false
	}
	for _, r := range multi.Rules {
		if r.Name == name {
			return r.Owner, true
		}
	}
	return "", false
}

// LoadRuntimeRules reads rules written by `herdr-bridge arm`.
func LoadRuntimeRules(stateDir string) ([]Rule, error) {
	dir := filepath.Join(stateDir, "rules")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	rules := make([]Rule, 0, len(names))
	for _, n := range names {
		path := filepath.Join(dir, n)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read rule %s: %w", path, err)
		}
		// Runtime rules are stored as a bare rule object; accept a
		// {"rules":[...]} wrapper too so a file can hold several rules.
		var single Rule
		if err := json.Unmarshal(data, &single); err == nil && single.Name != "" {
			single.Origin = path
			rules = append(rules, single)
			continue
		}
		var multi struct {
			Rules []Rule `json:"rules"`
		}
		if err := json.Unmarshal(data, &multi); err != nil {
			return nil, fmt.Errorf("parse rule %s: %w", path, err)
		}
		for _, r := range multi.Rules {
			r.Origin = path
			rules = append(rules, r)
		}
	}
	return rules, nil
}

// AllRules merges config rules with runtime rules; runtime rules win on name.
func (c *Config) AllRules() ([]Rule, error) {
	runtime, err := LoadRuntimeRules(c.StateDir)
	if err != nil {
		return nil, err
	}
	merged := make([]Rule, 0, len(c.Rules)+len(runtime))
	byName := map[string]int{}
	for _, r := range c.Rules {
		byName[r.Name] = len(merged)
		merged = append(merged, r)
	}
	for _, r := range runtime {
		c.applyDefaults(&r)
		if err := r.validate(); err != nil {
			return nil, err
		}
		if idx, ok := byName[r.Name]; ok {
			merged[idx] = r
			continue
		}
		byName[r.Name] = len(merged)
		merged = append(merged, r)
	}
	return merged, nil
}

// ConfigFilesForWatch lists files whose modification should reload the daemon.
func ConfigFilesForWatch(cfg *Config) []string {
	files := []string{}
	if cfg.Path != "" {
		files = append(files, cfg.Path)
	}
	dir := filepath.Join(cfg.StateDir, "rules")
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
				files = append(files, filepath.Join(dir, e.Name()))
			}
		}
	}
	return files
}
