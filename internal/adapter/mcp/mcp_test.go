package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// server is a fake MCP endpoint. It answers by method so a test states only what
// it cares about, and it records the headers it saw -- the session id and the
// protocol version are part of the contract and are invisible in a result.
type server struct {
	mu       sync.Mutex
	sessions []string
	methods  []string
	deletes  int

	sse      bool
	respond  func(method string, params json.RawMessage) (any, *rpcError)
	listPage map[string]listToolsResult
}

func (s *server) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			s.mu.Lock()
			s.deletes++
			s.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var req struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad request body: %v", err)
			return
		}
		s.mu.Lock()
		s.methods = append(s.methods, req.Method)
		s.sessions = append(s.sessions, r.Header.Get("Mcp-Session-Id"))
		s.mu.Unlock()

		if req.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "sess-1")
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted) // a notification
			return
		}

		var result any
		var rerr *rpcError
		switch {
		case s.respond != nil:
			result, rerr = s.respond(req.Method, req.Params)
		case req.Method == "tools/list":
			var p struct {
				Cursor string `json:"cursor"`
			}
			_ = json.Unmarshal(req.Params, &p)
			result = s.listPage[p.Cursor]
		default:
			result = map[string]any{}
		}

		body, _ := json.Marshal(rpcResponse{
			JSONRPC: "2.0", ID: req.ID,
			Result: mustRaw(result), Error: rerr,
		})
		if !s.sse {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
			return
		}
		// The same answer, SSE-framed, with a progress notification in front of
		// it -- which is exactly the shape that makes "take the first frame"
		// wrong.
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", `{"jsonrpc":"2.0","method":"notifications/progress"}`)
		fmt.Fprintf(w, "data: %s\n\n", body)
	})
}

func mustRaw(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return b
}

func (s *server) seen() ([]string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.methods...), append([]string(nil), s.sessions...)
}

func dial(t *testing.T, s *server) (*Client, []*Tool) {
	t.Helper()
	ts := httptest.NewServer(s.handler(t))
	t.Cleanup(ts.Close)
	c, tools, err := Connect(context.Background(), "memory", ts.URL, nil, 5*time.Second)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return c, tools
}

func TestConnectDiscoversToolsAndCarriesTheSession(t *testing.T) {
	s := &server{listPage: map[string]listToolsResult{
		"": {Tools: []toolDescriptor{
			{Name: "memory_search", Description: "search the graph",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)},
		}},
	}}
	_, tools := dial(t, s)

	if len(tools) != 1 || tools[0].Name() != "memory_search" {
		t.Fatalf("tools = %+v", tools)
	}
	// The server's schema reaches the model VERBATIM. Re-encoding somebody
	// else's JSON Schema is how a constraint quietly disappears.
	if got := string(tools[0].Schema().Parameters); !strings.Contains(got, `"q"`) {
		t.Errorf("schema was not passed through: %s", got)
	}

	methods, sessions := s.seen()
	if len(methods) < 2 || methods[0] != "initialize" {
		t.Fatalf("handshake did not happen first: %v", methods)
	}
	// Every request after initialize echoes the session id. Without it a server
	// that keeps per-session state answers each call as a stranger.
	for i, m := range methods {
		if m == "initialize" {
			continue
		}
		if sessions[i] != "sess-1" {
			t.Errorf("%s carried session %q, want sess-1", m, sessions[i])
		}
	}
}

// The transport permits either body shape for the same request, so both are read
// rather than one being assumed and the other met in production.
func TestConnectReadsAnSSEFramedResponse(t *testing.T) {
	s := &server{sse: true, listPage: map[string]listToolsResult{
		"": {Tools: []toolDescriptor{{Name: "memory_add"}}},
	}}
	_, tools := dial(t, s)
	if len(tools) != 1 || tools[0].Name() != "memory_add" {
		t.Fatalf("tools = %+v", tools)
	}
}

// A truncated tool list is a capability the agent is never told about, and the
// failure looks like the model choosing not to use it.
func TestListToolsFollowsCursors(t *testing.T) {
	s := &server{listPage: map[string]listToolsResult{
		"":   {Tools: []toolDescriptor{{Name: "a"}}, NextCursor: "p2"},
		"p2": {Tools: []toolDescriptor{{Name: "b"}}},
	}}
	_, tools := dial(t, s)
	if len(tools) != 2 || tools[0].Name() != "a" || tools[1].Name() != "b" {
		t.Fatalf("paging lost tools: %+v", tools)
	}
}

// A server repeating one cursor must not spin the boot forever.
func TestListToolsStopsOnARepeatedCursor(t *testing.T) {
	s := &server{listPage: map[string]listToolsResult{
		"": {Tools: []toolDescriptor{{Name: "a"}}, NextCursor: ""},
	}}
	s.respond = func(method string, _ json.RawMessage) (any, *rpcError) {
		if method == "tools/list" {
			return listToolsResult{Tools: []toolDescriptor{{Name: "a"}}, NextCursor: "same"}, nil
		}
		return map[string]any{}, nil
	}
	done := make(chan struct{})
	go func() { defer close(done); dial(t, s) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("tools/list did not terminate on a repeated cursor")
	}
}

// A tool with no parameters still needs a schema: several providers reject a
// function whose parameters are absent.
func TestToolWithNoSchemaGetsAnEmptyObject(t *testing.T) {
	s := &server{listPage: map[string]listToolsResult{
		"": {Tools: []toolDescriptor{{Name: "memory_stats"}}},
	}}
	_, tools := dial(t, s)
	if got := string(tools[0].Schema().Parameters); got != `{"type":"object","properties":{}}` {
		t.Fatalf("parameters = %s", got)
	}
}

func TestCallToolReturnsText(t *testing.T) {
	s := &server{
		listPage: map[string]listToolsResult{"": {Tools: []toolDescriptor{{Name: "memory_search"}}}},
	}
	s.respond = func(method string, params json.RawMessage) (any, *rpcError) {
		switch method {
		case "tools/list":
			return listToolsResult{Tools: []toolDescriptor{{Name: "memory_search"}}}, nil
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(params, &p)
			return callToolResult{Content: []contentBlock{
				{Type: "text", Text: "called " + p.Name + " with " + string(p.Arguments)},
			}}, nil
		}
		return map[string]any{}, nil
	}
	_, tools := dial(t, s)

	res, err := tools[0].Invoke(context.Background(), json.RawMessage(`{"q":"seed"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !strings.Contains(res.Content, `called memory_search with {"q":"seed"}`) {
		t.Fatalf("content = %q", res.Content)
	}
}

// FR-C6: a failure mid-turn is a Result, never an error. The turn has already
// done work; failing it because a remote server is unreachable throws that away,
// while telling the agent lets it say so or try another way.
func TestInvokeReportsFailuresAsResults(t *testing.T) {
	t.Run("the tool reports an error", func(t *testing.T) {
		s := &server{}
		s.respond = func(method string, _ json.RawMessage) (any, *rpcError) {
			if method == "tools/list" {
				return listToolsResult{Tools: []toolDescriptor{{Name: "memory_add"}}}, nil
			}
			return callToolResult{IsError: true,
				Content: []contentBlock{{Type: "text", Text: "entity already exists"}}}, nil
		}
		_, tools := dial(t, s)
		res, err := tools[0].Invoke(context.Background(), nil)
		if err != nil {
			t.Fatalf("a reported tool error became a turn failure: %v", err)
		}
		if !strings.Contains(res.Content, "entity already exists") {
			t.Errorf("the agent was not told what happened: %q", res.Content)
		}
	})

	t.Run("the transport fails", func(t *testing.T) {
		s := &server{}
		s.respond = func(method string, _ json.RawMessage) (any, *rpcError) {
			if method != "tools/call" {
				return listToolsResult{Tools: []toolDescriptor{{Name: "memory_add"}}}, nil
			}
			return nil, &rpcError{Code: -32603, Message: "internal error"}
		}
		_, tools := dial(t, s)
		res, err := tools[0].Invoke(context.Background(), nil)
		if err != nil {
			t.Fatalf("a transport failure became a turn failure: %v", err)
		}
		if !strings.Contains(res.Content, "unavailable") {
			t.Errorf("content = %q", res.Content)
		}
	})

	t.Run("the server is gone", func(t *testing.T) {
		s := &server{listPage: map[string]listToolsResult{
			"": {Tools: []toolDescriptor{{Name: "memory_add"}}}}}
		ts := httptest.NewServer(s.handler(t))
		c, tools, err := Connect(context.Background(), "memory", ts.URL, nil, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		ts.Close() // the graph goes away mid-turn
		res, invokeErr := tools[0].Invoke(context.Background(), nil)
		if invokeErr != nil {
			t.Fatalf("an unreachable server became a turn failure: %v", invokeErr)
		}
		if !strings.Contains(res.Content, "unavailable") {
			t.Errorf("content = %q", res.Content)
		}
		_ = c
	})
}

// An empty result reads to a model as a tool that did nothing. Saying so is the
// difference between the agent moving on and the agent retrying.
func TestInvokeNamesAnEmptyResult(t *testing.T) {
	s := &server{}
	s.respond = func(method string, _ json.RawMessage) (any, *rpcError) {
		if method == "tools/list" {
			return listToolsResult{Tools: []toolDescriptor{{Name: "memory_search"}}}, nil
		}
		return callToolResult{}, nil
	}
	_, tools := dial(t, s)
	res, _ := tools[0].Invoke(context.Background(), nil)
	if res.Content != "(no content returned)" {
		t.Fatalf("content = %q", res.Content)
	}
}

// A content type this harness cannot hand to the model is NAMED, not dropped. A
// server that starts returning images should show up as a legible gap.
func TestInvokeNamesAnUnsupportedContentType(t *testing.T) {
	s := &server{}
	s.respond = func(method string, _ json.RawMessage) (any, *rpcError) {
		if method == "tools/list" {
			return listToolsResult{Tools: []toolDescriptor{{Name: "memory_render"}}}, nil
		}
		return callToolResult{Content: []contentBlock{{Type: "image"}}}, nil
	}
	_, tools := dial(t, s)
	res, _ := tools[0].Invoke(context.Background(), nil)
	if !strings.Contains(res.Content, `unsupported content type "image"`) {
		t.Fatalf("content = %q", res.Content)
	}
}

// Boot is the opposite of a turn: a server that cannot be reached must stop the
// boot, because an agent told it has a memory that is not there is the failure
// this whole path exists to prevent.
func TestConnectFailsWhenTheServerIsUnreachable(t *testing.T) {
	_, _, err := Connect(context.Background(), "memory", "http://127.0.0.1:1/v1/mcp", nil, time.Second)
	if err == nil {
		t.Fatal("Connect succeeded against a dead endpoint")
	}
	if !strings.Contains(err.Error(), "memory") {
		t.Errorf("the failure does not name the server: %v", err)
	}
}

func TestCloseEndsTheSession(t *testing.T) {
	s := &server{listPage: map[string]listToolsResult{"": {}}}
	c, _ := dial(t, s)
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deletes != 1 {
		t.Fatalf("deletes = %d, want 1", s.deletes)
	}
}

// A server that never issued a session has nothing to release.
func TestCloseIsANoOpWithoutASession(t *testing.T) {
	c := New("memory", "http://127.0.0.1:1/v1/mcp", nil, time.Second)
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// The turn's project travels on every call, so a server that keeps a graph per
// project can tell them apart.
//
// Without this the ganglion wrote every project's memory into the member's
// GLOBAL graph -- the one server it is given carries the member's token and
// nothing else, so nothing in the request said which project the work belonged
// to. A project's memory filling up with another's is invisible until someone
// reads it.
func TestACallCarriesTheTurnsProject(t *testing.T) {
	var seen []string
	s := &server{listPage: map[string]listToolsResult{
		"": {Tools: []toolDescriptor{{Name: "memory_add"}}},
	}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get(ProjectHeader))
		s.handler(t).ServeHTTP(w, r)
	}))
	defer ts.Close()

	// Boot runs under no project, and must say so rather than guessing one.
	c, tools, err := Connect(context.Background(), "memory", ts.URL, nil, 5*time.Second)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	_ = c
	for i, h := range seen {
		if h != "" {
			t.Errorf("boot request %d carried project %q", i, h)
		}
	}

	before := len(seen)
	ctx := domain.WithProject(context.Background(), "seedtrial")
	if _, err := tools[0].Invoke(ctx, nil); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(seen) <= before {
		t.Fatal("the call never reached the server")
	}
	for i := before; i < len(seen); i++ {
		if seen[i] != "seedtrial" {
			t.Errorf("call %d carried project %q, want seedtrial", i, seen[i])
		}
	}
}

// A turn in the agent's own workspace sends nothing, so the server resolves the
// member's global graph exactly as it did before this existed.
func TestACallOutsideAProjectCarriesNoProject(t *testing.T) {
	var seen []string
	s := &server{listPage: map[string]listToolsResult{
		"": {Tools: []toolDescriptor{{Name: "memory_add"}}},
	}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get(ProjectHeader))
		s.handler(t).ServeHTTP(w, r)
	}))
	defer ts.Close()

	_, tools, err := Connect(context.Background(), "memory", ts.URL, nil, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tools[0].Invoke(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for i, h := range seen {
		if h != "" {
			t.Errorf("request %d carried project %q, want none", i, h)
		}
	}
}
