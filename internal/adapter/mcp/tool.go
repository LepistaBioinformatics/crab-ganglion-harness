package mcp

// The bridge from a remote MCP tool to this harness's tool port.
//
// The port is satisfied STRUCTURALLY -- Name/Schema/Invoke -- so nothing here
// imports the tool registry. That is what keeps AR-4 intact and, incidentally,
// what makes this package testable against an httptest server alone.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// emptyObjectSchema is what a tool declaring no parameters gets.
//
// Not omitted: several providers reject a function whose parameters are absent,
// and a remote tool with no arguments is ordinary.
var emptyObjectSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// Tool is one tool discovered from a server.
//
// The schema is the SERVER'S, verbatim. The harness does not re-declare the
// graph's fifteen tools, so a change on the server side reaches the agent
// without a harness release -- which is the whole reason discovery is done at
// boot rather than a tool list being written down here.
type Tool struct {
	client *Client
	schema domain.ToolSchema
}

func (t *Tool) Name() string              { return t.schema.Name }
func (t *Tool) Schema() domain.ToolSchema { return t.schema }
func (t *Tool) Server() string            { return t.client.Name }

// Invoke calls the remote tool.
//
// A FAILURE IS A RESULT, never an error (FR-C6, and DEC-2 before it). The turn
// has already done work by the time a tool runs; failing it because a remote
// server is unreachable throws that away, while telling the agent lets it say so
// or try another way. This holds for a transport failure as much as for a tool
// that reports one -- the agent cannot act on the difference, and the operator
// reads it in the log.
func (t *Tool) Invoke(ctx context.Context, args json.RawMessage) (domain.Result, error) {
	text, reported, err := t.client.CallTool(ctx, t.schema.Name, args)
	if err != nil {
		return domain.Result{Content: fmt.Sprintf(
			"tool %q is unavailable: %v", t.schema.Name, err)}, nil
	}
	if reported {
		return domain.Result{Content: fmt.Sprintf(
			"tool %q reported an error: %s", t.schema.Name, strings.TrimSpace(text))}, nil
	}
	if strings.TrimSpace(text) == "" {
		// An empty result reads to a model as a tool that did nothing. Saying so
		// is the difference between the agent moving on and the agent retrying.
		return domain.Result{Content: "(no content returned)"}, nil
	}
	return domain.Result{Content: text}, nil
}

// Connect initializes a server and returns its tools.
//
// Both steps happen at BOOT, and a failure of either is returned rather than
// swallowed: an agent told it has a memory that is not there is the failure this
// whole path exists to prevent. What happens LATER, mid-turn, is the opposite --
// see Tool.Invoke.
func Connect(
	ctx context.Context, name, url string, headers map[string]string, timeout time.Duration,
) (*Client, []*Tool, error) {
	c := New(name, url, headers, timeout)
	if err := c.Initialize(ctx); err != nil {
		return nil, nil, err
	}
	descriptors, err := c.ListTools(ctx)
	if err != nil {
		return nil, nil, err
	}
	out := make([]*Tool, 0, len(descriptors))
	for _, d := range descriptors {
		if strings.TrimSpace(d.Name) == "" {
			continue // a tool with no name cannot be called or reported
		}
		schema := domain.ToolSchema{
			Name:        d.Name,
			Description: d.Description,
			Parameters:  d.InputSchema,
		}
		// Absent AND explicit null: a server that omits inputSchema and one that
		// writes `"inputSchema": null` mean the same thing, and only the first
		// arrives here as an empty slice.
		if len(schema.Parameters) == 0 || string(schema.Parameters) == "null" {
			schema.Parameters = emptyObjectSchema
		}
		out = append(out, &Tool{client: c, schema: schema})
	}
	return c, out, nil
}
