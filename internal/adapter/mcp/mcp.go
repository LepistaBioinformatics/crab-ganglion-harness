// Package mcp is a minimal Model Context Protocol client: JSON-RPC 2.0 over
// streamable HTTP, hand-written, standard library only.
//
// Hand-written because the module has zero dependencies and that is a property
// worth more than the code this saves. The official Go SDK pulls a tree of its
// own; what this harness needs of MCP is three methods and a session header.
//
// It speaks exactly one transport. A stdio server would need a process to run,
// and the container has no package manager, no npx and no interpreter -- so a
// `command` server is refused at BOOT (config.loadMCP) rather than failing at
// the first call, when the agent has already been told it has a memory.
//
// The response body may be JSON or an SSE stream; the specification permits
// either for the same request, so both are read here rather than one being
// assumed and the other discovered in production.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// protocolVersion is what this client negotiates. Sent on initialize and,
// afterwards, on every request as MCP-Protocol-Version -- servers use it to keep
// serving an older client after they move on.
const protocolVersion = "2025-06-18"

// maxBody caps a response. A tool result is text the model has to read; a server
// that answers with more than this has failed differently from one that errors.
const maxBody = 8 << 20

// Client is one MCP server.
//
// Safe for concurrent use: the session id is the only mutable state and the
// sub-agent dispatcher can have several children calling one server at once.
type Client struct {
	// Name is the key under tools.mcp.servers. It appears in every failure this
	// package reports, because "the memory graph is unreachable" is only
	// actionable if the operator knows which server that was.
	Name    string
	URL     string
	Headers map[string]string
	HTTP    *http.Client

	mu      sync.Mutex
	session string
}

// New builds a client. The timeout is per REQUEST, not per turn: a tool call
// that hangs would otherwise hold the whole turn, and the turn's own context
// only bounds the turn.
func New(name, url string, headers map[string]string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		Name:    name,
		URL:     url,
		Headers: headers,
		HTTP:    &http.Client{Timeout: timeout},
	}
}

// --- wire -------------------------------------------------------------------

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// toolDescriptor is one entry of a tools/list result. InputSchema is kept RAW
// and handed to the model untouched: re-encoding somebody else's JSON Schema is
// how a constraint quietly disappears.
type toolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type listToolsResult struct {
	Tools      []toolDescriptor `json:"tools"`
	NextCursor string           `json:"nextCursor"`
}

// contentBlock is one piece of a tool result. Only text is read: this harness
// hands a tool result to the model as a string, and a server that returns an
// image would need the loop's attachment path, which no MCP server in this stack
// uses today. An unread type is REPORTED rather than dropped silently.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type callToolResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError"`
}

// --- requests ---------------------------------------------------------------

// Initialize performs the handshake and records the session id.
//
// The notifications/initialized that follows is required by the specification
// and its failure is NOT: a server that rejects the notification has still
// initialized, and refusing to boot over it would trade a working memory for a
// protocol nicety.
func (c *Client) Initialize(ctx context.Context) error {
	raw, err := c.do(ctx, rpcRequest{
		JSONRPC: "2.0", ID: 1, Method: "initialize",
		Params: map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name": "crab-ganglion-harness", "version": "1",
			},
		},
	})
	if err != nil {
		return err
	}
	_ = raw
	if err := c.notify(ctx, "notifications/initialized"); err != nil {
		return nil //nolint:nilerr // see the doc comment: the handshake already succeeded
	}
	return nil
}

// ListTools returns every tool the server offers, following cursors.
//
// Paging is followed rather than truncated at the first page: a truncated list
// is a capability the agent is never told about, and the failure looks like the
// model choosing not to use it.
func (c *Client) ListTools(ctx context.Context) ([]toolDescriptor, error) {
	var out []toolDescriptor
	cursor := ""
	// A server that returns the same cursor forever would spin; 32 pages is far
	// past anything real and bounded is bounded.
	for page := 0; page < 32; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.do(ctx, rpcRequest{JSONRPC: "2.0", ID: 2, Method: "tools/list", Params: params})
		if err != nil {
			return nil, err
		}
		var res listToolsResult
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("mcp %s: decode tools/list: %w", c.Name, err)
		}
		out = append(out, res.Tools...)
		if res.NextCursor == "" || res.NextCursor == cursor {
			break
		}
		cursor = res.NextCursor
	}
	return out, nil
}

// CallTool runs one tool and returns its text.
//
// The second return distinguishes a tool that REPORTED failure (isError, which
// is the server telling the agent something it can act on) from a transport
// failure. Both reach the agent as text; only the second is worth a log line.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (string, bool, error) {
	params := map[string]any{"name": name}
	if len(args) > 0 {
		params["arguments"] = json.RawMessage(args)
	}
	raw, err := c.do(ctx, rpcRequest{JSONRPC: "2.0", ID: 3, Method: "tools/call", Params: params})
	if err != nil {
		return "", false, err
	}
	var res callToolResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", false, fmt.Errorf("mcp %s: decode tools/call: %w", c.Name, err)
	}
	var b strings.Builder
	for _, block := range res.Content {
		switch block.Type {
		case "text", "":
			b.WriteString(block.Text)
		default:
			// Named, not dropped. A server that starts returning images should
			// show up as a legible gap rather than as an empty result.
			fmt.Fprintf(&b, "[unsupported content type %q]", block.Type)
		}
	}
	return b.String(), res.IsError, nil
}

// Close ends the session. A server that never issued one, or that does not
// implement DELETE, is not an error -- there is nothing left to release.
func (c *Client) Close(ctx context.Context) error {
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.URL, nil)
	if err != nil {
		return err
	}
	c.applyHeaders(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return nil
}

// --- transport --------------------------------------------------------------

// notify sends a notification: no id, and no response is expected.
func (c *Client) notify(ctx context.Context, method string) error {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: method})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.applyHeaders(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("mcp %s: %s on %s", c.Name, resp.Status, method)
	}
	return nil
}

// do sends one request and returns the JSON-RPC result.
func (c *Client) do(ctx context.Context, r rpcRequest) (json.RawMessage, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.applyHeaders(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: %s: %w", c.Name, r.Method, err)
	}
	defer resp.Body.Close()

	// The session id arrives on the initialize response and is echoed on every
	// request after it. Recorded before the status check: a server may return
	// one alongside an error, and losing it would start a second session.
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		c.mu.Lock()
		c.session = id
		c.mu.Unlock()
	}
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return nil, fmt.Errorf("mcp %s: %s on %s: %s",
			c.Name, resp.Status, r.Method, strings.TrimSpace(string(snippet)))
	}

	out, err := decodeResponse(resp)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: %s: %w", c.Name, r.Method, err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("mcp %s: %s: %w", c.Name, r.Method, out.Error)
	}
	return out.Result, nil
}

// ProjectHeader tells the server which of the member's projects a call belongs
// to.
//
// The same name crab-shell-proxy sends in the other direction on a turn, because
// it is the same fact. It is set from the TURN's context, never from a tool
// argument: the project is a property of the conversation, and a model able to
// name it could read and write another project's memory.
//
// A remote server that does not know the header ignores it, which is what makes
// this safe to send unconditionally.
const ProjectHeader = "X-Ganglion-Project"

// applyHeaders sets what every request to this server carries.
//
// Accept lists BOTH media types because the server chooses between them per
// response; a client that accepted only one would be answered 406 by a server
// that wanted the other.
func (c *Client) applyHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", protocolVersion)
	// Before the configured headers, so a server block may override it, and from
	// the REQUEST's context, which is the turn's -- initialize and tools/list run
	// at boot under no project and correctly send nothing.
	if p := domain.ProjectFrom(req.Context()); p != "" {
		req.Header.Set(ProjectHeader, p)
	}
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
}

// decodeResponse reads either shape the transport permits.
//
// For SSE the LAST message carrying a JSON-RPC response wins: a server may emit
// progress notifications (which have no id and no result) before the answer, and
// taking the first frame would return one of those.
func decodeResponse(resp *http.Response) (rpcResponse, error) {
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		var out rpcResponse
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		if err != nil {
			return out, err
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return out, fmt.Errorf("decode response: %w", err)
		}
		return out, nil
	}

	var last rpcResponse
	found := false
	sc := bufio.NewScanner(io.LimitReader(resp.Body, maxBody))
	// A tool result is inlined into one SSE data line, and those get long.
	sc.Buffer(make([]byte, 0, 64<<10), maxBody)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var out rpcResponse
		if json.Unmarshal([]byte(payload), &out) != nil {
			continue
		}
		if out.Result == nil && out.Error == nil {
			continue // a notification, not the answer
		}
		last, found = out, true
	}
	if err := sc.Err(); err != nil {
		return last, fmt.Errorf("read event stream: %w", err)
	}
	if !found {
		return last, fmt.Errorf("event stream carried no response")
	}
	return last, nil
}
