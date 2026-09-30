package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// APIError is a herdr socket API error ({"error":{"code","message"}}).
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

// IsCode reports whether err is an APIError with the given code.
func IsCode(err error, code string) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Code == code
	}
	return false
}

// AgentInfo mirrors the socket API AgentInfo (subset we use).
type AgentInfo struct {
	Name             *string           `json:"name"`
	Agent            *string           `json:"agent"`
	AgentStatus      string            `json:"agent_status"`
	PaneID           string            `json:"pane_id"`
	WorkspaceID      string            `json:"workspace_id"`
	TabID            string            `json:"tab_id"`
	Title            *string           `json:"title"`
	DisplayAgent     *string           `json:"display_agent"`
	TerminalTitle    *string           `json:"terminal_title_stripped"`
	Focused          bool              `json:"focused"`
	StateChangeSeq   uint64            `json:"state_change_seq"`
	InteractiveReady bool              `json:"interactive_ready"`
	LaunchPending    bool              `json:"launch_pending"`
	Tokens           map[string]string `json:"tokens"`
}

func (a AgentInfo) Kind() string {
	if a.Agent == nil {
		return ""
	}
	return *a.Agent
}

func (a AgentInfo) AgentName() string {
	if a.Name == nil {
		return ""
	}
	return *a.Name
}

func (a AgentInfo) TitleText() string {
	if a.Title != nil && *a.Title != "" {
		return *a.Title
	}
	if a.TerminalTitle != nil {
		return *a.TerminalTitle
	}
	return ""
}

// PaneInfo mirrors the socket API PaneInfo (subset we use).
type PaneInfo struct {
	PaneID        string  `json:"pane_id"`
	WorkspaceID   string  `json:"workspace_id"`
	TabID         string  `json:"tab_id"`
	Label         *string `json:"label"`
	Agent         *string `json:"agent"`
	AgentStatus   string  `json:"agent_status"`
	Title         *string `json:"title"`
	TerminalTitle *string `json:"terminal_title_stripped"`
	Focused       bool    `json:"focused"`
}

// Event is one pushed subscription frame: {"event": "...", "data": {...}}.
type Event struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

// SubscriptionSpec is one entry of events.subscribe params.subscriptions.
type SubscriptionSpec map[string]any

// The herdr socket transport is newline-delimited JSON over a Unix socket,
// with exactly one request per connection. events.subscribe keeps that
// connection open and pushes event lines after the acknowledgement.
type Client struct {
	SocketPath  string
	DialTimeout time.Duration
	Timeout     time.Duration
}

func NewClient(socketPath string) *Client {
	return &Client{
		SocketPath:  socketPath,
		DialTimeout: 5 * time.Second,
		Timeout:     30 * time.Second,
	}
}

type wireRequest struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

type wireResponse struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *APIError       `json:"error"`
}

func newID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("b%d", time.Now().UnixNano())
	}
	return "bridge_" + hex.EncodeToString(b[:])
}

func (c *Client) dial() (net.Conn, error) {
	d := net.Dialer{Timeout: c.DialTimeout}
	conn, err := d.Dial("unix", c.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", c.SocketPath, err)
	}
	return conn, nil
}

func encode(r wireRequest) ([]byte, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func readLine(br *bufio.Reader) ([]byte, error) {
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		trimmed := line[:len(line)-1]
		if len(trimmed) > 0 {
			return trimmed, nil
		}
	}
}

// Request sends one request on a fresh connection and reads one response.
func (c *Client) Request(method string, params any) (json.RawMessage, error) {
	conn, err := c.dial()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return c.RequestOn(conn, method, params)
}

// RequestOn is Request over a caller-provided connection.
func (c *Client) RequestOn(conn net.Conn, method string, params any) (json.RawMessage, error) {
	payload, err := encode(wireRequest{ID: newID(), Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Now().Add(c.Timeout)); err != nil {
		return nil, err
	}
	if _, err := conn.Write(payload); err != nil {
		return nil, fmt.Errorf("write %s: %w", method, err)
	}
	line, err := readLine(bufio.NewReader(conn))
	if err != nil {
		return nil, fmt.Errorf("read %s response: %w", method, err)
	}
	var resp wireResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", method, err)
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	return resp.Result, nil
}

// Ping verifies the server is reachable and speaking the socket API.
func (c *Client) Ping() error {
	_, err := c.Request("ping", map[string]any{})
	return err
}

// ListAgents returns every agent Herdr currently recognizes.
func (c *Client) ListAgents() ([]AgentInfo, error) {
	raw, err := c.Request("agent.list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var res struct {
		Type   string      `json:"type"`
		Agents []AgentInfo `json:"agents"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("decode agent.list: %w", err)
	}
	return res.Agents, nil
}

// ListPanes returns every live pane.
func (c *Client) ListPanes() ([]PaneInfo, error) {
	raw, err := c.Request("pane.list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var res struct {
		Type  string     `json:"type"`
		Panes []PaneInfo `json:"panes"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("decode pane.list: %w", err)
	}
	return res.Panes, nil
}

// AgentGet returns the agent currently occupying a pane or name.
func (c *Client) AgentGet(target string) (*AgentInfo, error) {
	raw, err := c.Request("agent.get", map[string]any{"target": target})
	if err != nil {
		return nil, err
	}
	var res struct {
		Agent AgentInfo `json:"agent"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("decode agent.get: %w", err)
	}
	return &res.Agent, nil
}

// PromptAgent submits text (plus Enter) to an agent pane. Server-side it never
// wakes a model by itself; it only delivers terminal input.
func (c *Client) PromptAgent(target, text string) error {
	_, err := c.Request("agent.prompt", map[string]any{"target": target, "text": text})
	return err
}

// WaitAgent blocks server-side until the agent reaches one of the given states.
func (c *Client) WaitAgent(target string, until []string, timeoutMS int) error {
	params := map[string]any{"target": target, "until": until}
	if timeoutMS > 0 {
		params["timeout_ms"] = timeoutMS
	}
	_, err := c.Request("agent.wait", params)
	return err
}

// ReadPane reads rendered pane output through the socket API.
func (c *Client) ReadPane(paneID, source string, lines *uint32, stripANSI *bool) (string, error) {
	params := map[string]any{"pane_id": paneID, "source": source}
	if lines != nil {
		params["lines"] = *lines
	}
	if stripANSI != nil {
		params["strip_ansi"] = *stripANSI
	}
	raw, err := c.Request("pane.read", params)
	if err != nil {
		return "", err
	}
	var res struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("decode pane.read: %w", err)
	}
	return res.Read.Text, nil
}

// SendText writes literal text into a pane without submitting it.
func (c *Client) SendText(paneID, text string) error {
	_, err := c.Request("pane.send_text", map[string]any{"pane_id": paneID, "text": text})
	return err
}

// SendKeys sends logical keys (for example ["enter"]) to a pane.
func (c *Client) SendKeys(paneID string, keys []string) error {
	_, err := c.Request("pane.send_keys", map[string]any{"pane_id": paneID, "keys": keys})
	return err
}

// Notify shows a Herdr notification (best effort, no model involved).
func (c *Client) Notify(title, body, sound string) error {
	params := map[string]any{"title": title}
	if body != "" {
		params["body"] = body
	}
	if sound != "" {
		params["sound"] = sound
	}
	_, err := c.Request("notification.show", params)
	return err
}

// SubConn is a live events.subscribe stream.
type SubConn struct {
	conn   net.Conn
	Events chan Event

	mu  sync.Mutex
	err error
}

// Specs returns the subscription specs this connection was opened with.
func (s *SubConn) Close() {
	if s == nil {
		return
	}
	_ = s.conn.Close()
}

// Err returns the terminal read error, if the stream ended abnormally.
func (s *SubConn) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *SubConn) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

// Subscribe opens a dedicated connection, registers the subscriptions, waits
// for the acknowledgement, and streams pushed events on the returned channel.
// The channel is closed when the stream ends.
func (c *Client) Subscribe(specs []SubscriptionSpec) (*SubConn, error) {
	conn, err := c.dial()
	if err != nil {
		return nil, err
	}
	params := map[string]any{"subscriptions": specs}
	payload, err := encode(wireRequest{ID: newID(), Method: "events.subscribe", Params: params})
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.SetDeadline(time.Now().Add(c.Timeout)); err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := conn.Write(payload); err != nil {
		conn.Close()
		return nil, fmt.Errorf("write events.subscribe: %w", err)
	}
	br := bufio.NewReader(conn)
	line, err := readLine(br)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read events.subscribe ack: %w", err)
	}
	var resp wireResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("decode events.subscribe ack: %w", err)
	}
	if resp.Error != nil {
		conn.Close()
		return nil, resp.Error
	}
	var ack struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(resp.Result, &ack); err != nil || ack.Type != "subscription_started" {
		conn.Close()
		return nil, fmt.Errorf("unexpected events.subscribe ack: %s", string(line))
	}
	// Subscriptions are long-lived and idle for arbitrary periods, so no
	// read deadline is applied after the acknowledgement.
	_ = conn.SetDeadline(time.Time{})

	sub := &SubConn{conn: conn, Events: make(chan Event, 256)}
	go sub.readLoop(br)
	return sub, nil
}

func (s *SubConn) readLoop(br *bufio.Reader) {
	defer close(s.Events)
	for {
		line, err := readLine(br)
		if err != nil {
			s.fail(err)
			return
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil || ev.Event == "" {
			continue
		}
		s.Events <- ev
	}
}
