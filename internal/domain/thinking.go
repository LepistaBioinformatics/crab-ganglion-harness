package domain

// Reasoning depth, the part of it the domain owns.
//
// The domain carries the LEVEL NAME as an opaque string and never the wire
// shape, and it does not own the vocabulary either -- that is config's, read
// from picoclaw's `thinking_level` key. What lives here is the one thing
// neither a tool nor a provider can hold: the cell a tool WRITES and the loop
// READS, whose lifetime is exactly one turn.

import (
	"context"
	"sync"
)

// Depth is the level the agent chose, for the remainder of one turn.
//
// A cell rather than a return value because of who sets it and who reads it: a
// tool sets it, and the LOOP reads it on the next completion. The two never
// meet -- a tool returns a domain.Result, and the loop learns nothing from it
// beyond text. The alternatives were widening domain.Result with a control
// field used by exactly one tool, or having the runtime recognise a tool by
// name, which is the coupling AR-3 exists to prevent.
//
// The loop makes one of these per turn and puts it in the context, so the
// lifetime is the turn and nothing outlives it. The mutex is not decoration:
// once sub-agents land, several children may hold the same context.
type Depth struct {
	mu    sync.Mutex
	level string
	// reason is what the agent said when it chose. Not used for control flow;
	// it is shown to the member and recorded on the turn.
	reason string
	// suppressed names the models that failed while carrying a depth field.
	suppressed map[string]bool
}

// Set records a level. A nil Depth is a no-op, so a tool invoked outside a turn
// -- in a test, or by a future caller that has no cell -- does not panic.
func (d *Depth) Set(level, reason string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.level, d.reason = level, reason
	d.mu.Unlock()
}

// Level is the chosen level, or "" when the agent has not chosen one and the
// model's own configured level therefore applies.
func (d *Depth) Level() string {
	if d == nil {
		return ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.level
}

// Reason is the one line the agent gave, or "".
func (d *Depth) Reason() string {
	if d == nil {
		return ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reason
}

// Suppress records that this model must be sent no depth field for the rest of
// the turn, because a request carrying one already failed on it.
//
// FOR THE REST OF THE TURN, and no longer. The failure may have been the
// endpoint's rather than the model's, and a harness that remembered it across
// turns would quietly stop thinking for a deployment that had one bad minute.
func (d *Depth) Suppress(model string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.suppressed == nil {
		d.suppressed = map[string]bool{}
	}
	d.suppressed[model] = true
	d.mu.Unlock()
}

// Suppressed reports whether Suppress was called for this model.
func (d *Depth) Suppressed(model string) bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.suppressed[model]
}

// depthKey is unexported and of an unexported type, so nothing outside this
// package can put a value under it or read one out by accident.
type depthKey struct{}

// WithDepth attaches a turn's depth cell to a context. The loop calls this once
// per turn; a tool reads it with DepthFrom.
func WithDepth(ctx context.Context, d *Depth) context.Context {
	return context.WithValue(ctx, depthKey{}, d)
}

// DepthFrom returns the turn's depth cell, or nil. Every method on *Depth is
// nil-safe, so a caller never has to check.
func DepthFrom(ctx context.Context) *Depth {
	d, _ := ctx.Value(depthKey{}).(*Depth)
	return d
}
