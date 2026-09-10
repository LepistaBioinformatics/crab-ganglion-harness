// Package tool is the ToolExecutor adapter, and the second place the hexagonal
// shape repeats (AR-3): one port outward (domain.ToolExecutor), one port inward
// (Tool), one adapter per tool.
//
// Adding a tool is a file plus a registration line -- not a change to the loop,
// the port, or this registry.
package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// Tool is the inward port. Every concrete tool implements it.
type Tool interface {
	Name() string
	Schema() domain.ToolSchema
	Invoke(ctx context.Context, args json.RawMessage) (domain.Result, error)
}

// Registry dispatches calls to the registered Tool with a matching name.
type Registry struct {
	tools map[string]Tool
	order []string
}

func NewRegistry(ts ...Tool) *Registry {
	r := &Registry{tools: map[string]Tool{}}
	for _, t := range ts {
		r.Register(t)
	}
	return r
}

func (r *Registry) Register(t Tool) {
	if _, dup := r.tools[t.Name()]; !dup {
		r.order = append(r.order, t.Name())
	}
	r.tools[t.Name()] = t
}

func (r *Registry) Available(context.Context) []domain.ToolSchema {
	out := make([]domain.ToolSchema, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.tools[n].Schema())
	}
	return out
}

// Invoke runs the named tool.
//
// An unknown name is a Result, not an error: models hallucinate tool names, and
// failing the turn over one throws away the work already done when telling the
// agent "there is no such tool" lets it recover. Same reasoning as a denial
// (DEC-2).
func (r *Registry) Invoke(ctx context.Context, call domain.ToolCall) (domain.Result, error) {
	t, ok := r.tools[call.Name]
	if !ok {
		return domain.Result{Content: fmt.Sprintf("There is no tool named %q.", call.Name)}, nil
	}
	return t.Invoke(ctx, call.Args)
}
