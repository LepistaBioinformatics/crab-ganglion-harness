package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/tool"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/config"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// fakeTool stands in for a built-in. Only the name matters here.
type fakeTool struct{ name string }

func (f fakeTool) Name() string              { return f.name }
func (f fakeTool) Schema() domain.ToolSchema { return domain.ToolSchema{Name: f.name} }
func (f fakeTool) Invoke(context.Context, json.RawMessage) (domain.Result, error) {
	return domain.Result{}, nil
}

// mcpEndpoint serves the handshake and a fixed tool list.
func mcpEndpoint(t *testing.T, names ...string) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if req.Method != "tools/list" {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{}}`, req.ID)
			return
		}
		var tools []string
		for _, n := range names {
			tools = append(tools, fmt.Sprintf(`{"name":%q}`, n))
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[%s]}}`,
			req.ID, strings.Join(tools, ","))
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

func quietLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func TestMCPToolsRegistersDiscoveredTools(t *testing.T) {
	reg := config.Registry{MCP: []config.MCPServer{
		{Name: "memory", URL: mcpEndpoint(t, "memory_search", "memory_add")},
	}}
	builtin := []tool.Tool{fakeTool{name: "shell"}}

	remote, clients, err := mcpTools(context.Background(), reg, builtin, quietLogger())
	if err != nil {
		t.Fatalf("mcpTools: %v", err)
	}
	if len(remote) != 2 || remote[0].Name() != "memory_search" {
		t.Fatalf("remote = %+v", remote)
	}
	// The clients come back so the boot can close them: a scale-to-zero agent
	// opens a session per cold start, and nothing else would release them.
	if len(clients) != 1 {
		t.Fatalf("clients = %d, want 1", len(clients))
	}
}

// FR-C5. Registry.Register OVERWRITES, so a remote server offering "shell" would
// silently replace the sandboxed one with whatever it does.
func TestMCPToolsRefusesACollisionWithABuiltin(t *testing.T) {
	reg := config.Registry{MCP: []config.MCPServer{
		{Name: "memory", URL: mcpEndpoint(t, "shell")},
	}}
	_, _, err := mcpTools(context.Background(), reg,
		[]tool.Tool{fakeTool{name: "shell"}}, quietLogger())
	if err == nil {
		t.Fatal("boot was allowed to continue with a shadowed built-in")
	}
	// Names BOTH, because the operator has to know which server to edit and
	// which tool it clashed with.
	for _, want := range []string{`"memory"`, `"shell"`, "built-in"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %s: %v", want, err)
		}
	}
}

// Two servers offering the same name is the same failure, and the refusal has to
// say which one lost -- "already offered by" is the only thing that tells an
// operator the tool they are calling is not the one they think.
func TestMCPToolsRefusesACollisionBetweenServers(t *testing.T) {
	reg := config.Registry{MCP: []config.MCPServer{
		{Name: "memory", URL: mcpEndpoint(t, "search")},
		{Name: "notes", URL: mcpEndpoint(t, "search")},
	}}
	_, _, err := mcpTools(context.Background(), reg, nil, quietLogger())
	if err == nil {
		t.Fatal("boot was allowed to continue with two tools of one name")
	}
	if !strings.Contains(err.Error(), `"notes"`) || !strings.Contains(err.Error(), `"memory"`) {
		t.Errorf("refusal does not name both servers: %v", err)
	}
}

// An unreachable server stops the boot. An agent told it has a memory that is
// not there is exactly the failure this path exists to prevent, and it would be
// invisible from inside the container.
func TestMCPToolsRefusesAnUnreachableServer(t *testing.T) {
	reg := config.Registry{MCP: []config.MCPServer{
		{Name: "memory", URL: "http://127.0.0.1:1/v1/mcp"},
	}}
	if _, _, err := mcpTools(context.Background(), reg, nil, quietLogger()); err == nil {
		t.Fatal("boot succeeded with an unreachable memory graph")
	}
}

// NFR-1's shape for this slice: no servers means no work, no clients and no
// change to what the agent is given.
func TestMCPToolsIsANoOpWithoutServers(t *testing.T) {
	remote, clients, err := mcpTools(context.Background(), config.Registry{}, nil, quietLogger())
	if err != nil || remote != nil || clients != nil {
		t.Fatalf("remote=%v clients=%v err=%v", remote, clients, err)
	}
}
