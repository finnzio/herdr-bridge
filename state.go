package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// KnownEntry is the last observed semantic state of a pane.
type KnownEntry struct {
	Status string    `json:"status"`
	Seq    uint64    `json:"seq,omitempty"`
	At     time.Time `json:"at"`
}

// Counters are processed-event totals since the daemon started.
type Counters struct {
	Events     int `json:"events"`
	Matched    int `json:"matched"`
	Delivered  int `json:"delivered"`
	Failed     int `json:"failed"`
	Suppressed int `json:"suppressed"`
	Reconnects int `json:"reconnects"`
}

// DeliveryRecord describes the most recent delivery attempt.
type DeliveryRecord struct {
	At     time.Time `json:"at"`
	Rule   string    `json:"rule"`
	Target string    `json:"target"`
	Mode   string    `json:"mode"`
	Text   string    `json:"text"`
	Error  string    `json:"error,omitempty"`
}

// Status is the live daemon snapshot exposed by `herdr-bridge status`.
type Status struct {
	PID           int             `json:"pid"`
	StartedAt     time.Time       `json:"started_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
	ConfigPath    string          `json:"config_path"`
	Socket        string          `json:"socket"`
	DryRun        bool            `json:"dry_run"`
	Rules         []string        `json:"rules"`
	WatchedPanes  []string        `json:"watched_panes"`
	Subscriptions int             `json:"subscriptions"`
	Counters      Counters        `json:"counters"`
	LastEventAt   *time.Time      `json:"last_event_at,omitempty"`
	LastDelivery  *DeliveryRecord `json:"last_delivery,omitempty"`
	LastError     string          `json:"last_error,omitempty"`
	SubscribedAt  *time.Time      `json:"subscribed_at,omitempty"`
}

// State is the bridge's durable state: observed pane statuses plus the live
// status snapshot. The known map survives restarts so a changed state that
// happened while the bridge was down is reported exactly once.
type State struct {
	dir string

	mu          sync.Mutex
	known       map[string]KnownEntry
	knownDirty  bool
	status      Status
	statusDirty bool
}

func NewState(dir string) *State {
	return &State{dir: dir, known: map[string]KnownEntry{}}
}

func (s *State) KnownPath() string  { return filepath.Join(s.dir, "known.json") }
func (s *State) StatusPath() string { return filepath.Join(s.dir, "status.json") }
func (s *State) PIDPath() string    { return filepath.Join(s.dir, "bridge.pid") }
func (s *State) LogPath() string    { return filepath.Join(s.dir, "bridge.log") }
func (s *State) RulesDir() string   { return filepath.Join(s.dir, "rules") }

// EnsureDir creates the state directory.
func (s *State) EnsureDir() error { return os.MkdirAll(s.dir, 0o755) }

// LoadKnown reads persisted pane states. A missing file is not an error.
func (s *State) LoadKnown() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.KnownPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var payload struct {
		Panes map[string]KnownEntry `json:"panes"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("parse %s: %w", s.KnownPath(), err)
	}
	if payload.Panes != nil {
		s.known = payload.Panes
	}
	return nil
}

// statusClass collapses Herdr's two spellings of "the agent is back at its
// prompt". Herdr reports "done" while the completion is still unseen and
// rewrites it to "idle" once the user visits the pane. Both mean the same
// thing to the bridge: the worker stopped working. A wake-up must not depend
// on the worker tab being active, so a rule that watches either spelling
// matches both.
func statusClass(status string) string {
	if status == statusDone {
		return statusIdle
	}
	return status
}

// Observe records a pane status. It reports whether this is a change against a
// previously known state; the very first observation of a pane is a baseline
// and never counts as a change.
func (s *State) Observe(paneID, status string, seq uint64) (changed bool, existed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.known[paneID]
	if ok && prev.Status == status {
		return false, true
	}
	if ok && prev.Status == statusDone && status == statusIdle {
		// Herdr rewrites "done" to "idle" when the user visits the pane. That
		// acknowledges the completion the bridge already reported; it is not a
		// second completion, so it must not wake the initiator again.
		s.known[paneID] = KnownEntry{Status: status, Seq: seq, At: time.Now()}
		s.knownDirty = true
		return false, true
	}
	s.known[paneID] = KnownEntry{Status: status, Seq: seq, At: time.Now()}
	s.knownDirty = true
	return ok, ok
}

// Prune removes known entries for panes that no longer exist.
func (s *State) Prune(live map[string]bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for pane := range s.known {
		if !live[pane] {
			delete(s.known, pane)
			removed++
			s.knownDirty = true
		}
	}
	return removed
}

// ReadStatusFromDisk loads the status snapshot written by a running daemon,
// which is how a separate CLI process observes it.
func (s *State) ReadStatusFromDisk() (Status, error) {
	var st Status
	data, err := os.ReadFile(s.StatusPath())
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, err
	}
	return st, nil
}

// Status returns a copy of the live status snapshot.
func (s *State) StatusSnapshot() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status
	st.Rules = append([]string(nil), s.status.Rules...)
	st.WatchedPanes = append([]string(nil), s.status.WatchedPanes...)
	return st
}

// Update mutates the live status snapshot under lock.
func (s *State) Update(fn func(*Status)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.status)
	s.status.UpdatedAt = time.Now()
	s.statusDirty = true
}

// AddCounters increments counters and marks the snapshot dirty.
func (s *State) AddCounters(fn func(*Counters)) {
	s.Update(func(st *Status) { fn(&st.Counters) })
}

// Flush writes changed state to disk. Call periodically and at shutdown.
func (s *State) Flush(force bool) error {
	s.mu.Lock()
	writeKnown := s.knownDirty
	writeStatus := s.statusDirty
	known := make(map[string]KnownEntry, len(s.known))
	for k, v := range s.known {
		known[k] = v
	}
	status := s.status
	s.knownDirty = false
	s.statusDirty = false
	s.mu.Unlock()

	if !writeKnown && !writeStatus && !force {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	if writeKnown || force {
		if err := writeJSONAtomic(s.KnownPath(), map[string]any{"panes": known}); err != nil {
			return err
		}
	}
	if writeStatus || force {
		if err := writeJSONAtomic(s.StatusPath(), status); err != nil {
			return err
		}
	}
	return nil
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// WritePID records this process as the running daemon.
func (s *State) WritePID(pid int) error {
	if err := s.EnsureDir(); err != nil {
		return err
	}
	return os.WriteFile(s.PIDPath(), []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

// ReadPID returns the recorded daemon pid, or 0 when absent.
func (s *State) ReadPID() int {
	data, err := os.ReadFile(s.PIDPath())
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}

// RemovePID clears the pid file.
func (s *State) RemovePID() { _ = os.Remove(s.PIDPath()) }

// ProcessAlive reports whether a pid is a live process.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// SignalPID sends a signal to a pid, ignoring a missing process.
func SignalPID(pid int, sig syscall.Signal) error {
	err := syscall.Kill(pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
