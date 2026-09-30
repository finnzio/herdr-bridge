package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeHerdr is a minimal herdr socket server: newline-delimited JSON, one
// request per connection, events.subscribe holding the connection open.
type fakeHerdr struct {
	t    *testing.T
	path string
	ln   net.Listener

	mu            sync.Mutex
	agents        []AgentInfo
	panes         []PaneInfo
	prompts       []capturedPrompt
	waits         []string
	notifications []string
	sentText      []capturedText
	sendKeys      [][]string
	subs          []SubscriptionSpec
	subRequests   [][]SubscriptionSpec
	subConns      []net.Conn
	readText      map[string]string
	closed        bool
}

type capturedPrompt struct {
	Target string
	Text   string
}

type capturedText struct {
	Pane string
	Text string
}

func newFakeHerdr(t *testing.T) *fakeHerdr {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "herdr.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeHerdr{t: t, path: path, ln: ln}
	go f.serve()
	t.Cleanup(f.Close)
	return f
}

func (f *fakeHerdr) Close() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	conns := append([]net.Conn(nil), f.subConns...)
	f.mu.Unlock()
	_ = f.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (f *fakeHerdr) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeHerdr) setAgents(agents ...AgentInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agents = agents
}

func (f *fakeHerdr) setReadText(paneID, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readText == nil {
		f.readText = map[string]string{}
	}
	f.readText[paneID] = text
}

func (f *fakeHerdr) setPanes(panes ...PaneInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.panes = panes
}

func (f *fakeHerdr) setAgentStatus(paneID, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.agents {
		if f.agents[i].PaneID == paneID {
			f.agents[i].AgentStatus = status
		}
	}
}

func (f *fakeHerdr) handle(conn net.Conn) {
	br := bufio.NewReader(conn)
	line, err := br.ReadBytes('\n')
	if err != nil {
		conn.Close()
		return
	}
	var req struct {
		ID     string          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		conn.Close()
		return
	}
	switch req.Method {
	case "ping":
		writeLine(conn, map[string]any{"id": req.ID, "result": map[string]any{"type": "pong"}})
	case "agent.list":
		f.mu.Lock()
		agents := append([]AgentInfo(nil), f.agents...)
		f.mu.Unlock()
		writeLine(conn, map[string]any{"id": req.ID, "result": map[string]any{"type": "agent_list", "agents": agents}})
	case "pane.list":
		f.mu.Lock()
		panes := append([]PaneInfo(nil), f.panes...)
		f.mu.Unlock()
		writeLine(conn, map[string]any{"id": req.ID, "result": map[string]any{"type": "pane_list", "panes": panes}})
	case "agent.get":
		var p struct {
			Target string `json:"target"`
		}
		_ = json.Unmarshal(req.Params, &p)
		agent, ok := f.findAgent(p.Target)
		if !ok {
			writeLine(conn, map[string]any{"id": req.ID, "error": map[string]any{"code": "agent_not_found", "message": "no agent"}})
			break
		}
		writeLine(conn, map[string]any{"id": req.ID, "result": map[string]any{"type": "agent_get", "agent": agent}})
	case "pane.read":
		var p struct {
			PaneID string `json:"pane_id"`
			Source string `json:"source"`
		}
		_ = json.Unmarshal(req.Params, &p)
		f.mu.Lock()
		text := ""
		if f.readText != nil {
			text = f.readText[p.PaneID]
		}
		f.mu.Unlock()
		writeLine(conn, map[string]any{"id": req.ID, "result": map[string]any{
			"type": "pane_read",
			"read": map[string]any{"pane_id": p.PaneID, "source": p.Source, "text": text,
				"format": "text", "revision": 1, "truncated": false, "workspace_id": "w1", "tab_id": "w1:t1"}}})
	case "agent.wait":
		var p struct {
			Target string `json:"target"`
		}
		_ = json.Unmarshal(req.Params, &p)
		f.mu.Lock()
		f.waits = append(f.waits, p.Target)
		f.mu.Unlock()
		writeLine(conn, map[string]any{"id": req.ID, "result": map[string]any{"type": "agent_wait"}})
	case "agent.prompt":
		var p struct {
			Target string `json:"target"`
			Text   string `json:"text"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if agent, ok := f.findAgent(p.Target); ok && agent.AgentStatus == "blocked" {
			writeLine(conn, map[string]any{"id": req.ID, "error": map[string]any{
				"code": "agent_blocked", "message": "agent is blocked and requires interactive input"}})
			break
		}
		f.mu.Lock()
		f.prompts = append(f.prompts, capturedPrompt{Target: p.Target, Text: p.Text})
		f.mu.Unlock()
		writeLine(conn, map[string]any{"id": req.ID, "result": map[string]any{"type": "agent_prompt"}})
	case "pane.send_text":
		var p struct {
			PaneID string `json:"pane_id"`
			Text   string `json:"text"`
		}
		_ = json.Unmarshal(req.Params, &p)
		f.mu.Lock()
		f.sentText = append(f.sentText, capturedText{Pane: p.PaneID, Text: p.Text})
		f.mu.Unlock()
		writeLine(conn, map[string]any{"id": req.ID, "result": map[string]any{"type": "pane_send_text"}})
	case "pane.send_keys":
		var p struct {
			PaneID string   `json:"pane_id"`
			Keys   []string `json:"keys"`
		}
		_ = json.Unmarshal(req.Params, &p)
		f.mu.Lock()
		f.sendKeys = append(f.sendKeys, p.Keys)
		f.mu.Unlock()
		writeLine(conn, map[string]any{"id": req.ID, "result": map[string]any{"type": "pane_send_keys"}})
	case "notification.show":
		var p struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		}
		_ = json.Unmarshal(req.Params, &p)
		f.mu.Lock()
		f.notifications = append(f.notifications, p.Title+" | "+p.Body)
		f.mu.Unlock()
		writeLine(conn, map[string]any{"id": req.ID, "result": map[string]any{"type": "notification_show", "shown": true}})
	case "events.subscribe":
		var p struct {
			Subscriptions []SubscriptionSpec `json:"subscriptions"`
		}
		_ = json.Unmarshal(req.Params, &p)
		f.mu.Lock()
		f.subs = p.Subscriptions
		f.subRequests = append(f.subRequests, p.Subscriptions)
		f.mu.Unlock()
		writeLine(conn, map[string]any{"id": req.ID, "result": map[string]any{"type": "subscription_started"}})
		f.mu.Lock()
		f.subConns = append(f.subConns, conn)
		f.mu.Unlock()
		// Hold the connection open; drain input until the client closes it.
		for {
			if _, err := br.ReadBytes('\n'); err != nil {
				conn.Close()
				return
			}
		}
	default:
		writeLine(conn, map[string]any{"id": req.ID, "error": map[string]any{"code": "unknown_method", "message": req.Method}})
	}
	conn.Close()
}

func (f *fakeHerdr) findAgent(target string) (AgentInfo, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.agents {
		if a.PaneID == target || a.AgentName() == target {
			return a, true
		}
		if a.Name != nil && *a.Name == target {
			return a, true
		}
	}
	return AgentInfo{}, false
}

// Push sends one event frame to every current subscriber.
func (f *fakeHerdr) Push(event string, data any) {
	f.mu.Lock()
	conns := append([]net.Conn(nil), f.subConns...)
	f.mu.Unlock()
	payload := map[string]any{"event": event, "data": data}
	for _, c := range conns {
		writeLine(c, payload)
	}
}

func (f *fakeHerdr) promptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

func (f *fakeHerdr) promptsSnapshot() []capturedPrompt {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]capturedPrompt(nil), f.prompts...)
}

func (f *fakeHerdr) waitsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.waits...)
}

func (f *fakeHerdr) notificationsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.notifications...)
}

func (f *fakeHerdr) subscribeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subRequests)
}

func (f *fakeHerdr) lastSubscriptions() []SubscriptionSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.subRequests) == 0 {
		return nil
	}
	return f.subRequests[len(f.subRequests)-1]
}

func writeLine(conn net.Conn, value any) {
	b, err := json.Marshal(value)
	if err != nil {
		return
	}
	_, _ = conn.Write(append(b, '\n'))
}

// waitFor polls a condition until it holds or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func testAgent(paneID, kind, status string, name string) AgentInfo {
	a := AgentInfo{PaneID: paneID, AgentStatus: status, WorkspaceID: "w1", TabID: "w1:t1"}
	if kind != "" {
		a.Agent = &kind
	}
	if name != "" {
		a.Name = &name
	}
	return a
}

func testPane(paneID string) PaneInfo {
	return PaneInfo{PaneID: paneID, WorkspaceID: "w1", TabID: "w1:t1"}
}

var _ = fmt.Sprintf
